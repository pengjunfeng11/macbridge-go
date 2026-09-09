package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"macbridge/internal/browser"
	"macbridge/internal/core"
	"macbridge/internal/host"
	"macbridge/internal/server"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const launchLabel = "local.macbridge.go"

type stringsFlag []string

func (s *stringsFlag) String() string     { return strings.Join(*s, ",") }
func (s *stringsFlag) Set(v string) error { *s = append(*s, v); return nil }
func quote(s string) string               { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
func localCommand(cfg core.Config, command string, args []string) error {
	switch command {
	case "init":
		if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
			return err
		}
		if err := os.MkdirAll(cfg.LogDir, 0700); err != nil {
			return err
		}
		tokenFile := core.Env("MAC_DEV_BRIDGE_HTTP_TOKEN_FILE", filepath.Join(cfg.DataDir, "http-token"))
		if err := os.MkdirAll(filepath.Dir(tokenFile), 0700); err != nil {
			return err
		}
		provided := os.Getenv("MAC_DEV_BRIDGE_HTTP_TOKEN")
		if provided != "" {
			if len(provided) < 24 {
				return fmt.Errorf("HTTP token must contain at least 24 printable ASCII bytes")
			}
			for _, r := range provided {
				if r < 33 || r > 126 {
					return fmt.Errorf("HTTP token must be printable ASCII without spaces")
				}
			}
		}
		_, statErr := os.Stat(tokenFile)
		if statErr != nil && !os.IsNotExist(statErr) {
			return statErr
		}
		if os.IsNotExist(statErr) || (provided != "" && os.Getenv("MAC_DEV_BRIDGE_HTTP_TOKEN_FILE") == "") {
			token := provided
			if token == "" {
				token = core.ID() + core.ID()
			}
			if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0600); err != nil {
				return err
			}
		}
		if err := os.Chmod(tokenFile, 0600); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(cfg.UnlockFile), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(cfg.UnlockFile, []byte(server.Acknowledgement+"\n"), 0600); err != nil {
			return err
		}
		if err := os.Chmod(cfg.UnlockFile, 0600); err != nil {
			return err
		}

		token, err := readToken(cfg)
		if err != nil {
			return err
		}
		loadRuntime(cfg)
		oauth, err := server.NewOAuth(cfg, token)
		if err != nil {
			return err
		}
		fmt.Println("Ready. Token file:", tokenFile)
		fmt.Println("OAuth client_id:", oauth.ClientID())
		fmt.Println("Start: macbridge http (or macbridge stdio)")
		return nil
	case "doctor":
		loadRuntime(cfg)
		exe, _ := os.Executable()
		_, tokenErr := readToken(cfg)
		_, unlockErr := os.Stat(cfg.UnlockFile)
		client := http.Client{Timeout: time.Second}
		resp, healthErr := client.Get("http://127.0.0.1:" + core.Env("MAC_DEV_BRIDGE_HTTP_PORT", "8787") + "/healthz")
		if resp != nil {
			resp.Body.Close()
		}
		b := browser.New(cfg)
		defer b.Close()
		return printJSON(map[string]any{"executable": exe, "dataDir": cfg.DataDir, "tokenPresent": tokenErr == nil, "unlockPresent": unlockErr == nil, "httpReachable": healthErr == nil, "browser": b.Status()})
	case "stop":
		return stopBridge(cfg)
	case "install-browser":
		flags := flag.NewFlagSet(command, flag.ContinueOnError)
		extension := flags.String("extension", packagePath("chrome-extension"), "Unpacked extension directory")
		extensionID := flags.String("extension-id", "", "Chrome extension ID (optional with manifest key)")
		profile := flags.String("profile-id", "", "Chrome signed-in profile ID to bind before first connection")
		if err := flags.Parse(args); err != nil {
			return err
		}
		if *extensionID == "" {
			raw, err := os.ReadFile(filepath.Join(*extension, "manifest.json"))
			if err != nil {
				return err
			}
			var manifest map[string]any
			json.Unmarshal(raw, &manifest)
			key, err := base64.StdEncoding.DecodeString(core.String(manifest, "key", ""))
			if err != nil || len(key) == 0 {
				return fmt.Errorf("manifest has no key; supply --extension-id from chrome://extensions")
			}
			sum := sha256.Sum256(key)
			var id strings.Builder
			for _, b := range sum[:16] {
				id.WriteByte('a' + (b >> 4))
				id.WriteByte('a' + (b & 15))
			}
			*extensionID = id.String()
		}
		if len(*extensionID) != 32 || strings.Trim(*extensionID, "abcdefghijklmnop") != "" {
			return fmt.Errorf("invalid extension ID")
		}
		exe, _ := os.Executable()
		wrapper := filepath.Join(cfg.DataDir, "chrome-host.sh")
		if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
			return err
		}
		script := "#!/bin/sh\nexport MAC_DEV_BRIDGE_DATA_DIR=" + quote(cfg.DataDir) + "\nexport MAC_DEV_BRIDGE_LOG_DIR=" + quote(cfg.LogDir) + "\nexport MAC_DEV_BRIDGE_UNLOCK_FILE=" + quote(cfg.UnlockFile) + "\nexport MAC_DEV_BRIDGE_CHROME_SOCKET=" + quote(cfg.ChromeSocket) + "\nexec " + quote(exe) + " chrome-host\n"
		if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
			return err
		}
		manifest := map[string]any{"name": browser.NativeHostName, "description": "MacBridge Go native host", "path": wrapper, "type": "stdio", "allowed_origins": []string{"chrome-extension://" + *extensionID + "/"}}
		target := filepath.Join(cfg.Home, "Library", "Application Support", "Google", "Chrome", "NativeMessagingHosts", browser.NativeHostName+".json")
		if err := core.AtomicJSON(target, manifest); err != nil {
			return err
		}
		if *profile != "" {
			if err := core.AtomicJSON(filepath.Join(cfg.DataDir, "chrome-profile-binding.json"), map[string]any{"profileId": *profile, "extensionId": *extensionID}); err != nil {
				return err
			}
		}
		fmt.Println("Native host installed:", target)
		fmt.Println("Load unpacked in chrome://extensions:", *extension)
		fmt.Println("Extension ID:", *extensionID)
		return nil
	case "uninstall-browser":
		err := os.Remove(filepath.Join(cfg.Home, "Library", "Application Support", "Google", "Chrome", "NativeMessagingHosts", browser.NativeHostName+".json"))
		if os.IsNotExist(err) {
			return nil
		}
		return err
	case "grant-browser", "grant-gui":
		flags := flag.NewFlagSet(command, flag.ContinueOnError)
		ttl := flags.Int("ttl", 900, "Lifetime in seconds")
		provider := flags.String("provider", "chrome-background", "Provider key")
		app := flags.String("app", "", "Foreground app name")
		var patterns stringsFlag
		flags.Var(&patterns, "url-pattern", "Allowed URL pattern (repeatable)")
		if err := flags.Parse(args); err != nil {
			return err
		}
		if *ttl < 1 || *ttl > 900 {
			return fmt.Errorf("ttl must be 1–900 seconds")
		}
		grant := map[string]any{"nonce": core.ID(), "provider": *provider, "expiresAt": time.Now().Add(time.Duration(*ttl) * time.Second).UTC().Format(time.RFC3339Nano)}
		path := filepath.Join(cfg.DataDir, "PERSONAL_BROWSER_APPROVED")
		if command == "grant-gui" {
			if *app == "" {
				return fmt.Errorf("--app is required")
			}
			if *ttl > 300 {
				*ttl = 300
				grant["expiresAt"] = time.Now().Add(time.Duration(*ttl) * time.Second).UTC().Format(time.RFC3339Nano)
			}
			grant["app"] = *app
			grant["apps"] = []string{*app}
			path = filepath.Join(cfg.DataDir, "FOREGROUND_GUI_APPROVED")
		} else {
			if len(patterns) == 0 {
				return fmt.Errorf("--url-pattern is required")
			}
			for _, p := range patterns {
				if strings.HasPrefix(p, "-") || strings.ContainsRune(p, 0) {
					return fmt.Errorf("invalid URL pattern")
				}
			}
			grant["allowedUrlPatterns"] = patterns
			if *provider == "chrome-background" {
				path = filepath.Join(cfg.DataDir, "chrome-background-grants", core.ID()+".json")
			}
		}
		if err := core.AtomicJSON(path, grant); err != nil {
			return err
		}
		fmt.Println("Grant saved:", path)
		return nil
	case "install":
		return installLaunchAgent(cfg)
	case "uninstall":
		if err := stopBridge(cfg); err != nil {
			return err
		}
		for _, label := range []string{launchLabel, tunnelLabel} {
			if err := os.Remove(filepath.Join(cfg.Home, "Library", "LaunchAgents", label+".plist")); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		return nil

	}
	return fmt.Errorf("unknown local command")
}
func printJSON(v any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(v)
}
func packagePath(name string) string {
	exe, _ := os.Executable()
	for _, root := range []string{filepath.Dir(filepath.Dir(exe)), filepath.Join(filepath.Dir(exe), "..", "Resources"), "."} {
		p := filepath.Join(root, name)
		if _, err := os.Stat(p); err == nil {
			abs, _ := filepath.Abs(p)
			return abs
		}
	}
	return name
}
func stopBridge(cfg core.Config) error {
	if err := os.Remove(cfg.UnlockFile); err != nil && !os.IsNotExist(err) {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	failures := []string{}
	runPS := func(pid int, field string) (string, error) {
		cmd := exec.CommandContext(ctx, "ps", "-p", fmt.Sprint(pid), "-o", field+"=")
		cmd.Env = append(os.Environ(), "LC_ALL=C")
		b, e := cmd.Output()
		return strings.TrimSpace(string(b)), e
	}
	identity := func(pid int) (string, bool, error) {
		if pid <= 1 {
			return "", false, fmt.Errorf("invalid recorded PID %d", pid)
		}
		if e := syscall.Kill(pid, 0); e == syscall.ESRCH {
			return "", false, nil
		} else if e != nil {
			return "", false, e
		}
		start, e := runPS(pid, "lstart")
		if e != nil {
			if syscall.Kill(pid, 0) == syscall.ESRCH {
				return "", false, nil
			}
			return "", false, e
		}
		state, e := runPS(pid, "stat")
		if e != nil {
			if syscall.Kill(pid, 0) == syscall.ESRCH {
				return "", false, nil
			}
			return "", false, e
		}
		if strings.HasPrefix(state, "Z") {
			return start, false, nil
		}
		return start, true, nil
	}
	groupLive := func(pgid int) bool { e := syscall.Kill(-pgid, 0); return e == nil || e == syscall.EPERM }
	waitGone := func(pid int, start string, wait time.Duration) bool {
		deadline := time.Now().Add(wait)
		for {
			now, live, e := identity(pid)
			if e == nil && (!live || now != start) {
				return true
			}
			if time.Now().After(deadline) || ctx.Err() != nil {
				return false
			}
			time.Sleep(30 * time.Millisecond)
		}
	}
	stopOwner := func(file string) {
		b, e := os.ReadFile(filepath.Join(cfg.DataDir, file))
		if os.IsNotExist(e) {
			return
		}
		if e != nil {
			failures = append(failures, file+": "+e.Error())
			return
		}
		var state struct {
			PID          int    `json:"pid"`
			Executable   string `json:"executable"`
			ProcessStart string `json:"processStart"`
		}
		if e = json.Unmarshal(b, &state); e != nil {
			failures = append(failures, file+": invalid metadata")
			return
		}
		start, live, e := identity(state.PID)
		if e != nil {
			failures = append(failures, file+": "+e.Error())
			return
		}
		if !live {
			return
		}
		command, e := runPS(state.PID, "comm")
		if e != nil || state.ProcessStart == "" || start != state.ProcessStart || state.Executable == "" || command != state.Executable {
			failures = append(failures, file+": live PID identity differs or cannot be verified; left untouched")
			return
		}
		if e = syscall.Kill(state.PID, syscall.SIGTERM); e != nil && e != syscall.ESRCH {
			failures = append(failures, file+": "+e.Error())
			return
		}
		grace := 3 * time.Second
		if file == "macbridge-desktop.json" {
			grace = 6 * time.Second
		}
		if waitGone(state.PID, start, grace) {
			return
		}
		// Recheck immediately before escalation because the PID may have been reused.
		current, live, e := identity(state.PID)
		if e == nil && live && current == start {
			_ = syscall.Kill(state.PID, syscall.SIGKILL)
		}
		if !waitGone(state.PID, start, 2*time.Second) {
			failures = append(failures, file+": process did not terminate")
		}
	}
	// Boot out only this installed service. Removing the unlock first also defeats
	// its PathState keepalive if launchctl is temporarily unavailable.
	for _, label := range []string{launchLabel, "local.macbridge.tunnel"} {
		plist := filepath.Join(cfg.Home, "Library", "LaunchAgents", label+".plist")
		if _, e := os.Stat(plist); e == nil {
			target := fmt.Sprintf("gui/%d/%s", os.Getuid(), label)
			if b, e := exec.CommandContext(ctx, "launchctl", "bootout", target).CombinedOutput(); e != nil {
				if exec.CommandContext(ctx, "launchctl", "print", target).Run() == nil {
					failures = append(failures, "LaunchAgent remains loaded: "+strings.TrimSpace(string(b)))
				}
			}
		}
	}

	stopOwner("macbridge-desktop.json")
	stopOwner("macbridge-http.json")
	stopOwner("cloudflared-process.json")
	stopOwner("tunnelclient-process.json")
	stopOwner("chrome-native-host.json")
	// Old native-host PID-only metadata cannot establish process identity.
	if _, e := os.Stat(filepath.Join(cfg.DataDir, "chrome-native-host.json")); os.IsNotExist(e) {
		if b, e := os.ReadFile(filepath.Join(cfg.DataDir, "chrome-native-host.pid")); e == nil {
			var pid int
			fmt.Sscan(string(b), &pid)
			_, live, e := identity(pid)
			if e != nil || live {
				failures = append(failures, "native host has PID-only metadata; cannot verify shutdown")
			}
		}
	}
	m, e := host.New(cfg)
	if e != nil {
		return e
	}
	defer m.Close()
	entries, e := os.ReadDir(filepath.Join(cfg.DataDir, "jobs"))
	if e != nil {
		return e
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		b, e := os.ReadFile(filepath.Join(cfg.DataDir, "jobs", entry.Name()))
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			failures = append(failures, entry.Name()+": "+e.Error())
			continue
		}
		var job map[string]any
		if e = json.Unmarshal(b, &job); e != nil {
			failures = append(failures, entry.Name()+": invalid metadata")
			continue
		}
		status := core.String(job, "status", "")
		if status == "succeeded" || status == "failed" || status == "cancelled" {
			continue
		}
		id := core.String(job, "id", "")
		pid := core.Int(job, "pid", 0)
		pgid := core.Int(job, "processGroupId", 0)
		if id == "" || pid <= 1 || pgid <= 1 {
			failures = append(failures, entry.Name()+": missing process identity")
			continue
		}
		verifySupervisor := func() {
			supervisor := core.Int(job, "supervisorPid", 0)
			if supervisor <= 1 {
				return
			}
			start, live, err := identity(supervisor)
			if err != nil || (live && !waitGone(supervisor, start, time.Second)) {
				failures = append(failures, id+": supervisor still live or unverified")
			}
		}
		if core.String(job, "controlSocketPath", "") != "" {
			result, err := m.Call(ctx, "shell_job_kill", map[string]any{"job_id": id, "signal": "SIGKILL"})
			if err == nil {
				state, _ := result.(map[string]any)
				if core.Bool(state, "killed", false) || state["running"] == false {
					deadline := time.Now().Add(3 * time.Second)
					completed := false
					for time.Now().Before(deadline) {
						r, err := m.Call(ctx, "shell_job_status", map[string]any{"job_id": id})
						if err == nil {
							current, _ := r.(map[string]any)
							completed = current["running"] == false
						}
						if completed {
							break
						}
						time.Sleep(30 * time.Millisecond)
					}
					if completed {
						verifySupervisor()
						continue
					}
				}
			}
		}
		// A crashed supervisor may leave children. Only persisted start identities
		// can authorize fallback signals; a fresh manager has no in-memory PTY owner.
		expected := core.String(job, "processStart", "")
		current, live, err := identity(pid)
		if err != nil {
			failures = append(failures, id+": "+err.Error())
			continue
		}
		if live {
			actualGroup, err := syscall.Getpgid(pid)
			if expected == "" || current != expected || err != nil || actualGroup != pgid {
				failures = append(failures, id+": process identity differs or is unavailable; left untouched")
				continue
			}
			if err = syscall.Kill(-pgid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
				failures = append(failures, id+": "+err.Error())
			}
		}
		tracked, _ := job["ttyProcesses"].(map[string]any)
		for key, value := range tracked {
			var target int
			fmt.Sscan(key, &target)
			saved, ok := value.(string)
			if !ok || target <= 1 || target == os.Getpid() {
				continue
			}
			now, alive, err := identity(target)
			if err != nil {
				failures = append(failures, id+": cannot verify terminal child "+key)
				continue
			}
			if alive && saved != "" && now == saved {
				_ = syscall.Kill(target, syscall.SIGKILL)
			}
		}
		deadline := time.Now().Add(2 * time.Second)
		for groupLive(pgid) && time.Now().Before(deadline) && ctx.Err() == nil {
			time.Sleep(30 * time.Millisecond)
		}
		if groupLive(pgid) {
			failures = append(failures, id+": process group remains alive or cannot be attributed safely")
		}
		verifySupervisor()
		for key, value := range tracked {
			var target int
			fmt.Sscan(key, &target)
			saved, _ := value.(string)
			now, alive, err := identity(target)
			if err != nil || (alive && now == saved) {
				failures = append(failures, id+": terminal child "+key+" still live or unverified")
			}
		}
	}
	if ctx.Err() != nil {
		failures = append(failures, ctx.Err().Error())
	}
	if len(failures) > 0 {
		return fmt.Errorf("stop incomplete: %s", strings.Join(failures, "; "))
	}
	fmt.Println("Stopped: LaunchAgent unloaded, recorded desktop/HTTP/tunnel/native-host processes and background job groups verified gone.")
	return nil
}

