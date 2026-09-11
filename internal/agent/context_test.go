package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpandMentions(t *testing.T) {
	ws := t.TempDir()
	os.MkdirAll(filepath.Join(ws, "pkg"), 0o755)
	os.WriteFile(filepath.Join(ws, "pkg/util.go"), []byte("package pkg\nfunc Util() {}\n"), 0o644)

	out := ExpandMentions(ws, "refactor @pkg/util.go to add logging")
	if !strings.Contains(out, `<file path="pkg/util.go">`) || !strings.Contains(out, "func Util()") {
		t.Fatalf("file not pinned:\n%s", out)
	}

	// Nonexistent path stays plain text, no block added.
	out = ExpandMentions(ws, "ping @nobody about this")
	if strings.Contains(out, "<file") {
		t.Fatalf("phantom file pinned:\n%s", out)
	}

	// Escape attempts are ignored.
	out = ExpandMentions(ws, "read @../../etc/passwd please")
	if strings.Contains(out, "<file") {
		t.Fatal("path escape pinned a file")
	}
}

func TestCompleteMention(t *testing.T) {
	ws := t.TempDir()
	os.MkdirAll(filepath.Join(ws, "internal/agent"), 0o755)
	os.WriteFile(filepath.Join(ws, "internal/agent/loop.go"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(ws, "main.go"), []byte("x"), 0o644)

	got := CompleteMention(ws, "ma")
	if len(got) != 1 || got[0] != "main.go" {
		t.Fatalf("got %v", got)
	}
	got = CompleteMention(ws, "internal/")
	if len(got) != 1 || got[0] != "internal/agent/" {
		t.Fatalf("got %v", got)
	}
}
