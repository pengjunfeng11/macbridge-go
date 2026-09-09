package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"macbridge/internal/core"
)

type byteRing struct {
	data  []byte
	total int64
}

func (r *byteRing) append(p []byte) {
	for len(p) > 0 {
		at := int(r.total % int64(len(r.data)))
		n := min(len(p), len(r.data)-at)
		copy(r.data[at:], p[:n])
		r.total += int64(n)
		p = p[n:]
	}
}
func (r *byteRing) slice(cursor int64, limit int) ([]byte, int64, int64, bool) {
	base := max(int64(0), r.total-int64(len(r.data)))
	lost := max(int64(0), base-cursor)
	cursor = max(cursor, base)
	cursor = min(cursor, r.total)
	n := min(int64(limit), r.total-cursor)
	out := make([]byte, n)
	for i := int64(0); i < n; {
		at := (cursor + i) % int64(len(r.data))
		count := min(n-i, int64(len(r.data))-at)
		copy(out[i:], r.data[at:at+count])
		i += count
	}
	return out, cursor + n, lost, cursor+n < r.total
}

type session struct {
	mu                                 sync.Mutex
	writeMu                            sync.Mutex
	closeMu                            sync.Mutex
	id, label, command, cwd, term, pts string
	processStart                       string
	argv                               []string
	master                             *os.File
	cmd                                *exec.Cmd
	cols, rows, idleMS, pending        int
	created, lastActivity, lastScan    time.Time
	ring                               byteRing
	notify                             chan struct{}
	exited, readerDone                 bool
	exitCode, exitSignal               any
	closeReason                        string
	closeResult                        map[string]any
	tracked                            map[int]string
	metadataPath                       string
}

