package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"macbridge/internal/core"
	"sync"
)

type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  map[string]any  `json:"params,omitempty"`
}

// Keep correlation IDs in their original JSON representation. Tool arguments
// still use ordinary decoded Go values, so schemas and handlers retain their types.
func parseRequest(raw []byte) (Request, error) {
	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		return req, err
	}
	if req.Method == "notifications/cancelled" || req.Method == "notifications/cancelled_request" {
		var cancel struct {
			Params struct {
				RequestID json.RawMessage `json:"requestId"`
			} `json:"params"`
		}
		if err := json.Unmarshal(raw, &cancel); err != nil {
			return req, err
		}
		if cancel.Params.RequestID != nil {
			if req.Params == nil {
				req.Params = map[string]any{}
			}
			req.Params["requestId"] = cancel.Params.RequestID
		}
	}
	return req, nil
}

type Session struct {
	mu          sync.Mutex
	initialized bool
	closed      bool
	calls       map[string]map[uint64]context.CancelFunc
	next        uint64
}

func (s *Session) setInitialized() { s.mu.Lock(); s.initialized = true; s.mu.Unlock() }
func (s *Session) ready() bool     { s.mu.Lock(); defer s.mu.Unlock(); return s.initialized && !s.closed }
func (s *Session) cancel(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cancel := range s.calls[id] {
		cancel()
	}
}
func (s *Session) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for _, calls := range s.calls {
		for _, cancel := range calls {
			cancel()
		}
	}
}
func (s *Session) track(id string, cancel context.CancelFunc) func() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		cancel()
		return func() {}
	}
	if s.calls == nil {
		s.calls = map[string]map[uint64]context.CancelFunc{}
	}
	if s.calls[id] == nil {
		s.calls[id] = map[uint64]context.CancelFunc{}
	}
	s.next++
	token := s.next
	s.calls[id][token] = cancel
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		delete(s.calls[id], token)
		if len(s.calls[id]) == 0 {
			delete(s.calls, id)
		}
		s.mu.Unlock()
	}
}

type preparedCall struct{}

func (s *Session) prepare(ctx context.Context, req Request) (context.Context, func()) {
	if req.Method != "tools/call" {
		return ctx, func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	untrack := s.track(string(req.ID), cancel)
	return context.WithValue(ctx, preparedCall{}, true), func() { cancel(); untrack() }
}
func rpcError(id json.RawMessage, code int, message string) map[string]any {
	if id == nil {
		id = json.RawMessage("null")
	}
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}
}
func (e *Engine) RPC(ctx context.Context, req Request, session *Session) map[string]any {
	if req.JSONRPC != "2.0" || req.Method == "" {
		return rpcError(nil, -32600, "Invalid Request")
	}
	if req.ID != nil {
		var id any
		if json.Unmarshal(req.ID, &id) != nil {
			return rpcError(nil, -32600, "Invalid request ID")
		}
		switch id.(type) {
		case nil, string, float64:
		default:
			return rpcError(nil, -32600, "Invalid request ID")
		}
	}

	failure := func(code int, message string) map[string]any {
		if req.ID == nil {
			return nil
		}
		return rpcError(req.ID, code, message)
	}
	meta, _ := req.Params["_meta"].(map[string]any)
	modern := meta["io.modelcontextprotocol/protocolVersion"] == Modern || req.Method == "server/discover"
	result := map[string]any{}
	response := func() map[string]any {
		if req.ID == nil {
			return nil
		}
		if modern {
			result["resultType"] = "complete"
			if _, ok := result["_meta"]; !ok {
				result["_meta"] = map[string]any{"io.modelcontextprotocol/serverInfo": map[string]any{"name": "mac-developer-bridge", "version": Version}}
			}
		}
		return map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}
	}
	switch req.Method {
	case "notifications/initialized", "initialized":
		session.setInitialized()
		return nil
	case "notifications/cancelled", "notifications/cancelled_request":
		id, _ := json.Marshal(req.Params["requestId"])
		session.cancel(string(id))
		return nil
	case "initialize":
		version := core.String(req.Params, "protocolVersion", "2025-11-25")
		switch version {
		case "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05":
		default:
			version = "2025-11-25"
		}
		result = map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{"listChanged": false}}, "serverInfo": map[string]any{"name": "mac-developer-bridge", "title": "MacBridge Go", "version": Version}, "instructions": "Unrestricted local Mac tools. Use chrome_* for background browser work and codex_thread_* for stored history."}
		return response()
	case "server/discover":
		result = map[string]any{"supportedVersions": []string{Modern, "2025-11-25", "2025-06-18"}, "capabilities": map[string]any{"tools": map[string]any{"listChanged": false}}, "ttlMs": 3600000, "cacheScope": "private", "instructions": "Unrestricted local Mac tools; background jobs retain exit results. Browser operations use the MDB tab group."}
		return response()
	case "ping":
		return response()
	}
	if !modern && !session.ready() {
		return failure(-32002, "Server not initialized")
	}
	switch req.Method {
	case "tools/list":
		tools, err := e.List(ctx)
		if err != nil {
			return failure(-32603, err.Error())
		}
		result["tools"] = tools
		return response()
	case "tools/call":
		name := core.String(req.Params, "name", "")
		args, ok := req.Params["arguments"].(map[string]any)
		if !ok {
			if req.Params["arguments"] != nil {
				return failure(-32602, "arguments must be an object")
			}
			args = map[string]any{}
		}
		callCtx := ctx
		if ctx.Value(preparedCall{}) != true {
			var finish func()
			callCtx, finish = session.prepare(ctx, req)
			defer finish()
		}
		value, err := e.Call(callCtx, name, args)
		if err != nil {
			value = ErrorResult(err)
		}
		b, _ := json.Marshal(value)
		json.Unmarshal(b, &result)
		return response()
	default:
		return failure(-32601, "Method not found")
	}
}
func (e *Engine) ServeStdio(ctx context.Context, r io.Reader, w io.Writer) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 65536), 8*1024*1024)
	var mu sync.Mutex
	var wg sync.WaitGroup
	session := &Session{}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	write := func(v map[string]any) {
		if v == nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		json.NewEncoder(w).Encode(v)
	}
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		req, err := parseRequest(line)
		if err != nil {
			write(rpcError(nil, -32700, "Parse error"))
			continue
		}
		// Initialization notifications must be visible to the following call.
		if req.Method == "initialize" || req.Method == "initialized" || req.Method == "notifications/initialized" || req.Method == "notifications/cancelled" || req.Method == "notifications/cancelled_request" {
			write(e.RPC(ctx, req, session))
			continue
		}
		callCtx, finish := session.prepare(ctx, req)
		wg.Add(1)
		go func() { defer wg.Done(); defer finish(); write(e.RPC(callCtx, req, session)) }()
	}
	cancel()
	wg.Wait()
	return scanner.Err()
}
