package agent

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/store"
)

// The lock order nothing under saveMu may invert: autosave used to report a
// failed save through Events.OnNotice while still holding saveMu, and a UI
// whose notice handler takes its own lock — the TUI's Session.mu, which every
// room post holds while calling UpdateSession — then deadlocked every
// terminal against the agent goroutine.
func TestFailedAutosaveNoticesOffTheSaveLock(t *testing.T) {
	// A HOME that is a regular file: every path under it fails to create, so
	// Session.Save returns an error and autosave takes its notice branch.
	home := filepath.Join(t.TempDir(), "home")
	if err := os.WriteFile(home, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	ag, dir := newTestAgent(t, &scriptedProvider{}, nil)
	ag.SetSession(store.NewSession("p", "m", dir))

	// uiMu stands in for tui.Session.mu: the notice handler takes it, and so
	// does the goroutine calling UpdateSession.
	var uiMu sync.Mutex
	entered := make(chan struct{}, 1)
	ag.Events.OnNotice = func(string) {
		select {
		case entered <- struct{}{}:
		default:
		}
		uiMu.Lock()
		uiMu.Unlock()
	}

	uiMu.Lock() // the UI is mid-Update, as it is for every room post
	saved := make(chan struct{})
	go func() { ag.autosave("hello"); close(saved) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the save never failed, so the notice path was not exercised")
	}

	// autosave is inside the notice. UpdateSession must still be able to run:
	// under the old code it blocked on saveMu, which autosave was holding
	// while it waited for the lock this goroutine holds.
	updated := make(chan struct{})
	go func() {
		ag.UpdateSession(func(s *store.Session) { s.Chat = append(s.Chat, store.ChatLine{Text: "line"}) })
		close(updated)
	}()
	select {
	case <-updated:
	case <-time.After(5 * time.Second):
		uiMu.Unlock()
		t.Fatal("UpdateSession blocked: autosave is holding saveMu across an Events callback")
	}
	uiMu.Unlock()
	<-saved
}
