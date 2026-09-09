package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"macbridge/internal/browser"
	"macbridge/internal/core"
	"macbridge/internal/host"
	"macbridge/internal/peers"
	"macbridge/internal/server"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		var fault *core.Fault
		if errors.As(err, &fault) && fault.Code == "BRIDGE_LOCKED" {
			os.Exit(78)
		}
		os.Exit(1)
	}
}
func run() error {
	cfg := core.FromEnv()
	mode := "stdio"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	switch mode {
	case "version", "--version":
		fmt.Println("MacBridge Go", server.Version)
		return nil
	case "help", "--help", "-h":
		fmt.Println("macbridge [stdio|http|desktop|init|stop|doctor|rotate-token|install|uninstall|tunnel|install-tunnel|install-browser|uninstall-browser|grant-browser|grant-gui|chrome-host|version]")
		return nil
	case "rotate-token":
		return rotateToken(cfg)
	case "supervise-job":
		if len(os.Args) != 3 {
			return fmt.Errorf("supervise-job requires a configuration file")
		}
		return host.SuperviseJob(os.Args[2])
	case "desktop":
		return runDesktop(cfg)
	case "tunnel":
		return runSecureTunnel(cfg)
	case "install-tunnel":
		return installSecureTunnel(cfg)
	case "chrome-host":
		return browser.NativeHost(cfg)
	case "init", "doctor", "stop", "install", "uninstall", "install-browser", "uninstall-browser", "grant-browser", "grant-gui":
		return localCommand(cfg, mode, os.Args[2:])
	case "stdio", "http":
	default:
		return fmt.Errorf("unknown command %q", mode)
	}
	acknowledged := os.Getenv("MAC_DEV_BRIDGE_FULL_ACCESS_ACK") == server.Acknowledgement
	os.Unsetenv("MAC_DEV_BRIDGE_FULL_ACCESS_ACK")
	os.Unsetenv("CONTROL_PLANE_API_KEY")
	var oauth *server.OAuth
	if mode == "http" {
		token, err := readToken(cfg)
		if err != nil {
			return err
		}
		loadRuntime(cfg)
		oauth, err = server.NewOAuth(cfg, token)
		if err != nil {
			return err
		}
		os.Unsetenv("MAC_DEV_BRIDGE_HTTP_TOKEN")
		os.Unsetenv("MAC_DEV_BRIDGE_OAUTH_CLIENT_SECRET")
	}
	unlocked := func() bool {
		if acknowledged {
			return true
		}
		b, e := os.ReadFile(cfg.UnlockFile)
		return e == nil && strings.TrimSpace(string(b)) == server.Acknowledgement
	}
	if mode == "stdio" && !unlocked() {
		return core.Error("BRIDGE_LOCKED", "Refusing to start unrestricted bridge without explicit unlock; run macbridge init")
	}
	hosts, err := host.New(cfg)
	if err != nil {
		return err
	}
	defer hosts.Close()
	children, err := peers.New(cfg)
	if err != nil {
		return err
	}
	defer children.Close()
	chrome := browser.New(cfg)
	defer chrome.Close()
	engine := &server.Engine{Config: cfg, Acknowledged: acknowledged}
	engine.List = func(ctx context.Context) ([]core.Tool, error) {
		tools := server.Builtins()
		extra, err := children.Tools(ctx)
		if err != nil {
			return nil, err
		}
		return append(tools, extra...), nil
	}
	engine.Execute = func(ctx context.Context, name string, args map[string]any) (any, error) {
		switch {
		case strings.HasPrefix(name, "chrome_") || strings.HasPrefix(name, "chatgpt_"):
			return chrome.Call(ctx, name, args)
		case strings.HasPrefix(name, "codex_") || strings.Contains(name, "__"):
			return children.Call(ctx, name, args)
		default:
			return hosts.Call(ctx, name, args)
		}
	}
	engine.State = func() map[string]any {
		state := hosts.Status()
		state["federation"] = children.Status()
		state["backgroundChrome"] = chrome.Status()
		state["ptyAvailable"] = true
		state["guiFocusPolicy"] = "background-first"
		var settings map[string]any
		b, _ := os.ReadFile(filepath.Join(cfg.DataDir, "settings.json"))
		json.Unmarshal(b, &settings)
		state["operatorSettings"] = settings
		return state
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer cancel()
	// Revocation has one owner across every transport and every live module.
	go func() {
		ticker := time.NewTicker(time.Duration(core.EnvInt("MAC_DEV_BRIDGE_UNLOCK_RECHECK_MS", 3000)) * time.Millisecond)
		defer ticker.Stop()
		wasUnlocked := unlocked()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				now := unlocked()
				if wasUnlocked && !now {
					cancel()
					return
				}
				wasUnlocked = now
			}
		}
	}()
	if mode == "stdio" {
		done := make(chan error, 1)
		go func() { done <- engine.ServeStdio(ctx, os.Stdin, os.Stdout) }()
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			return nil
		}
	}
	h := server.NewHTTP(engine, oauth)
	h.Responses = func(ctx context.Context, request map[string]any) (map[string]any, error) {
		return chrome.Responses(ctx, request, func(ctx context.Context, name string, args map[string]any) (any, error) {
			result, err := engine.Call(ctx, name, args)
			return result.StructuredContent, err
		})
	}
	h.ValidateResponses = chrome.ValidateResponses
	port := core.Env("MAC_DEV_BRIDGE_HTTP_PORT", "8787")
	if n, e := strconv.Atoi(port); e != nil || n < 0 || n > 65535 {
		return fmt.Errorf("invalid HTTP port")
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		return err
	}
	pidfile := filepath.Join(cfg.DataDir, "macbridge-http.json")
	startCmd := exec.Command("ps", "-p", fmt.Sprint(os.Getpid()), "-o", "lstart=")
	startCmd.Env = append(os.Environ(), "LC_ALL=C")
	startBytes, _ := startCmd.Output()
	exe, _ := os.Executable()
	if err = core.AtomicJSON(pidfile, map[string]any{"pid": os.Getpid(), "executable": exe, "startedAt": core.Now(), "processStart": strings.TrimSpace(string(startBytes)), "address": listener.Addr().String()}); err != nil {
		listener.Close()
		return err
	}
	defer os.Remove(pidfile)
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 32768}
	fmt.Fprintln(os.Stderr, "MacBridge Go listening on", listener.Addr(), "OAuth client_id:", oauth.ClientID())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(listener) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		hosts.Close()
		children.Close()
		timeout, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		srv.Shutdown(timeout)
		return nil
	}
}
func rotateToken(cfg core.Config) error {
	if os.Getenv("MAC_DEV_BRIDGE_HTTP_TOKEN") != "" && os.Getenv("MAC_DEV_BRIDGE_HTTP_TOKEN_FILE") == "" {
		return fmt.Errorf("the bearer is fixed by MAC_DEV_BRIDGE_HTTP_TOKEN; change that environment source or configure MAC_DEV_BRIDGE_HTTP_TOKEN_FILE before rotation")
	}
	if err := stopBridge(cfg); err != nil {
		return err
	}
	file := core.Env("MAC_DEV_BRIDGE_HTTP_TOKEN_FILE", filepath.Join(cfg.DataDir, "http-token"))
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return err
	}
	if err := os.WriteFile(file, []byte(core.ID()+core.ID()+"\n"), 0600); err != nil {
		return err
	}
	return localCommand(cfg, "init", nil)
}
func readToken(cfg core.Config) (string, error) {
	file := core.Env("MAC_DEV_BRIDGE_HTTP_TOKEN_FILE", filepath.Join(cfg.DataDir, "http-token"))
	token := os.Getenv("MAC_DEV_BRIDGE_HTTP_TOKEN")
	if token == "" || os.Getenv("MAC_DEV_BRIDGE_HTTP_TOKEN_FILE") != "" {
		b, e := os.ReadFile(file)
		if e != nil {
			return "", fmt.Errorf("read HTTP token %s: %w; run macbridge init", file, e)
		}
		token = strings.TrimSpace(string(b))
	}
	if len(token) < 24 {
		return "", fmt.Errorf("HTTP token must contain at least 24 printable ASCII bytes")
	}
	for _, c := range token {
		if c < 33 || c > 126 {
			return "", fmt.Errorf("HTTP token must be printable ASCII without spaces")
		}
	}
	return token, nil
}
func loadRuntime(cfg core.Config) {
	var settings map[string]any
	b, _ := os.ReadFile(filepath.Join(cfg.DataDir, "runtime.json"))
	json.Unmarshal(b, &settings)
	for key, env := range map[string]string{"publicURL": "MAC_DEV_BRIDGE_PUBLIC_URL", "httpPort": "MAC_DEV_BRIDGE_HTTP_PORT"} {
		if os.Getenv(env) == "" {
			if value, ok := settings[key]; ok {
				os.Setenv(env, fmt.Sprint(value))
			}
		}
	}
}