func (s *session) wake() { close(s.notify); s.notify = make(chan struct{}) }
func (m *Manager) getSession(a map[string]any) (*session, error) {
	id, e := core.Required(a, "session_id")
	if e != nil {
		return nil, e
	}
	m.mu.Lock()
	s := m.sessions[id]
	m.mu.Unlock()
	if s == nil {
		return nil, core.Error("PTY_SESSION_NOT_FOUND", "Unknown pty session: "+id)
	}
	return s, nil
}
func (m *Manager) startPTY(a map[string]any) (any, error) {
	command, e := core.Required(a, "command")
	if e != nil {
		return nil, e
	}
	argv := []string{}
	if raw, ok := a["args"]; ok {
		switch v := raw.(type) {
		case []string:
			argv = v
		case []any:
			for _, item := range v {
				s, ok := item.(string)
				if !ok {
					return nil, core.Error("INVALID_ARGUMENT", "args must contain strings")
				}
				argv = append(argv, s)
			}
		default:
			return nil, core.Error("INVALID_ARGUMENT", "args must be an array")
		}
	}
	if len(argv) > 256 {
		return nil, core.Error("INVALID_ARGUMENT", "args must have at most 256 entries")
	}
	env, e := environment(a)
	if e != nil {
		return nil, e
	}
	if raw, ok := a["env"].(map[string]any); ok && len(raw) > 64 {
		return nil, core.Error("INVALID_ARGUMENT", "env must have at most 64 properties")
	}
	cols, e := integer(a, "cols", 120, 20, 500)
	if e != nil {
		return nil, e
	}
	rows, e := integer(a, "rows", 30, 5, 200)
	if e != nil {
		return nil, e
	}
	idle, e := integer(a, "idle_timeout_ms", m.idleMS, 1000, 3600000)
	if e != nil {
		return nil, e
	}
	idle = min(idle, m.idleMS)
	term := core.String(a, "term", "xterm-256color")
	if term != "xterm-256color" && term != "xterm" && term != "vt100" && term != "dumb" {
		return nil, core.Error("INVALID_ARGUMENT", "Invalid terminal type")
	}
	cwd := m.cfg.Path(core.String(a, "cwd", m.cfg.Home))
	if raw, ok := a["env"].(map[string]any); !ok || raw["TERM"] == nil {
		env = append(env, "TERM="+term)
	}
	if !strings.ContainsRune(command, filepath.Separator) {
		pathValue := ""
		for _, kv := range env {
			if strings.HasPrefix(kv, "PATH=") {
				pathValue = strings.TrimPrefix(kv, "PATH=")
			}
		}
		resolved := ""
		for _, dir := range filepath.SplitList(pathValue) {
			candidate := filepath.Join(dir, command)
			if !filepath.IsAbs(candidate) {
				candidate = filepath.Join(cwd, candidate)
			}
			if i, err := os.Stat(candidate); err == nil && i.Mode().IsRegular() && i.Mode().Perm()&0111 != 0 {
				resolved = candidate
				break
			}
		}
		if resolved == "" {
			return nil, core.Error("PTY_COMMAND_NOT_FOUND", "Executable not found in child PATH: "+command)
		}
		command = resolved
	}
	cmd := exec.Command(command, argv...)
	cmd.Dir = cwd
	cmd.Env = env
	m.mu.Lock()
	defer m.mu.Unlock()
	select {
	case <-m.closed:
		return nil, core.Error("PTY_UNAVAILABLE", "Bridge is closing")
	default:
	}
	if len(m.sessions) >= m.maxSessions {
		var oldest *session
		for _, candidate := range m.sessions {
			candidate.mu.Lock()
			eligible := candidate.exited && candidate.readerDone
			candidate.mu.Unlock()
			if eligible && (oldest == nil || candidate.created.Before(oldest.created)) {
				oldest = candidate
			}
		}
		if oldest == nil {
			return nil, core.Error("PTY_SESSION_LIMIT", "PTY session capacity reached; close a session before starting another")
		}
		m.closeSession(oldest, "evicted", true, 0)
		delete(m.sessions, oldest.id)
	}
	master, slave, e := pty.Open()
	if e != nil {
		return nil, e
	}
	defer slave.Close()
	fail := func(err error) (any, error) { master.Close(); return nil, err }
	if e = pty.Setsize(master, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}); e != nil {
		return fail(e)
	}
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if e = cmd.Start(); e != nil {
		return fail(e)
	}
	_ = syscall.SetNonblock(int(master.Fd()), true)
	id := "pty-" + core.ID()
	now := time.Now()
	s := &session{id: id, label: core.String(a, "label", filepath.Base(command)), command: cmd.Path, cwd: cwd, term: term, pts: slave.Name(), argv: argv, master: master, cmd: cmd, cols: cols, rows: rows, idleMS: idle, created: now, lastActivity: now, ring: byteRing{data: make([]byte, m.ringBytes)}, notify: make(chan struct{}), tracked: map[int]string{}, metadataPath: filepath.Join(m.cfg.DataDir, "jobs", id+".json")}
	s.processStart, e = processStartIdentity(cmd.Process.Pid)
	if e != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		return fail(e)
	}
	meta := Job{ProcessStart: s.processStart, Pts: s.pts, TtyProcesses: s.tracked, ID: id, Kind: "pty", Label: s.label, PID: cmd.Process.Pid, ProcessGroupID: cmd.Process.Pid, Command: command, StartedAt: now.UTC().Format(time.RFC3339Nano), CWD: cwd, Status: "running", Running: true, MetadataPath: s.metadataPath}
	if e = core.AtomicJSON(s.metadataPath, meta); e != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		return fail(e)
	}
	m.sessions[id] = s
	go func() {
		buf := make([]byte, 32768)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				s.mu.Lock()
				s.ring.append(buf[:n])
				s.wake()
				s.mu.Unlock()
			}
			if errors.Is(err, syscall.EAGAIN) {
				time.Sleep(5 * time.Millisecond)
				continue
			}
			if err != nil {
				break
			}
		}
		s.mu.Lock()
		s.readerDone = true
		s.wake()
		s.mu.Unlock()
	}()
	go func() {
		_ = cmd.Wait()
		s.mu.Lock()
		s.exited = true
		s.exitCode, s.exitSignal = exitFields(cmd.ProcessState)
		s.wake()
		s.mu.Unlock()
	}()
	go snapshotTTY(s, true)
	return map[string]any{"sessionId": id, "leaderPid": cmd.Process.Pid, "helperPid": os.Getpid(), "pts": s.pts, "cols": cols, "rows": rows, "term": term, "cwd": cwd, "command": cmd.Path, "args": argv, "idleTimeoutMs": idle, "startedAt": now.UTC().Format(time.RFC3339Nano), "cursor": 0}, nil
}

