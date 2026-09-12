package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/tools"
)

type nullProvider struct{}

func (nullProvider) Name() string { return "null" }
func (nullProvider) Chat(context.Context, provider.ChatRequest, provider.StreamFunc) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{}, nil
}
func (nullProvider) ListModels(context.Context) ([]provider.ModelInfo, error) { return nil, nil }
func (nullProvider) Ping(context.Context) (string, error)                     { return "ok", nil }

// newTestModel builds a model and gives it its first WindowSizeMsg. Each
// prep func runs on the agent before the model is created, for state the
// UI reads at startup (e.g. an attached editor).
func newTestModel(t *testing.T, prep ...func(*agent.Agent)) *Model {
	t.Helper()
	cfg := config.Default()
	cfg.RepoMap = false
	reg, err := tools.NewRegistry(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(cfg, nullProvider{}, "m", reg, "")
	for _, f := range prep {
		f(ag)
	}
	m := New(cfg, ag, nullProvider{})
	m.rootCtx = context.Background()
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return m
}

// The transcript viewport must be a real, initialized viewport: PgUp and the
// mouse wheel scroll it. A zero-valued viewport.Model has an empty KeyMap
// and mouse wheel disabled, so every scroll key was silently ignored.
func TestTranscriptScrollsWithPageUpAndWheel(t *testing.T) {
	m := newTestModel(t)
	for i := 0; i < 200; i++ {
		m.appendLine(strings.Repeat("x", 10))
	}
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
	m.inputFor(0).SetValue("also add tests")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.ag.Pending() != 1 {
		t.Fatalf("pending = %d, want 1", m.ag.Pending())
	}
	if m.mode != modeBusy {
		t.Fatal("queuing must not change mode")
	}
	if !strings.Contains(m.transcript.String(), "queued") {
		t.Fatal("transcript does not show the queued message")
	}
}

// Whatever is still queued when a run finishes becomes the next turn.
func TestLeftoverQueueStartsNextTurn(t *testing.T) {
	m := newTestModel(t)
	m.mode = modeBusy
	m.ag.Enqueue("next thing please")
	m.Update(turnDoneMsg{})
	if m.mode != modeBusy {
		t.Fatalf("expected a new turn to start, mode=%v", m.mode)
	}
	if !strings.Contains(m.transcript.String(), "next thing please") {
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
