package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"macbridge/internal/core"
)

type Manager struct {
	cfg                                              core.Config
	mu                                               sync.Mutex
	files                                            sync.Mutex
	sessions                                         map[string]*session
	closed                                           chan struct{}
	closeOnce                                        sync.Once
	unlockWatched                                    bool
	maxSessions, ringBytes, idleMS, lifeMS, logBytes int
	defaultOutput, maxOutput                         int
}

func New(c core.Config) (*Manager, error) {
	if c.Shell == "" {
		c.Shell = "/bin/sh"
	}
	if c.Home == "" {
		c.Home, _ = os.UserHomeDir()
	}
	for _, p := range []string{c.DataDir, c.LogDir, filepath.Join(c.DataDir, "jobs")} {
		if e := os.MkdirAll(p, 0700); e != nil {
			return nil, e
		}
	}
	m := &Manager{cfg: c, sessions: map[string]*session{}, closed: make(chan struct{}), maxSessions: clamp(core.EnvInt("MAC_DEV_BRIDGE_PTY_MAX_SESSIONS", 8), 1, 64), ringBytes: clamp(core.EnvInt("MAC_DEV_BRIDGE_PTY_RING_BYTES", 262144), 4096, 4000000), idleMS: clamp(core.EnvInt("MAC_DEV_BRIDGE_PTY_IDLE_TIMEOUT_MS", 900000), 1000, 3600000), lifeMS: clamp(core.EnvInt("MAC_DEV_BRIDGE_PTY_MAX_LIFETIME_MS", 28800000), 5000, 86400000), logBytes: clamp(core.EnvInt("MAC_DEV_BRIDGE_JOB_LOG_MAX_BYTES", 8000000), 1024, 64000000)}
	m.maxOutput = clamp(core.EnvInt("MAC_DEV_BRIDGE_MAX_OUTPUT_BYTES", 8000000), 1024, 64000000)
	m.defaultOutput = min(m.maxOutput, clamp(core.EnvInt("MAC_DEV_BRIDGE_DEFAULT_OUTPUT_BYTES", 1000000), 1024, 8000000))
	if c.UnlockFile != "" {
		_, e := os.Stat(c.UnlockFile)
		m.unlockWatched = e == nil
	}
	go m.reap()
	return m, nil
}
func (m *Manager) Call(ctx context.Context, name string, a map[string]any) (any, error) {
	for _, key := range []string{"include_sha256", "append", "atomic", "create_parents", "recursive", "include_hidden", "force", "check_only", "reverse", "three_way", "strip_ansi", "collapse_carriage_returns"} {
		if v, ok := a[key]; ok {
			if _, ok = v.(bool); !ok {
				return nil, core.Error("INVALID_ARGUMENT", key+" must be a boolean")
			}
		}
	}
	for _, key := range []string{"command", "cwd", "stdin", "label", "job_id", "session_id", "path", "content", "encoding", "operation", "destination", "patch", "term", "signal", "data"} {
		if v, ok := a[key]; ok {
			if _, ok = v.(string); !ok {
				return nil, core.Error("INVALID_ARGUMENT", key+" must be a string")
			}
		}
	}
	switch name {
	case "shell_exec":
		return m.shellExec(ctx, a)
	case "shell_start":
		return m.startJob(ctx, a)
	case "shell_job_status", "shell_job_list", "shell_job_kill":
		return m.jobCall(ctx, name, a)
	case "fs_read", "fs_write", "fs_stat", "fs_list", "fs_manage", "apply_patch":
		return m.fsCall(ctx, name, a)
	case "pty_start", "pty_read", "pty_write", "pty_resize", "pty_signal", "pty_close":
		return m.ptyCall(ctx, name, a)
	default:
		return nil, core.Error("UNKNOWN_TOOL", "Unknown host tool: "+name)
	}
}
func (m *Manager) Status() map[string]any {
	m.mu.Lock()
	ss := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		ss = append(ss, s)
	}
	m.mu.Unlock()
	live := 0
	for _, s := range ss {
		s.mu.Lock()
		if !s.exited {
			live++
		}
		s.mu.Unlock()
	}
	return map[string]any{"jobDir": filepath.Join(m.cfg.DataDir, "jobs"), "jobLogMaxBytes": m.logBytes, "pty": map[string]any{"available": true, "maxSessions": m.maxSessions, "activeSessions": live, "retainedSessions": len(ss), "ringBytes": m.ringBytes, "idleTimeoutMs": m.idleMS, "maxLifetimeMs": m.lifeMS}}
}
func (m *Manager) Close() error {
	m.closeOnce.Do(func() {
		close(m.closed)
		m.mu.Lock()
		ss := make([]*session, 0, len(m.sessions))
		for _, s := range m.sessions {
			ss = append(ss, s)
		}
		m.mu.Unlock()
		for _, s := range ss {
			m.closeSession(s, "bridge_closed", true, 0)
		}
	})
	return nil
}
func clamp(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}
func integer(a map[string]any, k string, d, lo, hi int) (int, error) {
	n := core.Int(a, k, d)
	if v, ok := a[k]; ok {
		switch x := v.(type) {
		case float64:
			if float64(n) != x {
				return 0, core.Error("INVALID_ARGUMENT", k+" must be an integer")
			}
		case int, int64:
		default:
			return 0, core.Error("INVALID_ARGUMENT", k+" must be an integer")
		}
	}
	if n < lo || n > hi {
		return 0, core.Error("INVALID_ARGUMENT", fmt.Sprintf("%s must be between %d and %d", k, lo, hi))
	}
	return n, nil
}
func environment(a map[string]any) ([]string, error) {
	values := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			values[k] = v
		}
	}
	if raw, ok := a["env"]; ok {
		env, ok := raw.(map[string]any)
		if !ok {
			return nil, core.Error("INVALID_ARGUMENT", "env must be an object")
		}
		for k, v := range env {
			if k == "" || strings.ContainsAny(k, "=\x00") {
				return nil, core.Error("INVALID_ARGUMENT", "Invalid environment variable name")
			}
			if v == nil {
				delete(values, k)
				continue
			}
			switch v.(type) {
			case string, float64, int, int64, bool:
			default:
				return nil, core.Error("INVALID_ARGUMENT", "Invalid environment variable value")
			}
			text := fmt.Sprint(v)
			if strings.ContainsRune(text, 0) {
				return nil, core.Error("INVALID_ARGUMENT", "Environment values cannot contain NUL")
			}
			values[k] = text
		}
	}
	// Runtime credentials are never useful to the command being executed.
	delete(values, "CONTROL_PLANE_API_KEY")
	delete(values, "MAC_DEV_BRIDGE_FULL_ACCESS_ACK")
	result := make([]string, 0, len(values))
	for k, v := range values {
		result = append(result, k+"="+v)
	}
	return result, nil
}

