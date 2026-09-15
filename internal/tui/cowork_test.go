package tui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
)

// scriptedCoworker is a provider.Provider stand-in whose one reply is
// scripted per test; the other methods come from nullProvider (scroll_test).
type scriptedCoworker struct {
	nullProvider
	reply func(provider.ChatRequest) string
}

func (s scriptedCoworker) Chat(ctx context.Context, req provider.ChatRequest, onDelta provider.StreamFunc) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{Content: s.reply(req)}, nil
}

func scriptedProvider(reply func(provider.ChatRequest) string) provider.Provider {
	return scriptedCoworker{reply: reply}
}

// drainLocked is flush with the session lock held, the way View.Update holds
// it around its own drain. A consultation runs on a goroutine of its own and
// writes shared session state (entries, the status note) under mu while the
// test drains; a bare flush would read and write the same fields unlocked.
func drainLocked(s *Session, views ...*View) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range views {
		v.mb.drainInto(v)
	}
}

// idle reports the shared run state under the lock, for the same reason.
func idle(s *Session) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.running
}

func TestCoworkEntriesRenderInTheCoworkColourPerTheme(t *testing.T) {
	defer pinColorProfile()()
	dark, nord := stylesOr("dark"), stylesOr("nord")
	ask := entry{Kind: entryCoworkAsk, Label: "claude", Text: "why?"}
	ans := entry{Kind: entryCowork, Label: "claude", Text: "Because **x**."}
	if renderEntry(ask, dark, 80, false, true) == renderEntry(ask, nord, 80, false, true) {
		t.Fatal("cowork ask renders identically under two themes")
	}
	if got := renderEntry(ans, dark, 80, false, true); !strings.Contains(got, dark.Cowork.Render("claude> ")) || !strings.Contains(got, "Because") {
		t.Fatalf("cowork answer: %q", got)
	}
	if dark.Cowork.Render("x") == dark.User.Render("x") || dark.Cowork.Render("x") == dark.Tool.Render("x") {
		t.Fatal("cowork colour must differ from the user and tool colours")
	}
}

func TestConsultEventsBecomeSharedEntriesAndStatus(t *testing.T) {
	s, a, b := twoViews(t)
	s.ag.Events.OnConsultStart("claude", "why does it fail?", "tool")
	s.ag.Events.OnConsultProgress("claude", 2)
	s.ag.Events.OnConsultEnd(agent.ConsultResult{Coworker: "claude", Answer: "Line 12 is wrong.", Read: 2, Elapsed: 3 * time.Second}, nil)
	flush(a, b)
	for _, v := range []*View{a, b} {
		for _, want := range []string{"claude? why does it fail?", "claude> Line 12 is wrong.", "claude read 2 files in 3s"} {
			if !strings.Contains(v.wrapped, want) {
				t.Fatalf("view %d lacks %q:\n%s", v.id, want, v.wrapped)
			}
		}
	}
	s.ag.Events.OnConsultEnd(agent.ConsultResult{Coworker: "claude"}, errors.New("consultation declined"))
	flush(a, b)
	if !strings.Contains(a.wrapped, "claude: consultation declined") {
		t.Fatalf("declined note missing:\n%s", a.wrapped)
	}
}

func TestConsultConsentModalAndSessionAllow(t *testing.T) {
	s, a, b := twoViews(t)
	s.cfg.Coworkers = []config.CoworkerConfig{{Name: "claude", Provider: "ollama", Model: "opus", Online: true}}
	s.ag = agent.New(s.cfg, nullProvider{}, "m", s.ag.Tools, "")
	s.ag.Tools.Approve = s.approveFromAgent
	agent.CoworkerFactory = func(c *config.Config, cw config.CoworkerConfig) (provider.Provider, error) {
		return scriptedProvider(func(provider.ChatRequest) string { return "advice" }), nil
	}
	t.Cleanup(func() { agent.CoworkerFactory = nil })
	done := make(chan error, 1)
	go func() {
		_, err := s.ag.Consult(context.Background(), agent.ConsultRequest{Question: "q", Origin: "tool"})
		done <- err
	}()
	waitFor(t, func() bool { drainLocked(s, a, b); return a.mode == modeAsk && b.mode == modeAsk })
	if !strings.Contains(a.View(), "Co-working model") || !strings.Contains(a.View(), "coworker: claude") {
		t.Fatalf("consent modal:\n%s", a.View())
	}
	b.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	drainLocked(s, a, b)
	if !strings.Contains(a.wrapped, "consult approved") || !strings.Contains(a.wrapped, "co-worker claude allowed for this session") {
		t.Fatalf("verdict lines:\n%s", a.wrapped)
	}
	// The second consultation must not prompt.
	if _, err := s.ag.Consult(context.Background(), agent.ConsultRequest{Question: "q2", Origin: "tool"}); err != nil {
		t.Fatal(err)
	}
	drainLocked(s, a, b)
	if a.mode == modeAsk || b.mode == modeAsk {
		t.Fatal("prompted again after a session allow")
	}
}

