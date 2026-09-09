package host

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"macbridge/internal/core"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "supervise-job" {
		if e := SuperviseJob(os.Args[2]); e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}
func manager(t *testing.T) *Manager {
	t.Helper()
	root := t.TempDir()
	m, e := New(core.Config{Home: root, DataDir: filepath.Join(root, "data"), LogDir: filepath.Join(root, "logs"), Shell: "/bin/sh"})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { m.Close() })
	return m
}
func call(t *testing.T, m *Manager, name string, a map[string]any) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	r, e := m.Call(ctx, name, a)
	if e != nil {
		t.Fatalf("%s: %v", name, e)
	}
	return r.(map[string]any)
}
func asInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return -1
}
func poll(t *testing.T, m *Manager, name, key, id string, predicate func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		r := call(t, m, name, map[string]any{key: id})
		if predicate(r) {
			return r
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s timed out", name)
	return nil
}
func TestShellAndFilesystem(t *testing.T) {
	m := manager(t)
	r := call(t, m, "shell_exec", map[string]any{"command": "printf '%s' \"$VALUE\"; printf err >&2; exit 7", "env": map[string]any{"VALUE": "hello"}})
	if r["stdout"] != "hello" || r["stderr"] != "err" || r["exitCode"] != 7 {
		t.Fatal(r)
	}
	r = call(t, m, "shell_exec", map[string]any{"command": "sleep 20", "timeout_ms": 30})
	if r["timedOut"] != true || r["signal"] != "SIGTERM" {
		t.Fatal(r)
	}
	p := filepath.Join(m.cfg.Home, "file")
	w := call(t, m, "fs_write", map[string]any{"path": p, "content": "first", "mode": 0751, "expected_sha256": nil})
	digest := w["sha256"]
	read := call(t, m, "fs_read", map[string]any{"path": p, "offset": 1, "max_bytes": 3, "include_sha256": true})
	if read["content"] != "irs" || read["sha256"] != digest {
		t.Fatal(read)
	}
	call(t, m, "fs_write", map[string]any{"path": p, "content": "second", "expected_sha256": digest})
	if _, e := m.Call(context.Background(), "fs_write", map[string]any{"path": p, "content": "lost", "expected_sha256": digest}); e == nil {
		t.Fatal("Stale write accepted")
	}
	i, _ := os.Stat(p)
	if i.Mode().Perm() != 0751 {
		t.Fatal(i.Mode())
	}
	r = call(t, m, "fs_write", map[string]any{"path": p, "content": base64.StdEncoding.EncodeToString([]byte{0, 1, 2}), "encoding": "base64"})
	if asInt(r["bytesWritten"]) != 3 {
		t.Fatal(r)
	}
	src := filepath.Join(m.cfg.Home, "src")
	call(t, m, "fs_manage", map[string]any{"operation": "mkdir", "path": src})
	call(t, m, "fs_write", map[string]any{"path": filepath.Join(src, "nested", "data"), "content": "body"})
	call(t, m, "fs_manage", map[string]any{"operation": "symlink", "path": filepath.Join(src, "link"), "destination": "nested/data"})
	dest := filepath.Join(m.cfg.Home, "copy")
	call(t, m, "fs_manage", map[string]any{"operation": "copy", "path": src, "destination": dest, "recursive": true})
	link, _ := os.Readlink(filepath.Join(dest, "link"))
	if link != "nested/data" {
		t.Fatal(link)
	}
	listing := call(t, m, "fs_list", map[string]any{"path": dest, "recursive": true})
	if asInt(listing["count"]) != 3 {
		t.Fatal(listing)
	}
	patch := "diff --git a/new.txt b/new.txt\nnew file mode 100644\n--- /dev/null\n+++ b/new.txt\n@@ -0,0 +1 @@\n+created\n"
	applied := call(t, m, "apply_patch", map[string]any{"cwd": dest, "patch": patch})
	if applied["exitCode"] != 0 {
		t.Fatal(applied)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "new.txt")); string(b) != "created\n" {
		t.Fatal(string(b))
	}
}
func TestRecursiveCopyPreservesReadOnlyModesAndTimestamps(t *testing.T) {
	m := manager(t)
	source := filepath.Join(m.cfg.Home, "read-only source")
	destination := filepath.Join(m.cfg.Home, "copy destination")
	if e := os.Mkdir(source, 0700); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		_ = os.Chmod(source, 0700)
		_ = os.Chmod(destination, 0700)
	})
	file := filepath.Join(source, "payload")
	if e := os.WriteFile(file, []byte("copied payload"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(file, 0751|os.ModeSetuid); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(source, 0550|os.ModeSticky); e != nil {
		t.Fatal(e)
	}
	atime := time.Unix(1_400_000_000, 0)
	mtime := time.Unix(1_500_000_000, 0)
	for _, p := range []string{file, source} {
		if e := os.Chtimes(p, atime, mtime); e != nil {
			t.Fatal(e)
		}
	}
	call(t, m, "fs_manage", map[string]any{"operation": "copy", "path": source, "destination": destination, "recursive": true})
	for _, name := range []string{"", "payload"} {
		before, e := os.Stat(filepath.Join(source, name))
		if e != nil {
			t.Fatal(e)
		}
		after, e := os.Stat(filepath.Join(destination, name))
		if e != nil {
			t.Fatal(e)
		}
		if before.Mode() != after.Mode() || !after.ModTime().Equal(mtime) || !accessTime(after.Sys().(*syscall.Stat_t)).Equal(atime) {
			t.Fatalf("Copy lost mode or timestamps for %q: %v", name, after)
		}
	}
	b, e := os.ReadFile(filepath.Join(destination, "payload"))
	if e != nil || string(b) != "copied payload" {
		t.Fatal("Copied file differs", e)
	}
}

func TestJobsSurviveAndRetainResults(t *testing.T) {
	m := manager(t)
	start := call(t, m, "shell_start", map[string]any{"command": "printf before; sleep 1; printf after; exit 9", "max_log_bytes": 1024})
	id := start["id"].(string)
	m.Close()
	restarted, e := New(m.cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer restarted.Close()
	state := poll(t, restarted, "shell_job_status", "job_id", id, func(r map[string]any) bool { return r["running"] == false })
	if state["status"] != "failed" || asInt(state["exitCode"]) != 9 || state["stdout"].(map[string]any)["text"] != "beforeafter" {
		t.Fatal(state)
	}
	flood := call(t, restarted, "shell_start", map[string]any{"command": "i=0; while [ $i -lt 3000 ]; do printf abcd; printf efgh >&2; i=$((i+1)); done; printf OUT-END; printf ERR-END >&2", "max_log_bytes": 1024})
	state = poll(t, restarted, "shell_job_status", "job_id", flood["id"].(string), func(r map[string]any) bool { return r["running"] == false })
	for _, stream := range []string{"stdout", "stderr"} {
		tail := state[stream].(map[string]any)
		if tail["truncated"] != true || !strings.HasSuffix(tail["text"].(string), map[string]string{"stdout": "OUT-END", "stderr": "ERR-END"}[stream]) {
			t.Fatal(tail)
		}
		var size int64
		for _, suffix := range []string{"", ".1"} {
			i, e := os.Stat(state[stream+"Path"].(string) + suffix)
			if e == nil {
				size += i.Size()
			}
		}
		if size > 1024 {
			t.Fatalf("Logs grew to %d", size)
		}
	}
	live := call(t, restarted, "shell_start", map[string]any{"command": "sleep 30"})
	liveID := live["id"].(string)
	call(t, restarted, "shell_job_kill", map[string]any{"job_id": liveID, "signal": "SIGSTOP"})
	call(t, restarted, "shell_job_kill", map[string]any{"job_id": liveID, "signal": "SIGCONT"})
	j, e := readJob(live["metadataPath"].(string))
	if e != nil {
		t.Fatal(e)
	}
	j.PID = os.Getpid()
	j.ProcessGroupID = os.Getpid()
	if e = core.AtomicJSON(j.MetadataPath, j); e != nil {
		t.Fatal(e)
	}
	killed := call(t, restarted, "shell_job_kill", map[string]any{"job_id": liveID, "signal": "SIGKILL"})
	if asInt(killed["pid"]) == os.Getpid() {
		t.Fatal("Used corrupted PID")
	}
	state = poll(t, restarted, "shell_job_status", "job_id", liveID, func(r map[string]any) bool { return r["running"] == false })
	if state["status"] != "cancelled" || state["signal"] != "SIGKILL" {
		t.Fatal(state)
	}
	natural := call(t, restarted, "shell_start", map[string]any{"command": "kill -KILL $$"})
	state = poll(t, restarted, "shell_job_status", "job_id", natural["id"].(string), func(r map[string]any) bool { return r["running"] == false })
	if state["status"] != "failed" {
		t.Fatal(state)
	}
	if _, e = restarted.Call(context.Background(), "shell_start", map[string]any{"command": "echo bad", "cwd": filepath.Join(m.cfg.Home, "missing")}); e == nil {
		t.Fatal("Invalid cwd accepted")
	}
}
func TestPTYInteractiveAndCursor(t *testing.T) {
	m := manager(t)
	r := call(t, m, "pty_start", map[string]any{"command": "/bin/sh", "args": []any{"-c", "test -t 0 || exit 8; printf ready; read line; printf 'R:%s\\n' \"$line\""}})
	id := r["sessionId"].(string)
	poll(t, m, "pty_read", "session_id", id, func(r map[string]any) bool { return strings.Contains(r["text"].(string), "ready") })
	resized := call(t, m, "pty_resize", map[string]any{"session_id": id, "cols": 99, "rows": 22})
	if resized["ok"] != true {
		t.Fatal(resized)
	}
	call(t, m, "pty_write", map[string]any{"session_id": id, "data": "answer\r"})
	read := poll(t, m, "pty_read", "session_id", id, func(r map[string]any) bool { return r["exited"] == true })
	if !strings.Contains(read["text"].(string), "R:answer") {
		t.Fatal(read)
	}
	again := call(t, m, "pty_read", map[string]any{"session_id": id})
	if again["text"] != read["text"] || again["nextCursor"] != read["nextCursor"] {
		t.Fatal("Cursor replay changed")
	}
	end := call(t, m, "pty_read", map[string]any{"session_id": id, "cursor": asInt(read["nextCursor"])})
	if end["text"] != "" {
		t.Fatal(end)
	}
	closed := call(t, m, "pty_close", map[string]any{"session_id": id})
	if closed["containmentVerified"] != true {
		t.Fatal(closed)
	}
	call(t, m, "pty_close", map[string]any{"session_id": id})
}
func TestPTYLimitsAndConcurrentReads(t *testing.T) {
	t.Setenv("MAC_DEV_BRIDGE_PTY_MAX_SESSIONS", "1")
	t.Setenv("MAC_DEV_BRIDGE_PTY_RING_BYTES", "4096")
	m := manager(t)
	started := call(t, m, "pty_start", map[string]any{"command": "/bin/sh", "args": []any{"-c", "read value; printf done"}})
	id := started["sessionId"].(string)
	if _, e := m.Call(context.Background(), "pty_start", map[string]any{"command": "/bin/sh"}); e == nil {
		t.Fatal("Capacity exceeded")
	}
	call(t, m, "pty_write", map[string]any{"session_id": id, "data": strings.Repeat("a", 600)})
	if _, e := m.Call(context.Background(), "pty_write", map[string]any{"session_id": id, "data": strings.Repeat("b", 424)}); e == nil {
		t.Fatal("Canonical accumulation was not rejected")
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				_, _ = m.Call(context.Background(), "pty_read", map[string]any{"session_id": id})
			}
		}()
	}
	wg.Wait()
	call(t, m, "pty_close", map[string]any{"session_id": id, "force": true})
	flood := call(t, m, "pty_start", map[string]any{"command": "/bin/sh", "args": []any{"-c", "i=0; while [ $i -lt 4000 ]; do printf xyz; i=$((i+1)); done; printf END"}})
	state := poll(t, m, "pty_read", "session_id", flood["sessionId"].(string), func(r map[string]any) bool { return r["exited"] == true })
	if asInt(state["lostBytes"]) <= 0 || asInt(state["retainedBytes"]) != 4096 || !strings.HasSuffix(state["text"].(string), "END") {
		t.Fatal(state)
	}
}
func TestPTYCloseReclaimsJobControl(t *testing.T) {
	m := manager(t)
	r := call(t, m, "pty_start", map[string]any{"command": "/bin/sh", "args": []any{"-i"}})
	id := r["sessionId"].(string)
	call(t, m, "pty_write", map[string]any{"session_id": id, "data": "sleep 30 &\r"})
	time.Sleep(100 * time.Millisecond)
	closed := call(t, m, "pty_close", map[string]any{"session_id": id, "force": true})
	if closed["containmentVerified"] != true {
		t.Fatal(closed)
	}
}
func TestPTYIdleReclaims(t *testing.T) {
	t.Setenv("MAC_DEV_BRIDGE_PTY_IDLE_TIMEOUT_MS", "1000")
	m := manager(t)
	r := call(t, m, "pty_start", map[string]any{"command": "/bin/sh", "args": []any{"-c", "sleep 30"}})
	time.Sleep(1800 * time.Millisecond)
	state := call(t, m, "pty_read", map[string]any{"session_id": r["sessionId"]})
	if state["exited"] != true || state["closeReason"] != "idle_timeout" {
		t.Fatal(state)
	}
}
func TestByteRing(t *testing.T) {
	r := byteRing{data: make([]byte, 5)}
	r.append([]byte("abcdefg"))
	data, next, lost, more := r.slice(0, 3)
	if string(data) != "cde" || next != 5 || lost != 2 || !more {
		t.Fatal(string(data), next, lost, more)
	}
	data, next, _, more = r.slice(next, 5)
	if string(data) != "fg" || next != 7 || more {
		t.Fatal(string(data), next, more)
	}
}