func xmlText(s string) string {
	var out strings.Builder
	xml.EscapeText(&out, []byte(s))
	return out.String()
}
func installLaunchAgent(cfg core.Config) error {
	if err := localCommand(cfg, "init", nil); err != nil {
		return err
	}
	exe, _ := os.Executable()
	p := filepath.Join(cfg.Home, "Library", "LaunchAgents", launchLabel+".plist")
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>Label</key><string>` + launchLabel + `</string><key>ProgramArguments</key><array><string>` + xmlText(exe) + `</string><string>http</string></array><key>RunAtLoad</key><true/><key>KeepAlive</key><dict><key>PathState</key><dict><key>` + xmlText(cfg.UnlockFile) + `</key><true/></dict></dict><key>EnvironmentVariables</key><dict>` + launchEnvironmentXML(launchEnvironment(cfg)) + `</dict><key>StandardOutPath</key><string>` + xmlText(filepath.Join(cfg.LogDir, "http.stdout.log")) + `</string><key>StandardErrorPath</key><string>` + xmlText(filepath.Join(cfg.LogDir, "http.stderr.log")) + `</string></dict></plist>`

	if err := os.WriteFile(p, []byte(plist), 0600); err != nil {
		return err
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	exec.Command("launchctl", "bootout", domain+"/"+launchLabel).Run()
	if output, err := exec.Command("launchctl", "bootstrap", domain, p).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl: %w: %s", err, output)
	}
	fmt.Println("Installed LaunchAgent:", p)
	return nil
}

// Persist operator configuration for launchd, whose environment differs from the
// installing shell. The bearer is stored in its private token file, and startup
// acknowledgement must not bypass later removal of the unlock file.
func launchEnvironment(cfg core.Config) map[string]string {
	values := map[string]string{}
	for _, item := range os.Environ() {
		name, value, ok := strings.Cut(item, "=")
		if ok && strings.HasPrefix(name, "MAC_DEV_BRIDGE_") && name != "MAC_DEV_BRIDGE_HTTP_TOKEN" && name != "MAC_DEV_BRIDGE_FULL_ACCESS_ACK" {
			values[name] = value
		}
	}
	for name, value := range map[string]string{"PATH": os.Getenv("PATH"), "MAC_DEV_BRIDGE_DATA_DIR": cfg.DataDir, "MAC_DEV_BRIDGE_LOG_DIR": cfg.LogDir, "MAC_DEV_BRIDGE_UNLOCK_FILE": cfg.UnlockFile, "MAC_DEV_BRIDGE_SHELL": cfg.Shell, "MAC_DEV_BRIDGE_CHROME_SOCKET": cfg.ChromeSocket, "CODEX_BIN": cfg.CodexBin} {
		values[name] = value
	}
	return values
}
func launchEnvironmentXML(values map[string]string) string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		b.WriteString("<key>" + xmlText(name) + "</key><string>" + xmlText(values[name]) + "</string>")
	}
	return b.String()
}
