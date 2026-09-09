package host

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"macbridge/internal/core"
)

type capture struct {
	mu           sync.Mutex
	data         []byte
	limit, total int
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total += len(p)
	n := min(len(p), c.limit-len(c.data))
	c.data = append(c.data, p[:n]...)
	return len(p), nil
}
func exitFields(state interface {
	Sys() any
	ExitCode() int
}) (any, any) {
	if state == nil {
		return nil, nil
	}
	if w, ok := state.Sys().(syscall.WaitStatus); ok && w.Signaled() {
		return nil, signalName(w.Signal())
	}
	return state.ExitCode(), nil
}
func (m *Manager) shellExec(ctx context.Context, a map[string]any) (any, error) {
	command, e := core.Required(a, "command")
	if e != nil {
		return nil, e
	}
	timeout, e := integer(a, "timeout_ms", 600000, 0, 1800000)
	if e != nil {
		return nil, e
	}
	limit, e := integer(a, "max_output_bytes", m.defaultOutput, 1024, 64000000)
	if e != nil {
		return nil, e
	}
	env, e := environment(a)
	if e != nil {
		return nil, e
	}
	cwd := m.cfg.Path(core.String(a, "cwd", m.cfg.Home))
	cmd := exec.Command(m.cfg.Shell, "-lc", command)
	cmd.Dir = cwd
	cmd.Env = env
	cmd.Stdin = strings.NewReader(core.String(a, "stdin", ""))
	result, e := runCommand(ctx, cmd, time.Duration(timeout)*time.Millisecond, min(limit, m.maxOutput))
	if result != nil {
		result["command"] = command
		result["shell"] = m.cfg.Shell
		result["cwd"] = cwd
	}
	return result, e
}
func runCommand(ctx context.Context, cmd *exec.Cmd, timeout time.Duration, limit int) (map[string]any, error) {
	started := time.Now()
	out := &capture{limit: limit}
	errout := &capture{limit: limit}
	cmd.Stdout = out
	cmd.Stderr = errout
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second
	if e := cmd.Start(); e != nil {
		return nil, e
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var timer <-chan time.Time
	var t *time.Timer
	if timeout > 0 {
		t = time.NewTimer(timeout)
		timer = t.C
		defer t.Stop()
	}
	timedOut := false
	var waitErr error
	select {
	case waitErr = <-done:
	case <-timer:
		timedOut = true
	case <-ctx.Done():
	}
	if timedOut || ctx.Err() != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case waitErr = <-done:
		case <-time.After(2 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			waitErr = <-done
		}
	}
	if waitErr != nil {
		var ee *exec.ExitError
		if !errors.As(waitErr, &ee) {
			return nil, waitErr
		}
	}
	code, sig := exitFields(cmd.ProcessState)
	return map[string]any{"exitCode": code, "signal": sig, "timedOut": timedOut, "durationMs": time.Since(started).Milliseconds(), "stdout": string(bytes.ToValidUTF8(out.data, []byte("�"))), "stderr": string(bytes.ToValidUTF8(errout.data, []byte("�"))), "stdoutTruncated": out.total > len(out.data), "stderrTruncated": errout.total > len(errout.data), "capturedStdoutBytes": len(out.data), "capturedStderrBytes": len(errout.data)}, nil
}

var _ io.Writer = (*capture)(nil)
