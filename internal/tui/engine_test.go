package tui

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/engine"
)

// TestTaskAndNotesAreViewLocal: the ledger and the notes are a listing for
// the terminal that asked, like /coworkers — the store is shared, the
// rendering is not.
func TestTaskAndNotesAreViewLocal(t *testing.T) {
	s, a, b := twoViews(t)
	a.slashCommand("/task")
	if !strings.Contains(a.rendered.String(), "working memory is off") {
		t.Fatalf("no engine:\n%s", a.rendered.String())
	}
	st, err := engine.OpenAt(filepath.Join(t.TempDir(), "e"), s.ag.Tools.Root, "s", false, engine.Limits{NotesCap: 4096})
	if err != nil {
		t.Fatal(err)
	}
	s.ag.SetEngine(st)
	st.Plan("add flag", []string{"parse"})
	a.slashCommand("/task")
	a.slashCommand("/notes add remember me")
	a.slashCommand("/notes")
	if !strings.Contains(a.rendered.String(), "1. add flag") || !strings.Contains(a.rendered.String(), "1. remember me") {
		t.Fatalf("view a:\n%s", a.rendered.String())
	}
	if strings.Contains(b.rendered.String(), "add flag") {
		t.Fatal("listing leaked to the other terminal")
	}
}

// TestTaskIsViewLocal: /task is a private question about state, like
// /coworkers — it never becomes a transcript entry every terminal needs.
func TestTaskIsViewLocal(t *testing.T) {
	s := newTestSession(t)
	a, b := newTestView(t, s), newTestView(t, s)
	before := b.transcriptText()
	a.slashCommand("/task")
	if a.transcriptText() == before {
		t.Fatal("the asking view saw nothing")
	}
	if b.transcriptText() != before {
		t.Fatal("/task leaked into another terminal; it is a private question about state")
	}
}
