package browser

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"macbridge/internal/core"
)

type nativeBridge struct {
	cfg                         core.Config
	output                      io.Writer
	mu                          sync.Mutex
	writeMu                     sync.Mutex
	pending                     map[string]chan wireReply
	ready                       bool
	profile, extension, version string
	profileError                *core.Fault
	done                        chan struct{}
}

func readFrame(r io.Reader) ([]byte, error) {
	var size uint32
	if e := binary.Read(r, binary.LittleEndian, &size); e != nil {
		return nil, e
	}
	if size == 0 || size > MaxWireBytes {
		return nil, fmt.Errorf("native message length %d is invalid", size)
	}
	b := make([]byte, size)
	_, e := io.ReadFull(r, b)
	return b, e
}
func writeFrame(w io.Writer, b []byte) error {
	if e := binary.Write(w, binary.LittleEndian, uint32(len(b))); e != nil {
		return e
	}
	for len(b) > 0 {
		n, e := w.Write(b)
		if e != nil {
			return e
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
func (n *nativeBridge) send(v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	n.writeMu.Lock()
	defer n.writeMu.Unlock()
	// Chrome limits host-to-extension native messages to 1 MiB. Chunk large prompts.
	if len(b) < 900000 {
		return writeFrame(n.output, b)
	}
	id := core.ID()
	const part = 512000
	total := (len(b) + part - 1) / part
	for i := 0; i < total; i++ {
		end := (i + 1) * part
		if end > len(b) {
			end = len(b)
		}
		frame, _ := json.Marshal(map[string]any{"type": "chunk", "id": id, "index": i, "total": total, "data": base64.StdEncoding.EncodeToString(b[i*part : end])})
		if e := writeFrame(n.output, frame); e != nil {
			return e
		}
	}
	return nil
}
func (n *nativeBridge) hello(msg map[string]any) {
	profile := core.String(msg, "profileId", "")
	extension := core.String(msg, "extensionId", "")
	account, _ := msg["profile"].(map[string]any)
	email, gaia := core.String(account, "email", ""), core.String(account, "id", "")
	var failure *core.Fault
	legacyPath := core.Env("MAC_DEV_BRIDGE_CHROME_PROFILE_BINDING_FILE", filepath.Join(n.cfg.DataDir, "chrome-background-profile.json"))
	if raw, err := os.ReadFile(legacyPath); err == nil {
		var legacy struct{ ExpectedEmail, ExpectedGaiaID string }
		if json.Unmarshal(raw, &legacy) != nil || legacy.ExpectedEmail == "" || legacy.ExpectedGaiaID == "" {
			failure = core.Error("CHROME_PROFILE_BINDING_INVALID", "Legacy Chrome account binding is malformed").(*core.Fault)
		} else if !strings.EqualFold(email, legacy.ExpectedEmail) || gaia != legacy.ExpectedGaiaID {
			failure = core.Error("CHROME_PROFILE_MISMATCH", "Chrome primary account differs from the configured account binding").(*core.Fault)
		}
	} else if !os.IsNotExist(err) {
		failure = core.Error("CHROME_PROFILE_BINDING_INVALID", err.Error()).(*core.Fault)
	}
	if len(profile) < 8 || len(profile) > 128 || len(extension) != 32 {
		failure = core.Error("CHROME_PROFILE_INVALID", "Invalid extension profile identity").(*core.Fault)
	} else if failure == nil {
		file := filepath.Join(n.cfg.DataDir, "chrome-profile-binding.json")
		var stored struct {
			ProfileID   string `json:"profileId"`
			ExtensionID string `json:"extensionId"`
			Email       string `json:"email,omitempty"`
			GaiaID      string `json:"gaiaId,omitempty"`
		}
		raw, e := os.ReadFile(file)
		if e == nil {
			if json.Unmarshal(raw, &stored) != nil || stored.ProfileID != profile || stored.ExtensionID != extension || stored.GaiaID != "" && (stored.GaiaID != gaia || !strings.EqualFold(stored.Email, email)) {
				failure = core.Error("CHROME_PROFILE_MISMATCH", "This bridge is bound to a different Chrome profile or extension installation").(*core.Fault)
			}
		} else if os.IsNotExist(e) {
			if e = core.AtomicJSON(file, map[string]any{"profileId": profile, "extensionId": extension, "email": email, "gaiaId": gaia, "boundAt": core.Now()}); e != nil {
				failure = core.Error("CHROME_PROFILE_BIND_FAILED", e.Error()).(*core.Fault)
			}
		} else {
			failure = core.Error("CHROME_PROFILE_BIND_FAILED", e.Error()).(*core.Fault)
		}
	}
	n.mu.Lock()
	n.ready = failure == nil
	n.profile = profile
	n.extension = extension
	n.version = core.String(msg, "version", "")
	n.profileError = failure
	n.mu.Unlock()
	n.send(map[string]any{"type": "ready", "ok": failure == nil, "error": failure})
}
func (n *nativeBridge) status() map[string]any {
	n.mu.Lock()
	defer n.mu.Unlock()
	return map[string]any{"extensionReady": n.ready, "hostPid": os.Getpid(), "profileId": n.profile, "extensionId": n.extension, "version": n.version, "profileError": n.profileError, "socketPath": n.cfg.ChromeSocket}
}
func (n *nativeBridge) serve(conn net.Conn) {
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var request map[string]any
	if e := readLineJSON(conn, &request); e != nil {
		return
	}
	conn.SetReadDeadline(time.Time{})
	id := core.String(request, "id", core.ID())
	reply := wireReply{ID: id}
	if core.String(request, "method", "") == "host.status" {
		reply.OK = true
		reply.Result = n.status()
		json.NewEncoder(conn).Encode(reply)
		return
	}
	n.mu.Lock()
	ready := n.ready
	failure := n.profileError
	n.mu.Unlock()
	if !ready {
		if failure == nil {
			failure = core.Error("CHROME_EXTENSION_OFFLINE", "Chrome extension has not completed its handshake").(*core.Fault)
		}
		reply.Error = failure
		json.NewEncoder(conn).Encode(reply)
		return
	}
	routeID := core.ID()
	ch := make(chan wireReply, 1)
	n.mu.Lock()
	n.pending[routeID] = ch
	n.mu.Unlock()
	defer func() { n.mu.Lock(); delete(n.pending, routeID); n.mu.Unlock() }()
	request["id"] = routeID
	request["type"] = "request"
	if e := n.send(request); e != nil {
		reply.Error = core.Error("CHROME_NATIVE_SEND_FAILED", e.Error()).(*core.Fault)
	} else {
		ms := core.Int(request, "timeoutMs", 45000)
		if ms < 1000 {
			ms = 1000
		}
		if ms > 3720000 {
			ms = 3720000
		}
		timer := time.NewTimer(time.Duration(ms) * time.Millisecond)
		defer timer.Stop()
		select {
		case reply = <-ch:
		case <-timer.C:
			reply.Error = core.Error("CHROME_HOST_TIMEOUT", "Chrome operation deadline elapsed; it was not retried").(*core.Fault)
		case <-n.done:
			reply.Error = core.Error("CHROME_EXTENSION_OFFLINE", "Native messaging disconnected").(*core.Fault)
		}
	}
	reply.ID = id
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	json.NewEncoder(conn).Encode(reply)
}
func NativeHost(cfg core.Config) error {
	acknowledged := os.Getenv("MAC_DEV_BRIDGE_FULL_ACCESS_ACK") == "I_UNDERSTAND_THIS_GRANTS_FULL_ACCESS"
	os.Unsetenv("MAC_DEV_BRIDGE_FULL_ACCESS_ACK")
	if cfg.UnlockFile != "" && !acknowledged {
		raw, err := os.ReadFile(cfg.UnlockFile)
		if err != nil || strings.TrimSpace(string(raw)) != "I_UNDERSTAND_THIS_GRANTS_FULL_ACCESS" {
			return core.Error("BRIDGE_LOCKED", "Native Chrome bridge is disabled; run macbridge init before reconnecting")
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stop()
	go func() { <-ctx.Done(); os.Stdin.Close() }()
	err := runNativeHost(cfg, os.Stdin, os.Stdout)
	if ctx.Err() != nil {
		return nil
	}
	return err
}
func runNativeHost(cfg core.Config, input io.Reader, output io.Writer) error {
	if e := os.MkdirAll(cfg.DataDir, 0700); e != nil {
		return e
	}
	if e := os.MkdirAll(filepath.Dir(cfg.ChromeSocket), 0700); e != nil {
		return e
	}
	// Keep the lock file itself: unlinking it would let a waiting host and a new
	// host hold locks on different inodes while repairing the same socket.
	lock, e := os.OpenFile(cfg.ChromeSocket+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		return fmt.Errorf("another Chrome native host owns this socket: %w", e)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if existing, e := net.DialTimeout("unix", cfg.ChromeSocket, 200*time.Millisecond); e == nil {
		existing.Close()
		return errors.New("another Chrome native host is already connected")
	}
	if stat, e := os.Lstat(cfg.ChromeSocket); e == nil {
		if stat.Mode()&os.ModeSocket == 0 {
			return errors.New("Chrome socket path is occupied by a non-socket file")
		}
		if e = os.Remove(cfg.ChromeSocket); e != nil {
			return e
		}
	}
	listener, e := net.Listen("unix", cfg.ChromeSocket)
	if e != nil {
		return e
	}
	defer listener.Close()
	defer os.Remove(cfg.ChromeSocket)
	os.Chmod(cfg.ChromeSocket, 0600)
	pidfile := core.Env("MAC_DEV_BRIDGE_CHROME_NATIVE_PID_FILE", filepath.Join(cfg.DataDir, "chrome-native-host.pid"))
	if e = os.WriteFile(pidfile, []byte(fmt.Sprint(os.Getpid())), 0600); e != nil {
		return e
	}
	defer os.Remove(pidfile)
	executable, e := os.Executable()
	if e != nil {
		return e
	}
	ps := exec.Command("ps", "-p", fmt.Sprint(os.Getpid()), "-o", "lstart=")
	ps.Env = append(os.Environ(), "LC_ALL=C")
	start, e := ps.Output()
	if e != nil || strings.TrimSpace(string(start)) == "" {
		return fmt.Errorf("could not identify native host start time: %v", e)
	}
	identityFile := filepath.Join(cfg.DataDir, "chrome-native-host.json")
	if e = core.AtomicJSON(identityFile, map[string]any{"pid": os.Getpid(), "executable": executable, "processStart": strings.TrimSpace(string(start))}); e != nil {
		return e
	}
	defer os.Remove(identityFile)
	n := &nativeBridge{cfg: cfg, output: output, pending: map[string]chan wireReply{}, done: make(chan struct{})}
	defer close(n.done)
	go func() {
		for {
			conn, e := listener.Accept()
			if e != nil {
				return
			}
			go n.serve(conn)
		}
	}()
	for {
		b, e := readFrame(input)
		if errors.Is(e, io.EOF) {
			return nil
		}
		if e != nil {
			return e
		}
		var msg map[string]any
		if e = json.Unmarshal(b, &msg); e != nil {
			return e
		}
		if core.String(msg, "type", "") == "hello" {
			n.hello(msg)
			continue
		}
		var reply wireReply
		if json.Unmarshal(b, &reply) != nil {
			continue
		}
		n.mu.Lock()
		ch := n.pending[reply.ID]
		n.mu.Unlock()
		if ch != nil {
			select {
			case ch <- reply:
			default:
			}
		}
	}
}

// WaitForNativeHost is useful for installers without starting a browser or stealing focus.
func WaitForNativeHost(ctx context.Context, cfg core.Config) error {
	m := New(cfg)
	for {
		v, e := m.request(ctx, "host.status", nil, nil, time.Second)
		if e == nil {
			if out, ok := v.(map[string]any); ok && core.Bool(out, "extensionReady", false) {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
