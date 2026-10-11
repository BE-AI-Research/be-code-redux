package workledger

import (
	"os"
	"path/filepath"
	"testing"
)

// A sub-agent's run tracks its own reads, separately from the session's.
// Seen is per session and shared with the primary, so if the primary rewrote
// a file the sub-agent had read, the session-wide answer would be "unchanged"
// — true for the session, and exactly wrong for the sub-agent, whose summary
// was written against the older version.
func TestRunReportsWhatChangedUnderneathIt(t *testing.T) {
	ws := t.TempDir()
	a := filepath.Join(ws, "a.go")
	b := filepath.Join(ws, "b.go")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("v1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	r := NewRun(ws)
	r.Read(a, []byte("v1\n"))
	r.Read(b, []byte("v1\n"))

	// Somebody — another session, or this session's own primary — rewrites a.
	if err := os.WriteFile(a, []byte("moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := r.Drifted()
	if len(got) != 1 || got[0] != "a.go" {
		t.Fatalf("drifted = %v, want just a.go relative to the workspace", got)
	}
}

// The primary's own write counts. That is the case where a hand-back's advice
// is most likely to be stale, and the sub-agent cannot tell who moved it.
func TestRunCountsAWriteByThisSessionToo(t *testing.T) {
	ws := t.TempDir()
	f := filepath.Join(ws, "a.go")
	if err := os.WriteFile(f, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewRun(ws)
	r.Read(f, []byte("v1\n"))

	// The session's own Seen would now say "unchanged"; the run must not.
	s := NewSeen()
	s.Note(f, []byte("v1\n"))
	if err := os.WriteFile(f, []byte("primary rewrote it\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.Note(f, []byte("primary rewrote it\n")) // the session noted its own write

	if changed, _, _ := s.Changed(f); changed {
		t.Fatal("the session-wide Seen should consider its own write no change")
	}
	if got := r.Drifted(); len(got) != 1 {
		t.Errorf("the run must still report it: %v", got)
	}
}

// A file the run wrote itself is not drift: it is the run's own work.
func TestRunDoesNotReportItsOwnWrites(t *testing.T) {
	ws := t.TempDir()
	f := filepath.Join(ws, "a.go")
	if err := os.WriteFile(f, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewRun(ws)
	r.Read(f, []byte("v1\n"))
	if err := os.WriteFile(f, []byte("the run's own edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The registry reports a write as OnWrote(path) and then OnRead(path,
	// what is on disk after post_write hooks), in that order; Wrote forgets
	// the baseline so that read re-baselines on the run's own result.
	r.Wrote(f)
	r.Read(f, []byte("the run's own edit\n"))

	if got := r.Drifted(); len(got) != 0 {
		t.Errorf("a file the run wrote is its own work, not drift: %v", got)
	}
}

// A write to a file the run never read is its own work too, and never drift.
func TestRunNeverReportsAFileItOnlyWrote(t *testing.T) {
	ws := t.TempDir()
	f := filepath.Join(ws, "new.go")
	r := NewRun(ws)
	r.Wrote(f)
	if err := os.WriteFile(f, []byte("fresh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.Read(f, []byte("fresh\n"))
	if got := r.Drifted(); len(got) != 0 {
		t.Errorf("%v", got)
	}
}

// A file deleted underneath the run is drift too.
func TestRunReportsADeletedFile(t *testing.T) {
	ws := t.TempDir()
	f := filepath.Join(ws, "a.go")
	if err := os.WriteFile(f, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewRun(ws)
	r.Read(f, []byte("v1\n"))
	if err := os.Remove(f); err != nil {
		t.Fatal(err)
	}
	if got := r.Drifted(); len(got) != 1 {
		t.Errorf("a file removed underneath the run is drift: %v", got)
	}
}

// A nil run is safe: sub-agents can be configured without the work ledger.
func TestANilRunIsSafe(t *testing.T) {
	var r *Run
	r.Read("/x", []byte("v"))
	r.Wrote("/x")
	if got := r.Drifted(); got != nil {
		t.Errorf("a nil run reports nothing, got %v", got)
	}
}
