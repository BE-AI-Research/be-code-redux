package tui

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInputHistoryNavigation(t *testing.T) {
	h := loadInputHistory("")
	h.add("first", 0)
	h.add("second", 0)
	h.add("second", 0) // consecutive dupe ignored
	if len(h.lines) != 2 {
		t.Fatalf("lines = %d", len(h.lines))
	}
	if s, ok := h.prev(0); !ok || s != "second" {
		t.Fatalf("prev1 = %q", s)
	}
	if s, ok := h.prev(0); !ok || s != "first" {
		t.Fatalf("prev2 = %q", s)
	}
	if _, ok := h.prev(0); ok {
		t.Fatal("prev past start should fail")
	}
	if s, ok := h.next(0); !ok || s != "second" {
		t.Fatalf("next = %q", s)
	}
	if s, ok := h.next(0); !ok || s != "" {
		t.Fatalf("next to live = %q", s)
	}
}

func TestInputHistoryPersistence(t *testing.T) {
	p := filepath.Join(t.TempDir(), "history")
	h := loadInputHistory(p)
	h.add("build the thing", 0)
	h.add("test the thing", 0)
	h.save()

	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "build the thing\ntest the thing\n" {
		t.Fatalf("file: %q", data)
	}

	h2 := loadInputHistory(p)
	if len(h2.lines) != 2 {
		t.Fatalf("reload lines = %d", len(h2.lines))
	}
	if s, _ := h2.prev(0); s != "test the thing" {
		t.Fatalf("reload prev = %q", s)
	}
}
