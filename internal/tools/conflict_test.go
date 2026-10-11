package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func regWithHooks(t *testing.T) (*Registry, string) {
	t.Helper()
	root := t.TempDir()
	r, err := NewRegistry(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.ApproveWrites = false
	return r, root
}

// A write asks the hook, and a "keep theirs" answer must not write.
func TestWriteStopsWhenTheConflictHookSaysKeepTheirs(t *testing.T) {
	r, root := regWithHooks(t)
	f := filepath.Join(root, "a.txt")
	if err := os.WriteFile(f, []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	asked := ""
	r.OnConflict = func(_ context.Context, p string, _ []byte) (bool, error) {
		asked = p
		return false, nil
	}
	res := r.Dispatch(context.Background(), call("write_file", `{"path": "a.txt", "content": "mine\n"}`))

	if asked != f {
		t.Errorf("the hook was asked about %q, want %q", asked, f)
	}
	body, _ := os.ReadFile(f)
	if string(body) != "theirs\n" {
		t.Errorf("the file was overwritten anyway: %q", body)
	}
	if !res.IsError {
		t.Error("a declined write should report that it did not happen")
	}
	if !strings.Contains(res.Content, "a.txt") {
		t.Errorf("the result should name the file: %q", res.Content)
	}
}

func TestWriteProceedsWhenTheHookSaysKeepMine(t *testing.T) {
	r, root := regWithHooks(t)
	f := filepath.Join(root, "a.txt")
	if err := os.WriteFile(f, []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.OnConflict = func(context.Context, string, []byte) (bool, error) { return true, nil }

	if res := r.Dispatch(context.Background(), call("write_file", `{"path": "a.txt", "content": "mine\n"}`)); res.IsError {
		t.Fatalf("the write was refused: %s", res.Content)
	}
	body, _ := os.ReadFile(f)
	if string(body) != "mine\n" {
		t.Errorf("the file holds %q", body)
	}
}

// The hook's error is the "nothing could be kept" case: do not write, and say
// why in words a person can act on.
func TestWriteStopsWhenTheHookErrors(t *testing.T) {
	r, root := regWithHooks(t)
	f := filepath.Join(root, "a.txt")
	if err := os.WriteFile(f, []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.OnConflict = func(context.Context, string, []byte) (bool, error) {
		return false, errors.New("the old version could not be kept")
	}
	res := r.Dispatch(context.Background(), call("write_file", `{"path": "a.txt", "content": "mine\n"}`))
	if !res.IsError {
		t.Fatal("a write whose restore point failed must not be reported as done")
	}
	if !strings.Contains(res.Content, "could not be kept") {
		t.Errorf("the reason should reach the model: %q", res.Content)
	}
	body, _ := os.ReadFile(f)
	if string(body) != "theirs\n" {
		t.Errorf("the file was overwritten: %q", body)
	}
}

// A nil hook is the behaviour before this feature: no detection, no change.
func TestNoHookWritesAsBefore(t *testing.T) {
	r, root := regWithHooks(t)
	f := filepath.Join(root, "a.txt")
	if err := os.WriteFile(f, []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := r.Dispatch(context.Background(), call("write_file", `{"path": "a.txt", "content": "mine\n"}`)); res.IsError {
		t.Fatalf("a write with no work ledger failed: %s", res.Content)
	}
	body, _ := os.ReadFile(f)
	if string(body) != "mine\n" {
		t.Errorf("the file holds %q", body)
	}
}

// edit_file applies its replacement to what is on disk *now*, so the other
// writer's change is already in what it writes: nothing is overwritten, and
// a "keep mine / keep theirs" prompt would ask a person to choose between two
// things that are not in conflict. It never raises the gate. What it does owe
// is a word to the model, whose edit was decided against an older version.
func TestEditNeverPromptsButTellsTheModelTheFileMoved(t *testing.T) {
	r, root := regWithHooks(t)
	f := filepath.Join(root, "a.txt")
	if err := os.WriteFile(f, []byte("hello theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.OnConflict = func(context.Context, string, []byte) (bool, error) {
		t.Error("edit_file raised the conflict gate; its write already holds the other version")
		return false, nil
	}
	r.OnChanged = func(string) bool { return true }
	res := r.Dispatch(context.Background(), call("edit_file", `{"path": "a.txt", "old": "theirs", "new": "mine"}`))
	if res.IsError {
		t.Fatalf("edit failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "changed since") {
		t.Errorf("the model was not told the file moved under it: %q", res.Content)
	}
	body, _ := os.ReadFile(f)
	if string(body) != "hello mine\n" {
		t.Errorf("the edit did not land on the current contents: %q", body)
	}
}

// And an edit to a file that did not move says nothing extra.
func TestEditOfAnUnchangedFileSaysNothingExtra(t *testing.T) {
	r, root := regWithHooks(t)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.OnChanged = func(string) bool { return false }
	res := r.Dispatch(context.Background(), call("edit_file", `{"path": "a.txt", "old": "x", "new": "y"}`))
	if res.IsError || strings.Contains(res.Content, "changed since") {
		t.Errorf("unexpected result: %q", res.Content)
	}
}

// Subset carries OnChanged with the other seams, or a sub-agent's edit is
// never told the file moved.
func TestSubsetKeepsTheChangedHook(t *testing.T) {
	r, _ := regWithHooks(t)
	r.OnChanged = func(string) bool { return true }
	if r.Subset("edit_file").OnChanged == nil {
		t.Error("Subset dropped OnChanged")
	}
}

// read_file feeds the ledger what it saw, or Changed can never fire.
func TestReadFeedsTheLedger(t *testing.T) {
	r, root := regWithHooks(t)
	f := filepath.Join(root, "a.txt")
	if err := os.WriteFile(f, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var sawPath string
	var sawBody []byte
	r.OnRead = func(p string, b []byte) { sawPath, sawBody = p, b }

	if res := r.Dispatch(context.Background(), call("read_file", `{"path": "a.txt"}`)); res.IsError {
		t.Fatalf("read failed: %s", res.Content)
	}
	if sawPath != f {
		t.Errorf("OnRead saw %q, want %q", sawPath, f)
	}
	if string(sawBody) != "v1\n" {
		t.Errorf("OnRead saw %q", sawBody)
	}
}

// And a write records what it wrote, or our own next write looks like
// somebody else's change.
func TestWriteFeedsTheLedgerWhatItWrote(t *testing.T) {
	r, _ := regWithHooks(t)
	var sawBody []byte
	r.OnRead = func(_ string, b []byte) { sawBody = b }
	if res := r.Dispatch(context.Background(), call("write_file", `{"path": "a.txt", "content": "mine\n"}`)); res.IsError {
		t.Fatalf("write failed: %s", res.Content)
	}
	if string(sawBody) != "mine\n" {
		t.Errorf("after a write the ledger saw %q, want what we wrote", sawBody)
	}
}

// I4: a sub-agent's registry comes from Subset, which copied OnBeforeWrite but
// not these two — so a sub-agent wrote over another *session's* change with no
// prompt, no snapshot and no notice. The other writer there is another
// process, which is this feature's remit, not a sub-agent question.
func TestSubsetKeepsTheConflictGate(t *testing.T) {
	r, root := regWithHooks(t)
	f := filepath.Join(root, "a.txt")
	if err := os.WriteFile(f, []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	asked, noted := false, false
	r.OnConflict = func(context.Context, string, []byte) (bool, error) { asked = true; return false, nil }
	r.OnRead = func(string, []byte) { noted = true }

	sub := r.Subset("write_file", "read_file")
	res := sub.Dispatch(context.Background(), call("write_file", `{"path": "a.txt", "content": "mine\n"}`))
	if !asked {
		t.Error("a subset's write bypassed the conflict gate")
	}
	if !res.IsError {
		t.Error("the declined write was reported as done")
	}
	if body, _ := os.ReadFile(f); string(body) != "theirs\n" {
		t.Errorf("the file was overwritten: %q", body)
	}
	sub.Dispatch(context.Background(), call("read_file", `{"path": "a.txt"}`))
	if !noted {
		t.Error("a subset's read noted nothing, so it can never protect anything")
	}
}

// I6: post_write hooks exist to rewrite the file (gofmt -w, prettier --write).
// Recording the hash before they run made the session's own next edit a
// conflict titled "Another session changed this file" when nobody else had.
func TestAPostWriteHookDoesNotCreateAFalseConflict(t *testing.T) {
	r, root := regWithHooks(t)
	var seen []byte
	r.OnRead = func(_ string, b []byte) { seen = b }
	// A hook that rewrites the file, as a formatter does.
	r.Hooks = map[string][]string{"post_write": {"sh -c 'printf formatted > " + filepath.Join(root, "a.txt") + "'"}}

	if res := r.Dispatch(context.Background(), call("write_file", `{"path": "a.txt", "content": "mine\n"}`)); res.IsError {
		t.Fatalf("write failed: %s", res.Content)
	}
	if string(seen) != "formatted" {
		t.Errorf("the ledger recorded %q; it must record what is on disk after post_write hooks, "+
			"or the next edit to this file prompts about a change nobody else made", seen)
	}
}

// The work ledger needs to know which paths this session wrote, separately
// from what it read: "what is this session working on" is the record's whole
// point, and OnRead cannot tell a read from a write.
func TestWritesAreReportedSeparatelyFromReads(t *testing.T) {
	r, root := regWithHooks(t)
	var wrote []string
	r.OnWrote = func(p string) { wrote = append(wrote, p) }

	if res := r.Dispatch(context.Background(), call("write_file", `{"path": "a.txt", "content": "x\n"}`)); res.IsError {
		t.Fatalf("write failed: %s", res.Content)
	}
	if res := r.Dispatch(context.Background(), call("read_file", `{"path": "a.txt"}`)); res.IsError {
		t.Fatalf("read failed: %s", res.Content)
	}
	if res := r.Dispatch(context.Background(), call("edit_file", `{"path": "a.txt", "old": "x", "new": "y"}`)); res.IsError {
		t.Fatalf("edit failed: %s", res.Content)
	}

	if len(wrote) != 2 {
		t.Fatalf("OnWrote fired %d times, want 2 (the write and the edit, not the read): %v", len(wrote), wrote)
	}
	want := filepath.Join(root, "a.txt")
	for _, p := range wrote {
		if p != want {
			t.Errorf("OnWrote saw %q, want %q", p, want)
		}
	}
}

// And a subset carries it, like the other two seams.
func TestSubsetKeepsTheWroteHook(t *testing.T) {
	r, root := regWithHooks(t)
	fired := false
	r.OnWrote = func(string) { fired = true }
	sub := r.Subset("write_file")
	sub.Dispatch(context.Background(), call("write_file", `{"path": "a.txt", "content": "x\n"}`))
	if !fired {
		t.Error("a subset's write was not reported, so a sub-agent's work never reaches the record")
	}
	_ = root
}
