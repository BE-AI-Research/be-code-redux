package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/brown-enterprises/be-code/internal/live"
)

// One program per attached terminal: every view has its own input line, its
// own popups and its own scroll, while the transcript, the run state, the
// queue and the roster stay one session.

func TestEachViewTypesIntoItsOwnInput(t *testing.T) {
	_, a, b := twoViews(t)
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hello")})
	b.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("world")})
	if a.input.Value() != "hello" || b.input.Value() != "world" {
		t.Fatalf("inputs: a=%q b=%q", a.input.Value(), b.input.Value())
	}
	if !strings.Contains(a.View(), "hello") || strings.Contains(a.View(), "world") {
		t.Fatal("a view must render its own draft and nobody else's")
	}
}

func TestEnterSubmitsWithTheSendersLabel(t *testing.T) {
	s, a, b := twoViews(t)
	var started []string
	s.startTurnHook = func(text string) { started = append(started, text) }
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("from desk")})
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	flush(a, b)
	if len(started) != 1 || started[0] != "from desk" {
		t.Fatalf("started: %v", started)
	}
	if !strings.Contains(b.wrapped, "desk (pid 1)> from desk") {
		t.Fatalf("the other view did not see the labelled line:\n%s", b.wrapped)
	}
	if !b.running || b.mode != modeBusy {
		t.Fatalf("b running=%v mode=%v", b.running, b.mode)
	}
}

func TestPaletteIsLocalToTheViewThatOpenedIt(t *testing.T) {
	_, a, b := twoViews(t)
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	if a.mode != modePalette || b.mode == modePalette {
		t.Fatalf("modes: a=%v b=%v", a.mode, b.mode)
	}
	b.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("typing")})
	if b.input.Value() != "typing" || a.mode != modePalette {
		t.Fatal("b's keys must reach b's input and leave a's palette alone")
	}
}

func TestScrollIsPerView(t *testing.T) {
	s, a, b := twoViews(t)
	for i := 0; i < 200; i++ {
		s.appendEntry(entry{Kind: entryDim, Text: fmt.Sprintf("line %d", i)})
	}
	flush(a, b)
	a.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	if a.vp.AtBottom() || !b.vp.AtBottom() {
		t.Fatalf("a atBottom=%v b atBottom=%v", a.vp.AtBottom(), b.vp.AtBottom())
	}
}

func TestQuitEndsEveryView(t *testing.T) {
	_, a, b := twoViews(t)
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/quit")})
	_, cmdA := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	_ = cmdA
	flush(a, b)
	// each view returned tea.Quit on its quitMsg: drainInto records the last cmd
	if !a.quitSeen || !b.quitSeen {
		t.Fatalf("quit reached a=%v b=%v", a.quitSeen, b.quitSeen)
	}
}

func TestDetachedViewStopsReceiving(t *testing.T) {
	s, a, b := twoViews(t)
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "desk (pid 1)", UTF8: true}})
	s.detachView(2)
	s.appendEntry(entry{Kind: entryDim, Text: "after"})
	flush(a, b)
	if !strings.Contains(a.wrapped, "after") || strings.Contains(b.wrapped, "after") {
		t.Fatal("a detached view must not receive broadcasts")
	}
	if !strings.Contains(a.wrapped, "detached: tablet (pid 2)") {
		t.Fatalf("no detached line:\n%s", a.wrapped)
	}
}

// Ctrl+C twice quits — but only from the same terminal. A Ctrl+C on one
// terminal and a Ctrl+C on another are two people each clearing their own
// line, not a session-ending confirmation.
func TestQuitHintIsPerView(t *testing.T) {
	_, a, b := twoViews(t)
	a.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	b.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	flush(a, b)
	if a.quitSeen || b.quitSeen {
		t.Fatal("one Ctrl+C on each of two terminals must not quit either")
	}
	b.Update(tea.KeyMsg{Type: tea.KeyCtrlC}) // the same terminal, twice
	flush(a, b)
	if !a.quitSeen || !b.quitSeen {
		t.Fatalf("the same terminal's second Ctrl+C must end the session: a=%v b=%v", a.quitSeen, b.quitSeen)
	}
}

