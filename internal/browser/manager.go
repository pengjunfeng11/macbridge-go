package browser

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"macbridge/internal/core"
)

const MaxWireBytes = 10 * 1024 * 1024
const NativeHostName = "com.macbridge.native"

type Manager struct{ cfg core.Config }

func New(c core.Config) *Manager { return &Manager{cfg: c} }
func (m *Manager) Close() error  { return nil }

type wireReply struct {
	ID     string      `json:"id"`
	OK     bool        `json:"ok"`
	Result any         `json:"result,omitempty"`
	Error  *core.Fault `json:"error,omitempty"`
}

func (m *Manager) request(ctx context.Context, method string, args map[string]any, patterns []string, timeout time.Duration) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, e := (&net.Dialer{}).DialContext(ctx, "unix", m.cfg.ChromeSocket)
	if e != nil {
		return nil, core.Error("CHROME_EXTENSION_OFFLINE", "Chrome native host is offline: "+e.Error())
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if d, ok := ctx.Deadline(); ok {
		conn.SetDeadline(d)
	}
	payload := map[string]any{"id": core.ID(), "method": method, "args": args, "allowedUrlPatterns": patterns, "timeoutMs": timeout.Milliseconds()}
	if e = json.NewEncoder(conn).Encode(payload); e != nil {
		return nil, e
	}
	var response wireReply
	if e = json.NewDecoder(io.LimitReader(conn, MaxWireBytes)).Decode(&response); e != nil {
		return nil, fmt.Errorf("Chrome response: %w", e)
	}
	if !response.OK {
		if response.Error != nil {
			return nil, response.Error
		}
		return nil, core.Error("CHROME_EXTENSION_ERROR", "Chrome request failed")
	}
	return response.Result, nil
}
func (m *Manager) Status() map[string]any {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	v, e := m.request(ctx, "host.status", nil, nil, time.Second)
	if e != nil {
		return map[string]any{"extensionReady": false, "error": e.Error(), "socketPath": m.cfg.ChromeSocket}
	}
	if out, ok := v.(map[string]any); ok {
		return out
	}
	return map[string]any{"extensionReady": false}
}
func (m *Manager) patterns() ([]string, error) {
	strict := false
	if raw, e := os.ReadFile(filepath.Join(m.cfg.DataDir, "settings.json")); e == nil {
		var settings map[string]any
		if json.Unmarshal(raw, &settings) != nil || settings == nil {
			strict = true
		} else {
			strict = core.Bool(settings, "strictApprovals", false)
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	if !strict {
		return []string{"http://*:*/*", "https://*:*/*"}, nil
	}
	dir := core.Env("MAC_DEV_BRIDGE_BACKGROUND_CHROME_GRANT_DIR", filepath.Join(m.cfg.DataDir, "chrome-background-grants"))
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	legacy := core.Env("MAC_DEV_BRIDGE_PERSONAL_APPROVAL_FILE", filepath.Join(m.cfg.DataDir, "PERSONAL_BROWSER_APPROVED"))
	files = append([]string{legacy}, files...)
	patterns := []string{}
	seen := map[string]bool{}
	for i, file := range files {
		if i > 256 {
			break
		}
		raw, e := os.ReadFile(file)
		if e != nil {
			continue
		}
		var grant struct {
			Provider, Nonce, ExpiresAt string
			AllowedURLPatterns         []string `json:"allowedUrlPatterns"`
		}
		if json.Unmarshal(raw, &grant) != nil || grant.Provider != "chrome-background" {
			continue
		}
		expiry, e := time.Parse(time.RFC3339Nano, grant.ExpiresAt)
		remaining := time.Until(expiry)
		if e != nil || remaining <= 0 || remaining > 15*time.Minute || !regexp.MustCompile(`^[a-fA-F0-9]{32}$`).MatchString(grant.Nonce) {
			continue
		}
		if file == legacy && len(grant.AllowedURLPatterns) > 0 {
			// Import each legacy grant before another session replaces that one-use file.
			if e = core.AtomicJSON(filepath.Join(dir, grant.Nonce+".json"), json.RawMessage(raw)); e != nil {
				return nil, e
			}
			if current, err := os.ReadFile(legacy); err == nil && bytes.Equal(current, raw) {
				if err = os.Remove(legacy); err != nil && !os.IsNotExist(err) {
					return nil, err
				}
			}
		}
		for _, p := range grant.AllowedURLPatterns {
			if p != "" && !seen[p] {
				seen[p] = true
				patterns = append(patterns, p)
			}
		}
	}
	if len(patterns) == 0 {
		return nil, core.Error("PERSONAL_MODE_NOT_APPROVED", "Strict approvals requires an unexpired chrome-background URL grant")
	}
	return patterns, nil
}

var methods = map[string]string{"chrome_workspace_status": "workspace.status", "chrome_workspace_setup": "workspace.init", "chrome_tabs": "tabs.list", "chrome_open": "workspace.open", "chrome_navigate": "tabs.navigate", "chrome_snapshot": "tabs.snapshot", "chrome_click": "tabs.click", "chrome_fill": "tabs.fill", "chrome_close": "tabs.close", "chatgpt_extension_status": "chatgpt.extensionStatus", "chatgpt_conversation_start": "tabs.chatgptConversationStart"}

func (m *Manager) Call(ctx context.Context, name string, args map[string]any) (any, error) {
	method, ok := methods[name]
	if !ok {
		return nil, core.Error("UNKNOWN_TOOL", name)
	}
	if args == nil {
		args = map[string]any{}
	}
	tool := toolByName(name)
	props := tool.InputSchema["properties"].(map[string]any)
	for key := range args {
		if _, exists := props[key]; !exists {
			return nil, core.Error("INVALID_ARGUMENT", "Unknown browser argument: "+key)
		}
	}
	for _, key := range tool.InputSchema["required"].([]string) {
		if _, ok := args[key]; !ok {
			return nil, core.Error("INVALID_ARGUMENT", "Missing browser argument: "+key)
		}
	}
	translated := map[string]any{}
	aliases := map[string]string{"pool_size": "poolSize", "tab_id": "tabId", "max_tabs": "maxTabs", "url_contains": "urlContains", "title_contains": "titleContains", "max_text_chars": "maxTextChars", "max_elements": "maxElements", "allow_active": "allowActive", "thinking_effort": "thinkingEffort", "max_runtime_seconds": "maxRuntimeSeconds", "continue_in_work": "continueInWork", "project_id": "projectId", "conversation_id": "conversationId"}
	for key, value := range args {
		schema := props[key].(map[string]any)
		switch schema["type"] {
		case "string":
			text, ok := value.(string)
			if !ok {
				return nil, core.Error("INVALID_ARGUMENT", key+" must be a string")
			}
			if min, ok := schema["minLength"].(int); ok && len(text) < min {
				return nil, core.Error("INVALID_ARGUMENT", key+" is empty")
			}
			if max, ok := schema["maxLength"].(int); ok && len(text) > max {
				return nil, core.Error("INVALID_ARGUMENT", key+" exceeds maximum length")
			}
			if values, ok := schema["enum"].([]string); ok {
				found := false
				for _, v := range values {
					found = found || text == v
				}
				if !found {
					return nil, core.Error("INVALID_ARGUMENT", key+" has an unsupported value")
				}
			}
			if pattern, ok := schema["pattern"].(string); ok && !regexp.MustCompile(pattern).MatchString(text) {
				return nil, core.Error("INVALID_ARGUMENT", "Invalid "+key)
			}
		case "integer":
			var n int
			switch v := value.(type) {
			case float64:
				n = int(v)
				if float64(n) != v {
					return nil, core.Error("INVALID_ARGUMENT", key+" must be an integer")
				}
			case int:
				n = v
			default:
				return nil, core.Error("INVALID_ARGUMENT", key+" must be an integer")
			}
			if n < schema["minimum"].(int) || n > schema["maximum"].(int) {
				return nil, core.Error("INVALID_ARGUMENT", key+" is out of range")
			}
			value = n
		case "boolean":
			if _, ok := value.(bool); !ok {
				return nil, core.Error("INVALID_ARGUMENT", key+" must be boolean")
			}
		}
		target := key
		if a, ok := aliases[key]; ok {
			target = a
		}
		translated[target] = value
	}
	timeout := 45 * time.Second
	if name == "chatgpt_conversation_start" {
		if core.String(args, "transport", "runtime") == "raw" && core.String(args, "conversation_id", "") != "" {
			return nil, core.Error("CHATGPT_CONTINUATION_TRANSPORT_INVALID", "Raw transport cannot continue a conversation")
		}
		timeout = time.Duration(core.Int(args, "max_runtime_seconds", 600)+120) * time.Second
	}
	local := strings.HasPrefix(method, "workspace.") && method != "workspace.open" || method == "chatgpt.extensionStatus"
	if name == "chrome_close" {
		result, e := m.request(ctx, "workspace.release", translated, nil, timeout)
		if e != nil {
			return nil, e
		}
		if v, ok := result.(map[string]any); ok && core.Bool(v, "released", false) {
			return result, nil
		}
	}
	var patterns []string
	var e error
	if !local {
		patterns, e = m.patterns()
		if e != nil {
			return nil, e
		}
	}
	result, err := m.request(ctx, method, translated, patterns, timeout)
	if name == "chatgpt_extension_status" {
		inspection := m.openAIRegistration()
		if live, ok := result.(map[string]any); ok {
			for key, value := range live {
				inspection[key] = value
			}
		}
		if err != nil {
			inspection["liveError"] = err.Error()
			inspection["extensionReady"] = false
		}
		return inspection, nil
	}
	return result, err
}

func (m *Manager) openAIRegistration() map[string]any {
	root := filepath.Join(m.cfg.Home, "Library", "Application Support", "Google", "Chrome")
	registration := filepath.Join(root, "NativeMessagingHosts", "com.openai.codexextension.json")
	out := map[string]any{"nativeHostName": "com.openai.codexextension", "nativeHostRegistrationPath": registration, "nativeHostRegistered": false}
	if raw, err := os.ReadFile(registration); err == nil {
		var manifest map[string]any
		if json.Unmarshal(raw, &manifest) == nil {
			out["nativeHostRegistered"] = true
			out["nativeHostExecutable"] = manifest["path"]
		}
	}
	paths, _ := filepath.Glob(filepath.Join(root, "*", "Extensions", "hehggadaopoacecdllhhajmbjkdcmajg", "*", "manifest.json"))
	installations := []map[string]any{}
	for _, file := range paths {
		var manifest map[string]any
		if raw, err := os.ReadFile(file); err == nil && json.Unmarshal(raw, &manifest) == nil {
			installations = append(installations, map[string]any{"manifestPath": file, "version": manifest["version"], "name": manifest["name"]})
		}
	}
	out["localInstallations"] = installations
	return out
}
func toolByName(name string) core.Tool {
	for _, t := range Tools() {
		if t.Name == name {
			return t
		}
	}
	panic("unknown browser tool")
}
func Tools() []core.Tool {
	str := func(min, max int) map[string]any {
		return map[string]any{"type": "string", "minLength": min, "maxLength": max}
	}
	integer := func(min, max, def int) map[string]any {
		return map[string]any{"type": "integer", "minimum": min, "maximum": max, "default": def}
	}
	boolean := func(def bool) map[string]any { return map[string]any{"type": "boolean", "default": def} }
	tab := integer(0, 2147483647, 0)
	pool := integer(1, 32, 8)
	tools := []core.Tool{}
	add := func(name, description string, props map[string]any, required []string, readonly bool) {
		if props == nil {
			props = map[string]any{}
		}
		if required == nil {
			required = []string{}
		}
		tools = append(tools, core.Tool{Name: name, Description: description, InputSchema: map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}, Annotations: map[string]any{"readOnlyHint": readonly, "destructiveHint": !readonly, "openWorldHint": true, "idempotentHint": readonly}})
	}
	add("chrome_workspace_status", "Inspect the persistent MDB background tab pool without site access.", nil, nil, true)
	add("chrome_workspace_setup", "Grow the MDB tab pool; create tabs only while its Chrome window is already focused.", map[string]any{"pool_size": pool}, nil, false)
	add("chrome_tabs", "List approved tabs in the bound signed-in Chrome profile without focusing Chrome.", map[string]any{"url_contains": str(0, 20000), "title_contains": str(0, 20000), "max_tabs": integer(1, 500, 200)}, nil, true)
	add("chrome_open", "Lease an idle MDB background tab and navigate to an approved URL.", map[string]any{"url": str(1, 20000)}, []string{"url"}, false)
	add("chrome_navigate", "Navigate an approved tab without changing focus.", map[string]any{"tab_id": tab, "url": str(1, 20000)}, []string{"tab_id", "url"}, false)
	add("chrome_snapshot", "Read visible text and interactive element selectors; password values are redacted.", map[string]any{"tab_id": tab, "max_text_chars": integer(1000, 200000, 50000), "max_elements": integer(1, 500, 200)}, []string{"tab_id"}, true)
	add("chrome_click", "Send adaptive synthetic pointer/mouse events without a foreground activation.", map[string]any{"tab_id": tab, "selector": str(1, 10000)}, []string{"tab_id", "selector"}, false)
	add("chrome_fill", "Fill native or contenteditable fields and optionally submit their form.", map[string]any{"tab_id": tab, "selector": str(1, 10000), "value": str(0, 500000), "submit": boolean(false)}, []string{"tab_id", "selector", "value"}, false)
	add("chrome_close", "Return an MDB tab to its pool, or close an approved external tab.", map[string]any{"tab_id": tab, "allow_active": boolean(false)}, []string{"tab_id"}, false)
	add("chatgpt_extension_status", "Inspect OpenAI extension registration and the read-only ChatGPT page bridge.", nil, nil, true)
	model := str(1, 128)
	model["pattern"] = `^[A-Za-z0-9._:/-]{1,128}$`
	model["default"] = "gpt-5-6-pro"
	effort := str(1, 16)
	effort["enum"] = []string{"minimal", "low", "standard", "high", "max"}
	effort["default"] = "standard"
	transport := str(1, 16)
	transport["enum"] = []string{"runtime", "raw"}
	transport["default"] = "runtime"
	project := str(1, 132)
	project["pattern"] = `^g-p-[A-Za-z0-9_-]{8,128}$`
	conversation := str(8, 128)
	conversation["pattern"] = `^[A-Za-z0-9_-]{8,128}$`
	add("chatgpt_conversation_start", "Start or continue one exact ChatGPT conversation using its mounted runtime; verify the assistant message after reload. Raw private-request diagnostics remain optional.", map[string]any{"prompt": str(1, 4000000), "model": model, "transport": transport, "thinking_effort": effort, "max_runtime_seconds": integer(30, 3600, 600), "continue_in_work": boolean(true), "project_id": project, "conversation_id": conversation, "tab_id": tab}, []string{"prompt"}, false)
	return tools
}

// Read one socket request with a hard size bound, retaining JSONL compatibility.
func readLineJSON(r io.Reader, v any) error {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 4096), MaxWireBytes)
	if !s.Scan() {
		if e := s.Err(); e != nil {
			return e
		}
		return io.EOF
	}
	return json.Unmarshal(s.Bytes(), v)
}
