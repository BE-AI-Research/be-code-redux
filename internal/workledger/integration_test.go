package workledger

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// The whole feature through the real seams, wired the way cmd/root.go wires
// it. Every Critical the final review found lived between two units that each
// passed their own tests, so this exercises the chain rather than the parts.
//
// internal/tools does not import this package — the seams are hooks — so a
// test here may import it without a cycle.
func TestThroughTheRealRegistry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := gitRepo(t)
	reg, err := tools.NewRegistry(ws, nil)
	if err != nil {
		t.Fatal(err)
	}
	reg.ApproveWrites = false

	l := Open(ws, Options{Enabled: true, KeepOverwritten: 0, PID: 4242, Model: "m"})
	defer l.Close()

	turn := 1
	l.SetState(func() (string, int) { return "003", turn })
	var notices []string
	l.SetWarn(func(s string) { notices = append(notices, s) })

	reg.OnRead = l.OnRead
	reg.OnWrote = l.OnWrote
	asked := 0
	reg.OnConflict = func(ctx context.Context, p string, mine []byte) (bool, error) {
		return l.OnConflict(ctx, p, mine, func(string, string) bool { asked++; return true },
			Attended, func(s string) { notices = append(notices, s) })
	}

	call := func(name, args string) tools.Result {
		return reg.Dispatch(context.Background(), provider.ToolCall{ID: "1", Name: name, Arguments: args})
	}

	// A first write is not a conflict, and it reaches the record.
	if res := call("write_file", `{"path": "a.go", "content": "ours\n"}`); res.IsError {
		t.Fatalf("first write: %s", res.Content)
	}
	if asked != 0 {
		t.Error("a file nobody else had touched raised a conflict")
	}
	if !containsPath(l.rec.Writing, "a.go") {
		t.Errorf("the record does not name what was written: %v", l.rec.Writing)
	}
	if l.rec.Task != "003" {
		t.Errorf("task = %q, want 003", l.rec.Task)
	}

	// Another process changes it underneath us.
	if err := os.WriteFile(filepath.Join(ws, "a.go"), []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The next write asks, and on yes it lands with the old version kept.
	if res := call("write_file", `{"path": "a.go", "content": "mine\n"}`); res.IsError {
		t.Fatalf("second write: %s", res.Content)
	}
	if asked != 1 {
		t.Errorf("asked %d times, want 1", asked)
	}
	body, _ := os.ReadFile(filepath.Join(ws, "a.go"))
	if string(body) != "mine\n" {
		t.Errorf("the approved write did not land: %q", body)
	}
	joined := strings.Join(notices, " ")
	if !strings.Contains(joined, "pre-overwrite/") || !strings.Contains(joined, "git restore") {
		t.Errorf("the notice must say where the old version went and how to get it: %q", joined)
	}

	// Another outside change, then an edit rather than a whole-file write:
	// the edit is applied to the current contents, so nobody is asked, and
	// the model is told the file moved under it.
	if err := os.WriteFile(filepath.Join(ws, "a.go"), []byte("mine\ntheirs again\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg.OnChanged = l.Changed
	res := call("edit_file", `{"path": "a.go", "old": "mine", "new": "ours"}`)
	if res.IsError {
		t.Fatalf("edit: %s", res.Content)
	}
	if asked != 1 {
		t.Errorf("an edit raised the conflict prompt (asked %d times); its write already holds their change", asked)
	}
	if !strings.Contains(res.Content, "changed since") {
		t.Errorf("the model was not told the file moved: %q", res.Content)
	}
	body, _ = os.ReadFile(filepath.Join(ws, "a.go"))
	if string(body) != "ours\ntheirs again\n" {
		t.Errorf("their change was not kept through the edit: %q", body)
	}

	// A new turn starts the written list again.
	turn = 2
	if res := call("write_file", `{"path": "b.go", "content": "next turn\n"}`); res.IsError {
		t.Fatalf("third write: %s", res.Content)
	}
	if containsPath(l.rec.Writing, "a.go") {
		t.Errorf("the previous turn's file is still listed: %v", l.rec.Writing)
	}
	if !containsPath(l.rec.Writing, "b.go") {
		t.Errorf("this turn's file is missing: %v", l.rec.Writing)
	}
}