// While the agent works, Enter queues the sender's own text under the
// sender's label and leaves every other terminal's draft alone.
func TestEnterQueuesOnlyTheSendersTextWithLabel(t *testing.T) {
	s, a, b := twoViews(t)
	s.running = true
	a.mode, b.mode = modeBusy, modeBusy
	b.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("do it later")})
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("mine")})
	b.Update(tea.KeyMsg{Type: tea.KeyEnter})
	flush(a, b)
	items := s.ag.Items()
	if len(items) != 1 || items[0].Text != "do it later" || items[0].From != 2 {
		t.Fatalf("queue: %+v", items)
	}
	if got := a.input.Value(); got != "mine" {
		t.Fatalf("view a's draft was disturbed: %q", got)
	}
	if !strings.Contains(a.rendered.String(), "tablet (pid 2)> ") {
		t.Fatalf("queued line lacks the sender label:\n%s", a.rendered.String())
	}
}

// The queue popup lists the messages this terminal queued, and nobody
// else's — nobody edits another person's draft.
func TestQueuePopupShowsOnlyThisViewsMessages(t *testing.T) {
	s, a, b := twoViews(t)
	s.running = true
	a.mode, b.mode = modeBusy, modeBusy
	s.ag.EnqueueFrom("from one", 1)
	s.ag.EnqueueFrom("from two", 2)
	b.Update(tea.KeyMsg{Type: tea.KeyUp})
	if b.mode != modeQueue {
		t.Fatalf("Up on an empty input opens the queue popup: %v", b.mode)
	}
	if a.mode == modeQueue {
		t.Fatal("the popup belongs to the terminal that opened it")
	}
	v := b.View()
	if !strings.Contains(v, "from two") || strings.Contains(v, "from one") {
		t.Fatalf("popup must list only this view's messages:\n%s", v)
	}
	b.Update(tea.KeyMsg{Type: tea.KeyEnter}) // edit
	if got := b.input.Value(); got != "from two" {
		t.Fatalf("edit pulled %q into view b's input", got)
	}
	if items := s.ag.Items(); len(items) != 1 || items[0].From != 1 {
		t.Fatalf("view a's message must remain queued: %+v", items)
	}
}

func TestTranscriptPrefixUsesLabelOnlyWithSeveralClients(t *testing.T) {
	m := newTestModel(t)
	if got := m.userPrefix(0); got != "you> " {
		t.Fatalf("single client prefix %q", got)
	}
	m = twoClients(t)
	if got := m.userPrefix(1); got != "desk (pid 1)> " {
		t.Fatalf("multi client prefix %q", got)
	}
}

func TestInProcessModelStillUsesClientZero(t *testing.T) {
	m := newTestModel(t)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("abc")})
	if got := m.input.Value(); got != "abc" {
		t.Fatalf("plain KeyMsg goes to the local input: %q", got)
	}
	if !strings.Contains(m.View(), "abc") {
		t.Fatal("in-process view renders its textarea")
	}
}

func TestDetachCommandDetachesTheTypingClient(t *testing.T) {
	s, _, b := twoViews(t)
	detached := make(chan int, 1)
	s.detachClient = func(id int) { detached <- id }
	b.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/detach")})
	_, cmd := b.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("/detach returned no command")
	}
	cmd()
	if id := <-detached; id != 2 {
		t.Fatalf("detached client %d, want the one that typed /detach", id)
	}
}

func TestBottomLineListsClientLabels(t *testing.T) {
	m := twoClients(t)
	v := m.View()
	if !strings.Contains(v, "⧉ 2") || !strings.Contains(v, "desk (pid 1), tablet (pid 2)") {
		t.Fatalf("bottom line:\n%s", v)
	}
	if strings.Contains(v, "input:") || strings.Contains(v, "Ctrl+] t") {
		t.Fatal("holder text must be gone")
	}
}

// Input recall walks one shared store of lines — everything anyone
// submitted — but each terminal keeps its own place in it, so one view's
// Up never moves another's.
func TestInputHistoryCursorIsPerView(t *testing.T) {
	s, a, b := twoViews(t)
	// Submit while a run is in progress: Enter records the line in the
	// shared history exactly as it does in modeInput, but queues it instead
	// of launching three overlapping agent runs at one Agent.
	s.running = true
	a.mode, b.mode = modeBusy, modeBusy
	for _, step := range []struct {
		v    *View
		text string
	}{{a, "one"}, {a, "two"}, {b, "three"}} {
		step.v.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(step.text)})
		step.v.Update(tea.KeyMsg{Type: tea.KeyEnter})
	}
	s.ag.DrainInbox()
	s.running = false
	a.mode, b.mode = modeInput, modeInput
	// View a last submitted "two", so its cursor sits where the list ended
	// then: Up walks back from the newest line in the shared store.
	a.Update(tea.KeyMsg{Type: tea.KeyUp})
	if got := a.input.Value(); got != "three" {
		t.Fatalf("view a first Up = %q, want the newest shared line", got)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyUp})
	if got := a.input.Value(); got != "two" {
		t.Fatalf("view a second Up = %q", got)
	}
	// View b has not navigated at all: its own Up starts from the newest.
	b.Update(tea.KeyMsg{Type: tea.KeyUp})
	if got := b.input.Value(); got != "three" {
		t.Fatalf("view b Up = %q; view a's navigation moved its cursor", got)
	}
	// ...and view b's navigation left view a where it was.
	a.Update(tea.KeyMsg{Type: tea.KeyUp})
	if got := a.input.Value(); got != "one" {
		t.Fatalf("view a third Up = %q; view b's navigation moved its cursor", got)
	}
}

