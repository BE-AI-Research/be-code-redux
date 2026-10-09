package tui

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Every action that handleAskKey puts in the no-"always" group must also have
// a case in the title/hint switch beside it. Without one it falls through to
// the defaults — title "Shell command", and a hint offering "a auto-approve
// all shell commands this session" for a key that is a no-op there. The
// compact-hint comment in view.go states the rule: "a bare y/n/a would
// advertise a key that does nothing."
//
// This is a source-reading guard because the two lists sit twenty lines apart
// in one method and have drifted once per feature for several features.
func TestEveryNoAlwaysActionHasItsOwnTitleAndHint(t *testing.T) {
	b, err := os.ReadFile("view.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)

	// The no-"always" group: the condition in handleAskKey that sets
	// decided = false.
	start := strings.Index(body, `a.Action == "browser_watch" || a.Action == "shell_after_web"`)
	if start < 0 {
		t.Fatal("could not find the no-\"always\" group in handleAskKey")
	}
	group := body[start : start+1200]
	if cut := strings.Index(group, "{"); cut > 0 {
		group = group[:cut]
	}
	actions := regexp.MustCompile(`a\.Action == "([a-z_]+)"`).FindAllStringSubmatch(group, -1)
	if len(actions) < 5 {
		t.Fatalf("only found %d actions in the no-\"always\" group; the parse is wrong", len(actions))
	}

	for _, m := range actions {
		action := m[1]
		if !strings.Contains(body, `case "`+action+`":`) {
			t.Errorf("%q has no \"always\" and no case in the title/hint switch, so it is titled "+
				"\"Shell command\" and offers an \"a\" key that does nothing", action)
		}
	}
}
