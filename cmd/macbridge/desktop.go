package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"macbridge/internal/core"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The desktop owner supervises transport processes; the AppKit menu is only a UI.
// Children are reaped here on exit, and exact identities allow `stop` after a crash.
func runDesktop(cfg core.Config) error {
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(cfg.DataDir, "desktop.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("desktop owner already running")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if err = localCommand(cfg, "init", nil); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()
	exe, _ := os.Executable()
	if err = recordOwner(cfg, "macbridge-desktop.json", os.Getpid(), exe); err != nil {
		return err
	}
	defer os.Remove(filepath.Join(cfg.DataDir, "macbridge-desktop.json"))
	loadRuntime(cfg)
	var settings map[string]any
	raw, _ := os.ReadFile(filepath.Join(cfg.DataDir, "runtime.json"))
	json.Unmarshal(raw, &settings)
	mode := core.Env("MAC_DEV_BRIDGE_TUNNEL_MODE", core.String(settings, "tunnelMode", "auto"))
	port := core.Env("MAC_DEV_BRIDGE_HTTP_PORT", "8787")
	if n, e := strconv.Atoi(port); e != nil || n < 1 || n > 65535 {
		return fmt.Errorf("desktop HTTP port must be between 1 and 65535")
	}
	publicURL := os.Getenv("MAC_DEV_BRIDGE_PUBLIC_URL")
	cf := findExecutable("cloudflared")
	config := core.String(settings, "cloudflaredConfig", "")
	if config != "" {
		config = cfg.Path(config)
	}
	if config == "" {
		for _, name := range []string{"config.yml", "config.yaml"} {
			p := filepath.Join(cfg.Home, ".cloudflared", name)
			if _, e := os.Stat(p); e == nil {
				config = p
				break
			}
		}
	}
	tunnel, hostname := "", ""
	if config != "" {
		tunnel, hostname = namedTunnelForPort(config, port)
	}
	if mode == "auto" {
		switch {
		case publicURL != "":
			mode = "off"
		case tunnel != "" && hostname != "":
			mode = "named"
		case cf != "":
			mode = "quick"
		default:
			mode = "off"
		}
	}
	if mode != "off" && mode != "named" && mode != "quick" {
		return fmt.Errorf("tunnelMode must be auto, off, named, or quick")
	}
	if mode != "off" && cf == "" {
		return fmt.Errorf("cloudflared not found; install it or use tunnelMode off")
	}
	if mode == "named" && (tunnel == "" || hostname == "") {
		return fmt.Errorf("named tunnel requires a tunnel and a hostname ingress routing to http://127.0.0.1:" + port + " in cloudflared config")
	}
	children := newDesktopChildren(cfg)
	defer children.close()
	env := os.Environ()
	if mode != "off" {
		log, err := desktopLog(cfg, "tunnel.log")
		if err != nil {
			return err
		}
		defer log.Close()
		urls := make(chan string, 1)
		output := &tunnelOutput{log: log, urls: urls}
		args, removeConfig, err := desktopTunnelArgs(cfg, mode, config, tunnel, port)
		if err != nil {
			return err
		}
		defer removeConfig()
		if mode == "named" {
			publicURL = "https://" + hostname
		}
		if err = children.start(cf, args, env, output, "cloudflared-process.json"); err != nil {
			return err
		}
		if mode == "quick" {
			select {
			case publicURL = <-urls:
			case e := <-children.exits:
				return e
			case <-ctx.Done():
				return nil
			case <-time.After(45 * time.Second):
				return fmt.Errorf("cloudflared did not announce a URL within 45 seconds; see tunnel.log")
			}
		}
	}
	if publicURL == "" {
		publicURL = "http://127.0.0.1:" + port
	}
	u, e := url.Parse(publicURL)
	if e != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("public URL must be an http(s) origin without credentials, path, query, or fragment")
	}
	publicURL = strings.TrimRight(publicURL, "/")
	// The issuer is fixed before the HTTP server starts, including quick tunnels.
	env = append(env, "MAC_DEV_BRIDGE_PUBLIC_URL="+publicURL)
	log, err := desktopLog(cfg, "bridge.log")
	if err != nil {
		return err
	}
	defer log.Close()
	defer children.close()
	if err = children.start(exe, []string{"http"}, env, log, "macbridge-http.json"); err != nil {
		return err
	}
	if err = core.AtomicJSON(filepath.Join(cfg.DataDir, "desktop-state.json"), map[string]any{"publicURL": publicURL, "mcpURL": publicURL + "/mcp", "tunnelMode": mode, "httpPort": port, "pid": os.Getpid(), "startedAt": core.Now()}); err != nil {
		return err
	}
	defer os.Remove(filepath.Join(cfg.DataDir, "desktop-state.json"))
	fmt.Fprintln(os.Stderr, "MacBridge ready:", publicURL+"/mcp")
	select {
	case <-ctx.Done():
		return nil
	case e := <-children.exits:
		return e
	}
}

