package browser

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"macbridge/internal/core"
)

func TestNativeFrames(t *testing.T) {
	var b bytes.Buffer
	payload := []byte(`{"hello":"世界"}`)
	if e := writeFrame(&b, payload); e != nil {
		t.Fatal(e)
	}
	actual, e := readFrame(&b)
	if e != nil || !bytes.Equal(actual, payload) {
		t.Fatal(string(actual), e)
	}
	b.Reset()
	binary.Write(&b, binary.LittleEndian, uint32(MaxWireBytes+1))
	if _, e = readFrame(&b); e == nil {
		t.Fatal("oversized message accepted")
	}
	b.Reset()
	binary.Write(&b, binary.LittleEndian, uint32(5))
	b.WriteByte(1)
	if _, e = readFrame(&b); e == nil {
		t.Fatal("short native frame accepted")
	}
}

func TestNativeEndToEndAndProfileBinding(t *testing.T) {
	dir, e := os.MkdirTemp("", "mb-native-")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(dir)
	cfg := core.Config{Home: dir, DataDir: dir, ChromeSocket: filepath.Join(dir, "chrome.sock")}
	inputR, inputW := io.Pipe()
	outputR, outputW := io.Pipe()
	defer inputR.Close()
	defer outputR.Close()
	finished := make(chan error, 1)
	go func() { finished <- runNativeHost(cfg, inputR, outputW); outputW.Close() }()
	var writes sync.Mutex
	send := func(v any) error {
		writes.Lock()
		defer writes.Unlock()
		raw, _ := json.Marshal(v)
		return writeFrame(inputW, raw)
	}
	ready := make(chan map[string]any, 4)
	failures := make(chan error, 1)
	var chunks atomic.Int32
	go func() {
		fragments := map[string][][]byte{}
		for {
			raw, e := readFrame(outputR)
			if e != nil {
				if e != io.EOF {
					select {
					case failures <- e:
					default:
					}
				}
				return
			}
			var request map[string]any
			if e = json.Unmarshal(raw, &request); e != nil {
				failures <- e
				return
			}
			if request["type"] == "ready" {
				ready <- request
				continue
			}
			if request["type"] == "chunk" {
				chunks.Add(1)
				id := request["id"].(string)
				if fragments[id] == nil {
					fragments[id] = make([][]byte, int(request["total"].(float64)))
				}
				part, _ := base64.StdEncoding.DecodeString(request["data"].(string))
				fragments[id][int(request["index"].(float64))] = part
				complete := true
				for _, p := range fragments[id] {
					complete = complete && p != nil
				}
				if !complete {
					continue
				}
				raw = bytes.Join(fragments[id], nil)
				delete(fragments, id)
				if e = json.Unmarshal(raw, &request); e != nil {
					failures <- e
					return
				}
			}
			args, _ := request["args"].(map[string]any)
			result := map[string]any{"method": request["method"], "args": args, "patterns": request["allowedUrlPatterns"]}
			if request["method"] == "workspace.release" {
				result["released"] = true
			}
			if e = send(map[string]any{"type": "response", "id": request["id"], "ok": true, "result": result}); e != nil {
				return
			}
		}
	}()
	defer func() {
		inputW.Close()
		select {
		case e := <-finished:
			if e != nil {
				t.Error(e)
			}
		case <-time.After(2 * time.Second):
			t.Error("native host leaked after EOF")
		}
	}()
	if e = send(map[string]any{"type": "hello", "profileId": "profile-original-123", "extensionId": strings.Repeat("a", 32), "version": "1"}); e != nil {
		t.Fatal(e)
	}
	select {
	case response := <-ready:
		if response["ok"] != true {
			t.Fatal(response)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no native handshake")
	}
	if duplicateErr := runNativeHost(cfg, strings.NewReader(""), io.Discard); duplicateErr == nil {
		t.Fatal("duplicate native host stole the live socket")
	}
	manager := New(cfg)
	if manager.Status()["extensionReady"] != true {
		t.Fatal(manager.Status())
	}
	value, e := manager.Call(context.Background(), "chrome_snapshot", map[string]any{"tab_id": float64(9), "max_elements": float64(10)})
	if e != nil {
		t.Fatal(e)
	}
	result := value.(map[string]any)
	if result["method"] != "tabs.snapshot" || result["args"].(map[string]any)["tabId"] != float64(9) {
		t.Fatal(result)
	}
	if e = core.AtomicJSON(filepath.Join(dir, "settings.json"), map[string]any{"strictApprovals": true}); e != nil {
		t.Fatal(e)
	}
	if _, e = manager.Call(context.Background(), "chrome_tabs", nil); e == nil {
		t.Fatal("strict browser call without grant accepted")
	}
	if _, e = manager.Call(context.Background(), "chrome_close", map[string]any{"tab_id": 3}); e != nil {
		t.Fatal("pool cleanup required site grant:", e)
	}
	grant := map[string]any{"provider": "chrome-background", "nonce": strings.Repeat("b", 32), "expiresAt": time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339), "allowedUrlPatterns": []string{"https://example.com/*"}}
	if e = core.AtomicJSON(filepath.Join(dir, "chrome-background-grants", strings.Repeat("b", 32)+".json"), grant); e != nil {
		t.Fatal(e)
	}
	if _, e = manager.Call(context.Background(), "chrome_tabs", nil); e != nil {
		t.Fatal(e)
	}
	if e = core.AtomicJSON(filepath.Join(dir, "settings.json"), map[string]any{"strictApprovals": false}); e != nil {
		t.Fatal(e)
	}
	prompt := strings.Repeat("界", 800000)
	value, e = manager.Call(context.Background(), "chatgpt_conversation_start", map[string]any{"prompt": prompt})
	if e != nil {
		t.Fatal(e)
	}
	if value.(map[string]any)["args"].(map[string]any)["prompt"] != prompt || chunks.Load() < 2 {
		t.Fatal("large native prompt did not survive fragmentation")
	}
	if e = send(map[string]any{"type": "hello", "profileId": "different-profile", "extensionId": strings.Repeat("a", 32), "version": "1"}); e != nil {
		t.Fatal(e)
	}
	select {
	case response := <-ready:
		if response["ok"] != false {
			t.Fatal("profile rebinding silently accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("profile mismatch did not respond")
	}
	if manager.Status()["extensionReady"] != false {
		t.Fatal("mismatched profile is ready")
	}
	if _, e = manager.Call(context.Background(), "chrome_workspace_status", nil); e == nil {
		t.Fatal("mismatched profile reached extension")
	}
	select {
	case e := <-failures:
		t.Fatal(e)
	default:
	}
}

func TestBrowserValidationAndOfflineInspection(t *testing.T) {
	dir := t.TempDir()
	m := New(core.Config{Home: dir, DataDir: dir, ChromeSocket: filepath.Join(dir, "missing")})
	cases := []struct {
		name string
		args map[string]any
	}{{"chrome_fill", map[string]any{"tab_id": 1, "selector": "#a", "value": false}}, {"chrome_open", map[string]any{"url": "https://x/", "extra": true}}, {"chrome_snapshot", map[string]any{"tab_id": 1.5}}, {"chatgpt_conversation_start", map[string]any{"prompt": "hi", "transport": "raw", "conversation_id": "abcdefgh"}}, {"chatgpt_conversation_start", map[string]any{"prompt": "hi", "thinking_effort": "unsupported"}}}
	for _, c := range cases {
		if _, e := m.Call(context.Background(), c.name, c.args); e == nil {
			t.Fatalf("accepted invalid args: %#v", c)
		}
	}
	v, e := m.Call(context.Background(), "chatgpt_extension_status", nil)
	if e != nil || v.(map[string]any)["nativeHostRegistered"] != false {
		t.Fatal(v, e)
	}
}

func TestAccountBindingAndLockedNativeHost(t *testing.T) {
	t.Setenv("MAC_DEV_BRIDGE_FULL_ACCESS_ACK", "")
	dir := t.TempDir()
	cfg := core.Config{DataDir: dir, UnlockFile: filepath.Join(dir, "missing-unlock")}
	if err := NativeHost(cfg); err == nil {
		t.Fatal("native host started after explicit stop removed unlock")
	}
	var output bytes.Buffer
	n := &nativeBridge{cfg: cfg, output: &output}
	msg := map[string]any{"type": "hello", "profileId": "stable-profile-id", "extensionId": strings.Repeat("a", 32), "profile": map[string]any{"email": "test@example.com", "id": "12345", "signedIn": true}}
	n.hello(msg)
	if n.status()["extensionReady"] != true {
		t.Fatal(n.status())
	}
	msg["profile"] = map[string]any{"email": "different@example.com", "id": "99999", "signedIn": true}
	n.hello(msg)
	if n.status()["extensionReady"] != false {
		t.Fatal("changed primary account retained browser access")
	}
	msg["profile"] = map[string]any{"email": "test@example.com", "id": "12345", "signedIn": true}
	n.hello(msg)
	if n.status()["extensionReady"] != true {
		t.Fatal("original profile could not reconnect")
	}
	if e := core.AtomicJSON(filepath.Join(dir, "chrome-background-profile.json"), map[string]any{"expectedEmail": "legacy@example.com", "expectedGaiaId": "8888", "profileDirectory": "Default"}); e != nil {
		t.Fatal(e)
	}
	n.hello(msg)
	if n.status()["extensionReady"] != false {
		t.Fatal("legacy account binding ignored")
	}
}

func TestLegacyBrowserGrantsRemainAdditive(t *testing.T) {
	dir := t.TempDir()
	m := New(core.Config{DataDir: dir})
	if e := core.AtomicJSON(filepath.Join(dir, "settings.json"), map[string]any{"strictApprovals": true}); e != nil {
		t.Fatal(e)
	}
	legacy := filepath.Join(dir, "PERSONAL_BROWSER_APPROVED")
	for i, prefix := range []string{"a", "b"} {
		grant := map[string]any{"provider": "chrome-background", "nonce": strings.Repeat(prefix, 32), "expiresAt": time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339), "allowedUrlPatterns": []string{"https://" + prefix + ".example/*"}}
		if e := core.AtomicJSON(legacy, grant); e != nil {
			t.Fatal(e)
		}
		patterns, e := m.patterns()
		if e != nil || len(patterns) != i+1 {
			t.Fatal(patterns, e)
		}
		if _, e = os.Stat(legacy); !os.IsNotExist(e) {
			t.Fatal("legacy grant was not imported")
		}
	}
	patterns, e := m.patterns()
	if e != nil || len(patterns) != 2 {
		t.Fatal("earlier session's grant lost", patterns, e)
	}
}
