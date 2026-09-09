package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"macbridge/internal/core"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) (*HTTP, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := core.Config{Home: dir, DataDir: dir, LogDir: filepath.Join(dir, "logs"), UnlockFile: filepath.Join(dir, "unlock")}
	token := strings.Repeat("a", 40)
	o, err := NewOAuth(cfg, token)
	if err != nil {
		t.Fatal(err)
	}
	engine := &Engine{Config: cfg, Acknowledged: true, State: func() map[string]any { return nil }, List: func(context.Context) ([]core.Tool, error) {
		return []core.Tool{{Name: "echo", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []any{"text"}, "additionalProperties": false}}}, nil
	}, Execute: func(ctx context.Context, _ string, args map[string]any) (any, error) { return args, nil }}
	return NewHTTP(engine, o), token
}
func request(h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:23456"
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if strings.HasPrefix(path, "/token") || path == "/authorize" || path == "/revoke" {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(w.Body.String(), err)
	}
	return v
}
func TestProtocolsAndIsolation(t *testing.T) {
	h, token := fixture(t)
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"ok"}}}`
	if w := request(h, "POST", "/mcp", call, ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	w := request(h, "POST", "/mcp", call, token)
	if decode(t, w)["error"].(map[string]any)["code"] != float64(-32002) {
		t.Fatal(w.Body.String())
	}
	request(h, "POST", "/mcp", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`, token)
	if w = request(h, "POST", "/mcp", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, token); w.Code != 202 {
		t.Fatal(w.Code)
	}
	w = request(h, "POST", "/mcp", call, token)
	if !strings.Contains(w.Body.String(), `"text":"ok"`) {
		t.Fatal(w.Body.String())
	}
	modern := strings.Replace(call, `"name":"echo"`, `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"},"name":"echo"`, 1)
	if w = request(h, "POST", "/mcp", modern, token); !strings.Contains(w.Body.String(), `"resultType":"complete"`) {
		t.Fatal(w.Body.String())
	}
	for _, bad := range []string{"null", "[]", "1", "{} {}"} {
		if w = request(h, "POST", "/mcp", bad, token); w.Code != 400 {
			t.Fatal(bad, w.Code)
		}
	}
	w = request(h, "POST", "/mcp", strings.Replace(modern, `"text":"ok"`, `"text":22`, 1), token)
	if !strings.Contains(w.Body.String(), "INVALID_ARGUMENT") {
		t.Fatal(w.Body.String())
	}
	r := httptest.NewRequest("POST", "http://localhost/v1/responses", strings.NewReader(`{}`))
	r.RemoteAddr = "127.0.0.1:9"
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("CF-Ray", "tunnel")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
}
func TestOAuthPKCERotationPersistence(t *testing.T) {
	h, token := fixture(t)
	verifier := strings.Repeat("v", 60)
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {h.OAuth.ClientID()}, "redirect_uri": {"https://chatgpt.com/connector/oauth/test_123"}, "response_type": {"code"}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "state": {"original-state"}, "resource": {"http://localhost/mcp"}}
	w := request(h, "GET", "/authorize?"+q.Encode(), "", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	rid := regexp.MustCompile(`name="rid" value="([^"]+)"`).FindStringSubmatch(w.Body.String())
	if len(rid) != 2 {
		t.Fatal(w.Body.String())
	}
	w = request(h, "POST", "/authorize", url.Values{"rid": {rid[1]}, "bearer": {token}}.Encode(), "")
	if w.Code != 302 {
		t.Fatal(w.Code, w.Body.String())
	}
	redirect, _ := url.Parse(w.Header().Get("Location"))
	if redirect.Query().Get("state") != "original-state" {
		t.Fatal(redirect)
	}
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {h.OAuth.ClientID()}, "code": {redirect.Query().Get("code")}, "redirect_uri": {q.Get("redirect_uri")}, "code_verifier": {verifier}, "resource": {"http://localhost/mcp"}}
	w = request(h, "POST", "/token", form.Encode(), "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	tokens := decode(t, w)
	if w = request(h, "POST", "/token", form.Encode(), ""); w.Code != 400 {
		t.Fatal("code replay", w.Code)
	}
	access := tokens["access_token"].(string)
	if w = request(h, "POST", "/mcp", `{"jsonrpc":"2.0","id":2,"method":"ping"}`, access); w.Code != 200 {
		t.Fatal(w.Code)
	}
	again, err := NewOAuth(h.Engine.Config, token)
	if err != nil {
		t.Fatal(err)
	}
	h.OAuth = again
	w = request(h, "POST", "/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {again.ClientID()}, "refresh_token": {tokens["refresh_token"].(string)}}.Encode(), "")
	if w.Code != 200 {
		t.Fatal("persisted refresh", w.Code, w.Body.String())
	}
	if w = request(h, "POST", "/revoke-all", "", access); w.Code != 403 {
		t.Fatal("OAuth must not revoke all")
	}
	w = request(h, "POST", "/revoke-all", "", token)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w = request(h, "POST", "/mcp", `{}`, access); w.Code != 401 {
		t.Fatal("revoked access still live")
	}
	rotated, err := NewOAuth(h.Engine.Config, strings.Repeat("b", 40))
	if err != nil {
		t.Fatal(err)
	}
	if len(rotated.state.Access) != 0 || len(rotated.state.Refresh) != 0 {
		t.Fatal("rotation did not revoke")
	}
}
func TestRedirectValidation(t *testing.T) {
	h, _ := fixture(t)
	for _, s := range []string{"https://chatgpt.com.evil.test/connector/oauth/x", "https://chatgpt.com@evil.test/connector/oauth/x", "https://chatgpt.com:443/connector/oauth/x", "https://chatgpt.com/connector/oauth/x?next=evil"} {
		if h.OAuth.validRedirect(s) {
			t.Fatal(s)
		}
	}
}
func TestSSEOutputAndStdioEOF(t *testing.T) {
	h, token := fixture(t)
	h.Responses = func(context.Context, map[string]any) (map[string]any, error) {
		return map[string]any{"id": "resp_test", "object": "response", "status": "completed", "output": []any{map[string]any{"id": "call_1", "type": "function_call", "name": "echo", "call_id": "c1", "arguments": `{"text":"ok"}`, "status": "completed"}}}, nil
	}
	w := request(h, "POST", "/v1/responses", `{"stream":true}`, token)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "response.function_call_arguments.done") || !strings.Contains(w.Body.String(), "response.completed") {
		t.Fatal(w.Code, w.Body.String())
	}
	var out bytes.Buffer
	input := strings.NewReader("bad-json\n{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"ping\"}\n")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.Engine.ServeStdio(ctx, input, &out); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&out)
	count := 0
	for {
		var v any
		if err := decoder.Decode(&v); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		count++
	}
	if count != 2 {
		t.Fatal(count)
	}
}

