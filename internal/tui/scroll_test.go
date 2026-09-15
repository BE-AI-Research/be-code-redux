package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/provider"
)

type nullProvider struct{}

func (nullProvider) Name() string { return "null" }
func (nullProvider) Chat(context.Context, provider.ChatRequest, provider.StreamFunc) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{}, nil
}
func (nullProvider) ListModels(context.Context) ([]provider.ModelInfo, error) { return nil, nil }
func (nullProvider) Ping(context.Context) (string, error)                     { return "ok", nil }

// The transcript viewport must be a real, initialized viewport: PgUp and the
// mouse wheel scroll it. A zero-valued viewport.Model has an empty KeyMap
// and mouse wheel disabled, so every scroll key was silently ignored.
func TestTranscriptScrollsWithPageUpAndWheel(t *testing.T) {
	m := newTestModel(t)
	for i := 0; i < 200; i++ {
		m.appendEntry(entry{Kind: entryPlain, Text: strings.Repeat("x", 10)})
	}
	flush(m)
	if !m.vp.AtBottom() {
		t.Fatal("expected transcript pinned to bottom after append")
	}
	bottom := m.vp.YOffset

	m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	if m.vp.YOffset >= bottom {
		t.Fatalf("PgUp did not scroll: offset %d (bottom %d)", m.vp.YOffset, bottom)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	if m.vp.YOffset != bottom {
		t.Fatalf("PgDown did not return to bottom: %d != %d", m.vp.YOffset, bottom)
	}
	m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
	if m.vp.YOffset >= bottom {
		t.Fatalf("mouse wheel did not scroll: offset %d (bottom %d)", m.vp.YOffset, bottom)
	}
}

// Switching provider must go through SetModel so the model profile
// (compat mode, think-stripping, tool catalog) follows the new model.
func TestSetProviderAppliesModelProfile(t *testing.T) {
	m := newTestModel(t)
	m.cfg.Model = "deepseek-r1:7b"
	m.setProvider("llamacpp")
	if m.ag.Profile.Family != "deepseek-r1" {
		t.Fatalf("profile not refreshed: family=%q model=%q", m.ag.Profile.Family, m.ag.Model)
	}
}

// The status bar's context percentage is relative to the point where
// compaction actually triggers (the usable limit), so 100% means "about to
// compact", not "about to hit the raw window".
func TestUsageSnapshotUsesLimit(t *testing.T) {
	m := newTestModel(t)
	m.ag.ApplyWindow(8192)
	if got := m.usageSnapshot().budget; got != m.ag.History.Limit() {
		t.Fatalf("budget in status = %d, want limit %d", got, m.ag.History.Limit())
	}
}

// While a run is in progress, Enter queues the typed text for the agent
// instead of being ignored, and the transcript says so.
func TestBusyEnterQueuesMessage(t *testing.T) {
	m := newTestModel(t)
	m.mode = modeBusy
	m.input.SetValue("also add tests")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.ag.Pending() != 1 {
		t.Fatalf("pending = %d, want 1", m.ag.Pending())
	}
	if m.mode != modeBusy {
		t.Fatal("queuing must not change mode")
	}
	if !strings.Contains(m.rendered.String(), "queued") {
		t.Fatal("transcript does not show the queued message")
	}
}

// Whatever is still queued when a run finishes becomes the next turn.
func TestLeftoverQueueStartsNextTurn(t *testing.T) {
	m := newTestModel(t)
	m.mode = modeBusy
	var ran string // stands in for the next turn's run, and records it
	m.startTurnHook = func(text string) { ran = text }
	m.ag.Enqueue("next thing please")
	m.finishTurn(nil, nil)
	flush(m)
	if ran != "next thing please" {
		t.Fatalf("the next turn ran %q, want the queued text", ran)
	}
	if m.mode != modeBusy {
		t.Fatalf("expected a new turn to start, mode=%v", m.mode)
	}
	if !strings.Contains(m.rendered.String(), "next thing please") {
		t.Fatal("queued text not echoed as the new turn")
	}
}

// Esc cancels the run and discards the queue (the user is stopping, not
// scheduling more work).
func TestEscDiscardsQueue(t *testing.T) {
	m := newTestModel(t)
	m.mode = modeBusy
	m.cancelFn = func() {}
	m.ag.Enqueue("more")
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.ag.Pending() != 0 {
		t.Fatalf("queue survived cancel: %d", m.ag.Pending())
	}
}

// A short transcript sits at the bottom of its viewport, just above the
// input line, not at the top with blank rows under it. A terminal that shows
// only the bottom of a frame taller than its screen (a phone's pty reports
// the rows under the keyboard) then still sees the newest lines; and a long
// transcript already reads that way, so the two states look alike.
func TestShortTranscriptIsAnchoredToTheBottom(t *testing.T) {
	m := newTestModel(t) // 80x24: header hidden, viewport 24-3-1-1 = 19 rows
	m.appendEntry(entry{Kind: entryDim, Text: "first line"})
	m.appendEntry(entry{Kind: entryDim, Text: "second line"})
	flush(m)
	rows := strings.Split(m.View(), "\n")
	vpRows := rows[:m.vp.Height]
	// The transcript keeps its one blank row between the last line and the
	// input (every entry ends in a newline), as a full viewport shows it.
	h := len(vpRows)
	if strings.TrimSpace(vpRows[h-1]) != "" || !strings.Contains(vpRows[h-2], "second line") || !strings.Contains(vpRows[h-3], "first line") {
		t.Fatalf("short transcript is not anchored to the bottom of the viewport:\n%s", strings.Join(vpRows, "\n"))
	}
	if strings.TrimSpace(vpRows[0]) != "" {
		t.Fatalf("top viewport row should be padding, got %q", vpRows[0])
	}
	// Mouse coordinates still map onto the wrapped lines: the row above the
	// blank one is the second entry.
	line, _, ok := m.transcriptCoords(0, m.headerHeight()+m.vp.Height-2)
	if !ok || !strings.Contains(m.plainLines()[line], "second line") {
		t.Fatalf("coords: ok=%v line=%d %q", ok, line, m.plainLines()[min(line, len(m.plainLines())-1)])
	}
	// Once the transcript outgrows the viewport, no padding remains.
	for i := 0; i < 40; i++ {
		m.appendEntry(entry{Kind: entryDim, Text: "more"})
	}
	flush(m)
	if strings.TrimSpace(strings.Split(m.View(), "\n")[0]) == "" {
		t.Fatal("padding left in a full viewport")
	}
}
