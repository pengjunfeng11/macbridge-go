// Run the compiled release through its public HTTP interface with isolated data.
// Usage: go run ./scripts/acceptance.go ./bin/macbridge
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	if e := accept(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func accept() error {
	if len(os.Args) != 2 {
		return fmt.Errorf("usage: go run ./scripts/acceptance.go ./bin/macbridge")
	}
	binary, e := filepath.Abs(os.Args[1])
	if e != nil {
		return e
	}
	dir, e := os.MkdirTemp("", "mb-accept-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(dir)
	data := filepath.Join(dir, "data")
	logs := filepath.Join(dir, "logs")
	env := []string{}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "MAC_DEV_BRIDGE_") && !strings.HasPrefix(entry, "CONTROL_PLANE_") {
			env = append(env, entry)
		}
	}
	env = append(env, "MAC_DEV_BRIDGE_DATA_DIR="+data, "MAC_DEV_BRIDGE_LOG_DIR="+logs, "MAC_DEV_BRIDGE_HTTP_PORT=0", "MAC_DEV_BRIDGE_MCP_SERVERS_JSON={}")
	run := func(args ...string) error {
		c := exec.Command(binary, args...)
		c.Env = env
		b, e := c.CombinedOutput()
		if e != nil {
			return fmt.Errorf("%v: %w: %s", args, e, b)
		}
		return nil
	}
	if e = run("init"); e != nil {
		return e
	}
	defer run("stop")
	tokenRaw, e := os.ReadFile(filepath.Join(data, "http-token"))
	if e != nil {
		return e
	}
	token := strings.TrimSpace(string(tokenRaw))
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var process *exec.Cmd
	var done chan error
	start := func() (string, error) {
		process = exec.CommandContext(ctx, binary, "http")
		process.Env = env
		process.Stderr = os.Stderr
		if e := process.Start(); e != nil {
			return "", e
		}
		done = make(chan error, 1)
		go func(c *exec.Cmd, ch chan error) { ch <- c.Wait() }(process, done)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			b, _ := os.ReadFile(filepath.Join(data, "macbridge-http.json"))
			var meta map[string]any
			json.Unmarshal(b, &meta)
			if address, ok := meta["address"].(string); ok {
				return "http://" + address, nil
			}
			select {
			case e := <-done:
				return "", fmt.Errorf("HTTP exited: %v", e)
			default:
			}
			time.Sleep(20 * time.Millisecond)
		}
		return "", fmt.Errorf("HTTP startup timed out")
	}
	stop := func() error {
		if process == nil {
			return nil
		}
		_ = process.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
			process = nil
			return nil
		case <-time.After(7 * time.Second):
			_ = process.Process.Kill()
			return fmt.Errorf("HTTP shutdown timed out")
		}
	}
	defer stop()
	base, e := start()
	if e != nil {
		return e
	}
	client := http.Client{Timeout: 10 * time.Second}
	session := ""
	id := 0
	rpc := func(method string, params map[string]any) (map[string]any, error) {
		id++
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		req, _ := http.NewRequest("POST", base+"/mcp", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		response, e := client.Do(req)
		if e != nil {
			return nil, e
		}
		defer response.Body.Close()
		if sid := response.Header.Get("Mcp-Session-Id"); sid != "" {
			session = sid
		}
		var value map[string]any
		if e = json.NewDecoder(response.Body).Decode(&value); e != nil {
			return nil, e
		}
		if response.StatusCode != 200 || value["error"] != nil {
			return nil, fmt.Errorf("RPC %s failed: %v", method, value)
		}
		result, ok := value["result"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("missing result: %v", value)
		}
		return result, nil
	}
	tool := func(name string, args map[string]any) (map[string]any, error) {
		r, e := rpc("tools/call", map[string]any{"name": name, "arguments": args, "_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28"}})
		if e != nil {
			return nil, e
		}
		if r["isError"] == true {
			return nil, fmt.Errorf("%s: %v", name, r)
		}
		v, ok := r["structuredContent"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: structured result missing", name)
		}
		return v, nil
	}
	response, e := client.Get(base + "/healthz")
	if e != nil {
		return e
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("health failed")
	}
	req, _ := http.NewRequest("POST", base+"/mcp", strings.NewReader(`{}`))
	response, e = client.Do(req)
	if e != nil {
		return e
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		return fmt.Errorf("HTTP auth not enforced")
	}
	if _, e = rpc("initialize", map[string]any{"protocolVersion": "2025-11-25"}); e != nil {
		return e
	}
	if session == "" {
		return fmt.Errorf("missing MCP session identity")
	}
	catalog, e := rpc("tools/list", map[string]any{"_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28"}})
	if e != nil {
		return e
	}
	if len(catalog["tools"].([]any)) != 33 {
		return fmt.Errorf("expected all 33 builtins")
	}
	out, e := tool("shell_exec", map[string]any{"command": "printf go-native-http", "cwd": dir})
	if e != nil {
		return e
	}
	if out["stdout"] != "go-native-http" {
		return fmt.Errorf("shell output mismatch: %v", out)
	}
	file := filepath.Join(dir, "roundtrip.txt")
	if _, e = tool("fs_write", map[string]any{"path": file, "content": "中文 / Go ✓", "atomic": true}); e != nil {
		return e
	}
	out, e = tool("fs_read", map[string]any{"path": file, "include_sha256": true})
	if e != nil {
		return e
	}
	if out["content"] != "中文 / Go ✓" || len(out["sha256"].(string)) != 64 {
		return fmt.Errorf("file roundtrip mismatch")
	}
	pty, e := tool("pty_start", map[string]any{"command": "/bin/sh", "args": []string{"-c", "printf 'native-pty'; sleep 0.1"}})
	if e != nil {
		return e
	}
	sid, _ := pty["session_id"].(string)
	if sid == "" {
		sid, _ = pty["sessionId"].(string)
	}
	if sid == "" {
		return fmt.Errorf("PTY id missing: %v", pty)
	}
	time.Sleep(200 * time.Millisecond)
	out, e = tool("pty_read", map[string]any{"session_id": sid})
	if e != nil {
		return e
	}
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), "native-pty") {
		return fmt.Errorf("PTY output mismatch: %v", out)
	}
	if _, e = tool("pty_close", map[string]any{"session_id": sid}); e != nil {
		return e
	}
	job, e := tool("shell_start", map[string]any{"command": "sleep 0.4; printf persisted; exit 7", "cwd": dir})
	if e != nil {
		return e
	}
	jobID, _ := job["id"].(string)
	if jobID == "" {
		jobID, _ = job["jobId"].(string)
	}
	if e = stop(); e != nil {
		return e
	}
	session = ""
	base, e = start()
	if e != nil {
		return e
	}
	time.Sleep(500 * time.Millisecond)
	out, e = tool("shell_job_status", map[string]any{"job_id": jobID})
	if e != nil {
		return e
	}
	raw, _ = json.Marshal(out)
	if out["exitCode"] != float64(7) || !strings.Contains(string(raw), "persisted") {
		return fmt.Errorf("job did not retain terminal result across restart: %s", raw)
	}
	if e = run("stop"); e != nil {
		return e
	}
	if _, e = os.Stat(filepath.Join(data, "FULL_ACCESS_ENABLED")); !os.IsNotExist(e) {
		return fmt.Errorf("stop did not revoke unlock")
	}
	// stop already reaped the HTTP process; consume its completion before deferred cleanup.
	select {
	case <-done:
		process = nil
	case <-time.After(3 * time.Second):
		return fmt.Errorf("stop left HTTP running")
	}
	fmt.Println("PASS: compiled binary → real HTTP/auth/MCP session → 33 tools → shell → atomic UTF-8 file + SHA-256 → real PTY → detached job across restart → verified stop")
	return nil
}
