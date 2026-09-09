package peers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"macbridge/internal/core"
)

func integerArg(a map[string]any, key string, def, min, max int) (int, error) {
	v, ok := a[key]
	if !ok {
		return def, nil
	}
	n := core.Int(a, key, -1)
	switch x := v.(type) {
	case float64:
		if x != float64(n) {
			return 0, fmt.Errorf("%s must be an integer", key)
		}
	case int, int64, json.Number:
	default:
		return 0, fmt.Errorf("%s must be an integer", key)
	}
	if n < min || n > max {
		return 0, fmt.Errorf("%s must be between %d and %d", key, min, max)
	}
	return n, nil
}
func (m *Manager) codex(ctx context.Context, name string, a map[string]any) (any, error) {
	timeout, e := integerArg(a, "timeout_ms", 30000, 1000, 120000)
	if e != nil {
		return nil, e
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Millisecond)
	defer cancel()
	params := map[string]any{}
	method := ""
	switch name {
	case "codex_thread_read":
		id, e := core.Required(a, "thread_id")
		if e != nil {
			return nil, e
		}
		params["threadId"] = id
		params["includeTurns"] = core.Bool(a, "include_turns", true)
		method = "thread/read"
	case "codex_thread_list":
		method = "thread/list"
		params["sortKey"] = core.String(a, "sort_key", "recency_at")
		params["sortDirection"] = core.String(a, "sort_direction", "desc")
	case "codex_thread_turns_list":
		id, e := core.Required(a, "thread_id")
		if e != nil {
			return nil, e
		}
		params["threadId"] = id
		params["sortDirection"] = core.String(a, "sort_direction", "asc")
		params["itemsView"] = core.String(a, "items_view", "full")
		method = "thread/turns/list"
	default:
		return nil, core.Error("UNKNOWN_TOOL", "Unknown Codex tool: "+name)
	}
	if name != "codex_thread_read" {
		limit, e := integerArg(a, "limit", 50, 1, 200)
		if e != nil {
			return nil, e
		}
		params["limit"] = limit
	}
	for in, out := range map[string]string{"cursor": "cursor", "search_term": "searchTerm", "cwd": "cwd", "sort_key": "sortKey", "sort_direction": "sortDirection", "items_view": "itemsView"} {
		if v, ok := a[in]; ok {
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("%s must be a string", in)
			}
			if in == "cwd" {
				s = m.cfg.Path(s)
			}
			params[out] = s
		}
	}
	for in, out := range map[string]string{"archived": "archived", "is_pinned": "isPinned", "use_state_db_only": "useStateDbOnly", "include_turns": "includeTurns"} {
		if v, ok := a[in]; ok {
			b, ok := v.(bool)
			if !ok {
				return nil, fmt.Errorf("%s must be a boolean", in)
			}
			params[out] = b
		}
	}
	for key, out := range map[string]string{"model_providers": "modelProviders", "source_kinds": "sourceKinds"} {
		if v, ok := a[key]; ok {
			raw, e := json.Marshal(v)
			if e != nil {
				return nil, e
			}
			var strings []string
			if e = json.Unmarshal(raw, &strings); e != nil || string(raw) == "null" {
				return nil, fmt.Errorf("%s must be a string array", key)
			}
			params[out] = strings
		}
	}
	select {
	case <-m.ctx.Done():
		return nil, core.Error("PROVIDER_GONE", "Bridge is closing")
	default:
	}
	c, e := launch(m.cfg.CodexBin, []string{"app-server"}, os.Environ(), m.cfg.Home, nil)
	if e != nil {
		return nil, e
	}
	defer c.stop()
	m.activeMu.Lock()
	if m.closed {
		m.activeMu.Unlock()
		return nil, core.Error("PROVIDER_GONE", "Bridge is closing")
	}
	m.activeCodex[c] = true
	m.activeMu.Unlock()
	defer func() { m.activeMu.Lock(); delete(m.activeCodex, c); m.activeMu.Unlock() }()
	_, e = c.request(ctx, "initialize", map[string]any{"clientInfo": map[string]any{"name": "mac-developer-bridge", "title": "Mac Developer Bridge", "version": "1.0.0-go"}, "capabilities": map[string]any{"experimentalApi": true}})
	if e != nil {
		return nil, e
	}
	if e = c.notify("initialized", map[string]any{}); e != nil {
		return nil, e
	}
	raw, e := c.request(ctx, method, params)
	if e != nil {
		return nil, e
	}
	var result any
	e = json.Unmarshal(raw, &result)
	return result, e
}
