package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDesktopNamedTunnelChoosesMatchingIngress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	config := `# existing multi-service config
tunnel: 'fixture-tunnel' # identity
credentials-file: '/tmp/not-used-by-test.json'
ingress:
  - hostname: unrelated.example.com
    service: http://localhost:3000
  - hostname: "bridge.example.com" # this listener only
    service: 'http://127.0.0.1:8787'
    originRequest:
      noTLSVerify: false
  - service: http_status:404
`
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if tunnel, host := namedTunnelForPort(path, "8787"); tunnel != "fixture-tunnel" || host != "bridge.example.com" {
		t.Fatal(tunnel, host)
	}
	if tunnel, host := namedTunnelForPort(path, "1234"); tunnel != "fixture-tunnel" || host != "" {
		t.Fatal(tunnel, host)
	}
	if tunnel, host := namedTunnel(path); tunnel != "fixture-tunnel" || host != "unrelated.example.com" {
		t.Fatal(tunnel, host)
	}
	if e := os.WriteFile(path, []byte("tunnel: fixture\ningress:\n  - hostname: ipv6.example.com\n    service: http://[::1]:8787\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, host := namedTunnelForPort(path, "8787"); host != "" {
		t.Fatal("IPv6 ingress cannot reach IPv4-only HTTP", host)
	}
	for _, bad := range []string{"https://bridge.example.com", "*.example.com", "a.example.com/extra", "a.example.com?x=y", "a.example.com@evil", "-bad.example.com"} {
		if validTunnelHostname(bad) {
			t.Errorf("accepted hostname %q", bad)
		}
	}
}
func TestDesktopYAMLScalars(t *testing.T) {
	for _, tt := range []struct {
		raw, want string
		ok        bool
	}{
		{` 'name with # punctuation' # comment`, "name with # punctuation", true},
		{`'it''s named'`, "it's named", true},
		{`"double quoted" # comment`, "double quoted", true},
		{"value\t# comment", "value", true},
		{`*alias`, "", false}, {`"unterminated`, "", false}, {`'value' junk`, "value", false},
	} {
		value, ok := yamlScalar(tt.raw)
		if ok != tt.ok || (ok && value != tt.want) {
			t.Errorf("%q -> %q, %v", tt.raw, value, ok)
		}
	}
}
func TestDesktopQuickTunnelParsingAndBoundedOutput(t *testing.T) {
	for _, tt := range []struct{ text, want string }{
		{"Visit https://some-long-name.trycloudflare.com |", "https://some-long-name.trycloudflare.com"},
		{"https://x.trycloudflare.com", "https://x.trycloudflare.com"},
		{"https://x.trycloudflare.com.evil", ""}, {"https://x.trycloudflare.comfoo", ""},
		{"https://-bad.trycloudflare.com", ""}, {"https://bad-.trycloudflare.com", ""},
	} {
		if got := quickTunnelURL(tt.text); got != tt.want {
			t.Errorf("%q -> %q", tt.text, got)
		}
	}
	urls := make(chan string, 1)
	var output bytes.Buffer
	writer := &tunnelOutput{log: &output, urls: urls}
	for _, chunk := range []string{"line https://split-na", "me.trycloudflare.com |\n"} {
		if _, err := writer.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if got := <-urls; got != "https://split-name.trycloudflare.com" {
		t.Fatal(got)
	}
	if output.String() != "line https://split-name.trycloudflare.com |\n" {
		t.Fatal(output.String())
	}
	writer.log = io.Discard
	if n, err := writer.Write(bytes.Repeat([]byte{'x'}, 2*1024*1024)); n != 2*1024*1024 || err != nil {
		t.Fatal(n, err)
	}
	if len(writer.tail) > 512 {
		t.Fatal("unbounded retained tunnel output")
	}
}
func desktopProcessAlive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	b, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "stat=").Output()
	return err == nil && !strings.HasPrefix(strings.TrimSpace(string(b)), "Z")
}
func TestDesktopChildGroupCleanupAfterLeaderExit(t *testing.T) {
	cfg := stopFixture(t)
	children := newDesktopChildren(cfg)
	defer children.close()
	pidFile := filepath.Join(cfg.DataDir, "descendant.pid")
	command := "(trap '' TERM; exec /bin/sleep 30) &\nprintf '%s' \"$!\" > " + quote(pidFile) + "\nwait"
	if err := children.start("/bin/sh", []string{"-c", command}, os.Environ(), io.Discard, "fixture-owner.json"); err != nil {
		t.Fatal(err)
	}
	var pid int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(string(raw))
			if pid > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("fixture descendant never started")
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	var record map[string]any
	raw, err := os.ReadFile(filepath.Join(cfg.DataDir, "fixture-owner.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if record["processStart"] == "" || record["executable"] != "/bin/sh" {
		t.Fatal(record)
	}
	if err := children.children[0].cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(time.Second)
	for desktopProcessAlive(children.children[0].cmd.Process.Pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if desktopProcessAlive(children.children[0].cmd.Process.Pid) {
		t.Fatal("leader still alive")
	}
	if !desktopProcessAlive(pid) {
		t.Fatal("fixture descendant unexpectedly gone")
	}
	children.close()
	select {
	case <-children.children[0].done:
	default:
		t.Fatal("child wait or output copier was not reclaimed")
	}
	deadline = time.Now().Add(2 * time.Second)
	for desktopProcessAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if desktopProcessAlive(pid) {
		t.Fatalf("descendant %d survived cleanup", pid)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "fixture-owner.json")); !os.IsNotExist(err) {
		t.Fatal("stale child metadata", err)
	}
	children.close()
}
func TestDesktopChildStartFailureReapsProcess(t *testing.T) {
	cfg := stopFixture(t)
	children := newDesktopChildren(cfg)
	defer children.close()
	if err := os.Mkdir(filepath.Join(cfg.DataDir, "conflict.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := children.start("/bin/sleep", []string{"30"}, os.Environ(), io.Discard, "conflict.json"); err == nil {
		t.Fatal("metadata failure accepted")
	}
	child := children.children[0]
	select {
	case <-child.done:
	default:
		t.Fatal("failed startup child not reaped")
	}
	if desktopProcessAlive(child.cmd.Process.Pid) {
		t.Fatalf("failed startup child %d alive", child.cmd.Process.Pid)
	}
}

func TestDesktopQuickTunnelHasIsolatedConfig(t *testing.T) {
	cfg := stopFixture(t)
	args, cleanup, err := desktopTunnelArgs(cfg, "quick", "/not-read/config.yml", "other", "8989")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if len(args) != 6 || args[1] != "--config" || args[5] != "http://127.0.0.1:8989" {
		t.Fatal(args)
	}
	raw, err := os.ReadFile(args[2])
	if err != nil || string(raw) != "{}\n" {
		t.Fatal(string(raw), err)
	}
	if st, err := os.Stat(args[2]); err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("private config mode", err)
	}
	cleanup()
	if _, err := os.Stat(args[2]); !os.IsNotExist(err) {
		t.Fatal("temporary config not removed", err)
	}
	named, cleanupNamed, err := desktopTunnelArgs(cfg, "named", "/fixture/config.yml", "fixture", "8989")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupNamed()
	if strings.Join(named, " ") != "tunnel --config /fixture/config.yml --no-autoupdate run fixture" {
		t.Fatal(named)
	}
}