func TestCancelledCallDoesNotBeginSideEffects(t *testing.T) {
	h, _ := fixture(t)
	executed := false
	h.Engine.Execute = func(context.Context, string, map[string]any) (any, error) { executed = true; return nil, nil }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.Engine.Call(ctx, "echo", map[string]any{"text": "cancelled"}); err != context.Canceled {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if executed {
		t.Fatal("Already-cancelled call executed side effects")
	}
	// Cancellation while discovering a federated tool must also stop dispatch.
	h.Engine.List = func(context.Context) ([]core.Tool, error) {
		cancel()
		return []core.Tool{{Name: "provider__echo", InputSchema: map[string]any{"type": "object"}}}, nil
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	if _, err := h.Engine.Call(ctx, "provider__echo", map[string]any{}); err != context.Canceled {
		t.Fatalf("expected discovery cancellation, got %v", err)
	}
	if executed {
		t.Fatal("Discovery-cancelled call executed side effects")
	}
}

func TestHTTPStatefulCancellation(t *testing.T) {
	h, token := fixture(t)
	started := make(chan struct{})
	h.Engine.Execute = func(ctx context.Context, _ string, args map[string]any) (any, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	initialize := request(h, "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`, token)
	sessionID := initialize.Header().Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatal("initialize did not return session ID")
	}
	send := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "http://localhost/mcp", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Mcp-Session-Id", sessionID)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- send(`{"jsonrpc":"2.0","id":77,"method":"tools/call","params":{"name":"echo","arguments":{"text":"wait"}}}`)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("call did not start")
	}
	if w := send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":77}}`); w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	select {
	case w := <-done:
		if !strings.Contains(w.Body.String(), "context canceled") {
			t.Fatal(w.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("Stateful cancellation did not stop the active call")
	}
}

func TestStdioNotificationsHaveNoResponse(t *testing.T) {
	h, _ := fixture(t)
	input := strings.NewReader("{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n{\"jsonrpc\":\"2.0\",\"method\":\"notifications/cancelled\",\"params\":{\"requestId\":7}}\n{\"jsonrpc\":\"2.0\",\"id\":8,\"method\":\"ping\"}\n")
	var output bytes.Buffer
	if err := h.Engine.ServeStdio(context.Background(), input, &output); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&output)
	var first map[string]any
	if err := decoder.Decode(&first); err != nil || first["id"] != float64(8) {
		t.Fatal(first, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("Notification emitted response: %#v (%v)", extra, err)
	}
}

func TestDefaultSettingsAndOriginalForegroundGrant(t *testing.T) {
	h, _ := fixture(t)
	result, err := h.Engine.Call(context.Background(), "bridge_status", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	status := result.StructuredContent.(map[string]any)
	if status["operatorSettings"].(map[string]any)["strictApprovals"] != false {
		t.Fatal(status)
	}
	if err = core.AtomicJSON(filepath.Join(h.Engine.Config.DataDir, "settings.json"), map[string]any{"strictApprovals": true}); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"command": `if false; then osascript -e 'tell application "Slack" to activate'; fi`}
	if err = h.Engine.focusPolicy(args); err == nil {
		t.Fatal("Strict approval was not required")
	}
	file := filepath.Join(h.Engine.Config.DataDir, "FOREGROUND_GUI_APPROVED")
	if err = core.AtomicJSON(file, map[string]any{"nonce": strings.Repeat("a", 32), "allowedApps": []string{"Slack"}, "expiresAt": time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	if err = h.Engine.focusPolicy(args); err != nil {
		t.Fatal("Original allowedApps grant rejected", err)
	}
	if _, err = os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("Foreground grant was not consumed")
	}
}
