package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"macbridge/internal/core"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const Version = "1.0.0"
const Modern = "2026-07-28"
const Acknowledgement = "I_UNDERSTAND_THIS_GRANTS_FULL_ACCESS"

type Engine struct {
	Config       core.Config
	Execute      core.Handler
	List         func(context.Context) ([]core.Tool, error)
	State        func() map[string]any
	Acknowledged bool
	auditMu      sync.Mutex
}

func (e *Engine) Unlocked() bool {
	if e.Acknowledged {
		return true
	}
	b, err := os.ReadFile(e.Config.UnlockFile)
	return err == nil && strings.TrimSpace(string(b)) == Acknowledgement
}
func (e *Engine) Call(ctx context.Context, name string, args map[string]any) (core.Result, error) {
	if err := ctx.Err(); err != nil {
		return core.Result{}, err
	}
	if !e.Unlocked() {
		return core.Result{}, core.Error("BRIDGE_LOCKED", "Full-access unlock revoked or missing")
	}
	tools := Builtins()
	var err error
	var spec *core.Tool
	for i := range tools {
		if tools[i].Name == name {
			spec = &tools[i]
			break
		}
	}
	if spec == nil {
		tools, err = e.List(ctx)
		if err != nil {
			return core.Result{}, err
		}
		for i := range tools {
			if tools[i].Name == name {
				spec = &tools[i]
				break
			}
		}
	}
	if spec == nil {
		return core.Result{}, core.Error("UNKNOWN_TOOL", "Unknown tool: "+name)
	}
	if err = core.Validate(spec.InputSchema, args); err != nil {
		return core.Result{}, core.Error("INVALID_ARGUMENT", err.Error())
	}
	if err := ctx.Err(); err != nil {
		return core.Result{}, err
	}
	started := time.Now()
	var value any
	switch name {
	case "bridge_status":
		hostname, _ := os.Hostname()
		cwd, _ := os.Getwd()
		value = map[string]any{"bridgeVersion": Version, "implementation": "Go", "pid": os.Getpid(), "hostname": hostname, "uid": os.Getuid(), "gid": os.Getgid(), "home": e.Config.Home, "platform": runtime.GOOS, "architecture": runtime.GOARCH, "runtime": runtime.Version(), "shell": e.Config.Shell, "codexBin": e.Config.CodexBin, "cwd": cwd, "dataDir": e.Config.DataDir, "jobDir": filepath.Join(e.Config.DataDir, "jobs"), "auditLog": e.auditPath(), "auditMode": core.Env("MAC_DEV_BRIDGE_AUDIT_MODE", "metadata"), "fullAccessUnlocked": true, "fullAccessUnlockFile": e.Config.UnlockFile, "accessModel": "Unrestricted host user; no sandbox or path allowlist", "tunnelRuntimeKeyScrubbedFromChildEnvironment": true}
		for k, v := range e.State() {
			value.(map[string]any)[k] = v
		}
		state := value.(map[string]any)
		settings, _ := state["operatorSettings"].(map[string]any)
		if settings == nil {
			settings = map[string]any{}
		}
		if _, ok := settings["strictApprovals"].(bool); !ok {
			settings["strictApprovals"] = false
		}
		state["operatorSettings"] = settings

	case "audit_tail":
		value, err = e.auditTail(core.Int(args, "max_bytes", 100000))
	default:
		if name == "shell_exec" || name == "shell_start" {
			err = e.focusPolicy(args)
		}
		if err == nil {
			err = ctx.Err()
		}
		if err == nil {
			value, err = e.Execute(ctx, name, args)
		}
	}
	e.audit(name, args, time.Since(started), err)
	if err != nil {
		return core.Result{}, err
	}
	if result, ok := value.(core.Result); ok {
		return result, nil
	}
	b, err := json.Marshal(value)
	if err != nil {
		return core.Result{}, err
	}
	return core.Result{Content: []map[string]any{{"type": "text", "text": string(b)}}, StructuredContent: value}, nil
}
func ErrorResult(err error) core.Result {
	code := "TOOL_ERROR"
	var fault *core.Fault
	if errors.As(err, &fault) {
		code = fault.Code
	}
	v := map[string]any{"error": err.Error(), "code": code}
	b, _ := json.Marshal(v)
	return core.Result{Content: []map[string]any{{"type": "text", "text": string(b)}}, StructuredContent: v, IsError: true}
}
func (e *Engine) auditPath() string {
	return core.Env("MAC_DEV_BRIDGE_AUDIT_LOG", filepath.Join(e.Config.LogDir, "audit.jsonl"))
}
func (e *Engine) audit(name string, args map[string]any, elapsed time.Duration, err error) {
	mode := core.Env("MAC_DEV_BRIDGE_AUDIT_MODE", "metadata")
	if mode == "off" {
		return
	}
	raw, _ := json.Marshal(args)
	sum := sha256.Sum256(raw)
	record := map[string]any{"timestamp": core.Now(), "tool": name, "durationMs": elapsed.Milliseconds(), "argsBytes": len(raw), "argsSha256": hex.EncodeToString(sum[:]), "ok": err == nil}
	if err != nil {
		record["error"] = err.Error()
	}
	if mode == "full" {
		safe := map[string]any{}
		for k, v := range args {
			if k == "data" || k == "stdin" || k == "content" || k == "prompt" || k == "env" {
				safe[k] = "[REDACTED]"
			} else {
				safe[k] = v
			}
		}
		record["args"] = safe
	}
	b, _ := json.Marshal(record)
	e.auditMu.Lock()
	defer e.auditMu.Unlock()
	os.MkdirAll(filepath.Dir(e.auditPath()), 0700)
	f, err := os.OpenFile(e.auditPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err == nil {
		defer f.Close()
		f.Write(append(b, '\n'))
	}
}
func (e *Engine) auditTail(max int) (any, error) {
	f, err := os.Open(e.auditPath())
	if os.IsNotExist(err) {
		return map[string]any{"text": "", "returnedBytes": 0}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if max < 1 {
		max = 100000
	}
	if max > 8000000 {
		max = 8000000
	}
	offset := st.Size() - int64(max)
	if offset < 0 {
		offset = 0
	}
	b := make([]byte, st.Size()-offset)
	n, err := f.ReadAt(b, offset)
	if n > 0 {
		err = nil
	}
	return map[string]any{"text": string(b[:n]), "returnedBytes": n, "size": st.Size(), "truncated": offset > 0}, err
}
func (e *Engine) focusPolicy(args map[string]any) error {
	cmd := strings.ToLower(core.String(args, "command", ""))
	directChrome := (strings.Contains(cmd, "osascript") && (strings.Contains(cmd, "chrome") || strings.Contains(cmd, "chromium"))) || strings.Contains(cmd, "google chrome.app/contents/macos/") || strings.Contains(cmd, "open https://") || strings.Contains(cmd, "open http://") || strings.Contains(cmd, "open -g http")
	if directChrome {
		return core.Error("CHROME_BACKGROUND_REQUIRED", "Chrome web work must use the built-in chrome_* tools and the MDB tab group for browser operations")
	}
	var settings struct {
		Strict bool `json:"strictApprovals"`
	}
	b, _ := os.ReadFile(filepath.Join(e.Config.DataDir, "settings.json"))
	json.Unmarshal(b, &settings)
	if !settings.Strict || !(strings.Contains(cmd, "osascript") && (strings.Contains(cmd, "system events") || strings.Contains(cmd, "activate"))) {
		return nil
	}
	file := core.Env("MAC_DEV_BRIDGE_FOREGROUND_GUI_APPROVAL_FILE", filepath.Join(e.Config.DataDir, "FOREGROUND_GUI_APPROVED"))
	b, err := os.ReadFile(file)
	if err != nil {
		return core.Error("GUI_FOCUS_BLOCKED", "Strict approvals is enabled; a foreground application grant is required")
	}
	var grant struct {
		App         string   `json:"app"`
		Apps        []string `json:"apps"`
		AllowedApps []string `json:"allowedApps"`
		Expires     string   `json:"expiresAt"`
	}
	json.Unmarshal(b, &grant)
	expires, _ := time.Parse(time.RFC3339Nano, grant.Expires)
	apps := append(append(grant.Apps, grant.AllowedApps...), grant.App)
	matches := false
	for _, a := range apps {
		if a != "" && strings.Contains(cmd, strings.ToLower(a)) {
			matches = true
		}
	}
	if !matches || time.Now().After(expires) {
		return core.Error("GUI_FOCUS_BLOCKED", "Foreground application grant is expired or does not match")
	}
	claim := file + "." + core.ID()
	if err = os.Rename(file, claim); err != nil {
		return err
	}
	return os.Remove(claim)
}

func Builtins() []core.Tool {
	tools := core.Builtins()
	for i := range tools {
		p, _ := tools[i].InputSchema["properties"].(map[string]any)
		switch tools[i].Name {
		case "fs_read":
			p["include_sha256"] = map[string]any{"type": "boolean", "default": false}
		case "fs_write":
			p["expected_sha256"] = map[string]any{"type": []any{"string", "null"}, "pattern": "^[0-9a-fA-F]{64}$"}
		case "shell_start":
			p["max_log_bytes"] = map[string]any{"type": "integer", "minimum": float64(1024), "maximum": float64(64000000)}
		}
	}
	return tools
}
func ToolNames(tools []core.Tool) string {
	names := []string{}
	for _, t := range tools {
		names = append(names, t.Name)
	}
	return fmt.Sprint(names)
}