// The bottom line never overruns this terminal's width, however long the
// model name and however many terminals are attached: the label list is
// truncated, and dropped entirely when there is no room for it.
func TestBottomLineFitsTheWidth(t *testing.T) {
	m := newTestModel(t)
	m.cfg.Layout = "full" // keep the full bottom line at narrow widths too
	m.ag.SetModel("hf.co/unsloth/Qwen3.8-27B-GGUF:UD-Q4_K_XL")
	m.clients = []live.ClientInfo{
		{ID: 1, Label: "desk (pid 1111)", UTF8: true},
		{ID: 2, Label: "ssh from 10.0.0.5 (pid 2222)", UTF8: true},
		{ID: 3, Label: "phone over tailscale (pid 3333)", UTF8: true},
	}
	for _, w := range []int{56, 60, 72, 84, 100, 140} {
		m.Update(tea.WindowSizeMsg{Width: w, Height: 30})
		if got := lipgloss.Width(m.bottomLine()); got > w {
			t.Fatalf("bottom line is %d cells wide at width %d:\n%s", got, w, m.bottomLine())
		}
	}
	// The boundary itself: with no usable room the list is dropped, not
	// emitted whole (and never sliced with a negative bound).
	for _, room := range []int{-3, 0, 1} {
		if got := m.clientLabels(room); got != "" {
			t.Fatalf("clientLabels(%d) = %q, want the list dropped", room, got)
		}
	}
	if got := m.clientLabels(8); got != "desk (p…" {
		t.Fatalf("clientLabels(8) = %q", got)
	}
}

// Messages left in the queue when a run ends start the next turn as one
// request, but the transcript still says who wrote each of them.
func TestLeftoverQueueEchoesEverySender(t *testing.T) {
	s, a, b := twoViews(t)
	s.running = true
	a.mode, b.mode = modeBusy, modeBusy
	s.ag.EnqueueFrom("from one", 1)
	s.ag.EnqueueFrom("from two", 2)
	var ran string // stands in for the next turn's run, and records it
	s.startTurnHook = func(text string) { ran = text }
	s.finishTurn(nil, nil)
	flush(a, b)
	// One request, in the order the messages were queued.
	if ran != "from one\nfrom two" {
		t.Fatalf("the next turn ran %q, want both queued messages as one request", ran)
	}
	tr := a.rendered.String()
	for _, want := range []string{"desk (pid 1)> from one", "tablet (pid 2)> from two"} {
		if !strings.Contains(tr, want) {
			t.Fatalf("transcript lacks %q:\n%s", want, tr)
		}
	}
	if a.mode != modeBusy || !a.running {
		t.Fatalf("leftovers did not start the next turn: mode=%v running=%v", a.mode, a.running)
	}
	if s.ag.Pending() != 0 {
		t.Fatalf("queue not drained: %d", s.ag.Pending())
	}
}

// /resume CODE has to be typeable at the prompt: the space that ends the
// command name arrives as tea.KeySpace, and must hand the text back to the
// input line rather than filtering the palette.
func TestSpaceClosesThePaletteAndKeepsTheCommand(t *testing.T) {
	m := newTestModel(t)
	m.Update(runes("/"))
	if m.mode != modePalette {
		t.Fatalf("mode %v after /", m.mode)
	}
	for _, r := range []string{"r", "e", "s", "u", "m", "e"} {
		m.Update(runes(r))
	}
	m.Update(tea.KeyMsg{Type: tea.KeySpace})
	if m.mode == modePalette {
		t.Fatal("a space must close the palette")
	}
	for _, r := range []string{"A", "B", "C", "1", "2", "3"} {
		m.Update(runes(r))
	}
	if got := m.input.Value(); got != "/resume ABC123" {
		t.Fatalf("input = %q, want %q", got, "/resume ABC123")
	}
}
