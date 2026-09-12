package agent

import (
	"context"
	"testing"

	"github.com/brown-enterprises/be-code/internal/provider"
)

func TestInboxPeekRemoveAndHold(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, nil)
	ag.Enqueue("first")
	ag.Enqueue("second")
	if got := ag.Peek(); len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("peek = %v", got)
	}
	if text, ok := ag.Remove(0); !ok || text != "first" {
		t.Fatalf("remove = %q %v", text, ok)
	}
	if _, ok := ag.Remove(5); ok {
		t.Fatal("removing a gone index must report false")
	}
	if got := ag.Peek(); len(got) != 1 || got[0] != "second" {
		t.Fatalf("after remove peek = %v", got)
	}
}

// While the UI holds the queue open for editing, the loop must not deliver
// anything; after release it delivers as usual.
func TestHeldInboxIsNotDelivered(t *testing.T) {
	var ag *Agent
	calls := 0
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		switch calls {
		case 1:
			ag.Enqueue("change of plan")
			ag.Hold(true)
			return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "1", Name: "list_dir", Arguments: "{}"}}}, nil
		case 2:
			last := req.Messages[len(req.Messages)-1]
			if last.Role == provider.RoleUser {
				t.Errorf("held message was delivered: %q", last.Content)
			}
			ag.Hold(false)
			return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "2", Name: "list_dir", Arguments: "{}"}}}, nil
		case 3:
			last := req.Messages[len(req.Messages)-1]
			if last.Role != provider.RoleUser || last.Content == "" {
				t.Errorf("released message not delivered: %+v", last)
			}
			return &provider.ChatResponse{Content: "done"}, nil
		}
		return &provider.ChatResponse{Content: "done"}, nil
	}}
	ag, _ = newTestAgent(t, p, nil)
	if _, err := ag.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d", calls)
	}
}