func recordOwner(cfg core.Config, name string, pid int, executable string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-p", fmt.Sprint(pid), "-o", "lstart=")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	raw, e := cmd.Output()
	if e != nil {
		return e
	}
	start := strings.TrimSpace(string(raw))
	if start == "" {
		return fmt.Errorf("cannot record process identity")
	}
	return core.AtomicJSON(filepath.Join(cfg.DataDir, name), map[string]any{"pid": pid, "executable": executable, "processStart": start, "startedAt": core.Now()})
}
func desktopLog(cfg core.Config, name string) (*os.File, error) {
	if e := os.MkdirAll(cfg.LogDir, 0700); e != nil {
		return nil, e
	}
	path := filepath.Join(cfg.LogDir, name)
	// ponytail: general transport logs rotate on start at 16 MiB; use system log rotation for long-lived noisy tunnels.
	if st, e := os.Stat(path); e == nil && st.Size() > 16*1024*1024 {
		_ = os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
}
func findExecutable(name string) string {
	if path, e := exec.LookPath(name); e == nil {
		return path
	}
	for _, dir := range []string{"/opt/homebrew/bin", "/usr/local/bin"} {
		p := filepath.Join(dir, name)
		if st, e := os.Stat(p); e == nil && st.Mode()&0111 != 0 {
			return p
		}
	}
	return ""
}

// A process has one waiter; cleanup waits for whole process groups, not just leaders.
type desktopChild struct {
	cmd    *exec.Cmd
	done   chan struct{}
	record string
}
type desktopChildren struct {
	cfg      core.Config
	children []*desktopChild
	exits    chan error
	once     sync.Once
}

func newDesktopChildren(cfg core.Config) *desktopChildren {
	return &desktopChildren{cfg: cfg, exits: make(chan error, 2)}
}
func (d *desktopChildren) start(command string, args, env []string, output io.Writer, record string) error {
	cmd := exec.Command(command, args...)
	cmd.Env, cmd.Stdout, cmd.Stderr = env, output, output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	child := &desktopChild{cmd: cmd, done: make(chan struct{}), record: record}
	d.children = append(d.children, child)
	go func() {
		err := cmd.Wait()
		close(child.done)
		if err == nil {
			err = fmt.Errorf("%s exited", filepath.Base(command))
		}
		select {
		case d.exits <- err:
		default:
		}
	}()
	if err := recordOwner(d.cfg, record, cmd.Process.Pid, command); err != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-child.done
		return err
	}
	return nil
}
func (d *desktopChildren) close() {
	d.once.Do(func() {
		for _, child := range d.children {
			_ = syscall.Kill(-child.cmd.Process.Pid, syscall.SIGTERM)
		}
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			live := false
			for _, child := range d.children {
				if syscall.Kill(-child.cmd.Process.Pid, 0) == nil {
					live = true
				}
			}
			if !live {
				break
			}
			time.Sleep(40 * time.Millisecond)
		}
		for _, child := range d.children {
			// The leader may already be gone while a descendant still owns the group.
			_ = syscall.Kill(-child.cmd.Process.Pid, syscall.SIGKILL)
			select {
			case <-child.done:
				_ = os.Remove(filepath.Join(d.cfg.DataDir, child.record))
			case <-time.After(2 * time.Second):
			}
		}
	})
}

// Writes log bytes directly, retaining only enough overlap to find a split URL.
// A long unbroken log line cannot stall a subprocess or grow memory without bound.
type tunnelOutput struct {
	mu   sync.Mutex
	log  io.Writer
	urls chan<- string
	tail string
}

func (o *tunnelOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n, err := o.log.Write(p)
	text := o.tail + string(p[:n])
	if u := quickTunnelURL(text); u != "" {
		select {
		case o.urls <- u:
		default:
		}
	}
	if len(text) > 512 {
		text = text[len(text)-512:]
	}
	o.tail = text
	return n, err
}

var quickURL = regexp.MustCompile(`https://[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.trycloudflare\.com(?:[^a-zA-Z0-9_.-]|$)`)

