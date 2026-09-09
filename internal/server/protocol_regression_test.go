package server

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"macbridge/internal/core"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

func authorizeCode(t *testing.T, h *HTTP, token, resource string) (string, string) {
	t.Helper()
	verifier := strings.Repeat("v", 60)
	sum := sha256.Sum256([]byte(verifier))
	query := url.Values{"client_id": {h.OAuth.ClientID()}, "redirect_uri": {"https://chatgpt.com/connector/oauth/regression"}, "response_type": {"code"}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "resource": {resource}}
	page := request(h, "GET", "/authorize?"+query.Encode(), "", "")
	if page.Code != 200 {
		t.Fatal(page.Code, page.Body.String())
	}
	if !strings.Contains(page.Header().Get("Content-Security-Policy"), "form-action 'self' https://chatgpt.com;") {
		t.Fatal("Consent redirect would be blocked", page.Header())
	}
	match := regexp.MustCompile(`name="rid" value="([^"]+)"`).FindStringSubmatch(page.Body.String())
	if len(match) != 2 {
		t.Fatal(page.Body.String())
	}
	approved := request(h, "POST", "/authorize", url.Values{"rid": {match[1]}, "bearer": {token}}.Encode(), "")
	if approved.Code != 302 {
		t.Fatal(approved.Code, approved.Body.String())
	}
	destination, err := url.Parse(approved.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return destination.Query().Get("code"), verifier
}
func TestOAuthOriginalClientCompatibility(t *testing.T) {
	t.Setenv("MAC_DEV_BRIDGE_OAUTH_CLIENT_ID", "bridge client/+")
	t.Setenv("MAC_DEV_BRIDGE_OAUTH_CLIENT_SECRET", "space : plus+ slash/%")
	h, token := fixture(t)
	for _, path := range []string{"/.well-known/oauth-authorization-server", "/.well-known/oauth-protected-resource/mcp"} {
		options := request(h, "OPTIONS", path, "", "")
		if options.Code != 204 || options.Body.Len() != 0 || options.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Fatal(options.Code, options.Header(), options.Body.String())
		}
		if request(h, "POST", path, "", "").Code != 405 {
			t.Fatal("Discovery accepted POST")
		}
	}
	code, verifier := authorizeCode(t, h, token, "HTTP://LOCALHOST/mcp/")
	// An omitted redirect_uri and a root resource alias are accepted by the original.
	raw, _ := json.Marshal(map[string]any{"grant_type": "authorization_code", "code": code, "code_verifier": verifier, "resource": "http://localhost/"})
	r := httptest.NewRequest("POST", "http://localhost/token", strings.NewReader(string(raw)))
	r.Header.Set("Content-Type", "application/json")
	r.SetBasicAuth(url.QueryEscape(h.OAuth.ClientID()), url.QueryEscape(h.OAuth.secret))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	tokens := decode(t, w)
	refresh := tokens["refresh_token"].(string)
	// Repeated refresh inside the two-minute window and an omitted optional secret work.
	var newest string
	for range 2 {
		w = request(h, "POST", "/token", url.Values{"client_id": {h.OAuth.ClientID()}, "grant_type": {"refresh_token"}, "refresh_token": {refresh}, "resource": {"http://localhost/mcp/"}}.Encode(), "")
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		newest = decode(t, w)["refresh_token"].(string)
	}
	h.OAuth.mu.Lock()
	old := h.OAuth.state.Refresh[digest(refresh)]
	old.Expires = time.Now().Add(-time.Second).UnixMilli()
	h.OAuth.state.Refresh[digest(refresh)] = old
	h.OAuth.mu.Unlock()
	w = request(h, "POST", "/token", url.Values{"client_id": {h.OAuth.ClientID()}, "grant_type": {"refresh_token"}, "refresh_token": {refresh}}.Encode(), "")
	if w.Code != 400 {
		t.Fatal("Expired grace token accepted", w.Code)
	}
	w = request(h, "POST", "/token", url.Values{"client_id": {h.OAuth.ClientID()}, "grant_type": {"refresh_token"}, "refresh_token": {newest}}.Encode(), "")
	if w.Code != 200 {
		t.Fatal("Expired parent invalidated newest token", w.Code, w.Body.String())
	}
	access := decode(t, w)["access_token"].(string)
	r = httptest.NewRequest("POST", "http://localhost/revoke", strings.NewReader(`{"token":"`+access+`"}`))
	r.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || request(h, "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, access).Code != 401 {
		t.Fatal("JSON revocation failed", w.Code, w.Body.String())
	}
}

