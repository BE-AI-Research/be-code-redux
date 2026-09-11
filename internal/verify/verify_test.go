package verify

import (
	"context"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func touch(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDetect(t *testing.T) {
	cases := []struct {
		marker string
		kind   string
	}{
		{"go.mod", "go"},
		{"package.json", "node"},
		{"pyproject.toml", "python"},
		{"Cargo.toml", "rust"},
		{"Makefile", "make"},
	}
	for _, c := range cases {
		dir := t.TempDir()
		touch(t, dir, c.marker, "x")
		if got := Detect(dir).Kind; got != c.kind {
			t.Errorf("%s -> %s, want %s", c.marker, got, c.kind)
		}
	}
	if got := Detect(t.TempDir()).Kind; got != "none" {
		t.Errorf("empty dir -> %s, want none", got)
	}
}

func TestRunChecksStopsAtFirstFailure(t *testing.T) {
	dir := t.TempDir()
	proj := Project{Kind: "test", Checks: []Check{
		{Name: "pass", Command: "true", Timeout: timeoutS(10)},
		{Name: "fail", Command: "echo boom >&2; false", Timeout: timeoutS(10)},
		{Name: "never", Command: "true", Timeout: timeoutS(10)},
	}}
	rep := RunChecks(context.Background(), dir, proj)
	if len(rep.Results) != 2 {
		t.Fatalf("expected stop after failure, got %d results", len(rep.Results))
	}
	if rep.Passed() {
		t.Fatal("report should not pass")
	}
	sum := rep.ModelSummary()
	if !strings.Contains(sum, "PASS: pass") || !strings.Contains(sum, "FAIL: fail") || !strings.Contains(sum, "boom") {
		t.Fatalf("summary missing detail:\n%s", sum)
	}
}

func TestGoProjectEndToEnd(t *testing.T) {
	if _, err := exec("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	dir := t.TempDir()
	touch(t, dir, "go.mod", "module example.com/tiny\n\ngo 1.22\n")
	touch(t, dir, "main.go", "package main\n\nfunc main() {}\n")
	proj := Detect(dir)
	rep := RunChecks(context.Background(), dir, proj)
	if !rep.Passed() {
		t.Fatalf("clean go project failed verification:\n%s", rep.ModelSummary())
	}

	// Now break it and confirm the failure is captured for the model.
	touch(t, dir, "main.go", "package main\n\nfunc main() { undefinedCall() }\n")
	rep = RunChecks(context.Background(), dir, proj)
	if rep.Passed() {
		t.Fatal("broken go project passed")
	}
	if !strings.Contains(rep.ModelSummary(), "undefinedCall") {
		t.Fatalf("compiler error not fed back:\n%s", rep.ModelSummary())
	}
}

func TestHeadTail(t *testing.T) {
	s := strings.Repeat("a", 100) + "MIDDLE" + strings.Repeat("b", 100)
	out := headTail(s, 60)
	if !strings.Contains(out, "elided") {
		t.Fatal("no elision marker")
	}
	if !strings.HasPrefix(out, "a") || !strings.HasSuffix(out, "b") {
		t.Fatal("head/tail not preserved")
	}
	if headTail("short", 60) != "short" {
		t.Fatal("short strings must pass through")
	}
}

// helpers

func timeoutS(n int) time.Duration { return time.Duration(n) * time.Second }

func exec(name string) (string, error) { return osexec.LookPath(name) }

// A check may name a fallback command; it runs only when the primary fails,
// so shell-specific "a || b" strings are not needed (they break PowerShell).
func TestCheckFallbackRunsWhenPrimaryFails(t *testing.T) {
	root := t.TempDir()
	rep := RunChecks(context.Background(), root, Project{Kind: "x", Checks: []Check{
		{Name: "flaky", Command: "exit 1", Fallback: "exit 0", Timeout: 10 * time.Second},
	}})
	if !rep.Passed() {
		t.Fatalf("fallback not used: %s", rep.Human())
	}
}
