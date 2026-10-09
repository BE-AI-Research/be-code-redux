package tui

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/agent"
)

// writeProjectPack puts a pack where discovery finds a project pack, so
// activating it needs the trust question — the one that has to be asked from
// outside Update.
func writeProjectPack(t *testing.T, root, name string) {
	t.Helper()
	dir := filepath.Join(root, ".becode", "packs", name)
	if err := os.MkdirAll(filepath.Join(dir, "chunks"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"schema":1,"name":"` + name + `","version":"1.0.0","description":"a test pack",
		"core":{"file":"core.md","budget_tokens":1500},
		"chunks":{"things":{"file":"chunks/things.md","summary":"how things work"}},
		"detect":{"all":["go.mod"]}}`
	for rel, s := range map[string]string{
		"pack.json":        body,
		"core.md":          "pack rules",
		"chunks/things.md": "the things chunk",
	} {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Update holds the session lock for its whole body and its own comment says
// nothing under that lock may block on a program. /packs ran inline there, so
// a workspace with an unapproved project pack deadlocked the whole TUI the
// moment somebody typed the command /packs itself advertises: LoadPacks ->
// trustProjectPack -> Approve -> Session.Ask, which relocks the same
// non-reentrant mutex. Every attached terminal froze, Esc and Ctrl-C
// included, with the alt screen still up.
//
// The timeout is the assertion. Without one this test would hang the package
// rather than fail it.
func TestPacksCommandDoesNotBlockUpdate(t *testing.T) {
	v := newTestModel(t)
	writeProjectPack(t, v.ag.Tools.Root, "fromrepo")

	// An approver that waits until it is released, standing in for a person
	// with the modal open. If the command runs under Update's lock, this call
	// never even arrives, because Session.Ask blocks first.
	asked := make(chan struct{}, 1)
	release := make(chan struct{})
	v.ag.Tools.Approve = func(string, string) bool {
		asked <- struct{}{}
		<-release
		return true
	}

	// Update's contract, reproduced exactly: it holds the session lock for
	// its whole body and dispatches the command inside it, then Bubble Tea
	// runs whatever tea.Cmd came back — off that lock, on its own goroutine.
	done := make(chan struct{})
	var cmd tea.Cmd
	go func() {
		defer close(done)
		v.mu.Lock()
		defer v.mu.Unlock()
		_, cmd = v.slashCommand("/packs reload")
	}()

	select {
	case <-done:
		// Returned with the lock released, which is the point: the command
		// did not sit under it waiting for an answer.
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("/packs blocked while holding the session lock: Session.Ask would deadlock the TUI")
	}
	if cmd == nil {
		close(release)
		t.Fatal("/packs reload returned no command, so the work never runs")
	}
	go cmd()

	// The question must still actually be asked, from wherever it now runs.
	select {
	case <-asked:
	case <-time.After(5 * time.Second):
		t.Fatal("the trust question was never asked")
	}
	close(release)
}

// The lock must be genuinely free while the question is open: another
// terminal's keystroke has to keep working, which is the whole reason the ask
// is a shared, broadcast one.
func TestOtherTerminalsKeepWorkingWhileAPackQuestionIsOpen(t *testing.T) {
	s := newTestSession(t)
	v1 := s.NewView(0, "local")
	v1.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	v2 := s.NewView(1, "other")
	v2.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	writeProjectPack(t, s.ag.Tools.Root, "fromrepo")

	asked := make(chan struct{}, 1)
	release := make(chan struct{})
	s.ag.Tools.Approve = func(string, string) bool {
		asked <- struct{}{}
		<-release
		return true
	}

	go func() {
		var cmd tea.Cmd
		func() {
			v1.mu.Lock()
			defer v1.mu.Unlock()
			_, cmd = v1.slashCommand("/packs reload")
		}()
		if cmd != nil {
			cmd() // as Bubble Tea runs it: off the lock
		}
	}()
	select {
	case <-asked:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("the trust question was never asked")
	}

	// With the question open, the second terminal must still be served.
	typed := make(chan struct{})
	go func() {
		defer close(typed)
		v2.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	}()
	select {
	case <-typed:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("a second terminal was frozen while the pack question was open")
	}
	close(release)
}

var _ = agent.PackRow{} // keep the agent import honest if the helpers change