var ansiPattern = regexp.MustCompile("\x1b(?:\\[[0-?]*[ -/]*[@-~]|\\][^\x07]*(?:\x07|\x1b\\\\)|[@-_])")

func renderPTY(data []byte, strip, collapse bool) string {
	s := string(bytes.ToValidUTF8(data, []byte("�")))
	if strip {
		s = ansiPattern.ReplaceAllString(s, "")
	}
	if collapse {
		s = strings.ReplaceAll(s, "\r\n", "\n")
		lines := strings.Split(s, "\n")
		for i, line := range lines {
			parts := strings.Split(line, "\r")
			lines[i] = parts[len(parts)-1]
		}
		s = strings.Join(lines, "\n")
	}
	return s
}
func (m *Manager) ptyCall(ctx context.Context, name string, a map[string]any) (any, error) {
	if name == "pty_start" {
		return m.startPTY(a)
	}
	s, e := m.getSession(a)
	if e != nil {
		return nil, e
	}
	if name == "pty_close" {
		grace, e := integer(a, "grace_ms", 2000, 0, 10000)
		if e != nil {
			return nil, e
		}
		force := core.Bool(a, "force", false)
		reason := "closed"
		if force {
			reason = "force_closed"
		}
		return m.closeSession(s, reason, force, time.Duration(grace)*time.Millisecond), nil
	}
	if name == "pty_read" {
		cursor, e := integer(a, "cursor", 0, 0, int(^uint(0)>>1))
		if e != nil {
			return nil, e
		}
		limit, e := integer(a, "max_bytes", 65536, 1024, 1000000)
		if e != nil {
			return nil, e
		}
		wait, e := integer(a, "wait_ms", 0, 0, 30000)
		if e != nil {
			return nil, e
		}
		snapshotTTY(s, false)
		s.mu.Lock()
		if int64(cursor) > s.ring.total {
			s.mu.Unlock()
			return nil, core.Error("PTY_CURSOR_FUTURE", "cursor exceeds total output")
		}
		if wait > 0 && s.ring.total <= int64(cursor) && !(s.exited && s.readerDone) {
			notify := s.notify
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-notify:
			case <-time.After(time.Duration(wait) * time.Millisecond):
			}
			s.mu.Lock()
		}
		data, next, lost, truncated := s.ring.slice(int64(cursor), limit)
		idle := time.Since(s.lastActivity).Milliseconds()
		s.lastActivity = time.Now()
		r := map[string]any{"sessionId": s.id, "cursor": cursor, "nextCursor": next, "text": renderPTY(data, core.Bool(a, "strip_ansi", true), core.Bool(a, "collapse_carriage_returns", true)), "lostBytes": lost, "truncated": truncated, "totalBytes": s.ring.total, "retainedBytes": min(s.ring.total, int64(len(s.ring.data))), "exited": s.exited && s.readerDone, "exitCode": s.exitCode, "exitSignal": s.exitSignal, "closeReason": s.closeReason, "cols": s.cols, "rows": s.rows, "idleMs": idle}
		s.mu.Unlock()
		return r, nil
	}
	snapshotTTY(s, name == "pty_write")
	s.mu.Lock()
	if s.exited {
		s.mu.Unlock()
		return nil, core.Error("PTY_EXITED", "PTY session has exited")
	}
	s.lastActivity = time.Now()
	s.mu.Unlock()
	switch name {
	case "pty_write":
		data, ok := a["data"].(string)
		if !ok {
			return nil, core.Error("INVALID_ARGUMENT", "data must be a string")
		}
		payload := []byte(data)
		if len(payload) > 65536 {
			return nil, core.Error("PTY_WRITE_TOO_LARGE", "data exceeds 65536 bytes")
		}
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		pending := s.pending
		longest := pending
		for _, b := range payload {
			switch b {
			case '\r', '\n', 3, 4, 21:
				pending = 0
			case 8, 127:
				pending = max(0, pending-1)
			default:
				pending++
				longest = max(longest, pending)
			}
		}
		var canonicalMode any
		if longest >= 1024 {
			canon, e := canonical(s.master)
			if e != nil {
				return nil, core.Error("PTY_WRITE_MODE_UNKNOWN", e.Error())
			}
			canonicalMode = canon
			if canon {
				return nil, core.Error("PTY_WRITE_CANON_LIMIT", "Canonical input lines must contain at most 1023 bytes")
			}
			pending = 0
		}
		written := 0
		initialPending := s.pending
		defer func() {
			if canonicalMode == false {
				s.pending = 0
				return
			}
			accepted := initialPending
			for _, b := range payload[:written] {
				switch b {
				case '\r', '\n', 3, 4, 21:
					accepted = 0
				case 8, 127:
					accepted = max(0, accepted-1)
				default:
					accepted++
				}
			}
			s.pending = accepted
		}()
		deadline := time.Now().Add(2 * time.Second)
		for written < len(payload) {
			n, e := syscall.Write(int(s.master.Fd()), payload[written:])
			if n > 0 {
				written += n
			}
			if e == syscall.EAGAIN {
				if time.Now().After(deadline) {
					return nil, core.Error("PTY_WRITE_BLOCKED", fmt.Sprintf("Terminal accepted %d of %d bytes; input is blocked", written, len(payload)))
				}
				if e = waitDuration(ctx, 5*time.Millisecond); e != nil {
					return nil, e
				}
				continue
			}
			if e != nil {
				return nil, e
			}
			if n == 0 {
				return nil, io.ErrShortWrite
			}
		}
		return map[string]any{"sessionId": s.id, "bytesWritten": written, "canonicalMode": canonicalMode, "pendingLineBytes": longest}, nil
	case "pty_resize":
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		cols, e := integer(a, "cols", 0, 20, 500)
		if e != nil {
			return nil, e
		}
		rows, e := integer(a, "rows", 0, 5, 200)
		if e != nil {
			return nil, e
		}
		if e = pty.Setsize(s.master, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}); e != nil {
			return nil, e
		}
		size, e := pty.GetsizeFull(s.master)
		if e != nil {
			return nil, e
		}
		if int(size.Cols) != cols || int(size.Rows) != rows {
			return nil, core.Error("PTY_RESIZE_UNCONFIRMED", "Kernel terminal size differs from requested size")
		}
		s.mu.Lock()
		s.cols = cols
		s.rows = rows
		s.mu.Unlock()
		return map[string]any{"sessionId": s.id, "ok": true, "cols": cols, "rows": rows}, nil
	case "pty_signal":
		name, e := core.Required(a, "signal")
		if e != nil {
			return nil, e
		}
		if !strings.Contains("|INT|TERM|KILL|HUP|QUIT|USR1|USR2|WINCH|TSTP|CONT|", "|"+name+"|") {
			return nil, core.Error("PTY_SIGNAL_NOT_ALLOWED", "Unsupported PTY signal")
		}
		sig, e := parseSignal(name)
		if e != nil {
			return nil, e
		}
		if e = syscall.Kill(-s.cmd.Process.Pid, sig); e != nil {
			return nil, core.Error("PTY_SIGNAL_UNCONFIRMED", e.Error())
		}
		return map[string]any{"sessionId": s.id, "delivered": true, "signal": name, "targetProcessGroup": s.cmd.Process.Pid, "note": "Delivered to the whole process group. Use pty_write with Ctrl-C to interrupt only the foreground program."}, nil
	}
	return nil, core.Error("UNKNOWN_TOOL", name)
}

