package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJobMarkRoundTrips(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(t.TempDir(), "engine")
	s, err := OpenAt(dir, root, "sess", false, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	s.SetCards(cardsForTest())
	task := s.Plan("port the scanner", []string{"look around"})
	id, err := s.AddJob(task, "port the scanner", "details here", "big", []string{"internal/scan"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, task+".") {
		t.Fatalf("id %q not a child of %q", id, task)
	}
	check := func(s *Store) {
		t.Helper()
		n := s.tree.Find(id)
		if n == nil || !n.Job || n.Owner != "big" || len(n.Scope) != 1 || n.Scope[0] != "internal/scan" {
			t.Fatalf("node: %+v", n)
		}
	}
	check(s)
	if n := s.tree.Find(id); len(n.Evidence.Notes) != 1 || n.Evidence.Notes[0].Text != "details here" {
		t.Fatalf("notes: %+v", n.Evidence.Notes)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	var body string
	files, _ := filepath.Glob(filepath.Join(root, ".be-code", "tasks", "0*.md"))
	for _, f := range files {
		b, _ := os.ReadFile(f)
		body += string(b)
	}
	if !strings.Contains(body, "  @big  scope: internal/scan  job\n") {
		t.Fatalf("document:\n%s", body)
	}
	s2, err := OpenAt(dir, root, "sess", true, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	check(s2)
}

func TestJobMarkSurvivesHandEdit(t *testing.T) {
	text, f := splitFields("port it  @big  scope: a/b  job")
	if text != "port it" || !f.job || f.owner != "big" {
		t.Fatalf("%q %+v", text, f)
	}
	text, f = splitFields("finish the job")
	if f.job || text != "finish the job" {
		t.Fatalf("text-only job parsed as field: %q %+v", text, f)
	}
	s, root := openTest(t, "sess", false)
	dir := filepath.Join(root, ".be-code", "tasks")
	os.MkdirAll(dir, 0o755)
	doc := "# Task\n\n- [ ] 1. Task\n  - [ ] 1.1. hand job  @big  scope: x  job\n"
	if err := os.WriteFile(filepath.Join(dir, "001-task.md"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	s2, err := OpenAt(filepath.Join(t.TempDir(), "e"), root, "sess", false, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	_ = s
	n := s2.tree.Find("1.1")
	if n == nil || !n.Job {
		t.Fatalf("hand-written job lost: %+v", n)
	}
}
