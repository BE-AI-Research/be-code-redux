package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/provider"
)

// startedRun puts the session into a run without a model goroutine: the hook
// stands in for RunFull, exactly as the leftover-queue tests use it.
func startedRun(t *testing.T, s *Session, from *View) {
	t.Helper()
	s.startTurnHook = func(string) {}
	from.Update(runes("do a thing"))
	from.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !s.running {
		t.Fatal("the run never started")
	}
}

// A terminal that attaches while the agent is working starts busy. Starting
// in modeInput would let its Enter reach Submit and launch a *second*
// concurrent RunFull on the one agent.
func TestAttachingMidRunStartsBusy(t *testing.T) {
	s, a, b := twoViews(t)
	startedRun(t, s, a)
	flush(a, b)

	late := s.NewView(3, "late (pid 3)")
	late.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if late.mode != modeBusy {
		t.Fatalf("a terminal attached mid-run is in mode %v, want busy", late.mode)
	}
	if late.input.Placeholder != busyPlaceholder {
		t.Fatalf("placeholder = %q, want the busy one", late.input.Placeholder)
	}

	// Enter queues rather than starting a second run.
	before := s.ag.Pending()
	late.Update(runes("and this too"))
	late.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := s.ag.Pending(); got != before+1 {
		t.Fatalf("pending = %d, want %d: Enter did not queue", got, before+1)
	}

	// And it returns to input with everyone else when the run ends.
	s.ag.DrainInbox() // so finishTurn does not start the next turn
	s.finishTurn(nil, nil)
	flush(a, b, late)
	if late.mode != modeInput {
		t.Fatalf("mode after the run = %v, want input", late.mode)
	}
}

// A terminal that attaches while a shared question is open sees it, and can
// answer it. Without this a detach-and-reattach during a file-write approval
// leaves the agent goroutine parked in Ask for good.
func TestAttachingWhileAnAskIsOpenShowsIt(t *testing.T) {
	s, a, b := twoViews(t)
	decided := make(chan bool, 1)
	go func() { decided <- s.approveFromAgent("file_write", "--- a\n+++ b\n") }()
	waitFor(t, func() bool { flush(a, b); return a.mode == modeAsk && b.mode == modeAsk })

	late := s.NewView(3, "late (pid 3)")
	late.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if late.mode != modeAsk {
		t.Fatalf("a terminal attached with an ask open is in mode %v, want ask", late.mode)
	}
	if !strings.Contains(late.View(), "approval required") {
		t.Fatalf("the late terminal does not render the modal:\n%s", late.View())
	}
	late.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	select {
	case ok := <-decided:
		if !ok {
			t.Fatal("y from the late terminal must approve")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the late terminal could not answer the ask")
	}
}

// The last line of defence for the same wedge: ending the session releases
// whatever is parked in Ask, rather than leaving the agent goroutine there.
func TestQuitReleasesAParkedAsk(t *testing.T) {
	s, a, b := twoViews(t)
	done := make(chan bool, 1)
	go func() { done <- s.approveFromAgent("file_write", "--- a\n+++ b\n") }()
	waitFor(t, func() bool { flush(a, b); return a.mode == modeAsk })
	s.Quit()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("a quit must not approve the write")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Quit left the agent goroutine parked in Ask")
	}
}

// I1, one direction: a picker loaded by /menu while an approval is on screen
// must not take the screen away from a write nobody has answered.
func TestAPickerCannotDisplaceAnOpenApproval(t *testing.T) {
	s, a, b := twoViews(t)
	decided := make(chan bool, 1)
	go func() { decided <- s.approveFromAgent("file_write", "--- a\n+++ b\n") }()
	waitFor(t, func() bool { flush(a, b); return a.mode == modeAsk && b.mode == modeAsk })

	cmd := b.askList("Select model", func() ([]pickItem, error) {
		return []pickItem{{id: "one", label: "one"}}, nil
	}, func(*View, string, int) tea.Cmd { return nil })
	ran := make(chan struct{})
	go func() { cmd(); close(ran) }()
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("askList raised a picker over the open approval instead of refusing")
	}
	flush(a, b)

	select {
	case ok := <-decided:
		t.Fatalf("the approval was silently decided (%v) by a picker nobody answered", ok)
	default:
	}
	if a.shownAsk == nil || a.shownAsk.Kind != askApproval {
		t.Fatalf("view a no longer shows the approval: %+v", a.shownAsk)
	}
	if !strings.Contains(b.rendered.String(), "a prompt is already open") {
		t.Fatalf("the terminal that opened the picker was not told why:\n%s", b.rendered.String())
	}
	// And the approval is still answerable.
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	select {
	case ok := <-decided:
		if !ok {
			t.Fatal("y must approve")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the approval was never answered")
	}
}

