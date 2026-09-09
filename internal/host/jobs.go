package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"macbridge/internal/core"
)

type Job struct {
	ID              string         `json:"id"`
	Label           string         `json:"label"`
	Kind            string         `json:"kind"`
	PID             int            `json:"pid"`
	ProcessGroupID  int            `json:"processGroupId"`
	ProcessStart    string         `json:"processStart,omitempty"`
	Pts             string         `json:"pts,omitempty"`
	TtyProcesses    map[int]string `json:"ttyProcesses,omitempty"`
	SupervisorPID   int            `json:"supervisorPid"`
	Command         string         `json:"command"`
	Shell           string         `json:"shell"`
	CWD             string         `json:"cwd"`
	StartedAt       string         `json:"startedAt"`
	CompletedAt     any            `json:"completedAt"`
	Status          string         `json:"status"`
	Running         any            `json:"running"`
	ExitCode        any            `json:"exitCode"`
	Signal          any            `json:"signal"`
	RequestedSignal string         `json:"requestedSignal,omitempty"`
	LastSignal      string         `json:"lastSignal,omitempty"`
	Error           string         `json:"error,omitempty"`
	StdoutPath      string         `json:"stdoutPath"`
	StderrPath      string         `json:"stderrPath"`
	StdoutBytes     int64          `json:"stdoutBytes"`
	StderrBytes     int64          `json:"stderrBytes"`
	LogMaxBytes     int            `json:"logMaxBytes"`
	ControlSocket   string         `json:"controlSocketPath"`
	MetadataPath    string         `json:"metadataPath"`
	ShellExitedAt   string         `json:"shellExitedAt,omitempty"`
}
type jobStart struct {
	Job Job
	Env []string
}

var jobIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func terminal(j Job) bool {
	return j.Status == "succeeded" || j.Status == "failed" || j.Status == "cancelled"
}
func jobMap(j Job) map[string]any {
	b, _ := json.Marshal(j)
	r := map[string]any{}
	_ = json.Unmarshal(b, &r)
	return r
}
func readJob(p string) (Job, error) {
	var j Job
	b, e := os.ReadFile(p)
	if e == nil {
		e = json.Unmarshal(b, &j)
	}
	return j, e
}
func (m *Manager) startJob(ctx context.Context, a map[string]any) (any, error) {
	command, e := core.Required(a, "command")
	if e != nil {
		return nil, e
	}
	env, e := environment(a)
	if e != nil {
		return nil, e
	}
	limit, e := integer(a, "max_log_bytes", m.logBytes, 1024, 64000000)
	if e != nil {
		return nil, e
	}
	id := fmt.Sprintf("%d-%s", time.Now().UnixMilli(), core.ID()[:10])
	dir := filepath.Join(m.cfg.DataDir, "jobs")
	j := Job{ID: id, Label: core.String(a, "label", "background-job"), Kind: "shell", Command: command, Shell: m.cfg.Shell, CWD: m.cfg.Path(core.String(a, "cwd", m.cfg.Home)), StartedAt: core.Now(), Status: "starting", Running: true, LogMaxBytes: limit, MetadataPath: filepath.Join(dir, id+".json"), StdoutPath: filepath.Join(dir, id+".stdout.log"), StderrPath: filepath.Join(dir, id+".stderr.log")}
	f, e := os.CreateTemp(dir, ".jobstart-*")
	if e != nil {
		return nil, e
	}
	startPath := f.Name()
	if e = f.Chmod(0600); e == nil {
		e = json.NewEncoder(f).Encode(jobStart{j, env})
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		os.Remove(startPath)
		return nil, e
	}
	exe, e := os.Executable()
	if e != nil {
		os.Remove(startPath)
		return nil, e
	}
	cmd := exec.Command(exe, "supervise-job", startPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if e = cmd.Start(); e != nil {
		os.Remove(startPath)
		return nil, e
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			current, err := readJob(j.MetadataPath)
			if err == nil {
				if current.Error != "" && current.PID == 0 {
					return nil, errors.New(current.Error)
				}
				if current.PID > 0 {
					return jobMap(current), nil
				}
			}
		case err := <-exited:
			current, re := readJob(j.MetadataPath)
			if re == nil && current.PID > 0 {
				return jobMap(current), nil
			}
			os.Remove(startPath)
			if re == nil && current.Error != "" {
				return nil, errors.New(current.Error)
			}
			return nil, fmt.Errorf("Job supervisor failed: %v", err)
		case <-deadline.C:
			_ = cmd.Process.Signal(syscall.SIGTERM)
			return nil, errors.New("Job supervisor startup timed out")
		case <-ctx.Done():
			_ = cmd.Process.Signal(syscall.SIGTERM)
			return nil, ctx.Err()
		}
	}
}
func jobControl(ctx context.Context, j Job, request map[string]any) (map[string]any, error) {
	if j.ControlSocket == "" {
		return nil, os.ErrNotExist
	}
	conn, e := (&net.Dialer{}).DialContext(ctx, "unix", j.ControlSocket)
	if e != nil {
		return nil, e
	}
	defer conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	request["jobId"] = j.ID
	if e = json.NewEncoder(conn).Encode(request); e != nil {
		return nil, e
	}
	var response map[string]any
	e = json.NewDecoder(io.LimitReader(conn, 100000000)).Decode(&response)
	if e == nil && response["error"] != nil {
		return nil, fmt.Errorf("%v", response["error"])
	}
	return response, e
}
func inspectJob(ctx context.Context, j Job, limit int) (map[string]any, error) {
	if !terminal(j) {
		req := map[string]any{"operation": "status"}
		if limit > 0 {
			req["maxLogBytes"] = limit
		}
		if result, e := jobControl(ctx, j, req); e == nil {
			return result, nil
		}
		if latest, e := readJob(j.MetadataPath); e == nil {
			j = latest
		}
		if !terminal(j) {
			j.Status = "unknown"
			j.Running = nil
			j.Error = "Supervisor unavailable; completion could not be verified"
		}
	}
	r := jobMap(j)
	if limit > 0 {
		var e error
		r["stdout"], e = rotatedTail(j.StdoutPath, j.StdoutBytes, limit)
		if e != nil {
			return nil, e
		}
		r["stderr"], e = rotatedTail(j.StderrPath, j.StderrBytes, limit)
		if e != nil {
			return nil, e
		}
	}
	return r, nil
}
func (m *Manager) jobCall(ctx context.Context, name string, a map[string]any) (any, error) {
	dir := filepath.Join(m.cfg.DataDir, "jobs")
	if name == "shell_job_list" {
		entries, e := os.ReadDir(dir)
		if e != nil {
			return nil, e
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() > entries[j].Name() })
		results := []any{}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			if len(results) >= 1000 {
				break
			}
			j, e := readJob(filepath.Join(dir, entry.Name()))
			if e != nil {
				results = append(results, map[string]any{"metadataFile": entry.Name(), "error": e.Error()})
				continue
			}
			if j.Kind == "pty" {
				results = append(results, jobMap(m.ptyJobState(j)))
				continue
			}
			r, e := inspectJob(ctx, j, 0)
			if e != nil {
				results = append(results, map[string]any{"metadataFile": entry.Name(), "error": e.Error()})
			} else {
				results = append(results, r)
			}
		}
		return map[string]any{"jobs": results}, nil
	}
	id, e := core.Required(a, "job_id")
	if e != nil {
		return nil, e
	}
	if !jobIDPattern.MatchString(id) {
		return nil, core.Error("INVALID_ARGUMENT", "Invalid job_id")
	}
	j, e := readJob(filepath.Join(dir, id+".json"))
	if e != nil {
		return nil, e
	}
	if name == "shell_job_status" {
		limit, e := integer(a, "max_log_bytes", 100000, 1024, 8000000)
		if e != nil {
			return nil, e
		}
		if j.Kind == "pty" {
			j = m.ptyJobState(j)
			r := jobMap(j)
			empty := map[string]any{"text": "", "size": 0, "returnedBytes": 0, "truncated": false}
			r["stdout"] = empty
			r["stderr"] = empty
			return r, nil
		}
		return inspectJob(ctx, j, limit)
	}
	sig := core.String(a, "signal", "SIGTERM")
	if j.Kind == "pty" {
		parsed, e := parseSignal(sig)
		if e != nil {
			return nil, e
		}
		m.mu.Lock()
		s := m.sessions[id]
		m.mu.Unlock()
		killed := false
		running := false
		if s != nil {
			s.mu.Lock()
			running = !s.exited
			s.mu.Unlock()
			if running {
				killed = syscall.Kill(-s.cmd.Process.Pid, parsed) == nil
			}
		}
		return map[string]any{"jobId": id, "pid": j.PID, "signal": sig, "killed": killed, "running": running}, nil
	}
	if _, e = parseSignal(sig); e != nil {
		return nil, e
	}
	if !terminal(j) {
		if r, e := jobControl(ctx, j, map[string]any{"operation": "signal", "signal": sig}); e == nil {
			return r, nil
		}
	}
	r, e := inspectJob(ctx, j, 0)
	if e != nil {
		return nil, e
	}
	return map[string]any{"jobId": id, "pid": j.PID, "signal": sig, "killed": false, "running": r["running"], "status": r["status"]}, nil
}

