// Package peers owns child MCP sessions and scoped Codex app-server clients.
// Providers execute with ordinary host permissions; roots and environment choices
// preserve the upstream interface and do not claim to be a shell sandbox.
package peers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"macbridge/internal/core"
)

const modern = "2026-07-28"
const legacy = "2025-06-18"

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
var toolPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var envPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var noncePattern = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)
var denyPattern = regexp.MustCompile(`(_TOKEN|_SECRET|_KEY|PASSWORD)$`)
var environmentKeys = []string{"PATH", "HOME", "TMPDIR", "USER", "LOGNAME", "LANG", "LC_ALL", "LC_CTYPE", "TERM"}

type flagCheck struct {
	Args         []string `json:"args"`
	RequireFlags []string `json:"requireFlags"`
	TimeoutMS    int      `json:"timeoutMs"`
}
type providerConfig struct {
	Key            string         `json:"key"`
	Command        string         `json:"command"`
	Args           []string       `json:"args"`
	Env            map[string]any `json:"env"`
	Cwd            string         `json:"cwd"`
	Mode           string         `json:"mode"`
	PersonalArgs   []string       `json:"personalArgs"`
	MaxResultBytes int            `json:"maxResultBytes"`
	CallTimeoutMS  int            `json:"callTimeoutMs"`
	FlagCheck      *flagCheck     `json:"flagCheck"`
}
type grant struct {
	Nonce              string   `json:"nonce"`
	Provider           string   `json:"provider"`
	ExpiresAt          string   `json:"expiresAt"`
	AllowedURLPatterns []string `json:"allowedUrlPatterns"`
	expiry             time.Time
}
type provider struct {
	m                              *Manager
	cfg                            providerConfig
	mu                             sync.Mutex
	c                              *rpcClient
	state, era, version, lastError string
	serverInfo                     any
	rootsDir                       string
	env, refused                   []string
	advertised                     []core.Tool
	tools                          map[string]string
	registered                     bool
	grant                          *grant
	restarts                       []time.Time
	retryAt                        time.Time
	metadataPath                   string
	initialDeadline                time.Time
}
type Manager struct {
	cfg          core.Config
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	mu           sync.Mutex
	providers    []*provider
	owners       map[string]*provider
	activeMu     sync.Mutex
	activeCodex  map[*rpcClient]bool
	startupError string
	closed       bool
	ready        chan struct{}
}

