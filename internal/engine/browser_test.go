package engine

import (
	"encoding/json"
	"strings"
	"testing"
)

// Final review M1: working memory counts a browser call, recording its
// action and the page URL — never any text the page supplied.
func TestBrowserCallIsRecordedWithoutPageText(t *testing.T) {
	root, dir := t.TempDir(), t.TempDir()
	s, err := OpenAt(dir, root, "s1", false, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	id := s.Plan("look something up", []string{"read the docs"})
	s.SetStatus(id+".1", StatusDoing, "")
	s.Observe(Event{Tool: "browser",
		Args:    map[string]any{"action": "type", "ref": "e3", "text": "SECRET PAGE TEXT", "url": "https://docs.test/page"},
		Content: "page: SECRET PAGE TEXT — docs.test/page\nbutton \"SECRET PAGE TEXT\" [e4]"})
	s.Observe(Event{Tool: "browser", Args: map[string]any{"action": "click", "url": "https://docs.test/page"},
		Content: "SECRET PAGE TEXT", IsError: true})
	n := s.Tree().Find(id + ".1")
	if n.Calls != 2 || len(n.Evidence.Raw) != 2 {
		t.Fatalf("browser calls not recorded: calls=%d raw=%d", n.Calls, len(n.Evidence.Raw))
	}
	b, _ := json.Marshal(n.Evidence)
	if strings.Contains(string(b), "SECRET PAGE TEXT") {
		t.Fatalf("page text reached working memory:\n%s", b)
	}
	if !strings.Contains(n.Evidence.Raw[0].Args, "https://docs.test/page") || !strings.Contains(n.Evidence.Raw[0].Args, "type") {
		t.Fatalf("the item lacks the action and URL: %+v", n.Evidence.Raw[0])
	}
	s.SetStatus(id+".1", StatusDone, "")
	b, _ = json.Marshal(s.Tree().Find(id + ".1").Evidence)
	if strings.Contains(string(b), "SECRET PAGE TEXT") {
		t.Fatalf("page text reached the distilled record:\n%s", b)
	}
}
