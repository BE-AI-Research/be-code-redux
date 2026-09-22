package agent

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/engine"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/tools"
)

func fixClock(t *testing.T) func(time.Duration) {
	t.Helper()
	at := time.Date(2026, 9, 20, 14, 32, 7, 0, time.Local)
	prev := timeNow
	timeNow = func() time.Time { return at }
	t.Cleanup(func() { timeNow = prev })
	return func(d time.Duration) { at = at.Add(d) }
}

// Every tool result ends with the time, what the call cost and how full the
// context is: appended text, so the server's prompt cache is untouched, and a
// running series the model can read its own pace from.
func TestToolResultsCarryATimeFooter(t *testing.T) {
	fixClock(t)
	ag, _ := newTestAgent(t, &scriptedProvider{}, nil)
	ag.Tools.AddTool(&countingTool{name: "probe"})
	var shown string
	ag.Events.OnToolEnd = func(_ string, r tools.Result) { shown = r.Content }

	res := ag.dispatch(context.Background(), provider.ToolCall{Name: "probe", Arguments: `{}`})
	re := regexp.MustCompile(`\n\[14:32:07 · took [0-9.]+(ms|s) · context \d+%\]$`)
	if !re.MatchString(res.Content) {
		t.Fatalf("no time footer on the model's copy: %q", res.Content)
	}
	if strings.Contains(shown, "14:32:07") {
		t.Fatalf("the footer is for the model; the UI's copy should not carry it: %q", shown)
	}
}

func TestTheTimeFooterNamesTheOpenStep(t *testing.T) {
	advance := fixClock(t)
	ag, _ := newTestAgent(t, &scriptedProvider{}, nil)
	ag.Tools.AddTool(&countingTool{name: "probe"})
	st := withEngine(t, ag)
	id := st.Plan("t", []string{"a"})
	st.SetStatus(id+".1", engine.StatusDoing, "")
	started, _ := func() (time.Time, bool) { _, s, ok := st.DoingClock(); return s, ok }()
	timeNow = func() time.Time { return started.Add(14 * time.Minute) }
	_ = advance
	res := ag.dispatch(context.Background(), provider.ToolCall{Name: "probe", Arguments: `{}`})
	if !strings.Contains(res.Content, "· step "+id+".1 open 14m ·") {
		t.Fatalf("the open step is not in the footer: %q", res.Content)
	}
}

func TestTimeAwarenessCanBeTurnedOff(t *testing.T) {
	fixClock(t)
	ag, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { c.TimeAwareness = false })
	ag.Tools.AddTool(&countingTool{name: "probe"})
	res := ag.dispatch(context.Background(), provider.ToolCall{Name: "probe", Arguments: `{}`})
	if res.Content != "same as ever" {
		t.Fatalf("footer with time_awareness off: %q", res.Content)
	}
	if got := ag.stampUser("hello"); got != "hello" {
		t.Fatalf("stamp with time_awareness off: %q", got)
	}
}

// The user's own messages say when they arrived, so a model resuming after a
// night away can tell.
func TestUserMessagesAreStampedOnArrival(t *testing.T) {
	fixClock(t)
	var sent []provider.Message
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		sent = req.Messages
		return &provider.ChatResponse{Content: "ok"}, nil
	}}
	ag, _ := newTestAgent(t, p, nil)
	if _, err := ag.Run(context.Background(), "lets resume"); err != nil {
		t.Fatal(err)
	}
	last := sent[len(sent)-1]
	if last.Role != provider.RoleUser || !strings.HasPrefix(last.Content, "lets resume") ||
		!strings.HasSuffix(last.Content, "\n\n[sent Sun 20 Sep 2026 14:32]") {
		t.Fatalf("user message not stamped: %q", last.Content)
	}
}

// The repeat detector compares results; a clock in them must not make two
// identical answers look different.
func TestTheTimeFooterDoesNotHideARepeat(t *testing.T) {
	advance := fixClock(t)
	ag, _ := newTestAgent(t, &scriptedProvider{}, nil)
	ag.Tools.AddTool(&countingTool{name: "probe"})
	var res tools.Result
	for i := 0; i < 3; i++ {
		res = ag.dispatch(context.Background(), provider.ToolCall{Name: "probe", Arguments: `{}`})
		advance(time.Second)
	}
	if !strings.Contains(res.Content, "this exact call 3 times") {
		t.Fatalf("the clock hid the repeat: %q", res.Content)
	}
}
