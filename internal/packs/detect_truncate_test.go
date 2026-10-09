package packs

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// bigWorkspace writes more files than detection will walk, with the file the
// pack is looking for placed last so the walk gives up before reaching it.
func bigWorkspace(t *testing.T, target string) string {
	t.Helper()
	ws := t.TempDir()
	// Enough to exceed walkLimit. Unreal's Content/ holding 30k .uasset files
	// is the real case, and it is not in skipDir.
	dir := filepath.Join(ws, "Content")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < walkLimit+200; i++ {
		p := filepath.Join(dir, fmt.Sprintf("asset%05d.uasset", i))
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if target != "" {
		if err := os.WriteFile(filepath.Join(ws, target), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return ws
}

func packWithDetect(t *testing.T, all string) *Pack {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "chunks"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"schema":1,"name":"p","version":"1.0.0","core":{"file":"core.md","budget_tokens":1500},
		"detect":{"all":["` + all + `"]}}`
	for rel, s := range map[string]string{"pack.json": body, "core.md": "rules"} {
		if err := os.WriteFile(filepath.Join(root, rel), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p, _, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Detection stops past walkLimit files or walkTime, and nothing recorded that
// it had given up — so /packs reported "does not match this workspace", a
// statement of fact that can simply be false. A person had no way to tell "no
// match" from "stopped looking", which is the difference between a pack that
// does not apply and one that would have.
func TestMatchesReportsThatItGaveUp(t *testing.T) {
	ws := bigWorkspace(t, "package.json")
	p := packWithDetect(t, "package.json")

	matched, truncated := p.MatchesDetail(ws)
	if !truncated {
		t.Error("the walk hit its limit and did not say so")
	}
	_ = matched // whether it found the file first is a race against the limit
}

// On a small tree nothing is truncated and the answer is plain.
func TestMatchesDoesNotClaimTruncationOnASmallTree(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := packWithDetect(t, "package.json")

	matched, truncated := p.MatchesDetail(ws)
	if !matched {
		t.Error("a file that is plainly there did not match")
	}
	if truncated {
		t.Error("claimed to have given up on a two-file workspace")
	}
}

// The verdict is reached as soon as it is decided, rather than walking the
// rest of the tree for nothing — which is what made the limit reachable at
// all, and cost up to walkTime per pack at startup.
//
// WalkDir is lexical, so this only helps when the match sorts before the big
// directory: "App.tsx" is visited before "Content/" and the walk stops there.
// Reverse the names and the limit is hit first, which is exactly why the
// truncation has to be reported rather than assumed away.
func TestMatchesStopsOnceTheAnswerIsKnown(t *testing.T) {
	ws := bigWorkspace(t, "")
	if err := os.WriteFile(filepath.Join(ws, "App.tsx"), []byte("export const x = 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := packWithDetect(t, "App.tsx")

	matched, truncated := p.MatchesDetail(ws)
	if !matched {
		t.Fatal("did not match a file the walk reaches before the limit")
	}
	if truncated {
		t.Error("kept walking after the answer was known, and hit the limit doing it")
	}
}

// Matches keeps its signature: callers that only want the verdict are not
// made to care about the walk.
func TestMatchesStillAnswersTheSimpleQuestion(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !packWithDetect(t, "package.json").Matches(ws) {
		t.Error("Matches disagrees with MatchesDetail")
	}
}