type processIdentity struct{ tty, start string }

func processTable() map[int]processIdentity {
	cmd := exec.Command("ps", "-axo", "pid=,tty=,lstart=")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	data, e := cmd.Output()
	result := map[int]processIdentity{}
	if e != nil {
		return result
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 7 {
			continue
		}
		pid, e := strconv.Atoi(f[0])
		if e == nil {
			rest := strings.TrimSpace(strings.TrimSpace(line)[len(f[0]):])
			rest = strings.TrimSpace(rest[len(f[1]):])
			result[pid] = processIdentity{f[1], rest}
		}
	}
	return result
}
func snapshotTTY(s *session, force bool) {
	s.mu.Lock()
	if s.closeResult != nil || (!force && time.Since(s.lastScan) < 500*time.Millisecond) {
		s.mu.Unlock()
		return
	}
	s.lastScan = time.Now()
	tty := strings.TrimPrefix(s.pts, "/dev/")
	s.mu.Unlock()
	table := processTable()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closeResult != nil {
		return
	}
	for pid, p := range table {
		if p.tty == tty {
			s.tracked[pid] = p.start
		}
	}
	meta := Job{ID: s.id, Kind: "pty", Label: s.label, PID: s.cmd.Process.Pid, ProcessGroupID: s.cmd.Process.Pid, ProcessStart: s.processStart, Pts: s.pts, TtyProcesses: s.tracked, Command: s.command, CWD: s.cwd, StartedAt: s.created.UTC().Format(time.RFC3339Nano), Status: "running", Running: !s.exited, ExitCode: s.exitCode, Signal: s.exitSignal, MetadataPath: s.metadataPath}
	if s.exited {
		meta.Status = "exited"
	}
	_ = core.AtomicJSON(s.metadataPath, meta)
}
func (m *Manager) closeSession(s *session, reason string, force bool, grace time.Duration) map[string]any {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	s.mu.Lock()
	if s.closeResult != nil {
		r := s.closeResult
		s.mu.Unlock()
		return r
	}
	s.mu.Unlock()
	snapshotTTY(s, true)
	pid := s.cmd.Process.Pid
	if !force {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		deadline := time.Now().Add(grace)
		for groupAlive(pid) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
	}
	killed := syscall.Kill(-pid, syscall.SIGKILL) == nil
	s.mu.Lock()
	tracked := make(map[int]string, len(s.tracked))
	for p, start := range s.tracked {
		tracked[p] = start
	}
	s.mu.Unlock()
	table := processTable()
	ttyKilled := []int{}
	recycled := []int{}
	for p, start := range tracked {
		if p <= 1 || p == os.Getpid() {
			continue
		}
		if live, ok := table[p]; ok {
			if live.start != start {
				recycled = append(recycled, p)
			} else if syscall.Kill(p, syscall.SIGKILL) == nil {
				ttyKilled = append(ttyKilled, p)
			}
		}
	}
	s.writeMu.Lock()
	_ = s.master.Close()
	s.writeMu.Unlock()
	deadline := time.Now().Add(time.Second)
	for groupAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	table = processTable()
	survivors := []int{}
	for p, start := range tracked {
		if live, ok := table[p]; ok && live.start == start {
			survivors = append(survivors, p)
		}
	}
	gone := !groupAlive(pid)
	s.mu.Lock()
	s.closeReason = reason
	r := map[string]any{"sessionId": s.id, "reason": reason, "leaderGroupKilled": killed, "leaderGroupError": nil, "helperKilled": false, "leaderGroupGone": gone, "ttyProcessesKilled": ttyKilled, "ttyRecycledSkipped": recycled, "containmentVerified": gone && len(survivors) == 0, "uncontainedPids": survivors, "exitCode": s.exitCode, "exitSignal": s.exitSignal, "totalBytes": s.ring.total}
	s.closeResult = r
	s.wake()
	s.mu.Unlock()
	_ = os.Remove(s.metadataPath)
	return r
}
func (m *Manager) reap() {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-m.closed:
			return
		case <-ticker.C:
			m.mu.Lock()
			ss := make([]*session, 0, len(m.sessions))
			for _, s := range m.sessions {
				ss = append(ss, s)
			}
			m.mu.Unlock()
			for _, s := range ss {
				s.mu.Lock()
				idle := time.Since(s.lastActivity) > time.Duration(s.idleMS)*time.Millisecond
				expired := time.Since(s.created) > time.Duration(m.lifeMS)*time.Millisecond
				closed := s.closeResult != nil
				revoked := false
				if m.unlockWatched {
					b, err := os.ReadFile(m.cfg.UnlockFile)
					revoked = err != nil || strings.TrimSpace(string(b)) != "I_UNDERSTAND_THIS_GRANTS_FULL_ACCESS"
				}
				ended := s.exited && s.readerDone
				s.mu.Unlock()
				if !closed && (idle || expired || revoked) {
					reason := "idle_timeout"
					if expired {
						reason = "max_lifetime"
					}
					if revoked {
						reason = "unlock_revoked"
					}
					m.closeSession(s, reason, true, 0)
				} else if !closed && !ended {
					snapshotTTY(s, false)
				}
			}
		}
	}
}