// I1, the other direction: an approval raised while a *picker* is open does
// take the screen — the picker has nobody waiting on its verdict — and every
// terminal is moved onto the approval.
func TestAnApprovalDisplacesAnOpenPicker(t *testing.T) {
	s, a, b := twoViews(t)
	picked := make(chan askAnswer, 1)
	go func() {
		picked <- s.Ask(context.Background(), &ask{Kind: askPicker, Title: "Select model",
			Items: []pickItem{{id: "one", label: "one"}}})
	}()
	waitFor(t, func() bool { flush(a, b); return a.mode == modeAsk && a.picker != nil })

	decided := make(chan bool, 1)
	go func() { decided <- s.approveFromAgent("file_write", "--- a\n+++ b\n") }()
	waitFor(t, func() bool {
		flush(a, b)
		return a.shownAsk != nil && a.shownAsk.Kind == askApproval &&
			b.shownAsk != nil && b.shownAsk.Kind == askApproval
	})
	select {
	case <-picked: // the displaced picker's goroutine is released
	case <-time.After(2 * time.Second):
		t.Fatal("the displaced picker left its goroutine parked")
	}
	b.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	select {
	case ok := <-decided:
		if !ok {
			t.Fatal("y must approve")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the approval that displaced the picker was never answerable")
	}
}

// I2: a terminal that goes away with its queue popup open must not leave the
// agent's delivery held for every later run.
func TestQueueHoldIsReleasedWhenAViewIsRetired(t *testing.T) {
	s, a, b := twoViews(t)
	s.running = true
	a.mode, b.mode = modeBusy, modeBusy
	s.ag.EnqueueFrom("later", 2)
	b.Update(tea.KeyMsg{Type: tea.KeyUp})
	if b.mode != modeQueue || !s.ag.Held() {
		t.Fatalf("the popup did not open and hold: mode=%v held=%v", b.mode, s.ag.Held())
	}
	s.retireView(b)
	if s.ag.Held() {
		t.Fatal("a retired terminal's queue popup left delivery held for good")
	}
}

// And the hold is per terminal: one closing its popup must not resume
// delivery under the other's cursor.
func TestQueueHoldIsRefcountedAcrossTerminals(t *testing.T) {
	s, a, b := twoViews(t)
	s.running = true
	a.mode, b.mode = modeBusy, modeBusy
	s.ag.EnqueueFrom("from one", 1)
	s.ag.EnqueueFrom("from two", 2)
	a.Update(tea.KeyMsg{Type: tea.KeyUp})
	b.Update(tea.KeyMsg{Type: tea.KeyUp})
	if a.mode != modeQueue || b.mode != modeQueue || !s.ag.Held() {
		t.Fatalf("both popups did not open: a=%v b=%v held=%v", a.mode, b.mode, s.ag.Held())
	}
	a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if !s.ag.Held() {
		t.Fatal("one terminal closing its popup released the other terminal's hold")
	}
	b.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if s.ag.Held() {
		t.Fatal("the last popup to close must release the hold")
	}
}

// A dropped broadcast must not leave a permanent hole in a view's buffer:
// the entries are still on the session, so the drain rebuilds from them.
func TestMailboxDropRebuildsRatherThanLeavingAGap(t *testing.T) {
	s := newTestSession(t)
	v := s.NewView(0, "local")
	v.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	v.mb = tinyMailbox(2)

	for i := 0; i < 6; i++ {
		s.appendEntry(entry{Kind: entryDim, Text: fmt.Sprintf("line %d", i)})
	}
	flush(v)
	for i := 0; i < 6; i++ {
		if want := fmt.Sprintf("line %d", i); !strings.Contains(v.rendered.String(), want) {
			t.Fatalf("%q is missing after a dropped broadcast:\n%s", want, v.rendered.String())
		}
	}
}

// And a dropped streamEndMsg must not leave the half-streamed reply on
// screen for ever, doubled by the entry that followed it.
func TestMailboxDropOfStreamEndResetsTheStream(t *testing.T) {
	s := newTestSession(t)
	v := s.NewView(0, "local")
	v.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	v.mb = tinyMailbox(1)
	v.streaming = "half a reply"

	s.broadcast(entryMsg{n: 0, e: entry{Kind: entryDim, Text: "filler"}})
	s.broadcast(streamEndMsg{}) // dropped: the queue is full
	flush(v)
	if v.streaming != "" {
		t.Fatalf("streaming = %q after a dropped streamEndMsg", v.streaming)
	}
}

func tinyMailbox(depth int) *mailbox {
	return &mailbox{ch: make(chan tea.Msg, depth), wake: make(chan struct{}, 1), done: make(chan struct{})}
}

// A panic while a shared picker's rows load must not take the host down with
// it: it is reported on the transcript like any other failed load.
func TestAskListSurvivesAPanickingLoad(t *testing.T) {
	m := newTestModel(t)
	cmd := m.askList("Select model", func() ([]pickItem, error) {
		panic("boom")
	}, func(*View, string, int) tea.Cmd { return nil })
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				t.Fatalf("the panic escaped askList: %v", rec)
			}
		}()
		cmd()
	}()
	flush(m)
	if !strings.Contains(m.rendered.String(), "boom") {
		t.Fatalf("the panic was not reported on the transcript:\n%s", m.rendered.String())
	}
}

// blockingProvider parks in Chat until the request context ends, so a test
// can cancel a run the way Esc does.
type blockingProvider struct{ nullProvider }

func (blockingProvider) Chat(ctx context.Context, _ provider.ChatRequest, _ provider.StreamFunc) (*provider.ChatResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// A cancelled /plan prints the same "cancelled" line every other cancelled
// turn does, not "plan failed: context canceled".
func TestCancelledPlanReportsCancelled(t *testing.T) {
	m := newTestModel(t)
	m.prov = blockingProvider{}
	m.ag.Provider = blockingProvider{}
	m.Update(runes("/plan rewrite the parser"))
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	// The plan runs on its own goroutine from here, so everything this test
	// reads it reads under the same lock Update holds — the drain included.
	snapshot := func() (bool, string) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.mb.drainInto(m)
		return m.running, m.rendered.String()
	}
	running, _ := snapshot()
	if !running {
		t.Fatal("/plan did not start a turn")
	}
	m.mu.Lock()
	cancel := m.cancelFn
	m.mu.Unlock()
	cancel()
	waitFor(t, func() bool { done, _ := snapshot(); return !done })
	_, text := snapshot()
	if strings.Contains(text, "plan failed") {
		t.Fatalf("a cancelled plan reported a failure:\n%s", text)
	}
	if !strings.Contains(text, "cancelled") {
		t.Fatalf("a cancelled plan did not say so:\n%s", text)
	}
}
