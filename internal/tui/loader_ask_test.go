package tui

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/live"
)

// consentLoader stands in for the real loader at the only point that
// matters here: it needs consent before it can put a window on the wire,
// and it asks for it through the registry's approver, which under a session
// is the shared modal.
type consentLoader struct {
	ag     *agent.Agent
	window int
	asked  atomic.Int32
}

func (l *consentLoader) Apply(context.Context, string) (int, error) {
	approve := l.ag.Tools.Approve
	if approve == nil {
		return 0, nil // nobody to ask is a refusal, exactly as in the real loader
	}
	l.asked.Add(1)
	if !approve("model_reload", "model m is loaded with an 8192-token window; config asks for 32768.\nReloading evicts anything else on this server using that model.") {
		return 0, nil
	}
	return l.window, nil
}
func (*consentLoader) OnEvicted(context.Context, string) {}
func (*consentLoader) OnWindowChanged(string, int)       {}

// The whole point of ruling T8-a: a window mismatch found at startup has to
// become a question somewhere a person can answer it. Under a TUI the only
// such place is the shared approval modal — stderr is wiped by the alt
// screen, and in a hosted session it is a log file. This is the path
// runInteractive and runSessionHost take once they have wired
// Registry.Approve.
func TestALoaderPromptReachesTheSharedModal(t *testing.T) {
	tempHome(t)
	s := newTestSession(t)
	v := s.NewView(0, "local")
	v.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	flush(v)

	l := &consentLoader{ag: s.ag, window: 32768}
	s.ag.SetLoader(l)
	s.ag.ResolveModel()

	waitFor(t, func() bool { flush(v); return v.mode == modeAsk })
	if !strings.Contains(v.View(), "Reload the model on the server") {
		t.Fatalf("the modal does not say whose machine is being changed:\n%s", v.View())
	}
	v.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	waitFor(t, func() bool { return s.ag.Window() == 32768 })
	if v.mode == modeAsk {
		t.Fatal("the modal stayed open after it was answered")
	}
}

// The hosted case: the host raises the question before any terminal has
// attached, because the session is started detached. A terminal that
// attaches afterwards must be shown it, or the question sits unanswered
// with the host's goroutine parked on it forever.
func TestALoaderPromptRaisedBeforeAnyTerminalIsShownOnAttach(t *testing.T) {
	tempHome(t)
	s := newTestSession(t)
	s.served = true

	l := &consentLoader{ag: s.ag, window: 32768}
	s.ag.SetLoader(l)
	s.ag.ResolveModel()
	waitFor(t, func() bool { return l.asked.Load() == 1 })
	// Nothing is rendering yet; the ask is on the session, waiting.
	waitFor(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.current() != nil
	})

	v := s.NewView(1, "desk (pid 1)")
	v.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "desk (pid 1)", UTF8: true}})
	flush(v)
	if v.mode != modeAsk {
		t.Fatalf("a terminal attaching after the question was raised was not shown it: mode %v", v.mode)
	}
	v.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	// Denied: the loader keeps the server's window, so nothing lands.
	time.Sleep(20 * time.Millisecond)
	if s.ag.Window() != 0 {
		t.Fatalf("a denial still changed the window: %d", s.ag.Window())
	}
}

// A model switch is typed into the input line, which Bubble Tea's Update
// owns. It must return at once whatever the backend is doing, or the whole
// terminal freezes behind a model reload.
func TestModelSwitchDoesNotBlockTheEventLoop(t *testing.T) {
	tempHome(t)
	s := newTestSession(t)
	v := s.NewView(0, "local")
	v.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	flush(v)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	s.ag.SetLoader(blockingLoader{release: release})

	start := time.Now()
	v.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/model other")})
	v.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("the event loop was blocked for %s by a model switch", d)
	}
	if s.ag.Model != "other" {
		t.Fatalf("the switch itself must still be immediate; model is %q", s.ag.Model)
	}
}

// blockingLoader never answers until released, standing in for a backend
// reloading a 27B model.
type blockingLoader struct{ release chan struct{} }

func (l blockingLoader) Apply(ctx context.Context, _ string) (int, error) {
	select {
	case <-l.release:
	case <-ctx.Done():
	}
	return 0, nil
}
func (blockingLoader) OnEvicted(context.Context, string) {}
func (blockingLoader) OnWindowChanged(string, int)       {}
