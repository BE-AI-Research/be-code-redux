package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/provider"
)

func call(name, args string) provider.ToolCall {
	return provider.ToolCall{ID: "1", Name: name, Arguments: args}
}

func scopedReg(t *testing.T, approve ApproveFunc) (*Registry, string) {
	t.Helper()
	dir := t.TempDir()
	for _, p := range []string{"internal/scan/a.go", "cmd/x.go"} {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("old\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reg, err := NewRegistry(dir, approve)
	if err != nil {
		t.Fatal(err)
	}
	reg.ApproveWrites = true
	sub := reg.Scoped([]string{"internal/scan"}, []string{"go vet ./...", "go test ./...", "true"}, "big (3.2)")
	return sub, dir
}

func TestScopedWritesStayInScope(t *testing.T) {
	var details []string
	sub, dir := scopedReg(t, func(action, detail string) bool { details = append(details, action+"\n"+detail); return true })
	res := sub.Dispatch(context.Background(), call("write_file", `{"path":"internal/scan/b.go","content":"new\n"}`))
	if res.IsError {
		t.Fatalf("in-scope write refused: %s", res.Content)
	}
	if _, err := os.Stat(filepath.Join(dir, "internal/scan/b.go")); err != nil {
		t.Fatal("file not written")
	}
	if len(details) != 1 || !strings.HasPrefix(details[0], "file_write\nsub-agent big (3.2):\n") {
		t.Fatalf("approval detail: %q", details)
	}
	res = sub.Dispatch(context.Background(), call("write_file", `{"path":"cmd/y.go","content":"x"}`))
	if !res.IsError || !strings.Contains(res.Content, "outside your scope (internal/scan)") || !strings.Contains(res.Content, "ask_main") {
		t.Fatalf("out-of-scope write: %+v", res)
	}
	res = sub.Dispatch(context.Background(), call("edit_file", `{"path":"cmd/x.go","old":"old","new":"new"}`))
	if !res.IsError || !strings.Contains(res.Content, "outside your scope") {
		t.Fatalf("out-of-scope edit: %+v", res)
	}
	if len(details) != 1 {
		t.Fatal("a refused write must not prompt")
	}
	res = sub.Dispatch(context.Background(), call("read_file", `{"path":"cmd/x.go"}`))
	if res.IsError {
		t.Fatalf("reads are allowed anywhere: %s", res.Content)
	}
}

func TestScopedShellRunsOnlyTheChecks(t *testing.T) {
	prompted := false
	sub, _ := scopedReg(t, func(action, detail string) bool { prompted = true; return true })
	res := sub.Dispatch(context.Background(), call("shell", `{"command":"go vet ./... && rm -rf /"}`))
	if !res.IsError || !strings.Contains(res.Content, "only the project's checks may be run: go vet ./..., go test ./...") {
		t.Fatalf("compound refused wrong: %+v", res)
	}
	res = sub.Dispatch(context.Background(), call("shell", `{"command":"ls"}`))
	if !res.IsError {
		t.Fatal("ls is not a check")
	}
	if prompted {
		t.Fatal("a refused command must not prompt")
	}
	// A hermetic command that exactly matches a check: assert the positive
	// half too, so a broken exact-match (e.g. a substring or prefix match)
	// cannot pass this test by accident.
	res = sub.Dispatch(context.Background(), call("shell", `{"command":"true"}`))
	if res.IsError {
		t.Fatalf("an allowed exact check must run: %+v", res)
	}
	if prompted {
		t.Fatal("an allowed check must not prompt")
	}
}

func TestScopedRegistryHasNoEscapeTools(t *testing.T) {
	sub, _ := scopedReg(t, nil)
	names := strings.Join(sub.Names(), " ")
	for _, banned := range []string{"process", "consult", "web_search", "web_fetch"} {
		if strings.Contains(names, banned) {
			t.Fatalf("%s reachable from a scoped registry: %s", banned, names)
		}
	}
	for _, want := range []string{"read_file", "write_file", "edit_file", "list_dir", "search", "shell"} {
		if !strings.Contains(names, want) {
			t.Fatalf("%s missing: %s", want, names)
		}
	}
}

// TestScopedFailsClosedOnNilScopeAndChecks is the fix for the critical
// finding: a nil scope or nil checks list must refuse everything, not fall
// open to the main registry's unconfined behaviour. subagent.CleanScope(nil)
// returns (nil, nil) and engine.Store.SetScope stores exactly that for a
// node with no scope assigned yet, so this is not hypothetical.
func TestScopedFailsClosedOnNilScopeAndChecks(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "cmd"), 0o755); err != nil {
		t.Fatal(err)
	}
	reg, err := NewRegistry(dir, func(string, string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	reg.ApproveWrites = true
	// ls is pre-approved on the main registry; a scoped registry with a nil
	// checks list must refuse it anyway, never fall through to ShellAllow.
	reg.ShellAllow = []string{"ls"}
	sub := reg.Scoped(nil, nil, "x")
	res := sub.Dispatch(context.Background(), call("write_file", `{"path":"cmd/y.go","content":"x"}`))
	if !res.IsError {
		t.Fatalf("a nil scope must refuse every write, not permit it: %+v", res)
	}
	res = sub.Dispatch(context.Background(), call("shell", `{"command":"ls"}`))
	if !res.IsError {
		t.Fatalf("a nil checks list must refuse every command, including one on ShellAllow: %+v", res)
	}
}

// TestScopedWriteRefusesSymlinkEscape is the fix for finding 5: resolve()
// and checkScope only look at a path textually, so a symlink already
// inside the scope that points outside it must still be caught before the
// write lands.
func TestScopedWriteRefusesSymlinkEscape(t *testing.T) {
	sub, dir := scopedReg(t, func(string, string) bool { return true })
	target := filepath.Join(dir, "cmd") // exists (scopedReg wrote cmd/x.go), outside the scope
	link := filepath.Join(dir, "internal/scan/out")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	res := sub.Dispatch(context.Background(), call("write_file", `{"path":"internal/scan/out/escape.go","content":"x"}`))
	if !res.IsError || !strings.Contains(res.Content, "outside your scope") {
		t.Fatalf("a write through a symlink that escapes the scope was not refused: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "cmd/escape.go")); err == nil {
		t.Fatal("the write landed outside the scope")
	}
}

// TestScopedWriteFiresOnBeforeWrite pins "checkpointed exactly as the main
// model's is" against a future Subset refactor dropping the hook.
func TestScopedWriteFiresOnBeforeWrite(t *testing.T) {
	dir := t.TempDir()
	reg, err := NewRegistry(dir, func(string, string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	reg.ApproveWrites = true
	var checkpointed []string
	reg.OnBeforeWrite = func(absPath string) error {
		checkpointed = append(checkpointed, absPath)
		return nil
	}
	sub := reg.Scoped([]string{"internal/scan"}, nil, "big")
	res := sub.Dispatch(context.Background(), call("write_file", `{"path":"internal/scan/c.go","content":"x"}`))
	if res.IsError {
		t.Fatalf("in-scope write refused: %s", res.Content)
	}
	if len(checkpointed) != 1 {
		t.Fatalf("OnBeforeWrite did not fire for an in-scope write: %v", checkpointed)
	}
}

// TestScopedToolsAreBoundOrReadOnlyAllowlisted is the fix for finding 4:
// every tool in a scoped registry must either be bound to that registry (so
// confinement checks read its scope/checks) or be named in the explicit
// read-only allowlist Scoped shares by reference from the parent. A future
// tool added to Scoped without being rebound, or without being read-only,
// must fail this test rather than slip through silently.
func TestScopedToolsAreBoundOrReadOnlyAllowlisted(t *testing.T) {
	sub, _ := scopedReg(t, nil)
	readOnlyAllow := map[string]bool{"lookup": true, "history": true, "show": true, "changes": true}
	for name, tool := range sub.byName {
		if readOnlyAllow[name] {
			continue
		}
		var bound *Registry
		switch tt := tool.(type) {
		case *readFileTool:
			bound = tt.r
		case *writeFileTool:
			bound = tt.r
		case *editFileTool:
			bound = tt.r
		case *listDirTool:
			bound = tt.r
		case *searchTool:
			bound = tt.r
		case *shellTool:
			bound = tt.r
		default:
			t.Fatalf("%s: unrecognised tool type %T in a scoped registry; bind it to the scoped registry or add it to the read-only allowlist", name, tool)
		}
		if bound != sub {
			t.Fatalf("%s is bound to a different registry than the scoped one, and is not in the read-only allowlist", name)
		}
	}
}

// TestScopedWriteThroughADanglingSymlinkGivesTheScopeWording: realExistingPath
// returned EvalSymlinks' raw error for a dangling link, so the sub-agent was
// told "lstat …: no such file or directory" — which reads as a harness bug —
// instead of being told it may not write there.
func TestScopedWriteThroughADanglingSymlinkGivesTheScopeWording(t *testing.T) {
	sub, dir := scopedReg(t, func(string, string) bool { return true })
	link := filepath.Join(dir, "internal/scan/dangling")
	if err := os.Symlink(filepath.Join(dir, "no-such-dir"), link); err != nil {
		t.Fatal(err)
	}
	res := sub.Dispatch(context.Background(), call("write_file", `{"path":"internal/scan/dangling/x.go","content":"x"}`))
	if !res.IsError {
		t.Fatalf("a write through a dangling symlink was accepted: %+v", res)
	}
	if strings.Contains(res.Content, "no such file or directory") ||
		!strings.Contains(res.Content, "outside your scope (internal/scan)") {
		t.Fatalf("raw lstat error instead of the scope wording: %q", res.Content)
	}
}

// TestSetScopeWidensARunningRegistry: the scope is read from the tool
// goroutine while the agent widens it (spec §2.7), so it is replaced under
// a lock and never mutated in place.
func TestSetScopeWidensARunningRegistry(t *testing.T) {
	sub, dir := scopedReg(t, func(string, string) bool { return true })
	res := sub.Dispatch(context.Background(), call("write_file", `{"path":"cmd/y.go","content":"x"}`))
	if !res.IsError {
		t.Fatal("an out-of-scope write was accepted before the widening")
	}
	sub.SetScope([]string{"internal/scan", "cmd"})
	if got := sub.Scope(); len(got) != 2 || got[1] != "cmd" {
		t.Fatalf("Scope: %q", got)
	}
	res = sub.Dispatch(context.Background(), call("write_file", `{"path":"cmd/y.go","content":"x"}`))
	if res.IsError {
		t.Fatalf("the widened scope did not take: %s", res.Content)
	}
	if _, err := os.Stat(filepath.Join(dir, "cmd/y.go")); err != nil {
		t.Fatal("file not written after the widening")
	}
	// And the refusal message quotes the current scope, not the original.
	res = sub.Dispatch(context.Background(), call("write_file", `{"path":"docs/z.md","content":"x"}`))
	if !res.IsError || !strings.Contains(res.Content, "outside your scope (internal/scan, cmd)") {
		t.Fatalf("refusal names a stale scope: %+v", res)
	}
}
