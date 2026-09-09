package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"macbridge/internal/core"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type HTTP struct {
	Engine            *Engine
	OAuth             *OAuth
	Responses         func(context.Context, map[string]any) (map[string]any, error)
	ValidateResponses func(map[string]any) error
	sessions          map[string]*Session
	session           Session
	slots             chan struct{}
	Timeout           time.Duration
	streamKeepalive   time.Duration
	mu                sync.Mutex
}

func NewHTTP(e *Engine, o *OAuth) *HTTP {
	return &HTTP{Engine: e, OAuth: o, slots: make(chan struct{}, 64), sessions: map[string]*Session{}, streamKeepalive: 15 * time.Second, Timeout: time.Duration(core.EnvInt("MAC_DEV_BRIDGE_HTTP_TIMEOUT_MS", 600000)) * time.Millisecond}
}
func readObject(w http.ResponseWriter, r *http.Request) (map[string]any, json.RawMessage, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 8*1024*1024)
	decoder := json.NewDecoder(r.Body)
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return nil, nil, err
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, nil, err
	}
	if v == nil {
		return nil, nil, fmt.Errorf("JSON object required")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, nil, fmt.Errorf("Exactly one JSON object required")
	}
	return v, raw, nil
}
func directLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		return false
	}
	for _, header := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "CF-Connecting-IP", "CF-Ray", "X-Real-IP"} {
		if r.Header.Get(header) != "" {
			return false
		}
	}
	return true
}
func (h *HTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		jsonResponse(w, 503, map[string]any{"error": "Too many active requests"})
		return
	}
	if r.URL.Path == "/healthz" {
		jsonResponse(w, 200, map[string]any{"ok": true, "version": Version, "implementation": "Go", "bridgeUnlocked": h.Engine.Unlocked()})
		return
	}
	if h.OAuth.Discovery(w, r) {
		return
	}
	switch r.URL.Path {
	case "/authorize":
		h.OAuth.Authorize(w, r)
		return
	case "/token":
		h.OAuth.Token(w, r)
		return
	case "/revoke":
		h.OAuth.Revoke(w, r)
		return
	case "/revoke-all":
		h.OAuth.RevokeAll(w, r)
		return
	}
	auth := h.OAuth.Auth(r)
	if auth == "" {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+h.OAuth.origin(r)+`/.well-known/oauth-protected-resource/mcp", scope="mcp"`)
		jsonResponse(w, 401, map[string]any{"error": "Unauthorized"})
		return
	}
	if r.Method == "DELETE" && r.URL.Path == "/mcp" {
		h.mu.Lock()
		id := r.Header.Get("Mcp-Session-Id")
		session := h.sessions[id]
		delete(h.sessions, id)
		h.mu.Unlock()
		if session != nil {
			session.close()
		}
		w.WriteHeader(204)
		return
	}
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST, DELETE")
		jsonResponse(w, 405, map[string]any{"error": "POST required"})
		return
	}
	switch r.URL.Path {
	case "/mcp", "/experimental/chatgpt/conversation", "/v1/responses":
	default:
		jsonResponse(w, 404, map[string]any{"error": "Not found"})
		return
	}
	if r.URL.Path != "/mcp" && (auth != "bearer" || !directLoopback(r)) {
		jsonResponse(w, 403, map[string]any{"error": "This route requires direct loopback and the static bearer"})
		return
	}
	body, raw, err := readObject(w, r)
	if err != nil {
		jsonResponse(w, 400, map[string]any{"error": err.Error()})
		return
	}
	timeout := h.Timeout
	if r.URL.Path != "/mcp" {
		seconds := core.Int(body, "max_runtime_seconds", 600)
		if r.URL.Path == "/v1/responses" {
			seconds = core.EnvInt("MAC_DEV_BRIDGE_CHATGPT_RESPONSES_MAX_RUNTIME_SECONDS", 600)
		}
		seconds = max(30, min(3600, seconds))
		timeout = time.Duration(seconds+120) * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	switch r.URL.Path {
	case "/experimental/chatgpt/conversation":
		value, err := h.Engine.Call(ctx, "chatgpt_conversation_start", body)
		if err != nil {
			status, code, _ := responseErrorDetails(err)
			jsonResponse(w, status, map[string]any{"error": err.Error(), "code": code})
			return
		}
		status := 200
		if value.IsError {
			structured, _ := value.StructuredContent.(map[string]any)
			status = browserFailureStatus(core.String(structured, "code", ""))
		}
		jsonResponse(w, status, value.StructuredContent)
	case "/v1/responses":
		if h.Responses == nil {
			jsonResponse(w, 503, map[string]any{"error": "Responses adapter unavailable"})
			return
		}
		if h.ValidateResponses != nil {
			if err := h.ValidateResponses(body); err != nil {
				responseFailure(w, err)
				return
			}
		}
		stream := core.Bool(body, "stream", false)
		if !stream {
			v, err := h.Responses(ctx, body)
			if err != nil {
				responseFailure(w, err)
				return
			}
			jsonResponse(w, 200, v)
			return
		}
		h.responsesStream(w, ctx, body)
	case "/mcp":
		req, err := parseRequest(raw)
		if err != nil {
			jsonResponse(w, 400, rpcError(nil, -32600, "Invalid Request"))
			return
		}
		session := h.httpSession(w, r, req)
		if session == nil {
			return
		}
		value := h.Engine.RPC(ctx, req, session)
		if value == nil {
			w.WriteHeader(202)
			return
		}
		if strings.Contains(r.Header.Get("Accept"), "text/event-stream") && !strings.Contains(r.Header.Get("Accept"), "application/json") {
			w.Header().Set("Content-Type", "text/event-stream")
			event(w, "message", value)
		} else {
			jsonResponse(w, 200, value)
		}
	}
}
func event(w http.ResponseWriter, name string, v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
func StreamResponse(w http.ResponseWriter, response map[string]any) {
	streamResponse(w, response, 0, false)
}
func streamResponse(w http.ResponseWriter, response map[string]any, seq int, started bool) {
	send := func(name string, v map[string]any) {
		v["type"] = name
		v["sequence_number"] = seq
		seq++
		event(w, name, v)
	}
	initial := map[string]any{}
	for k, v := range response {
		initial[k] = v
	}
	initial["status"] = "in_progress"
	initial["output"] = []any{}
	if !started {
		send("response.created", map[string]any{"response": initial})
		send("response.in_progress", map[string]any{"response": initial})
	}
	// Normalize slices produced either by Go implementations or JSON decoding.
	raw, _ := json.Marshal(response["output"])
	var outputs []map[string]any
	json.Unmarshal(raw, &outputs)
	for index, item := range outputs {
		id := item["id"]
		added := map[string]any{}
		for k, v := range item {
			added[k] = v
		}
		added["status"] = "in_progress"
		if item["type"] == "message" {
			added["content"] = []any{}
		}
		if item["type"] == "function_call" {
			added["arguments"] = ""
		}
		if item["type"] == "custom_tool_call" {
			added["input"] = ""
		}
		send("response.output_item.added", map[string]any{"output_index": index, "item": added})
		switch item["type"] {
		case "message":
			b, _ := json.Marshal(item["content"])
			var parts []map[string]any
			json.Unmarshal(b, &parts)
			for ci, part := range parts {
				send("response.content_part.added", map[string]any{"item_id": id, "output_index": index, "content_index": ci, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
				send("response.output_text.delta", map[string]any{"item_id": id, "output_index": index, "content_index": ci, "delta": part["text"]})
				send("response.output_text.done", map[string]any{"item_id": id, "output_index": index, "content_index": ci, "text": part["text"]})
				send("response.content_part.done", map[string]any{"item_id": id, "output_index": index, "content_index": ci, "part": part})
			}
		case "function_call":
			send("response.function_call_arguments.delta", map[string]any{"item_id": id, "output_index": index, "delta": item["arguments"]})
			send("response.function_call_arguments.done", map[string]any{"item_id": id, "output_index": index, "arguments": item["arguments"]})
		case "custom_tool_call":
			send("response.custom_tool_call_input.delta", map[string]any{"item_id": id, "output_index": index, "delta": item["input"]})
			send("response.custom_tool_call_input.done", map[string]any{"item_id": id, "output_index": index, "input": item["input"]})
		}
		send("response.output_item.done", map[string]any{"output_index": index, "item": item})
	}
	send("response.completed", map[string]any{"response": response})
	fmt.Fprint(w, "data: [DONE]\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func browserFailureStatus(code string) int {
	code = strings.ToUpper(code)
	for _, group := range []struct {
		patterns []string
		status   int
	}{
		{[]string{"INVALID", "REFUSED", "UNKNOWN", "SECURITY_FIELDS"}, 400},
		{[]string{"TAB_UNAVAILABLE", "SESSION_UNAVAILABLE", "PROFILE", "EXTENSION_OFFLINE", "MODEL_MISMATCH", "NOT_READY", "NOT_NEW_THREAD"}, 409},
		{[]string{"REQUIREMENTS_UNAVAILABLE", "RUNTIME_CONTRACT_CHANGED"}, 424},
		{[]string{"TIMEOUT"}, 504},
	} {
		for _, pattern := range group.patterns {
			if strings.Contains(code, pattern) {
				return group.status
			}
		}
	}
	return 502
}
func responseErrorDetails(err error) (status int, code, kind string) {
	status, code = 502, "browser_response_failed"
	var fault *core.Fault
	if errors.As(err, &fault) {
		code, status = fault.Code, browserFailureStatus(fault.Code)
	}
	var withStatus interface{ HTTPStatus() int }
	var withCode interface{ ErrorCode() string }
	if errors.As(err, &withStatus) {
		status = withStatus.HTTPStatus()
	}
	if errors.As(err, &withCode) {
		code = withCode.ErrorCode()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		status, code = 504, "browser_response_timeout"
	}
	kind = "server_error"
	if status < 500 {
		kind = "invalid_request_error"
	}
	return
}
func responseFailure(w http.ResponseWriter, err error) {
	status, code, kind := responseErrorDetails(err)
	jsonResponse(w, status, map[string]any{"error": map[string]any{"message": err.Error(), "type": kind, "code": code}})
}
func (h *HTTP) responsesStream(w http.ResponseWriter, ctx context.Context, body map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	pending := map[string]any{"id": "resp_" + core.ID(), "object": "response", "created_at": time.Now().Unix(), "model": strings.TrimPrefix(core.String(body, "model", ""), "chatgpt-runtime/"), "status": "in_progress", "output": []any{}, "error": nil}
	event(w, "response.created", map[string]any{"type": "response.created", "sequence_number": 0, "response": pending})
	event(w, "response.in_progress", map[string]any{"type": "response.in_progress", "sequence_number": 1, "response": pending})
	type outcome struct {
		value map[string]any
		err   error
	}
	completed := make(chan outcome, 1)
	go func() { value, err := h.Responses(ctx, body); completed <- outcome{value, err} }()
	interval := h.streamKeepalive
	if interval <= 0 {
		interval = 15 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	fail := func(err error) {
		_, code, _ := responseErrorDetails(err)
		pending["status"] = "failed"
		pending["error"] = map[string]any{"code": code, "message": err.Error()}
		event(w, "response.failed", map[string]any{"type": "response.failed", "sequence_number": 2, "response": pending})
		fmt.Fprint(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	for {
		select {
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				fail(ctx.Err())
			}
			return
		case result := <-completed:
			if result.err != nil {
				fail(result.err)
				return
			}
			if result.value == nil {
				fail(fmt.Errorf("Responses adapter returned no response"))
				return
			}
			// All events for one request must retain the id announced before generation.
			response := make(map[string]any, len(result.value))
			for key, value := range result.value {
				response[key] = value
			}
			response["id"], response["created_at"] = pending["id"], pending["created_at"]
			streamResponse(w, response, 2, true)
			return
		}
	}
}
func (h *HTTP) httpSession(w http.ResponseWriter, r *http.Request, req Request) *Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	if id := r.Header.Get("Mcp-Session-Id"); id != "" {
		s := h.sessions[id]
		if s == nil {
			jsonResponse(w, 404, map[string]any{"error": "Unknown MCP session"})
			return nil
		}
		return s
	}
	if req.Method == "initialize" {
		if len(h.sessions) >= 256 {
			jsonResponse(w, 503, map[string]any{"error": "MCP session limit reached; restart or close unused sessions"})
			return nil
		}
		id := core.ID()
		s := &Session{}
		h.sessions[id] = s
		w.Header().Set("Mcp-Session-Id", id)
		return s
	}
	if req.Method == "initialized" || req.Method == "notifications/initialized" {
		h.session.setInitialized()
	}
	// Headerless requests are stateless: equal JSON-RPC IDs in separate HTTP calls
	// cannot cancel or replace each other's contexts. Legacy readiness is retained.
	return &Session{initialized: h.session.ready()}
}
