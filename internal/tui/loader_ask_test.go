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
	"github.com/brown-enterprises/be-code/internal/provider"
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
func (*consentLoader) KeepAlive(string) time.Duration    { return 0 }

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
func (blockingLoader) KeepAlive(string) time.Duration    { return 0 }

// The lock order in a served session is turn lock first, session lock
// second: everything holding the turn lock writes to the session as it goes
// (deltas, notices), and that takes the lock Update holds. So a slash
// command that needs the turn lock must not wait for it inside Update — it
// would invert the order and hang the terminal, with no way to answer
// anything. /clear is the one that had to move.
func TestClearDoesNotWaitForTheTurnLockInsideUpdate(t *testing.T) {
	tempHome(t)
	s := newTestSession(t)
	v := s.NewView(0, "local")
	v.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	flush(v)

	// Give the agent a transcript and park something in Compact, which
	// holds the turn lock until its model call returns.
	for i := 0; i < 20; i++ {
		s.ag.History.Messages = append(s.ag.History.Messages,
			provider.Message{Role: provider.RoleUser, Content: strings.Repeat("x", 400)},
			provider.Message{Role: provider.RoleAssistant, Content: strings.Repeat("y", 400)})
	}
	s.ag.Provider = blockingProvider{}
	ctx, cancel := context.WithCancel(context.Background())
	held := make(chan struct{})
	go func() {
		close(held)
		_ = s.ag.CompactNow(ctx)
	}()
	<-held
	time.Sleep(50 * time.Millisecond) // let it reach the model call

	done := make(chan struct{})
	go func() {
		v.Update(runes("/clear"))
		v.Update(tea.KeyMsg{Type: tea.KeyEnter})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("Update blocked on the turn lock; a served terminal would be hung here")
	}
	cancel()
}

// N3. /clear and /resume swap the session from goroutines of their own
// now, and the header draws the resume code from it on every frame. Under
// -race that is a write against a read unless the swap goes through the
// session lock and the header reads through the agent's accessor.
//
// Everything a terminal does stays on one goroutine here, as Bubble Tea
// guarantees: the second party is the /clear goroutine that Update itself
// spawns, which is the whole point.
func TestSessionSwapsDoNotRaceTheHeader(t *testing.T) {
	tempHome(t)
	s := newTestSession(t)
	v := s.NewView(0, "local")
	v.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	flush(v)

	idle := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return !s.running
	}
	for i := 0; i < 40; i++ {
		v.Update(runes("/clear"))
		v.Update(tea.KeyMsg{Type: tea.KeyEnter})
		// Repaint while the swap is in flight: the header reads the session
		// on every frame.
		for n := 0; n < 100 && !idle(); n++ {
			v.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
			_ = v.View()
		}
		waitFor(t, func() bool { flush(v); return idle() })
	}
	if s.ag.CurrentSession() == nil {
		t.Fatal("no session after clearing")
	}
}

// R3. The shared modal waited on the session's own lifetime, so a
// resolution that gave up could not take its question off the screen: with
// the deadline shortened the modal was still open well past it, on every
// attached terminal, with nobody left waiting for an answer. The
// context-aware approver hands the asker's deadline to Ask, which already
// knows how to withdraw — CancelAsk, broadcast to every view.
func TestAResolutionsDeadlineWithdrawsTheSharedModal(t *testing.T) {
	s, a, b := twoViews(t)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	decided := make(chan bool, 1)
	go func() {
		decided <- s.ag.Tools.ApproveCtx(ctx, "model_reload",
			"model m is loaded with an 8192-token window; config asks for 32768.")
	}()
	waitFor(t, func() bool { flush(a, b); return a.mode == modeAsk && b.mode == modeAsk })

	select {
	case ok := <-decided:
		if ok {
			t.Fatal("a question nobody answered must never count as consent")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the asker was left parked on a question its deadline had already given up on")
	}
	waitFor(t, func() bool { flush(a, b); return a.mode != modeAsk && b.mode != modeAsk })
}

// A question withdrawn because its deadline passed tells every terminal that
// nobody answered; a plain cancellation (the user's own Esc) stays silent.
func TestADeadlineWithdrawalSaysNobodyAnswered(t *testing.T) {
	tempHome(t)
	s := newTestSession(t)
	v := s.NewView(0, "local")
	v.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	flush(v)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	done := make(chan askAnswer, 1)
	go func() { done <- s.Ask(ctx, &ask{Kind: askApproval, Action: "model_reload", Detail: "reload?"}) }()
	select {
	case ans := <-done:
		if ans.OK {
			t.Fatal("an unanswered question came back approved")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the deadline did not withdraw the question")
	}
	flush(v)
	if got := v.rendered.String(); !strings.Contains(got, noAnswerNote) {
		t.Fatalf("the terminal was not told why the modal closed:\n%s", got)
	}
	if answeredNote("") != "" || answeredNote(noAnswerNote) != noAnswerNote {
		t.Fatal("answeredNote must pass the withdrawal note through and stay silent for a cancellation")
	}
}

// Two real questions must not deny each other. A consent modal raised by a
// mid-run /model used to make the agent's next write approval count as
// denied, unseen; now the second question waits its turn and is answered.
func TestASecondApprovalQueuesBehindTheFirst(t *testing.T) {
	tempHome(t)
	s := newTestSession(t)
	v := s.NewView(0, "local")
	v.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	flush(v)

	first := make(chan askAnswer, 1)
	go func() {
		first <- s.Ask(context.Background(), &ask{Kind: askApproval, Action: "model_reload", Detail: "reload?"})
	}()
	waitFor(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.current() != nil })

	second := make(chan askAnswer, 1)
	go func() {
		second <- s.Ask(context.Background(), &ask{Kind: askApproval, Action: "file_write", Detail: "write a.go"})
	}()
	select {
	case ans := <-second:
		t.Fatalf("the second question was answered %+v while the first was still open", ans)
	case <-time.After(60 * time.Millisecond):
	}

	flush(v)
	v.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")}) // refuse the reload
	if ans := <-first; ans.OK {
		t.Fatal("the first question was refused, not approved")
	}
	waitFor(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		a := s.current()
		return a != nil && a.Action == "file_write"
	})
	flush(v)
	v.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}) // approve the write
	select {
	case ans := <-second:
		if !ans.OK || ans.Refused {
			t.Fatalf("the queued write approval came back %+v; it was answered yes", ans)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the queued question was never put to anyone")
	}
}
