package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// ProcessManager tracks background processes the agent starts (dev servers,
// watchers). Output is kept in a bounded ring so logs stay inspectable
// without unbounded memory.
type ProcessManager struct {
	mu     sync.Mutex
	nextID int
	procs  map[int]*bgProc
}

type bgProc struct {
	id      int
	command string
	cmd     *exec.Cmd
	buf     *ringBuf
	started time.Time
	done    bool
	exitErr error
}

func NewProcessManager() *ProcessManager {
	return &ProcessManager{procs: map[int]*bgProc{}}
}

// ringBuf is a fixed-capacity byte ring safe for one writer + readers.
type ringBuf struct {
	mu  sync.Mutex
	buf []byte
	cap int
}

func (r *ringBuf) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.cap {
		r.buf = r.buf[len(r.buf)-r.cap:]
	}
	return len(p), nil
}

func (r *ringBuf) Tail(n int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || n > len(r.buf) {
		n = len(r.buf)
	}
	return string(r.buf[len(r.buf)-n:])
}

// Start launches a background command in dir.
func (m *ProcessManager) Start(dir, command string) (int, error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("powershell", "-NoProfile", "-Command", command)
	} else {
		cmd = exec.Command("sh", "-c", command)
	}
	cmd.Dir = dir
	buf := &ringBuf{cap: 64 * 1024}
	cmd.Stdout = buf
	cmd.Stderr = buf
	setProcessGroup(cmd) // so Stop reaches the server the shell spawned
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	m.mu.Lock()
	m.nextID++
	id := m.nextID
	p := &bgProc{id: id, command: command, cmd: cmd, buf: buf, started: time.Now()}
	m.procs[id] = p
	m.mu.Unlock()
	go func() {
		err := cmd.Wait()
		m.mu.Lock()
		p.done = true
		p.exitErr = err
		m.mu.Unlock()
	}()
	return id, nil
}

func (m *ProcessManager) get(id int) (*bgProc, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.procs[id]
	return p, ok
}

// List summarizes tracked processes.
func (m *ProcessManager) List() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.procs) == 0 {
		return "no background processes"
	}
	ids := make([]int, 0, len(m.procs))
	for id := range m.procs {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	var b strings.Builder
	for _, id := range ids {
		p := m.procs[id]
		state := "running"
		if p.done {
			state = "exited"
			if p.exitErr != nil {
				state = "exited (" + p.exitErr.Error() + ")"
			}
		}
		fmt.Fprintf(&b, "[%d] %s — %s, up %s\n", id, p.command, state, time.Since(p.started).Round(time.Second))
	}
	return strings.TrimRight(b.String(), "\n")
}

// Stop terminates a process.
func (m *ProcessManager) Stop(id int) error {
	p, ok := m.get(id)
	if !ok {
		return fmt.Errorf("no process %d", id)
	}
	if !p.done && p.cmd.Process != nil {
		_ = killProcessGroup(p.cmd)
	}
	m.mu.Lock()
	delete(m.procs, id)
	m.mu.Unlock()
	return nil
}

// StopAll kills everything (session teardown).
func (m *ProcessManager) StopAll() {
	m.mu.Lock()
	ids := make([]int, 0, len(m.procs))
	for id := range m.procs {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		_ = m.Stop(id)
	}
}

// ---- the tool --------------------------------------------------------------

type processTool struct{ r *Registry }

func (t *processTool) Name() string { return "process" }
func (t *processTool) Description() string {
	return "Manage background processes (dev servers, watchers). action=start runs a command in the background and returns an id; list shows all; logs returns recent output; stop kills one."
}
func (t *processTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
		"action":{"type":"string","enum":["start","list","logs","stop"],"description":"What to do"},
		"command":{"type":"string","description":"Command line (action=start)"},
		"id":{"type":"integer","description":"Process id (logs/stop)"},
		"tail":{"type":"integer","description":"Bytes of log tail (default 4096)"}},
		"required":["action"]}`)
}
func (t *processTool) Run(_ context.Context, args map[string]any) Result {
	switch argString(args, "action") {
	case "start":
		command := strings.TrimSpace(argString(args, "command", "cmd"))
		if command == "" {
			return Result{IsError: true, Content: "command is required for start"}
		}
		if classifyCommand(command, t.r.ShellAllow, t.r.ShellDeny) == cmdDenied {
			return Result{IsError: true, Content: "this command matches the deny list"}
		}
		if t.r.Approve != nil && classifyCommand(command, t.r.ShellAllow, t.r.ShellDeny) != cmdAllowed {
			if !t.r.Approve("shell", command+"  (background)") {
				return Result{IsError: true, Content: "user denied this background command"}
			}
		}
		id, err := t.r.procs.Start(t.r.Root, command)
		if err != nil {
			return Result{IsError: true, Content: err.Error()}
		}
		return Result{Content: fmt.Sprintf("started background process [%d]; check it with action=logs id=%d", id, id)}
	case "list":
		return Result{Content: t.r.procs.List()}
	case "logs":
		id := argInt(args, 0, "id")
		p, ok := t.r.procs.get(id)
		if !ok {
			return Result{IsError: true, Content: fmt.Sprintf("no process %d (use action=list)", id)}
		}
		tail := argInt(args, 4096, "tail")
		out := p.buf.Tail(tail)
		if strings.TrimSpace(out) == "" {
			out = "(no output yet)"
		}
		state := "running"
		if p.done {
			state = "exited"
		}
		return Result{Content: truncate(fmt.Sprintf("[%d] %s (%s)\n%s", id, p.command, state, out), t.r.MaxOutput)}
	case "stop":
		id := argInt(args, 0, "id")
		if err := t.r.procs.Stop(id); err != nil {
			return Result{IsError: true, Content: err.Error()}
		}
		return Result{Content: fmt.Sprintf("stopped process [%d]", id)}
	default:
		return Result{IsError: true, Content: "action must be one of start, list, logs, stop"}
	}
}
