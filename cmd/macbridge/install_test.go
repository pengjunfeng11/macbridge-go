package main

import (
	"encoding/xml"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"macbridge/internal/core"
)

type plistNode struct {
	XMLName  xml.Name
	Text     string      `xml:",chardata"`
	Children []plistNode `xml:",any"`
}

func TestRotateTokenHonorsSource(t *testing.T) {
	cfg, _ := installerFixture(t)
	file := filepath.Join(cfg.DataDir, "custom", "bearer")
	t.Setenv("MAC_DEV_BRIDGE_HTTP_TOKEN_FILE", file)
	if e := localCommand(cfg, "init", nil); e != nil {
		t.Fatal(e)
	}
	before, e := os.ReadFile(file)
	if e != nil {
		t.Fatal(e)
	}
	if e = rotateToken(cfg); e != nil {
		t.Fatal(e)
	}
	after, e := os.ReadFile(file)
	if e != nil {
		t.Fatal(e)
	}
	if string(before) == string(after) || len(strings.TrimSpace(string(after))) < 24 {
		t.Fatal("custom token was not rotated")
	}
	t.Setenv("MAC_DEV_BRIDGE_HTTP_TOKEN_FILE", "")
	t.Setenv("MAC_DEV_BRIDGE_HTTP_TOKEN", strings.Repeat("x", 40))
	if e = rotateToken(cfg); e == nil {
		t.Fatal("cannot silently rotate an immutable external token source")
	}
	if _, e = os.Stat(cfg.UnlockFile); e != nil {
		t.Fatal("rejected rotation stopped the bridge")
	}
}

