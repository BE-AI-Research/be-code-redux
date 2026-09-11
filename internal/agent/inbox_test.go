package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/provider"
)

// A message typed while the agent works is delivered to the model before
// its next call, after the tool results it was waiting on.
func TestQueuedMessageDeliveredBeforeNextModelCall(t *testing.T) {
	var ag *Agent
	calls := 0
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		switch calls {
		case 1:
			ag.Enqueue("also rename the output file to result.txt")
			return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "1", Name: "list_dir", Arguments: "{}"}}}, nil
		case 2:
			last := req.Messages[len(req.Messages)-1]
			if last.Role != provider.RoleUser || !strings.Contains(last.Content, "rename the output file") {
				t.Errorf("queued message not delivered as the latest user message: %+v", last)
			}
			prev := req.Messages[len(req.Messages)-2]
			if prev.Role != provider.RoleTool {
				t.Errorf("queued message should follow the tool result, got %s", prev.Role)
			}
			return &provider.ChatResponse{Content: "done"}, nil
		}
		return &provider.ChatResponse{Content: "done"}, nil
	}}
	ag, _ = newTestAgent(t, p, nil)
	notes := collectNotices(ag)
	if _, err := ag.Run(context.Background(), "write output"); err != nil {
		t.Fatal(err)
	}
	if ag.Pending() != 0 {
		t.Fatalf("inbox not drained: %d", ag.Pending())
	}
	if !hasNotice(*notes, "queued") {
		t.Fatalf("no delivery notice: %v", *notes)
	}
}

// A message queued during the model's final call cannot be delivered in
// that run; it stays queued so the UI can start the next turn with it.
func TestQueuedMessageLeftoverAfterRun(t *testing.T) {
	var ag *Agent
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		ag.Enqueue("and then?")
		return &provider.ChatResponse{Content: "final"}, nil
	}}
	ag, _ = newTestAgent(t, p, nil)
	if _, err := ag.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	left := ag.DrainInbox()
	if len(left) != 1 || left[0] != "and then?" {
		t.Fatalf("leftover = %v", left)
	}
	if ag.Pending() != 0 {
		t.Fatal("drain did not empty the inbox")
	}
}
