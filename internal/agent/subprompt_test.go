package agent

import (
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/subagent"
)

// A sub-agent told which branch holds the tree as it began can read any
// file as it was then with the show tool, and decide for itself whether a
// change under it matters — the owner's "both solutions work, and the
// primary and agent can then decide which is relevant".
func TestRenderDispatchNamesTheBaseBranch(t *testing.T) {
	d := subagent.Dispatch{Node: "1.1", Text: "port it", Scope: []string{"internal/scan"},
		Base: "be-code/sub-agent/20261010-120000"}
	got := renderDispatch(d)
	if !strings.Contains(got, "be-code/sub-agent/20261010-120000") || !strings.Contains(got, "show") {
		t.Errorf("the dispatch does not tell the sub-agent where the tree it began from is:\n%s", got)
	}
	if plain := renderDispatch(subagent.Dispatch{Node: "1.1", Text: "port it", Scope: []string{"x"}}); strings.Contains(plain, "began") {
		t.Errorf("no base, no sentence about one:\n%s", plain)
	}
}

// The hand-back names what changed under the sub-agent while it worked, so
// the main model knows its summary was written against older versions of
// those files, and where the older tree is.
func TestHandBackLineNamesWhatChangedUnderneath(t *testing.T) {
	hb := subagent.HandBack{Node: "1.1", Owner: "big", Status: "done", Summary: "ported",
		Files: []string{"internal/scan/token.go"}, Drifted: []string{"internal/scan/a.go"},
		Base: "be-code/sub-agent/20261010-120000"}
	got := handBackLine(hb)
	for _, want := range []string{"changed under it: internal/scan/a.go", "be-code/sub-agent/20261010-120000", "ported"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if !strings.HasPrefix(got, "sub-agent big finished 1.1 (done, ") {
		t.Errorf("the line's opening is the one the UIs and tests already read: %q", got)
	}
	quiet := handBackLine(subagent.HandBack{Node: "1.1", Owner: "big", Status: "done", Summary: "x"})
	if strings.Contains(quiet, "changed under") || strings.Contains(quiet, "sub-agent/") {
		t.Errorf("nothing moved, nothing said: %q", quiet)
	}
}
