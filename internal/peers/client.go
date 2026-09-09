package peers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	"macbridge/internal/core"
)

const maxMessage = 18 * 1024 * 1024

type rpcError struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("RPC %d: %s", e.Code, e.Message) }

type reply struct {
	result json.RawMessage
	err    error
}
type rpcClient struct {
	cmd                   *exec.Cmd
	stdin, stdout, stderr *os.File
	writeMu               sync.Mutex
	mu                    sync.Mutex
	pending               map[string]chan reply
	next                  uint64
	done                  chan struct{}
	readerDone            chan struct{}
	roots                 []map[string]any
	log                   []byte
	lastActivity          time.Time
	exitErr               error
	closeOnce             sync.Once
}

func launch(command string, args, env []string, cwd string, roots []map[string]any) (*rpcClient, error) {
	c := &rpcClient{pending: map[string]chan reply{}, done: make(chan struct{}), readerDone: make(chan struct{}), roots: roots, lastActivity: time.Now()}
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, err
	}
	c.stdin, c.stdout, c.stderr = inW, outR, errR
	cmd := exec.Command(command, args...)
	cmd.Env = env
	cmd.Dir = cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin = inR
	cmd.Stdout = outW
	cmd.Stderr = errW
	c.cmd = cmd
	err = cmd.Start()
	inR.Close()
	outW.Close()
	errW.Close()
	if err != nil {
		inW.Close()
		outR.Close()
		errR.Close()
		return nil, err
	}
	go c.readLoop()
	go func() {
		b := make([]byte, 4096)
		for {
			n, e := errR.Read(b)
			if n > 0 {
				c.mu.Lock()
				c.log = append(c.log, b[:n]...)
				if len(c.log) > 200_000 {
					c.log = c.log[len(c.log)-200_000:]
				}
				c.mu.Unlock()
			}
			if e != nil {
				return
			}
		}
	}()
	go func() {
		err := cmd.Wait()
		select {
		case <-c.readerDone:
		case <-time.After(500 * time.Millisecond):
			outR.Close()
			<-c.readerDone
		}
		c.mu.Lock()
		c.exitErr = err
		c.mu.Unlock()
		close(c.done)
		c.failPending(core.Error("PROVIDER_GONE", fmt.Sprintf("Provider process exited: %v", err)))
		inW.Close()
		errR.Close()
		outR.Close()
	}()
	return c, nil
}
func (c *rpcClient) send(v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	b = append(b, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.stdin.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, e = c.stdin.Write(b)
	return e
}
func (c *rpcClient) notify(method string, params any) error {
	return c.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}
func (c *rpcClient) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.next++
	id := c.next
	key := strconv.FormatUint(id, 10)
	ch := make(chan reply, 1)
	c.pending[key] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, key); c.mu.Unlock() }()
	if e := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); e != nil {
		return nil, core.Error("PROVIDER_GONE", e.Error())
	}
	select {
	case r := <-ch:
		return r.result, r.err
	case <-ctx.Done():
		_ = c.notify("notifications/cancelled", map[string]any{"requestId": id, "reason": ctx.Err().Error()})
		return nil, core.Error("PROVIDER_TIMEOUT", fmt.Sprintf("%s: %s", method, ctx.Err()))
	case <-c.done:
		select {
		case r := <-ch:
			return r.result, r.err
		default:
			return nil, core.Error("PROVIDER_GONE", "Provider exited before responding")
		}
	}
}
func (c *rpcClient) failPending(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, ch := range c.pending {
		select {
		case ch <- reply{err: err}:
		default:
		}
		delete(c.pending, id)
	}
}
func (c *rpcClient) readLoop() {
	defer close(c.readerDone)
	r := bufio.NewReaderSize(c.stdout, 64*1024)
	var line []byte
	discard := false
	for {
		part, e := r.ReadSlice('\n')
		if !discard {
			if len(line)+len(part) > maxMessage {
				discard = true
				line = nil
				c.failPending(core.Error("RESULT_TOO_LARGE", "Provider response exceeded the framing limit; request a smaller result"))
			} else {
				line = append(line, part...)
			}
		}
		if e == bufio.ErrBufferFull {
			continue
		}
		if !discard && len(bytes.TrimSpace(line)) > 0 {
			c.handle(line)
		}
		line = nil
		discard = false
		if e != nil {
			return
		}
	}
}
func (c *rpcClient) handle(line []byte) {
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if json.Unmarshal(line, &msg) != nil {
		return
	}
	c.mu.Lock()
	c.lastActivity = time.Now()
	c.mu.Unlock()
	if msg.Method != "" {
		if len(msg.ID) == 0 {
			return
		}
		var result any
		var failure *rpcError
		switch msg.Method {
		case "roots/list":
			result = map[string]any{"roots": c.roots}
		case "ping":
			result = map[string]any{}
		case "sampling/createMessage", "elicitation/create":
			failure = &rpcError{Code: -32601, Message: "This gateway cannot supply interactive input; provide the required arguments up front"}
		default:
			failure = &rpcError{Code: -32601, Message: "Unsupported provider callback: " + msg.Method}
		}
		response := map[string]any{"jsonrpc": "2.0", "id": msg.ID}
		if failure != nil {
			response["error"] = failure
		} else {
			response["result"] = result
		}
		_ = c.send(response)
		return
	}
	c.mu.Lock()
	ch := c.pending[string(msg.ID)]
	delete(c.pending, string(msg.ID))
	c.mu.Unlock()
	if ch != nil {
		var err error
		if msg.Error != nil {
			err = msg.Error
		}
		ch <- reply{result: msg.Result, err: err}
	}
}
func (c *rpcClient) stop() {
	c.closeOnce.Do(func() {
		_ = c.stdin.Close()
		_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-c.done:
		case <-time.After(2 * time.Second):
			c.stdout.Close()
			c.stderr.Close()
		}
	})
}
func (c *rpcClient) details() (int, string, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending), string(c.log), c.lastActivity
}
func decode(raw json.RawMessage) (map[string]any, error) {
	var m map[string]any
	e := json.Unmarshal(raw, &m)
	if e == nil && m == nil {
		e = errors.New("Provider returned a non-object response")
	}
	return m, e
}
func requestFor(ctx context.Context, c *rpcClient, method string, params any, timeout time.Duration) (json.RawMessage, error) {
	next, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return c.request(next, method, params)
}

// Drain a probe while retaining only its first megabyte; stdout/stderr never
// share an unprotected bytes.Buffer because exec copies them concurrently.
type cappedWriter struct {
	mu sync.Mutex
	b  []byte
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	if left := 1_000_000 - len(w.b); left > 0 {
		if len(p) > left {
			p = p[:left]
		}
		w.b = append(w.b, p...)
	}
	return n, nil
}

var _ io.Writer = (*cappedWriter)(nil)