func parseRegistry(raw []byte) ([]providerConfig, error) {
	var registry struct {
		Providers []providerConfig `json:"providers"`
	}
	if err := json.Unmarshal(raw, &registry); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for i := range registry.Providers {
		p := &registry.Providers[i]
		if !keyPattern.MatchString(p.Key) || seen[p.Key] {
			return nil, fmt.Errorf("Invalid or duplicate provider key: %s", p.Key)
		}
		seen[p.Key] = true
		if p.Command == "" || strings.ContainsRune(p.Command, 0) {
			return nil, fmt.Errorf("Provider %s needs a command", p.Key)
		}
		if p.Mode == "" {
			p.Mode = "isolated"
		}
		if p.Mode != "isolated" && p.Mode != "personal" {
			return nil, fmt.Errorf("Provider %s: invalid mode", p.Key)
		}
		if p.MaxResultBytes == 0 {
			p.MaxResultBytes = 4_000_000
		}
		if p.MaxResultBytes < 1024 || p.MaxResultBytes > 16_000_000 {
			return nil, fmt.Errorf("Provider %s: maxResultBytes outside 1024..16000000", p.Key)
		}
		if p.CallTimeoutMS == 0 {
			p.CallTimeoutMS = 120_000
		}
		if p.CallTimeoutMS < 1000 || p.CallTimeoutMS > 120_000 {
			return nil, fmt.Errorf("Provider %s: callTimeoutMs outside 1000..120000", p.Key)
		}
		for _, v := range append(append([]string{p.Cwd}, p.Args...), p.PersonalArgs...) {
			if strings.ContainsRune(v, 0) {
				return nil, fmt.Errorf("Provider %s: NUL in process arguments", p.Key)
			}
		}
		for k, v := range p.Env {
			if !envPattern.MatchString(k) {
				return nil, fmt.Errorf("Invalid environment key: %s", k)
			}
			switch v.(type) {
			case string, float64, bool:
			default:
				return nil, fmt.Errorf("Invalid environment value for %s", k)
			}
			if strings.ContainsRune(fmt.Sprint(v), 0) {
				return nil, fmt.Errorf("NUL in environment value for %s", k)
			}
		}
		if f := p.FlagCheck; f != nil {
			if len(f.Args) == 0 {
				f.Args = []string{"--help"}
			}
			if f.TimeoutMS == 0 {
				f.TimeoutMS = 20000
			}
			f.TimeoutMS = bounded(f.TimeoutMS, 1000, 60000)
			for _, v := range append(append([]string{}, f.Args...), f.RequireFlags...) {
				if strings.ContainsRune(v, 0) {
					return nil, fmt.Errorf("NUL in flagCheck for %s", p.Key)
				}
			}
		}
	}
	return registry.Providers, nil
}
func bounded(n, min, max int) int {
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}
func childEnvironment(values map[string]any) ([]string, []string) {
	env := map[string]string{}
	for _, k := range environmentKeys {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	var refused []string
	for k, v := range values {
		if strings.HasPrefix(k, "MAC_DEV_BRIDGE_") || strings.HasPrefix(k, "npm_config_") || denyPattern.MatchString(k) || k == "AWS_ACCESS_KEY_ID" {
			refused = append(refused, k)
			continue
		}
		env[k] = fmt.Sprint(v)
	}
	var keys, out []string
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sort.Strings(refused)
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out, refused
}
func New(cfg core.Config) (*Manager, error) {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{cfg: cfg, ctx: ctx, cancel: cancel, owners: map[string]*provider{}, activeCodex: map[*rpcClient]bool{}, ready: make(chan struct{})}
	if cfg.CodexBin == "" {
		m.cfg.CodexBin = "codex"
	}
	if cfg.Home == "" {
		m.cfg.Home, _ = os.UserHomeDir()
	}
	raw := []byte(os.Getenv("MAC_DEV_BRIDGE_MCP_SERVERS_JSON"))
	var err error
	if len(bytes.TrimSpace(raw)) == 0 {
		if file := os.Getenv("MAC_DEV_BRIDGE_MCP_SERVERS"); file != "" {
			raw, err = os.ReadFile(file)
		} else {
			raw = []byte(`{}`)
		}
	}
	if err != nil {
		m.startupError = err.Error()
		close(m.ready)
		return m, nil
	}
	configs, err := parseRegistry(raw)
	if err != nil {
		m.startupError = err.Error()
		close(m.ready)
		return m, nil
	}
	var first sync.WaitGroup
	initialDeadline := time.Now().Add(time.Duration(bounded(core.EnvInt("MAC_DEV_BRIDGE_MCP_START_DEADLINE_MS", 15000), 1000, 120000)) * time.Millisecond)
	for _, cfg := range configs {
		env, refused := childEnvironment(cfg.Env)
		p := &provider{m: m, cfg: cfg, state: "configured", env: env, refused: refused, rootsDir: filepath.Join(m.cfg.DataDir, "federation", cfg.Key, "roots"), tools: map[string]string{}, initialDeadline: initialDeadline}
		m.providers = append(m.providers, p)
		first.Add(1)
		m.wg.Add(1)
		go p.run(&first)
	}
	go func() { first.Wait(); close(m.ready) }()
	return m, nil
}
func (m *Manager) awaitInitial(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.ctx.Done():
		return m.ctx.Err()
	case <-m.ready:
		return nil
	}
}
func (m *Manager) Tools(ctx context.Context) ([]core.Tool, error) {
	if err := m.awaitInitial(ctx); err != nil {
		return nil, err
	}
	tools := []core.Tool{}
	for _, p := range m.providers {
		p.mu.Lock()
		tools = append(tools, p.advertised...)
		p.mu.Unlock()
	}
	return tools, nil
}
func (m *Manager) Call(ctx context.Context, name string, args map[string]any) (any, error) {
	if name == "codex_thread_read" || name == "codex_thread_list" || name == "codex_thread_turns_list" {
		return m.codex(ctx, name, args)
	}
	if err := m.awaitInitial(ctx); err != nil {
		return nil, err
	}
	m.mu.Lock()
	p := m.owners[name]
	m.mu.Unlock()
	if p == nil {
		return nil, core.Error("UNKNOWN_TOOL", "Unknown federated tool: "+name)
	}
	p.mu.Lock()
	c, state, original, version, era := p.c, p.state, p.tools[name], p.version, p.era
	g := p.grant
	lastError := p.lastError
	p.mu.Unlock()
	if state != "ready" || c == nil || original == "" {
		return failure("PROVIDER_UNAVAILABLE", "Provider state was lost; take a fresh snapshot before retrying", map[string]any{"provider": p.cfg.Key, "state": state, "stateLost": true, "lastError": lastError}), nil
	}
	if g != nil && !time.Now().Before(g.expiry) {
		return failure("PERSONAL_MODE_NOT_APPROVED", "Personal browser grant expired", nil), nil
	}
	params := map[string]any{"name": original, "arguments": args}
	if args == nil {
		params["arguments"] = map[string]any{}
	}
	if era == "modern" {
		params["_meta"] = map[string]any{"io.modelcontextprotocol/protocolVersion": version}
	}
	raw, err := requestFor(ctx, c, "tools/call", params, time.Duration(p.cfg.CallTimeoutMS)*time.Millisecond)
	if err != nil {
		code := "PROVIDER_CALL_FAILED"
		var f *core.Fault
		if errors.As(err, &f) {
			code = f.Code
		}
		return failure(code, err.Error(), map[string]any{"provider": p.cfg.Key, "tool": name}), nil
	}
	if len(raw) > p.cfg.MaxResultBytes {
		return failure("RESULT_TOO_LARGE", "Request a smaller result from the provider", map[string]any{"byteLength": len(raw), "maxResultBytes": p.cfg.MaxResultBytes}), nil
	}
	var check struct {
		ResultType string `json:"resultType"`
		Request    any    `json:"request"`
	}
	_ = json.Unmarshal(raw, &check)
	if check.ResultType == "input_required" {
		return failure("INPUT_REQUIRED", "Provider requested interactive input; supply arguments up front", map[string]any{"request": check.Request}), nil
	}
	var result core.Result
	if err = json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	if result.Content == nil {
		result.Content = []map[string]any{}
	}
	return result, nil
}
func failure(code, message string, details map[string]any) core.Result {
	if details == nil {
		details = map[string]any{}
	}
	details["code"] = code
	details["error"] = message
	return core.Result{Content: []map[string]any{{"type": "text", "text": message}}, StructuredContent: details, IsError: true}
}
func (m *Manager) Status() map[string]any {
	states := []map[string]any{}
	for _, p := range m.providers {
		p.mu.Lock()
		s := map[string]any{"key": p.cfg.Key, "state": p.state, "mode": p.cfg.Mode, "era": p.era, "toolCount": len(p.advertised), "restarts": len(p.restarts), "maxRestarts": 5, "lastError": p.lastError, "serverInfo": p.serverInfo, "rootsDir": p.rootsDir, "envKeysRefused": p.refused, "pid": nil, "pendingCalls": 0, "personalGrant": nil}
		var forwarded []string
		for _, e := range p.env {
			forwarded = append(forwarded, strings.SplitN(e, "=", 2)[0])
		}
		s["envKeysForwarded"] = forwarded
		if !p.retryAt.IsZero() {
			s["nextRetryInMs"] = max(int64(0), time.Until(p.retryAt).Milliseconds())
		}
		if p.c != nil {
			pending, logs, _ := p.c.details()
			s["pid"] = p.c.cmd.Process.Pid
			s["processGroupId"] = p.c.cmd.Process.Pid
			s["pendingCalls"] = pending
			lines := strings.Split(strings.TrimSpace(logs), "\n")
			if len(lines) > 5 {
				lines = lines[len(lines)-5:]
			}
			s["stderrTail"] = lines
		}
		if p.grant != nil {
			s["personalGrant"] = map[string]any{"nonce": p.grant.Nonce, "expiresAt": p.grant.ExpiresAt, "allowedUrlPatterns": p.grant.AllowedURLPatterns, "expired": !time.Now().Before(p.grant.expiry), "remainingMs": max(int64(0), time.Until(p.grant.expiry).Milliseconds())}
		}
		p.mu.Unlock()
		states = append(states, s)
	}
	return map[string]any{"configured": len(m.providers), "started": true, "providers": states, "lastError": m.startupError, "childServerEnvAllowlist": environmentKeys}
}
func (m *Manager) Close() error {
	m.cancel()
	m.activeMu.Lock()
	m.closed = true
	var clients []*rpcClient
	for c := range m.activeCodex {
		clients = append(clients, c)
	}
	m.activeMu.Unlock()
	for _, c := range clients {
		c.stop()
	}
	m.wg.Wait()
	return nil
}
func (p *provider) setState(state string, err error) {
	p.mu.Lock()
	p.state = state
	if err != nil {
		p.lastError = err.Error()
	}
	p.mu.Unlock()
}
func (p *provider) run(first *sync.WaitGroup) {
	defer p.m.wg.Done()
	initial := true
	initialDone := func() {
		if initial {
			first.Done()
			initial = false
		}
	}
	defer initialDone()
	for {
		if p.m.ctx.Err() != nil {
			p.setState("stopped", nil)
			return
		}
		var startupDeadline time.Time
		if initial {
			startupDeadline = p.initialDeadline
			if !time.Now().Before(startupDeadline) {
				p.setState("failed", context.DeadlineExceeded)
				return
			}
		}
		err := p.start(startupDeadline)
		if err == nil {
			initialDone()
			err = p.watch()
		}
		p.mu.Lock()
		c := p.c
		p.c = nil
		p.grant = nil
		metadataPath := p.metadataPath
		p.metadataPath = ""
		p.mu.Unlock()
		if c != nil {
			c.stop()
		}
		if metadataPath != "" {
			_ = os.Remove(metadataPath)
		}
		if p.m.ctx.Err() != nil {
			p.setState("stopped", nil)
			return
		}
		var fault *core.Fault
		if p.cfg.Mode == "personal" || !errors.As(err, &fault) || (fault.Code != "PROVIDER_GONE" && fault.Code != "PROVIDER_UNHEALTHY") {
			p.setState("failed", err)
			return
		}
		p.mu.Lock()
		cutoff := time.Now().Add(-10 * time.Minute)
		for len(p.restarts) > 0 && p.restarts[0].Before(cutoff) {
			p.restarts = p.restarts[1:]
		}
		attempt := len(p.restarts)
		if attempt >= 5 {
			p.mu.Unlock()
			p.setState("failed", err)
			return
		}
		delay := time.Duration(500*(1<<attempt)) * time.Millisecond
		p.retryAt = time.Now().Add(delay)
		p.mu.Unlock()
		p.setState("restarting", err)
		if initial && time.Until(p.initialDeadline) < delay {
			delay = time.Until(p.initialDeadline)
		}
		select {
		case <-p.m.ctx.Done():
			p.setState("stopped", nil)
			return
		case <-time.After(delay):
		}
		p.mu.Lock()
		p.restarts = append(p.restarts, time.Now())
		p.retryAt = time.Time{}
		p.mu.Unlock()
	}
}
func (p *provider) start(initialDeadline time.Time) error {
	p.setState("starting", nil)
	deadline := time.Now().Add(time.Duration(bounded(core.EnvInt("MAC_DEV_BRIDGE_MCP_START_DEADLINE_MS", 15000), 1000, 120000)) * time.Millisecond)
	if !initialDeadline.IsZero() && initialDeadline.Before(deadline) {
		deadline = initialDeadline
	}
	ctx, cancel := context.WithDeadline(p.m.ctx, deadline)
	defer cancel()
	if err := os.MkdirAll(p.rootsDir, 0700); err != nil {
		return err
	}
	_ = os.Chmod(p.rootsDir, 0700)
	cwd := p.cfg.Cwd
	if cwd == "" {
		cwd = p.m.cfg.DataDir
	}
	if p.cfg.Mode == "personal" && p.cfg.FlagCheck == nil {
		return core.Error("PERSONAL_MODE_NOT_APPROVED", "Personal provider requires flagCheck before consuming approval")
	}
	if f := p.cfg.FlagCheck; f != nil {
		required := append([]string{}, f.RequireFlags...)
		if p.cfg.Mode == "personal" {
			required = append(required, "--allowedUrlPattern")
		}
		if len(required) > 0 {
			probeCtx, stop := context.WithTimeout(ctx, time.Duration(f.TimeoutMS)*time.Millisecond)
			cmd := exec.CommandContext(probeCtx, p.cfg.Command, f.Args...)
			cmd.Dir = cwd
			cmd.Env = p.env
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.Cancel = func() error {
				if cmd.Process == nil {
					return nil
				}
				return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			cmd.WaitDelay = time.Second
			w := &cappedWriter{}
			cmd.Stdout = w
			cmd.Stderr = w
			err := cmd.Run()
			stop()
			if err != nil {
				return fmt.Errorf("Provider flag verification: %w", err)
			}
			for _, flag := range required {
				if !bytes.Contains(w.b, []byte(flag)) {
					return fmt.Errorf("Provider %s does not document flag %s", p.cfg.Key, flag)
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	args := append([]string{}, p.cfg.Args...)
	if p.cfg.Mode == "personal" {
		g, err := consumeGrant(core.Env("MAC_DEV_BRIDGE_PERSONAL_APPROVAL_FILE", filepath.Join(p.m.cfg.DataDir, "PERSONAL_BROWSER_APPROVED")), p.cfg.Key)
		if err != nil {
			return err
		}
		p.mu.Lock()
		p.grant = g
		p.mu.Unlock()
		var expire context.CancelFunc
		ctx, expire = context.WithDeadline(ctx, g.expiry)
		defer expire()
		args = append([]string{}, p.cfg.PersonalArgs...)
		for _, pattern := range g.AllowedURLPatterns {
			args = append(args, "--allowedUrlPattern", pattern)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	roots := []map[string]any{{"uri": (&url.URL{Scheme: "file", Path: p.rootsDir}).String(), "name": p.cfg.Key + " workspace"}}
	c, err := launch(p.cfg.Command, args, p.env, cwd, roots)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.c = c
	p.mu.Unlock()
	birthCtx, birthCancel := context.WithTimeout(ctx, 2*time.Second)
	birthCmd := exec.CommandContext(birthCtx, "ps", "-p", strconv.Itoa(c.cmd.Process.Pid), "-o", "lstart=")
	birthCmd.Env = append(os.Environ(), "LC_ALL=C")
	birth, birthErr := birthCmd.Output()
	birthCancel()
	if birthErr != nil || len(bytes.TrimSpace(birth)) == 0 {
		if syscall.Kill(c.cmd.Process.Pid, 0) == syscall.ESRCH {
			return core.Error("PROVIDER_GONE", "Provider exited before its process identity could be recorded")
		}
		return fmt.Errorf("Cannot record provider process identity: %v", birthErr)
	}
	id := "mcp-" + p.cfg.Key + "-" + core.ID()
	metadataPath := filepath.Join(p.m.cfg.DataDir, "jobs", id+".json")
	if err = core.AtomicJSON(metadataPath, map[string]any{"id": id, "kind": "mcp-child", "label": "mcp-" + p.cfg.Key, "pid": c.cmd.Process.Pid, "processStart": strings.TrimSpace(string(birth)), "processGroupId": c.cmd.Process.Pid, "command": strings.Join(append([]string{p.cfg.Command}, args...), " "), "cwd": cwd, "startedAt": core.Now(), "stdoutPath": nil, "stderrPath": nil}); err != nil {
		return err
	}
	p.mu.Lock()
	p.metadataPath = metadataPath
	p.mu.Unlock()
	era, version, info, err := handshake(ctx, c, roots)
	if err != nil {
		return err
	}
	var tools []core.Tool
	cursor := ""
	for page := 0; page < 20; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if era == "modern" {
			params["_meta"] = map[string]any{"io.modelcontextprotocol/protocolVersion": version}
		}
		raw, e := c.request(ctx, "tools/list", params)
		if e != nil {
			return e
		}
		var result struct {
			Tools      []core.Tool `json:"tools"`
			NextCursor *string     `json:"nextCursor"`
		}
		if e = json.Unmarshal(raw, &result); e != nil {
			return e
		}
		tools = append(tools, result.Tools...)
		if result.NextCursor == nil {
			cursor = ""
			break
		}
		cursor = *result.NextCursor
		if page == 19 {
			return errors.New("Provider tools/list exceeds 20 pages")
		}
	}
	mapped := map[string]string{}
	for i := range tools {
		original := tools[i].Name
		name := p.cfg.Key + "__" + original
		if !toolPattern.MatchString(name) || original == "" || mapped[name] != "" {
			return fmt.Errorf("Invalid or duplicate provider tool: %s", name)
		}
		mapped[name] = original
		tools[i].Name = name
	}
	p.m.mu.Lock()
	defer p.m.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.registered {
		for name := range mapped {
			if owner := p.m.owners[name]; owner != nil && owner != p {
				return fmt.Errorf("Federated tool name collision: %s", name)
			}
		}
		for name := range mapped {
			p.m.owners[name] = p
		}
		p.advertised = tools
		p.registered = true
	}
	p.tools = mapped
	p.era = era
	p.version = version
	p.serverInfo = info
	p.state = "ready"
	p.lastError = ""
	return nil
}
func handshake(ctx context.Context, c *rpcClient, roots []map[string]any) (string, string, any, error) {
	info := map[string]any{"name": "mac-developer-bridge", "title": "Mac Developer Bridge", "version": "1.0.0-go"}
	caps := map[string]any{"roots": map[string]any{"listChanged": false}}
	version := modern
	for attempt := 0; attempt < 2; attempt++ {
		raw, err := requestFor(ctx, c, "server/discover", map[string]any{"protocolVersion": version, "clientInfo": info, "capabilities": caps, "roots": roots, "_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": version}}, 3*time.Second)
		if err == nil {
			m, e := decode(raw)
			if e != nil {
				return "", "", nil, e
			}
			if versions, ok := m["supportedVersions"].([]any); ok && len(versions) > 0 {
				if s, ok := versions[0].(string); ok {
					version = s
				}
				for _, s := range versions {
					if s == modern {
						version = modern
					}
				}
			}
			server := m["serverInfo"]
			if server == nil {
				if meta, ok := m["_meta"].(map[string]any); ok {
					server = meta["io.modelcontextprotocol/serverInfo"]
				}
			}
			return "modern", version, server, nil
		}
		var rpc *rpcError
		if errors.As(err, &rpc) && rpc.Code == -32022 {
			if supported, ok := rpc.Data["supported"].([]any); ok && attempt == 0 && len(supported) > 0 {
				if next, ok := supported[0].(string); ok {
					version = next
					continue
				}
			}
			return "", "", nil, errors.New("No mutually supported modern protocol version")
		}
		if ctx.Err() != nil {
			return "", "", nil, ctx.Err()
		}
		var fault *core.Fault
		if errors.As(err, &fault) && fault.Code == "PROVIDER_GONE" {
			return "", "", nil, err
		}
		break
	}
	raw, err := requestFor(ctx, c, "initialize", map[string]any{"protocolVersion": legacy, "clientInfo": info, "capabilities": caps}, 10*time.Second)
	if err != nil {
		return "", "", nil, err
	}
	m, err := decode(raw)
	if err != nil {
		return "", "", nil, err
	}
	v, _ := m["protocolVersion"].(string)
	switch v {
	case legacy, "2025-11-25", "2025-03-26", "2024-11-05":
	default:
		return "", "", nil, fmt.Errorf("Unsupported legacy protocol: %s", v)
	}
	if err = c.notify("notifications/initialized", map[string]any{}); err != nil {
		return "", "", nil, err
	}
	return "legacy", v, m["serverInfo"], nil
}
func (p *provider) watch() error {
	p.mu.Lock()
	c, g := p.c, p.grant
	p.mu.Unlock()
	ctx := p.m.ctx
	if g != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, g.expiry)
		defer cancel()
	}
	idle := time.Duration(bounded(core.EnvInt("MAC_DEV_BRIDGE_MCP_PING_IDLE_MS", 30000), 100, 600000)) * time.Millisecond
	ticker := time.NewTicker(idle)
	defer ticker.Stop()
	var expires <-chan time.Time
	var timer *time.Timer
	if g != nil {
		timer = time.NewTimer(time.Until(g.expiry))
		expires = timer.C
		defer timer.Stop()
	}
	for {
		select {
		case <-p.m.ctx.Done():
			return p.m.ctx.Err()
		case <-c.done:
			return core.Error("PROVIDER_GONE", "Provider process exited; browser state was lost")
		case <-expires:
			return core.Error("PERSONAL_MODE_NOT_APPROVED", "Personal browser grant expired; provider stopped")
		case <-ticker.C:
			_, _, last := c.details()
			if time.Since(last) < idle {
				continue
			}
			_, err := requestFor(ctx, c, "ping", map[string]any{}, min(10*time.Second, max(100*time.Millisecond, idle/2)))
			if err != nil {
				return core.Error("PROVIDER_UNHEALTHY", "Provider ping failed: "+err.Error())
			}
		}
	}
}
func consumeGrant(file, key string) (*grant, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, core.Error("PERSONAL_MODE_NOT_APPROVED", err.Error())
	}
	var g grant
	if err = json.Unmarshal(raw, &g); err != nil {
		return nil, core.Error("PERSONAL_MODE_NOT_APPROVED", "Invalid approval JSON")
	}
	g.expiry, err = time.Parse(time.RFC3339Nano, g.ExpiresAt)
	if err != nil || !noncePattern.MatchString(g.Nonce) || g.Provider != key || len(g.AllowedURLPatterns) == 0 || !time.Now().Before(g.expiry) || time.Until(g.expiry) > 15*time.Minute {
		return nil, core.Error("PERSONAL_MODE_NOT_APPROVED", "Grant is invalid, expired, for another provider, or exceeds 15 minutes")
	}
	for _, pattern := range g.AllowedURLPatterns {
		if pattern == "" || strings.HasPrefix(pattern, "-") || strings.ContainsRune(pattern, 0) {
			return nil, core.Error("PERSONAL_MODE_NOT_APPROVED", "Invalid URL pattern in approval")
		}
	}
	claim := file + ".consumed-" + core.ID()
	if err = os.Rename(file, claim); err != nil {
		return nil, core.Error("PERSONAL_MODE_NOT_APPROVED", "Grant was already consumed or cannot be claimed")
	}
	if err = os.Remove(claim); err != nil {
		return nil, err
	}
	return &g, nil
}
