package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/brown-enterprises/be-code/internal/live"
)

func runes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func twoClients(t *testing.T) *Model {
	t.Helper()
	m := newTestModel(t)
	m.served = true
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.Update(clientsMsg{{ID: 1, Label: "desk (pid 1)", UTF8: true}, {ID: 2, Label: "tablet (pid 2)", UTF8: true}})
	return m
}

func TestEachClientTypesIntoItsOwnInput(t *testing.T) {
	m := twoClients(t)
	m.Update(live.ClientKeyMsg{Client: 1, Key: runes("hello")})
	m.Update(live.ClientKeyMsg{Client: 2, Key: runes("world")})
	m.Update(live.ClientKeyMsg{Client: 1, Key: runes("!")})
	if got := m.inputFor(1).Value(); got != "hello!" {
		t.Fatalf("client 1 input %q", got)
	}
	if got := m.inputFor(2).Value(); got != "world" {
		t.Fatalf("client 2 input %q", got)
	}
	// The shared frame leaves the input columns blank on every input row:
	// the host splices each client's own line in there.
	lines := strings.Split(m.View(), "\n")
	block := lines[len(lines)-1-m.inputRows() : len(lines)-1]
	for i, ln := range block {
		row := []rune(ansi.Strip(ln))
		if len(row) < m.inputWidth() || strings.TrimSpace(string(row[:m.inputWidth()])) != "" {
			t.Fatalf("input row %d is not blank for its first %d columns: %q", i, m.inputWidth(), ln)
		}
	}
	if strings.Contains(m.View(), "hello!") || strings.Contains(m.View(), "world") {
		t.Fatal("shared frame must not render any client's input text")
	}
}

