package peers

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"macbridge/internal/core"
)

// The compiled Go test binary is the fixture child; no upstream runtime, Node,
// browser, network service, or installed Codex session participates.
func TestPeerProcess(t *testing.T) {
	if os.Getenv("PEER_FIXTURE") != "1" {
		return
	}
	for _, a := range os.Args {
		if a == "--help" {
			n, _ := strconv.Atoi(os.Getenv("PEER_HELP_DELAY_MS"))
			time.Sleep(time.Duration(n) * time.Millisecond)
			fmt.Println("--allowedUrlPattern --redactNetworkHeaders")
			os.Exit(0)
		}
	}
	if marker := os.Getenv("PEER_CRASH_ONCE"); marker != "" {
		if f, e := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600); e == nil {
			f.Close()
			os.Exit(9)
		}
	}
	type message struct {
		ID     any            `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
		Result any            `json:"result"`
	}
	send := func(id, result, err any) {
		m := map[string]any{"jsonrpc": "2.0", "id": id}
		if err != nil {
			m["error"] = err
		} else {
			m["result"] = result
		}
		b, _ := json.Marshal(m)
		fmt.Println(string(b))
	}
	tool := func(name string) map[string]any {
		return map[string]any{"name": name, "description": "Fixture " + name, "inputSchema": map[string]any{"type": "object", "$schema": "http://json-schema.org/draft-07/schema#", "additionalProperties": true}, "execution": map[string]any{"kind": "immediate"}}
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024), maxMessage)
	var rootCaller any
	for scanner.Scan() {
		var m message
		if json.Unmarshal(scanner.Bytes(), &m) != nil {
			continue
		}
		switch m.Method {
		case "server/discover":
			if os.Getenv("PEER_ERA") == "legacy" {
				send(m.ID, nil, map[string]any{"code": -32601, "message": "unknown"})
			} else if os.Getenv("PEER_ERA") == "negotiate" && m.Params["protocolVersion"] == modern {
				send(m.ID, nil, map[string]any{"code": -32022, "message": "version", "data": map[string]any{"supported": []string{"2027-01-01"}}})
			} else {
				send(m.ID, map[string]any{"supportedVersions": []string{core.String(m.Params, "protocolVersion", modern)}, "serverInfo": map[string]any{"name": "fixture"}}, nil)
			}
		case "initialize":
			send(m.ID, map[string]any{"protocolVersion": legacy}, nil)
		case "tools/list":
			n, _ := strconv.Atoi(os.Getenv("PEER_LIST_DELAY_MS"))
			time.Sleep(time.Duration(n) * time.Millisecond)
			if m.Params["cursor"] == nil {
				send(m.ID, map[string]any{"tools": []any{tool("echo"), tool("crash"), tool("big"), tool("input")}, "nextCursor": "page2"}, nil)
			} else {
				send(m.ID, map[string]any{"tools": []any{tool("roots")}}, nil)
			}
		case "tools/call":
			switch m.Params["name"] {
			case "crash":
				os.Exit(11)
			case "roots":
				rootCaller = m.ID
				b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "child-roots", "method": "roots/list"})
				fmt.Println(string(b))
			case "input":
				send(m.ID, map[string]any{"resultType": "input_required", "request": map[string]any{"message": "input"}}, nil)
			case "big":
				send(m.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": strings.Repeat("x", 3000)}}}, nil)
			default:
				send(m.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": "echo"}, map[string]any{"type": "image", "mimeType": "image/png", "data": "aGVsbG8=", "custom": true}, map[string]any{"type": "resource", "resource": map[string]any{"uri": "test://blob", "blob": "YQ=="}}}, "structuredContent": map[string]any{"args": m.Params["arguments"], "env": os.Environ(), "argv": os.Args}, "_meta": map[string]any{"fixture": true}}, nil)
			}
		case "ping":
			send(m.ID, map[string]any{}, nil)
		case "thread/list", "thread/read", "thread/turns/list":
			send(m.ID, map[string]any{"method": m.Method, "params": m.Params, "nextCursor": "retained-cursor"}, nil)
		case "":
			if m.ID == "child-roots" {
				b, _ := json.Marshal(m.Result)
				send(rootCaller, map[string]any{"content": []any{map[string]any{"type": "text", "text": string(b)}}}, nil)
			}
		}
	}
	os.Exit(0)
}
func fixtureConfig(t *testing.T, key string) providerConfig {
	t.Helper()
	return providerConfig{Key: key, Command: os.Args[0], Args: []string{"-test.run=TestPeerProcess", "--"}, Env: map[string]any{"PEER_FIXTURE": "1", "GORACE": "atexit_sleep_ms=0"}, Mode: "isolated"}
}
func managerFor(t *testing.T, configs ...providerConfig) *Manager {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"providers": configs})
	t.Setenv("MAC_DEV_BRIDGE_MCP_SERVERS_JSON", string(raw))
	dir := t.TempDir()
	m, e := New(core.Config{Home: dir, DataDir: dir, CodexBin: os.Args[0]})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { m.Close() })
	return m
}
func waitState(t *testing.T, m *Manager, want string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		s := m.Status()["providers"].([]map[string]any)
		if s[0]["state"] == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("wanted %s: %#v", want, m.Status())
}
func TestFederationProtocolsAndLifecycle(t *testing.T) {
	for _, era := range []string{"modern", "legacy", "negotiate"} {
		t.Run(era, func(t *testing.T) {
			p := fixtureConfig(t, "fixture")
			p.Env["PEER_ERA"] = era
			p.Env["GITHUB_TOKEN"] = "must-not-forward"
			p.Env["STUB_CUSTOM"] = "forward"
			m := managerFor(t, p)
			tools, e := m.Tools(context.Background())
			if e != nil || len(tools) != 5 {
				t.Fatalf("tools=%#v error=%v status=%#v", tools, e, m.Status())
			}
			b, _ := json.Marshal(tools[0])
			if !strings.Contains(string(b), `"execution"`) {
				t.Fatalf("tool metadata lost: %s", b)
			}
			v, e := m.Call(context.Background(), "fixture__echo", map[string]any{"message": "hi"})
			if e != nil {
				t.Fatal(e)
			}
			r := v.(core.Result)
			if r.IsError || len(r.Content) != 3 || r.Content[1]["custom"] != true || r.Meta["fixture"] != true {
				t.Fatalf("opaque content lost: %#v", r)
			}
			data, _ := json.Marshal(r.StructuredContent)
			if strings.Contains(string(data), "must-not-forward") || !strings.Contains(string(data), "STUB_CUSTOM=forward") {
				t.Fatalf("environment incorrect: %s", data)
			}
			roots, e := m.Call(context.Background(), "fixture__roots", nil)
			if e != nil || !strings.Contains(roots.(core.Result).Content[0]["text"].(string), "fixture/roots") {
				t.Fatalf("roots callback: %#v %v", roots, e)
			}
			input, _ := m.Call(context.Background(), "fixture__input", nil)
			if !input.(core.Result).IsError {
				t.Fatal("interactive request reported success")
			}
			pid := m.Status()["providers"].([]map[string]any)[0]["pid"].(int)
			metadataFiles, _ := filepath.Glob(filepath.Join(m.cfg.DataDir, "jobs", "mcp-*.json"))
			if len(metadataFiles) != 1 {
				t.Fatalf("Missing provider metadata: %v", metadataFiles)
			}
			metadataBytes, _ := os.ReadFile(metadataFiles[0])
			var metadata map[string]any
			if json.Unmarshal(metadataBytes, &metadata) != nil || core.String(metadata, "processStart", "") == "" {
				t.Fatalf("Process identity not recorded: %s", metadataBytes)
			}
			m.Close()
			if syscall.Kill(pid, 0) == nil {
				t.Fatal("Close leaked child")
			}
		})
	}
}
func TestProviderRecoveryAndBounds(t *testing.T) {
	t.Run("first-crash", func(t *testing.T) {
		p := fixtureConfig(t, "retry")
		p.Env["PEER_CRASH_ONCE"] = filepath.Join(t.TempDir(), "once")
		m := managerFor(t, p)
		waitState(t, m, "ready")
		tools, _ := m.Tools(context.Background())
		if len(tools) != 5 {
			t.Fatal("successful retry did not register tools")
		}
		_, _ = m.Call(context.Background(), "retry__crash", nil)
		waitState(t, m, "restarting")
		waitState(t, m, "ready")
		v, _ := m.Call(context.Background(), "retry__echo", nil)
		if v.(core.Result).IsError {
			t.Fatal("provider did not recover")
		}
	})
	t.Run("result-limit", func(t *testing.T) {
		p := fixtureConfig(t, "limit")
		p.MaxResultBytes = 1024
		m := managerFor(t, p)
		v, e := m.Call(context.Background(), "limit__big", nil)
		if e != nil || !v.(core.Result).IsError {
			t.Fatalf("oversized result accepted: %v %v", v, e)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		t.Setenv("MAC_DEV_BRIDGE_MCP_START_DEADLINE_MS", "1000")
		p := fixtureConfig(t, "slow")
		p.Env["PEER_LIST_DELAY_MS"] = "700"
		start := time.Now()
		m := managerFor(t, p)
		waitState(t, m, "failed")
		if time.Since(start) > 2*time.Second {
			t.Fatal("startup deadline was multiplied by pages")
		}
	})
}
func writeGrant(t *testing.T, file string, expiry time.Time) {
	t.Helper()
	g := grant{Nonce: strings.Repeat("a", 32), Provider: "personal", ExpiresAt: expiry.UTC().Format(time.RFC3339Nano), AllowedURLPatterns: []string{"https://example.com/*"}}
	raw, _ := json.Marshal(g)
	if e := os.WriteFile(file, raw, 0600); e != nil {
		t.Fatal(e)
	}
}
func TestGrantPreflightSingleUseAndExpiry(t *testing.T) {
	file := filepath.Join(t.TempDir(), "approval")
	t.Setenv("MAC_DEV_BRIDGE_PERSONAL_APPROVAL_FILE", file)
	writeGrant(t, file, time.Now().Add(time.Minute))
	p := fixtureConfig(t, "personal")
	p.Mode = "personal"
	p.PersonalArgs = p.Args
	m := managerFor(t, p)
	waitState(t, m, "failed")
	if _, e := os.Stat(file); e != nil {
		t.Fatal("missing flagCheck consumed grant")
	}
	p.FlagCheck = &flagCheck{Args: []string{"-test.run=TestPeerProcess", "--", "--help"}, RequireFlags: []string{"--bogus"}}
	m = managerFor(t, p)
	waitState(t, m, "failed")
	if _, e := os.Stat(file); e != nil {
		t.Fatal("bad flag consumed grant")
	}
	p.FlagCheck.RequireFlags = []string{"--allowedUrlPattern"}
	m = managerFor(t, p)
	waitState(t, m, "ready")
	if _, e := os.Stat(file); !os.IsNotExist(e) {
		t.Fatal("grant was not consumed")
	}
	m.Close()
	writeGrant(t, file, time.Now().Add(time.Minute))
	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := consumeGrant(file, "personal"); e == nil {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("grant race winners=%d", winners.Load())
	}
	writeGrant(t, file, time.Now().Add(150*time.Millisecond))
	p.Env["PEER_HELP_DELAY_MS"] = "250"
	m = managerFor(t, p)
	waitState(t, m, "failed")
	if _, e := os.Stat(file); e != nil {
		t.Fatal("expired grant was consumed after preflight")
	}
	delete(p.Env, "PEER_HELP_DELAY_MS")
	writeGrant(t, file, time.Now().Add(300*time.Millisecond))
	m = managerFor(t, p)
	waitState(t, m, "ready")
	waitState(t, m, "failed")
}
func TestCodexPaginationOptions(t *testing.T) {
	t.Setenv("PEER_FIXTURE", "1")
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "codex")
	quoted := "'" + strings.ReplaceAll(os.Args[0], "'", "'\\''") + "'"
	if e := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec "+quoted+" -test.run=TestPeerProcess -- \"$@\"\n"), 0700); e != nil {
		t.Fatal(e)
	}
	slow := fixtureConfig(t, "slow_codex_neighbor")
	slow.Env["PEER_LIST_DELAY_MS"] = "3000"
	registry, _ := json.Marshal(map[string]any{"providers": []providerConfig{slow}})
	t.Setenv("MAC_DEV_BRIDGE_MCP_SERVERS_JSON", string(registry))
	m, e := New(core.Config{Home: dir, DataDir: dir, CodexBin: wrapper})
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close()
	for _, tc := range []struct {
		name, method string
		args         map[string]any
	}{
		{"codex_thread_read", "thread/read", map[string]any{"thread_id": "test-id", "include_turns": false}},
		{"codex_thread_list", "thread/list", map[string]any{"limit": 7, "cursor": "page-token", "is_pinned": false, "use_state_db_only": true, "model_providers": []string{"openai"}, "source_kinds": []string{"cli"}, "search_term": "find", "cwd": "repo"}},
		{"codex_thread_turns_list", "thread/turns/list", map[string]any{"thread_id": "test-id", "cursor": "turns-page", "items_view": "full", "sort_direction": "desc"}},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		v, e := m.Call(ctx, tc.name, tc.args)
		cancel()
		if e != nil {
			t.Fatal(e)
		}
		result := v.(map[string]any)
		if result["method"] != tc.method || result["nextCursor"] != "retained-cursor" {
			t.Fatalf("Codex response changed: %#v", result)
		}
		params := result["params"].(map[string]any)
		want := map[string]map[string]any{
			"thread/read":       {"threadId": "test-id", "includeTurns": false},
			"thread/list":       {"limit": 7, "cursor": "page-token", "sortKey": "recency_at", "sortDirection": "desc", "isPinned": false, "useStateDbOnly": true, "modelProviders": []string{"openai"}, "sourceKinds": []string{"cli"}, "searchTerm": "find", "cwd": filepath.Join(dir, "repo")},
			"thread/turns/list": {"threadId": "test-id", "limit": 50, "cursor": "turns-page", "itemsView": "full", "sortDirection": "desc"},
		}[tc.method]
		wantJSON, _ := json.Marshal(want)
		actualJSON, _ := json.Marshal(params)
		if string(wantJSON) != string(actualJSON) {
			t.Fatalf("app-server wire params differ for %s: got %s want %s", tc.method, actualJSON, wantJSON)
		}
		for in, out := range map[string]string{"cursor": "cursor", "is_pinned": "isPinned", "include_turns": "includeTurns", "use_state_db_only": "useStateDbOnly"} {
			if expected, ok := tc.args[in]; ok && params[out] != expected {
				t.Fatalf("Codex filter %s was not forwarded: %#v", in, params)
			}
		}
	}
}

func TestNewKeepsNativeStartupIndependentAndCatalogComplete(t *testing.T) {
	p := fixtureConfig(t, "slow")
	p.Env["PEER_LIST_DELAY_MS"] = "450"
	start := time.Now()
	m := managerFor(t, p)
	if time.Since(start) > 250*time.Millisecond {
		t.Fatal("New blocked local transport startup on child discovery")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, err := m.Tools(ctx); err != context.DeadlineExceeded {
		t.Fatal("catalog did not respect caller deadline", err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel2()
	if _, err := m.Call(ctx2, "slow__echo", nil); err != context.DeadlineExceeded {
		t.Fatal("foreign call did not respect initial discovery deadline", err)
	}
	tools, err := m.Tools(context.Background())
	if err != nil || len(tools) != 5 {
		t.Fatal("first successful catalog was incomplete", len(tools), err)
	}
	for _, tool := range tools {
		if !strings.HasPrefix(tool.Name, "slow__") {
			t.Fatalf("native descriptor duplicated in peer catalog: %s", tool.Name)
		}
	}
}
func TestInitialCrashRetryDoesNotPublishEmptyCatalog(t *testing.T) {
	p := fixtureConfig(t, "recover")
	p.Env["PEER_CRASH_ONCE"] = filepath.Join(t.TempDir(), "once")
	m := managerFor(t, p)
	tools, err := m.Tools(context.Background())
	if err != nil || len(tools) != 5 {
		t.Fatal("first initial retry was published as an empty catalog", len(tools), err)
	}
}
func TestNoProvidersDoesNotDuplicateCodexCatalog(t *testing.T) {
	m := managerFor(t)
	tools, err := m.Tools(context.Background())
	if err != nil || len(tools) != 0 {
		t.Fatal("peer catalog included builtins", tools, err)
	}
}
