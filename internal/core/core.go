package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type Config struct{ Home, DataDir, LogDir, Shell, CodexBin, UnlockFile, ChromeSocket string }

func FromEnv() Config {
	h, _ := os.UserHomeDir()
	d := Env("MAC_DEV_BRIDGE_DATA_DIR", filepath.Join(h, "Library", "Application Support", "MacDeveloperBridge"))
	return Config{h, d, Env("MAC_DEV_BRIDGE_LOG_DIR", filepath.Join(h, "Library", "Logs", "MacDeveloperBridge")), Env("MAC_DEV_BRIDGE_SHELL", "/bin/zsh"), Env("CODEX_BIN", "codex"), Env("MAC_DEV_BRIDGE_UNLOCK_FILE", filepath.Join(d, "FULL_ACCESS_ENABLED")), Env("MAC_DEV_BRIDGE_CHROME_SOCKET", filepath.Join(d, "chrome-background.sock"))}
}
func Env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func EnvInt(k string, d int) int {
	n, e := strconv.Atoi(os.Getenv(k))
	if e != nil {
		return d
	}
	return n
}
func (c Config) Path(p string) string {
	if p == "~" {
		return c.Home
	}
	if len(p) > 1 && p[:2] == "~/" {
		return filepath.Join(c.Home, p[2:])
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(c.Home, p)
}
func ID() string {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}
func Now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
func String(a map[string]any, k, d string) string {
	v, ok := a[k].(string)
	if !ok {
		return d
	}
	return v
}
func Int(a map[string]any, k string, d int) int {
	switch v := a[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	}
	return d
}
func Bool(a map[string]any, k string, d bool) bool {
	v, ok := a[k].(bool)
	if !ok {
		return d
	}
	return v
}
func Required(a map[string]any, k string) (string, error) {
	v, ok := a[k].(string)
	if !ok || v == "" {
		return "", Error("INVALID_ARGUMENT", fmt.Sprintf("%s must be a non-empty string", k))
	}
	return v, nil
}

type Fault struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Fault) Error() string     { return e.Message }
func Error(code, msg string) error { return &Fault{code, msg} }

type Tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
	Extra       map[string]any `json:"-"`
}
type Result struct {
	Content           []map[string]any `json:"content"`
	StructuredContent any              `json:"structuredContent,omitempty"`
	IsError           bool             `json:"isError"`
	Meta              map[string]any   `json:"_meta,omitempty"`
}
type Handler func(context.Context, string, map[string]any) (any, error)

func AtomicJSON(path string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".state-*")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	return os.Rename(name, path)
}