func TestEnterQueuesOnlyTheSendersTextWithLabel(t *testing.T) {
	m := twoClients(t)
	m.mode = modeBusy
	m.running = true
	m.Update(live.ClientKeyMsg{Client: 2, Key: runes("do it later")})
	m.Update(live.ClientKeyMsg{Client: 1, Key: runes("mine")})
	m.Update(live.ClientKeyMsg{Client: 2, Key: tea.KeyMsg{Type: tea.KeyEnter}})
	items := m.ag.Items()
	if len(items) != 1 || items[0].Text != "do it later" || items[0].From != 2 {
		t.Fatalf("queue: %+v", items)
	}
	if got := m.inputFor(1).Value(); got != "mine" {
		t.Fatalf("client 1's draft was disturbed: %q", got)
	}
	if !strings.Contains(m.transcript.String(), "tablet (pid 2)> ") {
		t.Fatalf("queued line lacks the sender label:\n%s", m.transcript.String())
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

func TestQueuePopupShowsOnlyTheOpenersMessages(t *testing.T) {
	m := twoClients(t)
	m.mode = modeBusy
	m.running = true
	m.ag.EnqueueFrom("from one", 1)
	m.ag.EnqueueFrom("from two", 2)
	m.Update(live.ClientKeyMsg{Client: 2, Key: tea.KeyMsg{Type: tea.KeyUp}})
	if m.mode != modeQueue {
		t.Fatal("Up on an empty input opens the queue popup")
	}
	v := m.View()
	if !strings.Contains(v, "from two") || strings.Contains(v, "from one") {
		t.Fatalf("popup must list only the opener's messages:\n%s", v)
	}
	m.Update(live.ClientKeyMsg{Client: 2, Key: tea.KeyMsg{Type: tea.KeyEnter}}) // edit
	if got := m.inputFor(2).Value(); got != "from two" {
		t.Fatalf("edit pulled %q into client 2's input", got)
	}
	if items := m.ag.Items(); len(items) != 1 || items[0].From != 1 {
		t.Fatalf("client 1's message must remain queued: %+v", items)
	}
}

func TestPaletteIsOwnedByTheClientThatOpenedIt(t *testing.T) {
	m := twoClients(t)
	m.Update(live.ClientKeyMsg{Client: 2, Key: runes("/")})
	if m.mode != modePalette || m.paletteOwner != 2 {
		t.Fatalf("palette owner %d mode %v", m.paletteOwner, m.mode)
	}
	m.Update(live.ClientKeyMsg{Client: 1, Key: tea.KeyMsg{Type: tea.KeyEsc}})
	if m.mode != modePalette {
		t.Fatal("another client's Esc must not close the owner's palette")
	}
	m.Update(clientsMsg{{ID: 1, Label: "desk (pid 1)", UTF8: true}}) // owner detached
	if m.mode == modePalette {
		t.Fatal("palette must close when its owner detaches")
	}
	if m.inputs[2] != nil {
		t.Fatal("detached client's textarea must be dropped")
	}
}

func TestInProcessModelStillUsesClientZero(t *testing.T) {
	m := newTestModel(t)
	m.Update(runes("abc"))
	if got := m.inputFor(0).Value(); got != "abc" {
		t.Fatalf("plain KeyMsg goes to client 0: %q", got)
	}
	if !strings.Contains(m.View(), "abc") {
		t.Fatal("in-process view renders the single textarea")
	}
}

func TestDetachCommandDetachesTheTypingClient(t *testing.T) {
	m := twoClients(t)
	var detached []int
	m.detachClient = func(id int) { detached = append(detached, id) }
	m.Update(live.ClientKeyMsg{Client: 2, Key: runes("/detach")})
	_, cmd := m.Update(live.ClientKeyMsg{Client: 2, Key: tea.KeyMsg{Type: tea.KeyEnter}})
	if cmd != nil {
		cmd()
	}
	if len(detached) != 1 || detached[0] != 2 {
		t.Fatalf("detached %v", detached)
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
// submitted — but each terminal keeps its own place in it, so one client's
// Up never moves another's.
func TestInputHistoryCursorIsPerClient(t *testing.T) {
	m := twoClients(t)
	// Submit while a run is in progress: Enter records the line in the
	// shared history exactly as it does in modeInput, but queues it instead
	// of launching three overlapping agent runs at one Agent.
	m.mode = modeBusy
	m.running = true
	for _, step := range []struct {
		client int
		text   string
	}{{1, "one"}, {1, "two"}, {2, "three"}} {
		m.Update(live.ClientKeyMsg{Client: step.client, Key: runes(step.text)})
		m.Update(live.ClientKeyMsg{Client: step.client, Key: tea.KeyMsg{Type: tea.KeyEnter}})
	}
	m.ag.DrainInbox()
	m.mode, m.running = modeInput, false
	// Client 1 last submitted "two", so its cursor sits where the list ended
	// then: Up walks back from the newest line in the shared store.
	m.Update(live.ClientKeyMsg{Client: 1, Key: tea.KeyMsg{Type: tea.KeyUp}})
	if got := m.inputFor(1).Value(); got != "three" {
		t.Fatalf("client 1 first Up = %q, want the newest shared line", got)
	}
	m.Update(live.ClientKeyMsg{Client: 1, Key: tea.KeyMsg{Type: tea.KeyUp}})
	if got := m.inputFor(1).Value(); got != "two" {
		t.Fatalf("client 1 second Up = %q", got)
	}
	// Client 2 has not navigated at all: its own Up starts from the newest.
	m.Update(live.ClientKeyMsg{Client: 2, Key: tea.KeyMsg{Type: tea.KeyUp}})
	if got := m.inputFor(2).Value(); got != "three" {
		t.Fatalf("client 2 Up = %q; client 1's navigation moved its cursor", got)
	}
	// ...and client 2's navigation left client 1 where it was.
	m.Update(live.ClientKeyMsg{Client: 1, Key: tea.KeyMsg{Type: tea.KeyUp}})
	if got := m.inputFor(1).Value(); got != "one" {
		t.Fatalf("client 1 third Up = %q; client 2's navigation moved its cursor", got)
	}
}

// The menu acts for the terminal that opened it: another client cannot
// drive it, its entries run as the owner, and it closes if the owner goes.
func TestMenuIsOwnedByTheClientThatOpenedIt(t *testing.T) {
	m := twoClients(t)
	var detached []int
	m.detachClient = func(id int) { detached = append(detached, id) }

	m.Update(live.ClientKeyMsg{Client: 1, Key: runes("/menu")})
	m.Update(live.ClientKeyMsg{Client: 1, Key: tea.KeyMsg{Type: tea.KeyEnter}})
	if m.mode != modeMenu || m.menuOwner != 1 {
		t.Fatalf("menu owner %d mode %v", m.menuOwner, m.mode)
	}
	m.Update(live.ClientKeyMsg{Client: 2, Key: tea.KeyMsg{Type: tea.KeyEnter}})
	if m.mode != modeMenu {
		t.Fatal("another client's Enter must not drive the owner's menu")
	}

	m.Update(live.ClientKeyMsg{Client: 1, Key: runes("Detach")})
	_, cmd := m.Update(live.ClientKeyMsg{Client: 1, Key: tea.KeyMsg{Type: tea.KeyEnter}})
	if cmd != nil {
		cmd()
	}
	if len(detached) != 1 || detached[0] != 1 {
		t.Fatalf("menu entry detached %v, want the owner", detached)
	}

	m.Update(live.ClientKeyMsg{Client: 1, Key: runes("/menu")})
	m.Update(live.ClientKeyMsg{Client: 1, Key: tea.KeyMsg{Type: tea.KeyEnter}})
	if m.mode != modeMenu {
		t.Fatalf("menu did not reopen: %v", m.mode)
	}
	m.Update(clientsMsg{{ID: 2, Label: "tablet (pid 2)", UTF8: true}}) // owner detached
	if m.mode == modeMenu {
		t.Fatal("menu must close when its owner detaches")
	}
}

// The bottom line never overruns the shared width, however long the model
// name and however many terminals are attached: the label list is
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
	m := twoClients(t)
	m.mode = modeBusy
	m.running = true
	m.ag.EnqueueFrom("from one", 1)
	m.ag.EnqueueFrom("from two", 2)
	m.Update(turnDoneMsg{})
	tr := m.transcript.String()
	for _, want := range []string{"desk (pid 1)> from one", "tablet (pid 2)> from two"} {
		if !strings.Contains(tr, want) {
			t.Fatalf("transcript lacks %q:\n%s", want, tr)
		}
	}
	if m.mode != modeBusy || !m.running {
		t.Fatalf("leftovers did not start the next turn: mode=%v running=%v", m.mode, m.running)
	}
	if m.ag.Pending() != 0 {
		t.Fatalf("queue not drained: %d", m.ag.Pending())
	}
}

// A queue popup whose owner detaches after the run has already finished
// must not leave the session sitting in the busy mode.
func TestQueuePopupClosesToIdleWhenOwnerDetaches(t *testing.T) {
	m := twoClients(t)
	m.mode = modeBusy
	m.running = true
	m.ag.EnqueueFrom("from two", 2)
	m.Update(live.ClientKeyMsg{Client: 2, Key: tea.KeyMsg{Type: tea.KeyUp}})
	if m.mode != modeQueue {
		t.Fatalf("popup not open: %v", m.mode)
	}
	m.running = false // the run finished while the popup was open
	m.Update(clientsMsg{{ID: 1, Label: "desk (pid 1)", UTF8: true}})
	if m.mode != modeInput {
		t.Fatalf("mode after the owner detached = %v, want input", m.mode)
	}
	if m.ag.Held() {
		t.Fatal("delivery still held after the popup closed")
	}
}

// A popup belongs to one terminal, but the keyboard of every other terminal
// must keep working: a guest's keys go to that guest's own input line (in
// the mode underneath), and never to the owner's popup.
func TestGuestKeysReachTheirOwnInputWhileAPopupIsOpen(t *testing.T) {
	m := twoClients(t)
	m.Update(live.ClientKeyMsg{Client: 1, Key: runes("/")})
	if m.mode != modePalette || m.paletteOwner != 1 {
		t.Fatalf("palette owner %d mode %v", m.paletteOwner, m.mode)
	}
	for _, r := range []string{"x", "y", "z"} {
		m.Update(live.ClientKeyMsg{Client: 2, Key: runes(r)})
	}
	if got := m.inputFor(2).Value(); got != "xyz" {
		t.Fatalf("guest's typing landed in %q, want %q in its own input", got, "xyz")
	}
	if m.mode != modePalette || m.paletteOwner != 1 {
		t.Fatalf("guest's typing disturbed the owner's palette: mode %v owner %d", m.mode, m.paletteOwner)
	}
	if p := m.picker; p == nil || p.filter != "" {
		t.Fatalf("guest's runes reached the palette filter: %+v", p)
	}
	// The guest's Esc clears its own selection, and does not close the
	// owner's palette.
	m.Update(live.ClientKeyMsg{Client: 2, Key: tea.KeyMsg{Type: tea.KeyEsc}})
	if m.mode != modePalette {
		t.Fatal("guest's Esc closed the owner's palette")
	}
	// The guest's Ctrl+C clears only its own draft.
	m.Update(live.ClientKeyMsg{Client: 2, Key: tea.KeyMsg{Type: tea.KeyCtrlC}})
	if got := m.inputFor(2).Value(); got != "" {
		t.Fatalf("guest's Ctrl+C left %q in its own draft", got)
	}
	if m.mode != modePalette {
		t.Fatal("guest's Ctrl+C closed the owner's palette")
	}
	// And the owner's own Esc still closes it.
	m.Update(live.ClientKeyMsg{Client: 1, Key: tea.KeyMsg{Type: tea.KeyEsc}})
	if m.mode == modePalette {
		t.Fatal("the owner's Esc must close its own palette")
	}
}

// The same for the menu and the queue popup, and with a run in progress —
// where the mode underneath is modeBusy, so a guest's Enter queues its
// message instead of starting a turn.
func TestGuestKeysQueueWhileAnotherClientBrowsesTheMenu(t *testing.T) {
	m := twoClients(t)
	m.mode, m.running = modeBusy, true
	m.Update(live.ClientKeyMsg{Client: 1, Key: tea.KeyMsg{Type: tea.KeyCtrlQ}}) // queue popup, owner 1
	if m.mode != modeQueue {
		// No queued messages for client 1: openQueue says so and stays put.
		m.mode, m.queueOwner = modeQueue, 1
	}
	m.Update(live.ClientKeyMsg{Client: 2, Key: runes("later please")})
	m.Update(live.ClientKeyMsg{Client: 2, Key: tea.KeyMsg{Type: tea.KeyEnter}})
	if m.mode != modeQueue || m.queueOwner != 1 {
		t.Fatalf("guest's keys disturbed the owner's queue popup: mode %v owner %d", m.mode, m.queueOwner)
	}
	items := m.ag.Items()
	if len(items) != 1 || items[0].Text != "later please" || items[0].From != 2 {
		t.Fatalf("guest's Enter must queue its own text: %+v", items)
	}
	// A guest key that would open a popup of its own leaves the owner's
	// popup exactly where it was, and never leaves delivery held.
	m.Update(live.ClientKeyMsg{Client: 2, Key: tea.KeyMsg{Type: tea.KeyCtrlQ}})
	if m.mode != modeQueue || m.queueOwner != 1 {
		t.Fatalf("guest opened a popup of its own: mode %v owner %d", m.mode, m.queueOwner)
	}
}

// Ctrl+C twice quits — but only from the same terminal. A Ctrl+C on one
// terminal and a Ctrl+C on another are two people each clearing their own
// line, not a session-ending confirmation.
func TestQuitHintIsPerClient(t *testing.T) {
	m := twoClients(t)
	if _, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC}, 1); cmd != nil {
		t.Fatal("the first Ctrl+C must only arm the hint")
	}
	if _, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC}, 2); cmd != nil {
		t.Fatal("another terminal's Ctrl+C must not quit")
	}
	_, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC}, 2)
	if cmd == nil {
		t.Fatal("the same terminal's second Ctrl+C must quit")
	}
	if msg := cmd(); msg != tea.Quit() {
		t.Fatalf("second Ctrl+C returned %T, want a quit", msg)
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
	if got := m.inputFor(0).Value(); got != "/resume ABC123" {
		t.Fatalf("input = %q, want %q", got, "/resume ABC123")
	}
}
