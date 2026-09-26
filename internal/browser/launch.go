package browser

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/brown-enterprises/be-code/internal/procattr"
)

// launchTimeout bounds how long a launched browser may take to open its
// debugging port. A var so a test can shorten it.
var launchTimeout = 20 * time.Second

// Process is a browser BE-Code launched and therefore owns: it is closed
// when the session ends, unlike a browser BE-Code only attached to.
type Process struct {
	Exe    string
	WSURL  string
	cmd    *exec.Cmd
	exited chan struct{}
	stderr *tailBuffer
}

func launchArgs(profile string, headless bool) []string {
	// Port 0: the browser picks a free port and writes it to
	// DevToolsActivePort, so a launch never collides with whatever is on
	// 9222 (spec §1.3, amended).
	args := []string{"--remote-debugging-port=0", "--user-data-dir=" + profile, "--no-first-run", "--no-default-browser-check"}
	if headless {
		args = append(args, "--headless=new")
	}
	return append(args, "about:blank")
}

// Launch starts exe on profile and waits for its debugging port. A browser
// that exits at once is almost always another instance holding the same
// profile, and the error says so rather than waiting out a timeout.
func Launch(ctx context.Context, exe, profile string, headless bool) (*Process, error) {
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return nil, err
	}
	portFile := filepath.Join(profile, "DevToolsActivePort")
	os.Remove(portFile) // a stale copy names a port nobody is on
	cmd := exec.Command(exe, launchArgs(profile, headless)...)
	procattr.Hide(cmd)
	tail := &tailBuffer{max: 4096}
	cmd.Stderr = tail
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("launching %s: %w", exe, err)
	}
	p := &Process{Exe: exe, cmd: cmd, exited: make(chan struct{}), stderr: tail}
	go func() {
		cmd.Wait()
		close(p.exited)
	}()
	deadline := time.NewTimer(launchTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		if b, err := os.ReadFile(portFile); err == nil {
			lines := strings.Split(strings.TrimSpace(string(b)), "\n")
			if len(lines) >= 2 {
				p.WSURL = "ws://127.0.0.1:" + strings.TrimSpace(lines[0]) + strings.TrimSpace(lines[1])
				return p, nil
			}
		}
		select {
		case <-p.exited:
			return nil, fmt.Errorf("%s exited at once (%s) — is another browser already using %s? close it, or set browser.address to attach to it%s",
				exe, cmd.ProcessState, profile, tail.suffix())
		case <-deadline.C:
			p.Kill()
			return nil, fmt.Errorf("%s did not open its debugging port within %s%s", exe, launchTimeout, tail.suffix())
		case <-ctx.Done():
			p.Kill()
			return nil, ctx.Err()
		case <-tick.C:
		}
	}
}

// Kill ends the browser and waits (briefly) for it to go.
func (p *Process) Kill() {
	if p == nil || p.cmd.Process == nil {
		return
	}
	select {
	case <-p.exited:
		return
	default:
	}
	p.cmd.Process.Kill()
	select {
	case <-p.exited:
	case <-time.After(3 * time.Second):
	}
}

// Exited is closed once the process has ended.
func (p *Process) Exited() <-chan struct{} { return p.exited }

// tailBuffer keeps the last max bytes written to it: a browser's stderr,
// quoted in a launch error.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) suffix() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := strings.TrimSpace(string(t.b))
	if s == "" {
		return ""
	}
	return "; its last output:\n" + s
}
