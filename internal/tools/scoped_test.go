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
	sub := reg.Scoped([]string{"internal/scan"}, []string{"go vet ./...", "go test ./..."}, "big (3.2)")
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
	res = sub.Dispatch(context.Background(), call("shell", `{"command":"go vet ./..."}`))
	if prompted {
		t.Fatal("an allowed check must not prompt")
	}
	_ = res // it may fail in a temp dir with no module; only the gate is under test
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
