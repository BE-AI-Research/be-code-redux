package agent

import (
	"testing"

	"github.com/brown-enterprises/be-code/internal/store"
)

func TestUpdateSessionRunsUnderTheLockAndSurvivesNoSession(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, nil)
	ran := false
	ag.UpdateSession(func(*store.Session) { ran = true })
	if ran {
		t.Fatal("fn ran with no session")
	}
	ag.SetSession(&store.Session{ID: "s"})
	ag.UpdateSession(func(s *store.Session) { s.Chat = append(s.Chat, store.ChatLine{User: "alice", Text: "hi"}) })
	if len(ag.Session.Chat) != 1 {
		t.Fatalf("%+v", ag.Session.Chat)
	}
}

// UpdateSession from a UI goroutine while the agent autosaves: both touch the
// session's fields, so they share a lock. Run with -race.
func TestUpdateSessionDoesNotRaceAutosave(t *testing.T) {
	ag, dir := newTestAgent(t, &scriptedProvider{}, nil)
	ag.SetSession(&store.Session{ID: "20260921-000000-001"})
	t.Setenv("HOME", dir)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			ag.UpdateSession(func(s *store.Session) { s.Chat = append(s.Chat, store.ChatLine{User: "a", Text: "x"}) })
		}
	}()
	for i := 0; i < 200; i++ {
		ag.autosave("t")
	}
	<-done
}