func quickTunnelURL(line string) string {
	match := quickURL.FindString(line)
	if match == "" {
		return ""
	}
	end := strings.Index(match, ".trycloudflare.com") + len(".trycloudflare.com")
	return match[:end]
}

// Extract common scalar YAML fields; cloudflared validates the full YAML itself.
// An ingress is eligible only when it reaches this bridge's configured listener.
func namedTunnel(path string) (string, string) { return namedTunnelForPort(path, "") }
func namedTunnelForPort(path, port string) (string, string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	var tunnel, host, service, chosen string
	ingress, entry := false, false
	choose := func() {
		if chosen != "" || !validTunnelHostname(host) {
			return
		}
		if port == "" {
			chosen = host
			return
		}
		u, err := url.Parse(service)
		if err != nil || u.Scheme != "http" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return
		}
		if u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" {
			return
		}
		servicePort := u.Port()
		if servicePort == "" {
			servicePort = "80"
		}
		if servicePort == port {
			chosen = host
		}
	}
	for _, rawLine := range strings.Split(string(raw), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		top := len(rawLine) == len(strings.TrimLeft(rawLine, " \t"))
		if top && !strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "ingress:") {
			if ingress {
				choose()
				ingress, entry = false, false
			}
		}
		if strings.HasPrefix(line, "-") && ingress {
			choose()
			host, service = "", ""
			entry = true
			line = strings.TrimSpace(strings.TrimPrefix(line, "-"))
		}
		key, rawValue, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value, valid := yamlScalar(rawValue)
		if !valid {
			continue
		}
		if top && key == "tunnel" && tunnel == "" {
			tunnel = value
		}
		if top && key == "ingress" {
			ingress = true
			continue
		}
		if ingress && entry && key == "hostname" {
			host = value
		}
		if ingress && entry && key == "service" {
			service = value
		}
	}
	choose()
	if tunnel == "" || strings.HasPrefix(tunnel, "-") || strings.ContainsAny(tunnel, "\r\n\x00") {
		return "", ""
	}
	return tunnel, chosen
}
func yamlScalar(text string) (string, bool) {
	text = strings.TrimSpace(text)
	if text == "" || strings.HasPrefix(text, "#") {
		return "", true
	}
	if text[0] == '\'' {
		var out strings.Builder
		for i := 1; i < len(text); i++ {
			if text[i] != '\'' {
				out.WriteByte(text[i])
				continue
			}
			if i+1 < len(text) && text[i+1] == '\'' {
				out.WriteByte('\'')
				i++
				continue
			}
			rest := strings.TrimSpace(text[i+1:])
			return out.String(), rest == "" || strings.HasPrefix(rest, "#")
		}
		return "", false
	}
	if text[0] == '"' {
		for i := 1; i < len(text); i++ {
			if text[i] == '\\' {
				i++
				continue
			}
			if text[i] == '"' {
				value, err := strconv.Unquote(text[:i+1])
				rest := strings.TrimSpace(text[i+1:])
				return value, err == nil && (rest == "" || strings.HasPrefix(rest, "#"))
			}
		}
		return "", false
	}
	for i, r := range text {
		if r == '#' && i > 0 && (text[i-1] == ' ' || text[i-1] == '\t') {
			text = text[:i]
			break
		}
	}
	text = strings.TrimSpace(text)
	return text, !strings.ContainsAny(text, "{}[]\x00") && !strings.HasPrefix(text, "*") && !strings.HasPrefix(text, "&")
}
func validTunnelHostname(host string) bool {
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, ch := range label {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-') {
				return false
			}
		}
	}
	return true
}

func desktopTunnelArgs(cfg core.Config, mode, config, tunnel, port string) ([]string, func(), error) {
	if mode == "named" {
		return []string{"tunnel", "--config", config, "--no-autoupdate", "run", tunnel}, func() {}, nil
	}
	// An explicit empty config keeps quick tunnels independent of other local ingress.
	file, err := os.CreateTemp(cfg.DataDir, ".quick-tunnel-*.yml")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.Remove(file.Name()) }
	_, writeErr := io.WriteString(file, "{}\n")
	closeErr := file.Close()
	if writeErr != nil {
		cleanup()
		return nil, nil, writeErr
	}
	if closeErr != nil {
		cleanup()
		return nil, nil, closeErr
	}
	return []string{"tunnel", "--config", file.Name(), "--no-autoupdate", "--url", "http://127.0.0.1:" + port}, cleanup, nil
}