func TestResponsesEarlyEventsHeartbeatAndIdentity(t *testing.T) {
	h, token := fixture(t)
	finish := make(chan struct{})
	defer close(finish)
	h.streamKeepalive = 5 * time.Millisecond
	h.Responses = func(ctx context.Context, _ map[string]any) (map[string]any, error) {
		select {
		case <-finish:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return map[string]any{"id": "resp_callback", "status": "completed", "object": "response", "output": []any{map[string]any{"id": "msg_1", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "中文 answer", "annotations": []any{}}}}}}, nil
	}
	host := httptest.NewServer(h)
	defer host.Close()
	req, _ := http.NewRequest("POST", host.URL+"/v1/responses", strings.NewReader(`{"model":"chatgpt-browser","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: time.Second}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	scanner := bufio.NewScanner(response.Body)
	nextData := func() string {
		t.Helper()
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				return strings.TrimPrefix(line, "data: ")
			}
		}
		t.Fatal("Missing SSE data", scanner.Err())
		return ""
	}
	var created map[string]any
	if err = json.Unmarshal([]byte(nextData()), &created); err != nil {
		t.Fatal(err)
	}
	if created["type"] != "response.created" {
		t.Fatal(created)
	}
	pending := created["response"].(map[string]any)
	if pending["status"] != "in_progress" {
		t.Fatal(pending)
	}
	if !strings.Contains(nextData(), `"type":"response.in_progress"`) {
		t.Fatal("Missing early in-progress event")
	}
	heartbeat := false
	for scanner.Scan() {
		if scanner.Text() == ": keep-alive" {
			heartbeat = true
			break
		}
	}
	if !heartbeat {
		t.Fatal("Missing heartbeat while generation is pending")
	}
	finish <- struct{}{}
	sequence := 2
	var terminal map[string]any
	for {
		raw := nextData()
		if raw == "[DONE]" {
			break
		}
		var value map[string]any
		if err = json.Unmarshal([]byte(raw), &value); err != nil {
			t.Fatal(err)
		}
		if value["sequence_number"] != float64(sequence) {
			t.Fatal(sequence, value)
		}
		sequence++
		if value["type"] == "response.completed" {
			terminal = value["response"].(map[string]any)
		}
	}
	if terminal == nil || terminal["id"] != pending["id"] || terminal["created_at"] != pending["created_at"] {
		t.Fatal("SSE response identity changed", pending, terminal)
	}
}

type protocolTestError struct{}

func (protocolTestError) Error() string     { return "Page runtime unavailable" }
func (protocolTestError) HTTPStatus() int   { return 409 }
func (protocolTestError) ErrorCode() string { return "CHATGPT_NOT_READY" }
func TestResponsesFailuresAndConfiguredDeadline(t *testing.T) {
	h, token := fixture(t)
	h.Responses = func(context.Context, map[string]any) (map[string]any, error) {
		return nil, fmt.Errorf("generation failed: %w", protocolTestError{})
	}
	failed := request(h, "POST", "/v1/responses", `{"stream":true}`, token)
	if failed.Code != 200 || !strings.Contains(failed.Body.String(), `"type":"response.failed"`) || !strings.Contains(failed.Body.String(), `"code":"CHATGPT_NOT_READY"`) || !strings.HasSuffix(failed.Body.String(), "data: [DONE]\n\n") {
		t.Fatal(failed.Code, failed.Body.String())
	}
	nonstream := request(h, "POST", "/v1/responses", `{}`, token)
	if nonstream.Code != 409 {
		t.Fatal(nonstream.Code, nonstream.Body.String())
	}
	h.Responses = func(context.Context, map[string]any) (map[string]any, error) {
		return nil, errors.New("runtime transport broke")
	}
	nonstream = request(h, "POST", "/v1/responses", `{}`, token)
	if nonstream.Code != 502 || decode(t, nonstream)["error"].(map[string]any)["type"] != "server_error" {
		t.Fatal(nonstream.Code, nonstream.Body.String())
	}
	t.Setenv("MAC_DEV_BRIDGE_CHATGPT_RESPONSES_MAX_RUNTIME_SECONDS", "1200")
	h.Responses = func(ctx context.Context, _ map[string]any) (map[string]any, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) < 1310*time.Second {
			t.Fatal("Configured runtime shortened", deadline)
		}
		return map[string]any{"ok": true}, nil
	}
	if w := request(h, "POST", "/v1/responses", `{}`, token); w.Code != 200 {
		t.Fatal(w.Code)
	}
}
func TestClosedSessionAndUnknownNotifications(t *testing.T) {
	h, token := fixture(t)
	session := &Session{initialized: true}
	session.close()
	executed := false
	h.Engine.Execute = func(context.Context, string, map[string]any) (any, error) { executed = true; return nil, nil }
	result := h.Engine.RPC(context.Background(), Request{JSONRPC: "2.0", ID: json.RawMessage(`7`), Method: "tools/call", Params: map[string]any{"name": "echo", "arguments": map[string]any{"text": "late"}, "_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": Modern}}}, session)
	if executed || result["result"].(map[string]any)["isError"] != true {
		t.Fatal("Closed session dispatched a late tool call", result)
	}
	for _, method := range []string{"notifications/unknown", "unknown"} {
		body := `{"jsonrpc":"2.0","method":"` + method + `","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"` + Modern + `"}}}`
		if w := request(h, "POST", "/mcp", body, token); w.Code != 202 || w.Body.Len() != 0 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	var out strings.Builder
	if err := h.Engine.ServeStdio(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/unknown"}`+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatal("Unknown notification received a response", out.String())
	}
	// Exercise preparation after termination directly, without timing-dependent scheduling.
	ctx, finish := session.prepare(context.Background(), Request{Method: "tools/call", ID: json.RawMessage(`9`)})
	defer finish()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("Closed session accepted new work")
	}
}

func TestExperimentalToolErrorsUseOriginalStatus(t *testing.T) {
	h, token := fixture(t)
	if w := request(h, "POST", "/experimental/chatgpt/conversation", `{}`, token); w.Code != 400 {
		t.Fatal("Invalid prompt classified as server failure", w.Code, w.Body.String())
	}
	for _, test := range []struct {
		code   string
		status int
	}{{"CHROME_EXTENSION_OFFLINE", 409}, {"CHATGPT_RUNTIME_CONTRACT_CHANGED", 424}, {"CHATGPT_TIMEOUT", 504}} {
		h.Engine.Execute = func(context.Context, string, map[string]any) (any, error) {
			return nil, core.Error(test.code, "test failure")
		}
		w := request(h, "POST", "/experimental/chatgpt/conversation", `{"prompt":"test"}`, token)
		if w.Code != test.status || decode(t, w)["code"] != test.code {
			t.Fatal(test, w.Code, w.Body.String())
		}
	}
}

func TestJSONRPCLargeCorrelationIDsRemainExact(t *testing.T) {
	h, token := fixture(t)
	id := "9007199254740993"
	w := request(h, "POST", "/mcp", `{"jsonrpc":"2.0","id":`+id+`,"method":"ping"}`, token)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":`+id) {
		t.Fatal("Numeric request ID changed", w.Code, w.Body.String())
	}
	session := &Session{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	untrack := session.track(id, cancel)
	defer untrack()
	notification, err := parseRequest([]byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":` + id + `}}`))
	if err != nil {
		t.Fatal(err)
	}
	h.Engine.RPC(context.Background(), notification, session)
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("Cancellation ID was rounded")
	}
}
