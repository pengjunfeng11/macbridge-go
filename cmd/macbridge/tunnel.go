package main

import (
	"fmt"
	"macbridge/internal/core"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

const tunnelLabel = "local.macbridge.tunnel"

var securityTool = "/usr/bin/security"

func tunnelSettings(cfg core.Config) (string, string) {
	profileRaw, _ := os.ReadFile(filepath.Join(cfg.DataDir, "tunnel-profile"))
	profile := strings.TrimSpace(string(profileRaw))
	if profile == "" {
		profile = "macbridge-go"
	}
	clientRaw, _ := os.ReadFile(filepath.Join(cfg.DataDir, "tunnel-client-path"))
	client := strings.TrimSpace(string(clientRaw))
	if client == "" {
		client = findExecutable("tunnel-client")
	}
	if client == "" {
		client = filepath.Join(cfg.Home, ".local", "bin", "tunnel-client")
	}
	return core.Env("MAC_DEV_BRIDGE_PROFILE", profile), core.Env("TUNNEL_CLIENT_BIN", client)
}
func keychainIdentity(cfg core.Config) (string, string) {
	return core.Env("MAC_DEV_BRIDGE_KEYCHAIN_SERVICE", "OpenAI Secure MCP Tunnel Runtime"), core.Env("MAC_DEV_BRIDGE_KEYCHAIN_ACCOUNT", core.Env("USER", filepath.Base(cfg.Home)))
}
func tunnelKey(cfg core.Config) (string, error) {
	if key := os.Getenv("CONTROL_PLANE_API_KEY"); key != "" {
		return key, nil
	}
	service, account := keychainIdentity(cfg)
	b, e := exec.Command(securityTool, "find-generic-password", "-s", service, "-a", account, "-w").Output()
	if e != nil || strings.TrimSpace(string(b)) == "" {
		return "", fmt.Errorf("set CONTROL_PLANE_API_KEY or store the runtime key in Keychain service %q", service)
	}
	return strings.TrimSpace(string(b)), nil
}
func runSecureTunnel(cfg core.Config) error {
	profile, client := tunnelSettings(cfg)
	client, e := resolveTunnelClient(cfg, client)
	if e != nil {
		return e
	}
	if profile == "" || strings.Trim(profile, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._-") != "" {
		return fmt.Errorf("invalid tunnel profile name")
	}

	key, e := tunnelKey(cfg)
	if e != nil {
		return e
	}
	if st, e := os.Stat(client); e != nil || st.Mode()&0111 == 0 {
		return fmt.Errorf("tunnel-client is missing or not executable: %s", client)
	}
	if e = recordOwner(cfg, "tunnelclient-process.json", os.Getpid(), client); e != nil {
		return e
	}
	err := syscall.Exec(client, []string{client, "run", "--profile", profile, "--mcp.stdio-send-initialized-notification=true"}, append(os.Environ(), "CONTROL_PLANE_API_KEY="+key))
	_ = os.Remove(filepath.Join(cfg.DataDir, "tunnelclient-process.json"))
	return err
}
func installSecureTunnel(cfg core.Config) error {
	profile, client := tunnelSettings(cfg)
	client, e := resolveTunnelClient(cfg, client)
	if e != nil {
		return e
	}
	if profile == "" || strings.Trim(profile, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._-") != "" {
		return fmt.Errorf("invalid tunnel profile name")
	}

	if st, e := os.Stat(client); e != nil || st.Mode()&0111 == 0 {
		return fmt.Errorf("install tunnel-client and set TUNNEL_CLIENT_BIN first")
	}
	id := os.Getenv("CONTROL_PLANE_TUNNEL_ID")
	if id == "" {
		return fmt.Errorf("CONTROL_PLANE_TUNNEL_ID is required")
	}
	key, e := tunnelKey(cfg)
	if e != nil {
		return e
	}
	if e = localCommand(cfg, "init", nil); e != nil {
		return e
	}
	service, account := keychainIdentity(cfg)
	if e = exec.Command(securityTool, "add-generic-password", "-U", "-s", service, "-a", account, "-w", key).Run(); e != nil {
		return fmt.Errorf("save runtime key: %w", e)
	}
	exe, _ := os.Executable()
	cmd := exec.Command(client, "init", "--sample", "sample_mcp_stdio_local", "--profile", profile, "--tunnel-id", id, "--mcp-command", quote(exe))
	cmd.Env = append(os.Environ(), "CONTROL_PLANE_API_KEY="+key)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if e = cmd.Run(); e != nil {
		return fmt.Errorf("initialize tunnel profile: %w", e)
	}
	for name, value := range map[string]string{"tunnel-profile": profile, "tunnel-client-path": client, "tunnel-id": id} {
		if e = os.WriteFile(filepath.Join(cfg.DataDir, name), []byte(value+"\n"), 0600); e != nil {
			return e
		}
	}
	env := launchEnvironment(cfg)
	env["MAC_DEV_BRIDGE_KEYCHAIN_SERVICE"] = service
	env["MAC_DEV_BRIDGE_KEYCHAIN_ACCOUNT"] = account
	environment := launchEnvironmentXML(env)

	plist := `<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>Label</key><string>` + tunnelLabel + `</string><key>ProgramArguments</key><array><string>` + xmlText(exe) + `</string><string>tunnel</string></array><key>RunAtLoad</key><true/><key>KeepAlive</key><dict><key>PathState</key><dict><key>` + xmlText(cfg.UnlockFile) + `</key><true/></dict></dict><key>ThrottleInterval</key><integer>10</integer><key>EnvironmentVariables</key><dict>` + environment + `</dict><key>StandardOutPath</key><string>` + xmlText(filepath.Join(cfg.LogDir, "secure-tunnel.log")) + `</string><key>StandardErrorPath</key><string>` + xmlText(filepath.Join(cfg.LogDir, "secure-tunnel.log")) + `</string></dict></plist>`
	file := filepath.Join(cfg.Home, "Library", "LaunchAgents", tunnelLabel+".plist")
	if e = os.MkdirAll(filepath.Dir(file), 0700); e != nil {
		return e
	}
	if e = os.WriteFile(file, []byte(plist), 0600); e != nil {
		return e
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	_ = exec.Command("launchctl", "bootout", domain+"/"+tunnelLabel).Run()
	if b, e := exec.Command("launchctl", "bootstrap", domain, file).CombinedOutput(); e != nil {
		return fmt.Errorf("load tunnel LaunchAgent: %w: %s", e, b)
	}
	fmt.Println("Installed Secure MCP Tunnel profile:", profile)
	return nil
}

func resolveTunnelClient(cfg core.Config, raw string) (string, error) {
	if strings.HasPrefix(raw, "~/") {
		raw = cfg.Path(raw)
	}
	if !strings.ContainsRune(raw, filepath.Separator) {
		resolved, e := exec.LookPath(raw)
		if e != nil {
			return "", e
		}
		raw = resolved
	}
	absolute, e := filepath.Abs(raw)
	if e != nil {
		return "", e
	}
	resolved, e := filepath.EvalSymlinks(absolute)
	if e != nil {
		return "", e
	}
	info, e := os.Stat(resolved)
	if e != nil {
		return "", e
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", fmt.Errorf("tunnel-client is not executable: %s", resolved)
	}
	return resolved, nil
}