func plistDict(t *testing.T, node plistNode) map[string]plistNode {
	t.Helper()
	result := map[string]plistNode{}
	for i := 0; i < len(node.Children); i += 2 {
		if i+1 >= len(node.Children) || node.Children[i].XMLName.Local != "key" {
			t.Fatal("Invalid plist dictionary")
		}
		result[node.Children[i].Text] = node.Children[i+1]
	}
	return result
}
func readPlist(t *testing.T, path string) map[string]plistNode {
	t.Helper()
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var root plistNode
	if e = xml.Unmarshal(b, &root); e != nil {
		t.Fatal(e)
	}
	if len(root.Children) != 1 {
		t.Fatal("Invalid plist root")
	}
	return plistDict(t, root.Children[0])
}
func installerFixture(t *testing.T) (core.Config, string) {
	t.Helper()
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "MAC_DEV_BRIDGE_") || name == "CONTROL_PLANE_API_KEY" || name == "CONTROL_PLANE_TUNNEL_ID" || name == "TUNNEL_CLIENT_BIN" {
			t.Setenv(name, "")
		}
	}
	home := filepath.Join(t.TempDir(), "Home with spaces & <tag> 'quoted'")
	cfg := core.Config{Home: home, DataDir: filepath.Join(home, "App Data"), LogDir: filepath.Join(home, "App Logs"), UnlockFile: filepath.Join(home, "Custom Unlock", "nested", "enabled"), Shell: "/bin/sh", CodexBin: filepath.Join(home, "Codex Tools", "codex"), ChromeSocket: filepath.Join(home, "chrome.socket")}
	bin := filepath.Join(home, "Mock Tools")
	if e := os.MkdirAll(bin, 0700); e != nil {
		t.Fatal(e)
	}
	log := filepath.Join(home, "launchctl.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + quote(log) + "\nexit 0\n"
	if e := os.WriteFile(filepath.Join(bin, "launchctl"), []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	return cfg, bin
}
func TestHTTPInstallerPreservesPathsAndConfiguration(t *testing.T) {
	cfg, _ := installerFixture(t)
	tokenFile := filepath.Join(cfg.Home, "Custom Tokens", "http.token")
	t.Setenv("MAC_DEV_BRIDGE_HTTP_TOKEN_FILE", tokenFile)
	t.Setenv("MAC_DEV_BRIDGE_HTTP_PORT", "9898")
	t.Setenv("MAC_DEV_BRIDGE_PUBLIC_URL", "https://bridge.example.test")
	t.Setenv("MAC_DEV_BRIDGE_JOB_LOG_MAX_BYTES", "2048")
	t.Setenv("MAC_DEV_BRIDGE_FULL_ACCESS_ACK", "must-not-be-persisted")
	t.Setenv("CONTROL_PLANE_API_KEY", "fixture-key-not-for-launchd")
	if e := installLaunchAgent(cfg); e != nil {
		t.Fatal(e)
	}
	token, e := os.ReadFile(tokenFile)
	if e != nil || len(strings.TrimSpace(string(token))) < 24 {
		t.Fatal("Custom token was not created", e)
	}
	if e = localCommand(cfg, "init", nil); e != nil {
		t.Fatal(e)
	}
	again, _ := os.ReadFile(tokenFile)
	if string(token) != string(again) {
		t.Fatal("Re-initialization rotated the existing token")
	}
	if _, e = os.Stat(cfg.UnlockFile); e != nil {
		t.Fatal("Custom unlock parent was not created", e)
	}
	plist := readPlist(t, filepath.Join(cfg.Home, "Library", "LaunchAgents", launchLabel+".plist"))
	env := plistDict(t, plist["EnvironmentVariables"])
	for name, want := range map[string]string{"MAC_DEV_BRIDGE_HTTP_TOKEN_FILE": tokenFile, "MAC_DEV_BRIDGE_HTTP_PORT": "9898", "MAC_DEV_BRIDGE_PUBLIC_URL": "https://bridge.example.test", "MAC_DEV_BRIDGE_JOB_LOG_MAX_BYTES": "2048", "MAC_DEV_BRIDGE_DATA_DIR": cfg.DataDir, "MAC_DEV_BRIDGE_LOG_DIR": cfg.LogDir, "MAC_DEV_BRIDGE_UNLOCK_FILE": cfg.UnlockFile, "MAC_DEV_BRIDGE_SHELL": cfg.Shell, "MAC_DEV_BRIDGE_CHROME_SOCKET": cfg.ChromeSocket, "CODEX_BIN": cfg.CodexBin} {
		if env[name].Text != want {
			t.Fatalf("%s not preserved", name)
		}
	}
	for _, name := range []string{"MAC_DEV_BRIDGE_FULL_ACCESS_ACK", "CONTROL_PLANE_API_KEY", "MAC_DEV_BRIDGE_HTTP_TOKEN"} {
		if _, ok := env[name]; ok {
			t.Fatalf("Transient credential %s persisted", name)
		}
	}
	if e = localCommand(cfg, "uninstall", nil); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(cfg.Home, "Library", "LaunchAgents", launchLabel+".plist")); !os.IsNotExist(e) {
		t.Fatal("HTTP plist remains")
	}
	if _, e = os.Stat(tokenFile); e != nil {
		t.Fatal("Uninstall erased retained token", e)
	}
}
func TestBrowserInstallerRetainsCustomUnlockAndQuotedPaths(t *testing.T) {
	cfg, _ := installerFixture(t)
	if e := localCommand(cfg, "install-browser", []string{"--extension-id", strings.Repeat("a", 32), "--profile-id", "profile-test"}); e != nil {
		t.Fatal(e)
	}
	wrapper := filepath.Join(cfg.DataDir, "chrome-host.sh")
	b, e := os.ReadFile(wrapper)
	if e != nil {
		t.Fatal(e)
	}
	for _, path := range []string{cfg.DataDir, cfg.LogDir, cfg.UnlockFile, cfg.ChromeSocket} {
		if !strings.Contains(string(b), quote(path)) {
			t.Fatal("Wrapper lost quoted configuration")
		}
	}
	if e = localCommand(cfg, "uninstall-browser", nil); e != nil {
		t.Fatal(e)
	}
}
func TestTunnelRunCompletesStdioInitialization(t *testing.T) {
	if os.Getenv("MACBRIDGE_TEST_TUNNEL_RUN") == "1" {
		if err := runSecureTunnel(core.FromEnv()); err != nil {
			t.Fatal(err)
		}
		t.Fatal("tunnel process was not replaced")
	}
	cfg, bin := installerFixture(t)
	log := filepath.Join(cfg.Home, "tunnel-run.args")
	client := filepath.Join(bin, "tunnel client")
	if err := os.WriteFile(client, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+quote(log)+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MACBRIDGE_TEST_TUNNEL_RUN", "1")
	t.Setenv("MAC_DEV_BRIDGE_DATA_DIR", cfg.DataDir)
	t.Setenv("MAC_DEV_BRIDGE_PROFILE", "existing-profile")
	t.Setenv("TUNNEL_CLIENT_BIN", client)
	t.Setenv("CONTROL_PLANE_API_KEY", "fixture-runtime-key")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(exe, "-test.run=^TestTunnelRunCompletesStdioInitialization$").CombinedOutput(); err != nil {
		t.Fatalf("run tunnel: %v\n%s", err, output)
	}
	args, err := os.ReadFile(log)
	if err != nil || string(args) != "run\n--profile\nexisting-profile\n--mcp.stdio-send-initialized-notification=true\n" {
		t.Fatalf("stdio initialization compatibility missing: %q, %v", args, err)
	}
}

func TestTunnelInstallerMocksKeychainAndResolvesExecutable(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(filepath.Base(exe), " ") {
		// Exercise the real os.Executable path used by installed macOS bundles.
		relocated := filepath.Join(t.TempDir(), "MacBridge Go's installer test")
		binary, err := os.ReadFile(exe)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(relocated, binary, 0700); err != nil {
			t.Fatal(err)
		}
		if output, err := exec.Command(relocated, "-test.run=^TestTunnelInstallerMocksKeychainAndResolvesExecutable$").CombinedOutput(); err != nil {
			t.Fatalf("installer at quoted path: %v\n%s", err, output)
		}
		return
	}
	cfg, bin := installerFixture(t)
	log := filepath.Join(cfg.Home, "tunnel.args")
	client := filepath.Join(bin, "tunnel client with spaces")
	mock := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + quote(log) + "\nprintf '%s' \"$CONTROL_PLANE_API_KEY\" > " + quote(filepath.Join(cfg.Home, "tunnel.key.fixture")) + "\nexit 0\n"
	if e := os.WriteFile(client, []byte(mock), 0700); e != nil {
		t.Fatal(e)
	}
	link := filepath.Join(bin, "tunnel-alias")
	if e := os.Symlink(client, link); e != nil {
		t.Fatal(e)
	}
	securityLog := filepath.Join(cfg.Home, "keychain.args")
	security := filepath.Join(bin, "security mock")
	if e := os.WriteFile(security, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+quote(securityLog)+"\nexit 0\n"), 0700); e != nil {
		t.Fatal(e)
	}
	before := securityTool
	securityTool = security
	t.Cleanup(func() { securityTool = before })
	t.Setenv("TUNNEL_CLIENT_BIN", "tunnel-alias")
	t.Setenv("CONTROL_PLANE_API_KEY", "fixture-runtime-key")
	t.Setenv("CONTROL_PLANE_TUNNEL_ID", "fixture-tunnel-id")
	t.Setenv("MAC_DEV_BRIDGE_PROFILE", "fixture-profile")
	t.Setenv("MAC_DEV_BRIDGE_KEYCHAIN_SERVICE", "Fixture Keychain Service")
	if e := installSecureTunnel(cfg); e != nil {
		t.Fatal(e)
	}
	stored, e := os.ReadFile(filepath.Join(cfg.DataDir, "tunnel-client-path"))
	resolved, _ := filepath.EvalSymlinks(client)
	if e != nil || strings.TrimSpace(string(stored)) != resolved {
		t.Fatal("Tunnel path not canonicalized", e)
	}
	args, e := os.ReadFile(log)
	if e != nil || !strings.Contains(string(args), "init\n--sample\nsample_mcp_stdio_local\n--profile\nfixture-profile\n--tunnel-id\nfixture-tunnel-id\n--mcp-command\n") {
		t.Fatal("Tunnel init argv changed", e)
	}
	command := strings.Split(strings.TrimSpace(string(args)), "\n")
	decoded, e := exec.Command("/bin/sh", "-c", "set -- "+command[len(command)-1]+"; printf '%s\\n' \"$#\" \"$1\"").CombinedOutput()
	if e != nil || string(decoded) != "1\n"+exe+"\n" {
		t.Fatalf("Tunnel MCP command lost executable boundaries: %q, %v", decoded, e)
	}
	keys, e := os.ReadFile(securityLog)
	if e != nil || !strings.Contains(string(keys), "Fixture Keychain Service") {
		t.Fatal("Mock Keychain not called", e)
	}
	plist := readPlist(t, filepath.Join(cfg.Home, "Library", "LaunchAgents", tunnelLabel+".plist"))
	env := plistDict(t, plist["EnvironmentVariables"])
	if env["MAC_DEV_BRIDGE_UNLOCK_FILE"].Text != cfg.UnlockFile || env["CODEX_BIN"].Text != cfg.CodexBin {
		t.Fatal("Tunnel environment lost configuration")
	}
	if _, ok := env["CONTROL_PLANE_API_KEY"]; ok {
		t.Fatal("Runtime key persisted in plist")
	}
	if e = localCommand(cfg, "uninstall", nil); e != nil {
		t.Fatal(e)
	}
	for _, label := range []string{launchLabel, tunnelLabel} {
		if _, e = os.Stat(filepath.Join(cfg.Home, "Library", "LaunchAgents", label+".plist")); !os.IsNotExist(e) {
			t.Fatal("LaunchAgent plist remains", label)
		}
	}
}
func TestUninstallPreservesServiceFilesWhenStopIsIncomplete(t *testing.T) {
	cfg, _ := installerFixture(t)
	child, _ := testChild(t)
	if e := core.AtomicJSON(filepath.Join(cfg.DataDir, "macbridge-http.json"), map[string]any{"pid": child.Process.Pid, "executable": "/bin/sleep", "processStart": "stale identity"}); e != nil {
		t.Fatal(e)
	}
	for _, label := range []string{launchLabel, tunnelLabel} {
		p := filepath.Join(cfg.Home, "Library", "LaunchAgents", label+".plist")
		if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(p, []byte("fixture"), 0600); e != nil {
			t.Fatal(e)
		}
	}
	if e := localCommand(cfg, "uninstall", nil); e == nil {
		t.Fatal("Uninstall hid stop failure")
	}
	for _, label := range []string{launchLabel, tunnelLabel} {
		if _, e := os.Stat(filepath.Join(cfg.Home, "Library", "LaunchAgents", label+".plist")); e != nil {
			t.Fatal("Uninstall removed recovery metadata", e)
		}
	}
}