func TestConsultCommandRunsAsATurn(t *testing.T) {
	s, a, b := twoViews(t)
	s.cfg.Coworkers = []config.CoworkerConfig{{Name: "big", Provider: "ollama", Model: "qwen3:32b", Skills: "long reads"}}
	s.ag = agent.New(s.cfg, nullProvider{}, "m", s.ag.Tools, "")
	wireEvents(s) // the helper NewSession uses to install Events on an agent
	agent.CoworkerFactory = func(c *config.Config, cw config.CoworkerConfig) (provider.Provider, error) {
		return scriptedProvider(func(provider.ChatRequest) string { return "Try the other branch." }), nil
	}
	t.Cleanup(func() { agent.CoworkerFactory = nil })
	a.slashCommand("/coworkers")
	if !strings.Contains(a.wrapped, "big") || !strings.Contains(a.wrapped, "long reads") {
		t.Fatalf("/coworkers:\n%s", a.wrapped)
	}
	a.slashCommand("/consult big which branch?")
	waitFor(t, func() bool { drainLocked(s, a, b); return strings.Contains(b.wrapped, "big> Try the other branch.") })
	if !strings.Contains(b.wrapped, "big? which branch?") {
		t.Fatalf("question missing on the other terminal:\n%s", b.wrapped)
	}
	waitFor(t, func() bool { drainLocked(s, a, b); return idle(s) })
}

// blockingChat parks in Chat until it is released or its context is done,
// and says which happened. Both the primary and the co-worker use one, so a
// test can see exactly whose context a cancellation reached.
type blockingChat struct {
	nullProvider
	entered  chan struct{}
	release  chan struct{}
	ctxDone  chan struct{}
	inOnce   sync.Once
	doneOnce sync.Once
}

func newBlockingChat() *blockingChat {
	return &blockingChat{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		ctxDone: make(chan struct{}),
	}
}

func (b *blockingChat) Chat(ctx context.Context, _ provider.ChatRequest, _ provider.StreamFunc) (*provider.ChatResponse, error) {
	b.inOnce.Do(func() { close(b.entered) })
	select {
	case <-b.release:
		return &provider.ChatResponse{Content: "done"}, nil
	case <-ctx.Done():
		b.doneOnce.Do(func() { close(b.ctxDone) })
		return nil, ctx.Err()
	}
}

func closed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// F6: /consult asked in the middle of a run does not take the turn over. It
// runs on the session's root context with its cancel parked in
// consultCancel, a second one while it is in flight is refused rather than
// losing that cancel, and Esc stops the consultation *and* the run.
func TestConsultMidRunRunsBesideTheTurnAndEscCancelsBoth(t *testing.T) {
	s, a, b := twoViews(t)
	s.cfg.Coworkers = []config.CoworkerConfig{{Name: "big", Provider: "ollama", Model: "qwen3:32b"}}
	primary := newBlockingChat()
	s.ag = agent.New(s.cfg, primary, "m", s.ag.Tools, "")
	s.prov = primary
	wireEvents(s)
	coworker := newBlockingChat()
	agent.CoworkerFactory = func(*config.Config, config.CoworkerConfig) (provider.Provider, error) {
		return coworker, nil
	}
	t.Cleanup(func() {
		agent.CoworkerFactory = nil
		close(primary.release)
	})

	// A real turn, so there is a run context for Esc to cancel.
	a.Update(runes("do a thing"))
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, func() bool { drainLocked(s, a, b); return !idle(s) && closed(primary.entered) })

	// /consult while the run is in flight: busy-safe, and its own cancel.
	a.Update(runes("/consult big what now?"))
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	s.mu.Lock()
	haveCancel := s.consultCancel != nil
	s.mu.Unlock()
	if !haveCancel {
		t.Fatal("a mid-run /consult parked no cancel in consultCancel")
	}
	waitFor(t, func() bool { drainLocked(s, a, b); return closed(coworker.entered) })
	if idle(s) {
		t.Fatal("the mid-run consultation ended the turn")
	}

	// A second one would lose the first one's cancel, so it is refused.
	b.Update(runes("/consult big and this?"))
	b.Update(tea.KeyMsg{Type: tea.KeyEnter})
	drainLocked(s, a, b)
	if !strings.Contains(b.wrapped, "a consultation is already running") {
		t.Fatalf("the second consultation was not refused:\n%s", b.wrapped)
	}

	// Esc: the run and the consultation both stop.
	b.Update(tea.KeyMsg{Type: tea.KeyEsc})
	waitFor(t, func() bool { drainLocked(s, a, b); return closed(coworker.ctxDone) })
	waitFor(t, func() bool { drainLocked(s, a, b); return closed(primary.ctxDone) })
	waitFor(t, func() bool {
		drainLocked(s, a, b)
		s.mu.Lock()
		defer s.mu.Unlock()
		return !s.running && s.consultCancel == nil
	})
}