var signalNames = map[string]syscall.Signal{"HUP": syscall.SIGHUP, "INT": syscall.SIGINT, "QUIT": syscall.SIGQUIT, "ILL": syscall.SIGILL, "TRAP": syscall.SIGTRAP, "ABRT": syscall.SIGABRT, "BUS": syscall.SIGBUS, "FPE": syscall.SIGFPE, "KILL": syscall.SIGKILL, "USR1": syscall.SIGUSR1, "SEGV": syscall.SIGSEGV, "USR2": syscall.SIGUSR2, "PIPE": syscall.SIGPIPE, "ALRM": syscall.SIGALRM, "TERM": syscall.SIGTERM, "CHLD": syscall.SIGCHLD, "CONT": syscall.SIGCONT, "STOP": syscall.SIGSTOP, "TSTP": syscall.SIGTSTP, "TTIN": syscall.SIGTTIN, "TTOU": syscall.SIGTTOU, "URG": syscall.SIGURG, "XCPU": syscall.SIGXCPU, "XFSZ": syscall.SIGXFSZ, "VTALRM": syscall.SIGVTALRM, "PROF": syscall.SIGPROF, "WINCH": syscall.SIGWINCH, "IO": syscall.SIGIO, "SYS": syscall.SIGSYS}

func parseSignal(name string) (syscall.Signal, error) {
	s, ok := signalNames[strings.TrimPrefix(name, "SIG")]
	if !ok {
		return 0, core.Error("INVALID_SIGNAL", "Invalid signal: "+name)
	}
	return s, nil
}
func signalName(sig syscall.Signal) string {
	for k, v := range signalNames {
		if sig == v {
			return "SIG" + k
		}
	}
	return fmt.Sprint(sig)
}
func groupAlive(pid int) bool {
	if pid <= 1 {
		return false
	}
	e := syscall.Kill(-pid, 0)
	return e == nil || e == syscall.EPERM
}
func waitDuration(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
