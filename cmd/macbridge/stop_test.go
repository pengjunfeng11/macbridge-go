package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"macbridge/internal/core"
	"macbridge/internal/host"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "supervise-job" {
		if e := host.SuperviseJob(os.Args[2]); e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}
func stopFixture(t *testing.T) core.Config {
	t.Helper()
	dir := t.TempDir()
	cfg := core.Config{Home: dir, DataDir: filepath.Join(dir, "data"), LogDir: filepath.Join(dir, "logs"), Shell: "/bin/sh", UnlockFile: filepath.Join(dir, "data", "unlock")}
	if e := os.MkdirAll(cfg.DataDir, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(cfg.UnlockFile, []byte(serverAcknowledgement), 0600); e != nil {
		t.Fatal(e)
	}
	return cfg
}

const serverAcknowledgement = "I_UNDERSTAND_THIS_GRANTS_FULL_ACCESS"

func testIdentity(t *testing.T, pid int) string {
	t.Helper()
	cmd := exec.Command("ps", "-p", fmt.Sprint(pid), "-o", "lstart=")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	b, e := cmd.Output()
	if e != nil {
		t.Fatal(e)
	}
	return strings.TrimSpace(string(b))
}
func testChild(t *testing.T) (*exec.Cmd, <-chan error) {
	t.Helper()
	cmd := exec.Command("/bin/sleep", "30")
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	})
	return cmd, done
}
func hostCall(t *testing.T, m *host.Manager, name string, a map[string]any) map[string]any {
	t.Helper()
	r, e := m.Call(context.Background(), name, a)
	if e != nil {
		t.Fatal(name, e)
	}
	return r.(map[string]any)
}
func TestStopVerifiesOwnersJobsAndTerminalChildren(t *testing.T) {
	// The supervisor reuses this test binary. Disable the race runtime's extra
	// exit sleep so its shutdown timing matches the production executable.
	t.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0")
	cfg := stopFixture(t)
	bin := filepath.Join(cfg.Home, "bin")
	if e := os.MkdirAll(bin, 0700); e != nil {
		t.Fatal(e)
	}
	log := filepath.Join(cfg.Home, "launchctl.calls")
	mock := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + quote(log) + "\nexit 0\n"
	if e := os.WriteFile(filepath.Join(bin, "launchctl"), []byte(mock), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	plist := filepath.Join(cfg.Home, "Library", "LaunchAgents", launchLabel+".plist")
	if e := os.MkdirAll(filepath.Dir(plist), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(plist, []byte("test fixture only"), 0600); e != nil {
		t.Fatal(e)
	}
	tunnelPlist := filepath.Join(filepath.Dir(plist), "local.macbridge.tunnel.plist")
	if e := os.WriteFile(tunnelPlist, []byte("test fixture only"), 0600); e != nil {
		t.Fatal(e)
	}

	owner, _ := testChild(t)
	if e := core.AtomicJSON(filepath.Join(cfg.DataDir, "macbridge-http.json"), map[string]any{"pid": owner.Process.Pid, "executable": "/bin/sleep", "processStart": testIdentity(t, owner.Process.Pid)}); e != nil {
		t.Fatal(e)
	}
	hostCfg := cfg
	hostCfg.UnlockFile = ""
	m, e := host.New(hostCfg)
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close()
	job := hostCall(t, m, "shell_start", map[string]any{"command": "sleep 30"})
	pty := hostCall(t, m, "pty_start", map[string]any{"command": "/bin/sh", "args": []any{"-i"}})
	hostCall(t, m, "pty_write", map[string]any{"session_id": pty["sessionId"], "data": "sleep 30 &\r"})
	time.Sleep(700 * time.Millisecond)
	if e = stopBridge(cfg); e != nil {
		t.Fatal(e)
	}
	state := hostCall(t, m, "shell_job_status", map[string]any{"job_id": job["id"]})
	if state["running"] != false || state["status"] != "cancelled" {
		t.Fatal(state)
	}
	pstate := hostCall(t, m, "pty_read", map[string]any{"session_id": pty["sessionId"]})
	if pstate["exited"] != true {
		t.Fatal(pstate)
	}
	if _, e = os.Stat(cfg.UnlockFile); !os.IsNotExist(e) {
		t.Fatal("Unlock not removed")
	}
	calls, e := os.ReadFile(log)
	if e != nil || !strings.Contains(string(calls), "bootout gui/") || !strings.Contains(string(calls), "local.macbridge.tunnel") {
		t.Fatal(string(calls), e)
	}
}
func TestStopRefusesRecycledOwnerPID(t *testing.T) {
	cfg := stopFixture(t)
	child, _ := testChild(t)
	if e := core.AtomicJSON(filepath.Join(cfg.DataDir, "macbridge-http.json"), map[string]any{"pid": child.Process.Pid, "executable": "/bin/sleep", "processStart": "different process start"}); e != nil {
		t.Fatal(e)
	}
	e := stopBridge(cfg)
	if e == nil || !strings.Contains(e.Error(), "identity differs") {
		t.Fatal(e)
	}
	if e = syscall.Kill(child.Process.Pid, 0); e != nil {
		t.Fatal("Unrelated process was signalled", e)
	}
}
func TestStopReportsUnknownLiveJob(t *testing.T) {
	cfg := stopFixture(t)
	child, _ := testChild(t)
	path := filepath.Join(cfg.DataDir, "jobs", "unknown.json")
	if e := core.AtomicJSON(path, map[string]any{"id": "unknown", "kind": "pty", "pid": child.Process.Pid, "processGroupId": child.Process.Pid, "status": "unknown"}); e != nil {
		t.Fatal(e)
	}
	e := stopBridge(cfg)
	if e == nil || !strings.Contains(e.Error(), "process identity differs") {
		t.Fatal(e)
	}
	if e = syscall.Kill(child.Process.Pid, 0); e != nil {
		t.Fatal("Unverified process was signalled", e)
	}
}
func TestJobMetadataIdentities(t *testing.T) {
	cfg := stopFixture(t)
	cfg.UnlockFile = ""
	m, e := host.New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close()
	session := hostCall(t, m, "pty_start", map[string]any{"command": "/bin/sh", "args": []any{"-c", "sleep 30"}})
	time.Sleep(100 * time.Millisecond)
	data, e := os.ReadFile(filepath.Join(cfg.DataDir, "jobs", session["sessionId"].(string)+".json"))
	if e != nil {
		t.Fatal(e)
	}
	var metadata map[string]any
	if e = json.Unmarshal(data, &metadata); e != nil {
		t.Fatal(e)
	}
	if metadata["processStart"] == "" || metadata["pts"] == "" || len(metadata["ttyProcesses"].(map[string]any)) == 0 {
		t.Fatal(metadata)
	}
}
