package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/provider"
)

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	dir := t.TempDir()
	r, err := NewRegistry(dir, func(action, detail string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPathConfinement(t *testing.T) {
	r := testRegistry(t)
	for _, bad := range []string{"../etc/passwd", "../../x", "/etc/passwd", "a/../../b"} {
		if _, err := r.resolve(bad); err == nil {
			t.Errorf("resolve(%q) should have been rejected", bad)
		}
	}
	if _, err := r.resolve("sub/file.txt"); err != nil {
		t.Errorf("relative path inside root rejected: %v", err)
	}
	// Absolute path inside the root is fine.
	if _, err := r.resolve(filepath.Join(r.Root, "ok.txt")); err != nil {
		t.Errorf("absolute path inside root rejected: %v", err)
	}
}

func TestWriteReadEdit(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()

	res := r.Dispatch(ctx, provider.ToolCall{Name: "write_file",
		Arguments: `{"path":"pkg/hello.txt","content":"line one\nline two\nline three\n"}`})
	if res.IsError {
		t.Fatalf("write failed: %s", res.Content)
	}

	res = r.Dispatch(ctx, provider.ToolCall{Name: "read_file", Arguments: `{"path":"pkg/hello.txt"}`})
	if res.IsError || !strings.Contains(res.Content, "line two") {
		t.Fatalf("read failed: %s", res.Content)
	}

	// edit: unique match required
	res = r.Dispatch(ctx, provider.ToolCall{Name: "edit_file",
		Arguments: `{"path":"pkg/hello.txt","old_text":"line two","new_text":"LINE 2"}`})
	if res.IsError {
		t.Fatalf("edit failed: %s", res.Content)
	}
	data, _ := os.ReadFile(filepath.Join(r.Root, "pkg/hello.txt"))
	if !strings.Contains(string(data), "LINE 2") {
		t.Fatal("edit did not apply")
	}

	// edit: missing text is a friendly error
	res = r.Dispatch(ctx, provider.ToolCall{Name: "edit_file",
		Arguments: `{"path":"pkg/hello.txt","old_text":"never there","new_text":"x"}`})
	if !res.IsError || !strings.Contains(res.Content, "not found") {
		t.Fatalf("expected not-found error, got: %s", res.Content)
	}
}

func TestDispatchLooseArguments(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()

	// Double-encoded JSON string arguments (a common local-model mistake).
	res := r.Dispatch(ctx, provider.ToolCall{Name: "write_file",
		Arguments: `"{\"path\":\"a.txt\",\"content\":\"hi\"}"`})
	if res.IsError {
		t.Fatalf("double-encoded args not tolerated: %s", res.Content)
	}

	// Alternate key names.
	res = r.Dispatch(ctx, provider.ToolCall{Name: "read_file", Arguments: `{"file":"a.txt"}`})
	if res.IsError {
		t.Fatalf("alternate key 'file' not tolerated: %s", res.Content)
	}

	// Unknown tool gets a corrective message, not a crash.
	res = r.Dispatch(ctx, provider.ToolCall{Name: "make_coffee", Arguments: `{}`})
	if !res.IsError || !strings.Contains(res.Content, "available tools") {
		t.Fatalf("unknown tool handling wrong: %s", res.Content)
	}
}

func TestSearch(t *testing.T) {
	r := testRegistry(t)
	ctx := context.Background()
	os.WriteFile(filepath.Join(r.Root, "x.go"), []byte("package x\nfunc Needle() {}\n"), 0o644)
	os.WriteFile(filepath.Join(r.Root, "y.txt"), []byte("no needles here\n"), 0o644)

	res := r.Dispatch(ctx, provider.ToolCall{Name: "search",
		Arguments: `{"pattern":"Needle","glob":"*.go"}`})
	if res.IsError || !strings.Contains(res.Content, "x.go:2") {
		t.Fatalf("search failed: %s", res.Content)
	}
}

func TestShellApprovalDenied(t *testing.T) {
	dir := t.TempDir()
	r, _ := NewRegistry(dir, func(action, detail string) bool { return false })
	res := r.Dispatch(context.Background(), provider.ToolCall{Name: "shell",
		Arguments: `{"command":"echo hi"}`})
	if !res.IsError || !strings.Contains(res.Content, "denied") {
		t.Fatalf("denied shell should return an error result: %s", res.Content)
	}
}

func TestShellRuns(t *testing.T) {
	r := testRegistry(t)
	res := r.Dispatch(context.Background(), provider.ToolCall{Name: "shell",
		Arguments: `{"command":"echo be-code-test"}`})
	if res.IsError || !strings.Contains(res.Content, "be-code-test") {
		t.Fatalf("shell run failed: %s", res.Content)
	}
}

// write_file with no content key must fail loudly rather than truncate the
// file to empty (a silent data-loss path under -y).
func TestWriteFileRequiresContent(t *testing.T) {
	dir := t.TempDir()
	reg, _ := NewRegistry(dir, func(a, d string) bool { return true })
	os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("important"), 0o644)
	res := reg.Dispatch(context.Background(), provider.ToolCall{Name: "write_file", Arguments: `{"path":"keep.txt"}`})
	if !res.IsError || !strings.Contains(res.Content, "content") {
		t.Fatalf("missing content accepted: %+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "keep.txt")); string(b) != "important" {
		t.Fatalf("file was clobbered: %q", b)
	}
}

// Registry.MaxOutput bounds every tool result.
func TestRegistryMaxOutputTruncates(t *testing.T) {
	dir := t.TempDir()
	reg, _ := NewRegistry(dir, func(a, d string) bool { return true })
	reg.MaxOutput = 1024
	os.WriteFile(filepath.Join(dir, "big.txt"), []byte(strings.Repeat("0123456789\n", 1000)), 0o644)
	res := reg.Dispatch(context.Background(), provider.ToolCall{Name: "read_file", Arguments: `{"path":"big.txt"}`})
	if len(res.Content) > 1024+200 || !strings.Contains(res.Content, "truncated") {
		t.Fatalf("output not bounded by MaxOutput: len=%d", len(res.Content))
	}
}