type rotatingWriter struct {
	mu            sync.Mutex
	f             *os.File
	path          string
	size, segment int
	total         int64
}

func newRotating(p string, limit int) (*rotatingWriter, error) {
	f, e := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	return &rotatingWriter{f: f, path: p, segment: limit / 2}, e
}
func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	written := 0
	for len(p) > 0 {
		if w.size == w.segment {
			if e := w.f.Close(); e != nil {
				return written, e
			}
			if e := os.Remove(w.path + ".1"); e != nil && !os.IsNotExist(e) {
				return written, e
			}
			if e := os.Rename(w.path, w.path+".1"); e != nil {
				return written, e
			}
			f, e := os.OpenFile(w.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
			if e != nil {
				return written, e
			}
			w.f = f
			w.size = 0
		}
		n, e := w.f.Write(p[:min(len(p), w.segment-w.size)])
		written += n
		w.total += int64(n)
		w.size += n
		p = p[n:]
		if e != nil {
			return written, e
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}
func (w *rotatingWriter) count() int64 { w.mu.Lock(); defer w.mu.Unlock(); return w.total }
func (w *rotatingWriter) tail(limit int) (map[string]any, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return rotatedTail(w.path, w.total, limit)
}
func (w *rotatingWriter) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f != nil {
		w.f.Close()
	}
}
func readTail(p string, limit int) ([]byte, int64, error) {
	if p == "" {
		return nil, 0, nil
	}
	f, e := os.Open(p)
	if os.IsNotExist(e) {
		return nil, 0, nil
	}
	if e != nil {
		return nil, 0, e
	}
	defer f.Close()
	i, e := f.Stat()
	if e != nil {
		return nil, 0, e
	}
	data := make([]byte, min(int64(limit), i.Size()))
	n, e := f.ReadAt(data, i.Size()-int64(len(data)))
	if e == io.EOF {
		e = nil
	}
	return data[:n], i.Size(), e
}
func rotatedTail(p string, total int64, limit int) (map[string]any, error) {
	current, size, e := readTail(p, limit)
	if e != nil {
		return nil, e
	}
	previous, oldSize, e := readTail(p+".1", limit-len(current))
	if e != nil {
		return nil, e
	}
	data := append(previous, current...)
	total = max(total, size+oldSize)
	return map[string]any{"text": string(data), "size": size + oldSize, "returnedBytes": len(data), "truncated": total > int64(len(data)), "totalBytes": total}, nil
}

func SuperviseJob(configPath string) error {
	data, e := os.ReadFile(configPath)
	if e != nil {
		return e
	}
	var start jobStart
	e = json.Unmarshal(data, &start)
	_ = os.Remove(configPath)
	if e != nil {
		return e
	}
	j := start.Job
	j.SupervisorPID = os.Getpid()
	var mu sync.Mutex
	var out, errout *rotatingWriter
	persist := func() error {
		if out != nil {
			j.StdoutBytes = out.count()
		}
		if errout != nil {
			j.StderrBytes = errout.count()
		}
		return core.AtomicJSON(j.MetadataPath, j)
	}
	fail := func(err error) error {
		j.Status = "failed"
		j.Running = false
		j.Error = err.Error()
		j.CompletedAt = core.Now()
		_ = persist()
		return err
	}
	if j.LogMaxBytes < 1024 || j.LogMaxBytes > 64000000 {
		return fail(errors.New("Invalid log limit"))
	}
	out, e = newRotating(j.StdoutPath, j.LogMaxBytes)
	if e != nil {
		return fail(e)
	}
	defer out.close()
	errout, e = newRotating(j.StderrPath, j.LogMaxBytes)
	if e != nil {
		return fail(e)
	}
	defer errout.close()
	tmpRoot := os.TempDir()
	if len(tmpRoot) > 65 {
		tmpRoot = "/tmp"
	}
	controlDir, e := os.MkdirTemp(tmpRoot, "mdb-job-")
	if e != nil {
		return fail(e)
	}
	defer os.RemoveAll(controlDir)
	j.ControlSocket = filepath.Join(controlDir, "control.sock")
	listener, e := net.Listen("unix", j.ControlSocket)
	if e != nil {
		return fail(e)
	}
	defer listener.Close()
	cmd := exec.Command(j.Shell, "-lc", j.Command)
	cmd.Dir = j.CWD
	cmd.Env = start.Env
	cmd.Stdout = out
	cmd.Stderr = errout
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if e = cmd.Start(); e != nil {
		return fail(e)
	}
	j.PID = cmd.Process.Pid
	j.ProcessGroupID = j.PID
	j.ProcessStart, e = processStartIdentity(j.PID)
	if e != nil {
		_ = syscall.Kill(-j.PID, syscall.SIGKILL)
		_ = cmd.Wait()
		return fail(e)
	}
	j.Status = "running"
	j.Running = true
	if e = persist(); e != nil {
		_ = syscall.Kill(-j.PID, syscall.SIGKILL)
		_ = cmd.Wait()
		return e
	}
	done := make(chan struct{})
	defer close(done)
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(signals)
	go func() {
		for {
			select {
			case s := <-signals:
				mu.Lock()
				j.LastSignal = signalName(s.(syscall.Signal))
				j.RequestedSignal = j.LastSignal
				_ = syscall.Kill(-j.PID, s.(syscall.Signal))
				_ = persist()
				mu.Unlock()
				go func() {
					select {
					case <-done:
						return
					case <-time.After(2 * time.Second):
						mu.Lock()
						if !terminal(j) {
							j.LastSignal = "SIGKILL"
							_ = syscall.Kill(-j.PID, syscall.SIGKILL)
						}
						mu.Unlock()
					}
				}()
			case <-done:
				return
			}
		}
	}()
	go func() {
		for {
			conn, e := listener.Accept()
			if e != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				var req struct {
					JobID       string `json:"jobId"`
					Operation   string `json:"operation"`
					Signal      string `json:"signal"`
					MaxLogBytes int    `json:"maxLogBytes"`
				}
				if e := json.NewDecoder(io.LimitReader(conn, 65536)).Decode(&req); e != nil {
					return
				}
				mu.Lock()
				defer mu.Unlock()
				var result any
				if req.JobID != j.ID {
					result = map[string]any{"error": "Job identity mismatch"}
				} else if req.Operation == "signal" {
					sig, e := parseSignal(req.Signal)
					if e != nil {
						result = map[string]any{"error": e.Error()}
					} else {
						killed := false
						if !terminal(j) {
							e = syscall.Kill(-j.PID, sig)
							killed = e == nil
							if killed {
								j.LastSignal = signalName(sig)
								if sig == syscall.SIGTERM || sig == syscall.SIGINT || sig == syscall.SIGHUP || sig == syscall.SIGKILL || sig == syscall.SIGQUIT {
									j.RequestedSignal = j.LastSignal
								}
								_ = persist()
							}
						}
						result = map[string]any{"jobId": j.ID, "pid": j.PID, "signal": req.Signal, "killed": killed, "running": j.Running}
					}
				} else if req.Operation == "status" {
					j.StdoutBytes = out.count()
					j.StderrBytes = errout.count()
					r := jobMap(j)
					if req.MaxLogBytes > 0 && req.MaxLogBytes <= 8000000 {
						r["stdout"], _ = out.tail(req.MaxLogBytes)
						r["stderr"], _ = errout.tail(req.MaxLogBytes)
					}
					result = r
				} else {
					result = map[string]any{"error": "Unknown operation"}
				}
				_ = json.NewEncoder(conn).Encode(result)
			}()
		}
	}()
	waitErr := cmd.Wait()
	mu.Lock()
	j.ExitCode, j.Signal = exitFields(cmd.ProcessState)
	j.ShellExitedAt = core.Now()
	_ = persist()
	mu.Unlock()
	for groupAlive(cmd.Process.Pid) {
		time.Sleep(100 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	j.CompletedAt = core.Now()
	j.Running = false
	cancelled := j.RequestedSignal != ""
	if j.Signal != nil {
		cancelled = j.Signal == j.LastSignal
	}
	switch {
	case cancelled:
		j.Status = "cancelled"
	case waitErr != nil:
		j.Status = "failed"
	default:
		j.Status = "succeeded"
	}
	var ee *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &ee) {
		j.Error = waitErr.Error()
	}
	return persist()
}

func (m *Manager) ptyJobState(j Job) Job {
	m.mu.Lock()
	s := m.sessions[j.ID]
	m.mu.Unlock()
	if s == nil {
		j.Running = nil
		j.Status = "unknown"
		j.Error = "PTY sessions cannot be resumed after a bridge restart"
		return j
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	j.Running = !s.exited
	j.ExitCode = s.exitCode
	j.Signal = s.exitSignal
	if s.exited {
		j.Status = "failed"
		if s.exitCode == 0 {
			j.Status = "succeeded"
		}
		if s.closeReason != "" {
			j.Status = "cancelled"
		}
	}
	return j
}

func processStartIdentity(pid int) (string, error) {
	cmd := exec.Command("ps", "-p", fmt.Sprint(pid), "-o", "lstart=")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	b, e := cmd.Output()
	if e != nil {
		return "", e
	}
	identity := strings.TrimSpace(string(b))
	if identity == "" {
		return "", errors.New("Process start identity unavailable")
	}
	return identity, nil
}
