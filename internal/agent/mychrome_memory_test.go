package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/browser/browsertest"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// In the person's own Chrome, working memory — which the model reads back
// and the task documents keep — records a withheld page by its host alone,
// never its path or query.
func TestMyChromeWorkingMemoryKeepsOnlyTheHost(t *testing.T) {
	fb := browsertest.New(t)
	fb.SetWSOnly(true)
	ps := browsertest.NewPage(fb, "https://bank.test/accounts", "My accounts — Bank", browsertest.EmptyTree)
	ps.Lock()
	ps.Pages["https://acme.test/login"] = [2]string{"Sign in", browsertest.FormTree}
	ps.Pages["https://mail.test/inbox?q=SECRETQUERY"] = [2]string{"Inbox", browsertest.FormTree}
	ps.ReadText = "SECRET MAIL"
	ps.Unlock()
	dir := t.TempDir()
	fb.WritePortFile(dir)

	calls := 0
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		switch calls {
		case 1:
			return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "1", Name: "browser", Arguments: `{"action":"open","url":"https://acme.test/login"}`}}}, nil
		case 2:
			// A redirect lands while the page is being read.
			ps.NavigateOn("Runtime.evaluate", "https://mail.test/inbox?q=SECRETQUERY")
			return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "2", Name: "browser", Arguments: `{"action":"read"}`}}}, nil
		}
		return &provider.ChatResponse{Content: "done"}, nil
	}}
	ag, _ := newTestAgent(t, p, nil)
	bt := tools.NewBrowser(tools.BrowserConfig{UseMyChrome: true, ChromeUserDataDir: dir,
		SnapshotChars: 12000, SettleTimeout: 2, QuietWindow: 20 * time.Millisecond})
	ag.Tools.AddTool(bt)
	ag.Tools.Approve = func(string, string) bool { return true }
	t.Cleanup(ag.Tools.Close)
	st := withEngine(t, ag)
	id := st.Plan("check mail", []string{"look"})
	st.SetStatus(id+".1", "doing", "")
	ag.RefreshSystem()
	if _, err := ag.Run(context.Background(), "check the page"); err != nil {
		t.Fatal(err)
	}
	var withheld string
	for _, m := range p.reqs[len(p.reqs)-1].Messages {
		if m.Role == provider.RoleTool && m.ToolCallID == "2" {
			withheld = m.Content
		}
	}
	if !strings.Contains(withheld, "the page on mail.test is not shown") {
		t.Fatalf("the read was not withheld:\n%s", withheld)
	}
	ev, _ := json.Marshal(st.Tree().Find(id + ".1").Evidence)
	if strings.Contains(string(ev), "SECRET") || strings.Contains(string(ev), "inbox") || !strings.Contains(string(ev), "mail.test") {
		t.Fatalf("working memory evidence:\n%s", ev)
	}
	shown := shownBeyondTheConversation(ag)
	if strings.Contains(shown, "SECRET") || strings.Contains(shown, "/inbox") {
		t.Fatalf("rendered working memory:\n%s", shown)
	}
}
