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
