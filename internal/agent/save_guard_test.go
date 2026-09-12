package agent

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/store"
)

// The save guard is the last line of defence against two programs owning one
// session file: before saving, the agent reloads the on-disk pid stamp and
// refuses to overwrite a file a *live* other process owns. A stale stamp (a
// host that has gone) is taken over, or a crashed session could never be
// resumed again.
func TestAutosaveSkipsWhenAnotherLiveHostOwnsTheFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	a, dir := newTestAgent(t, &scriptedProvider{}, nil)
	a.Session = store.NewSession("test", "m", dir)
	var notices []string
	a.Events.OnNotice = func(s string) { notices = append(notices, s) }

	// A real other process stands in for the other host: its pid is alive.
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skip("no sleep binary")
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	other := *a.Session
	other.HostPID = cmd.Process.Pid
	other.Title = "the other host's work"
	if err := other.Save(); err != nil {
		t.Fatal(err)
	}

	a.autosave("hello")
	if !a.saveDisabled {
		t.Fatal("autosave must be disabled when a live host owns the file")
	}
	// The exit save (cmd/root.go:finishSession) asks the same question.
	if blocked, owner := a.SaveGuard(); !blocked || owner != cmd.Process.Pid {
		t.Fatalf("SaveGuard = %v, %d; want true, %d", blocked, owner, cmd.Process.Pid)
	}
	got, err := store.Load(a.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HostPID != cmd.Process.Pid || got.Title != "the other host's work" {
		t.Fatalf("the other host's file was overwritten: %+v", got)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "session file is owned by live host") {
		t.Fatalf("notice: %q", notices)
	}

	// Once disabled, it stays disabled for the rest of the run: no further
	// reload, no save, and no second warning.
	a.autosave("hello again")
	if len(notices) != 1 {
		t.Fatalf("the warning must appear once, not per save: %q", notices)
	}

	// A dead stamp is ignored: this run takes the file over.
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	a.saveDisabled = false
	if blocked, owner := a.SaveGuard(); blocked {
		t.Fatalf("SaveGuard on a dead stamp = %v, %d; want false", blocked, owner)
	}
	a.autosave("hello")
	got, err = store.Load(a.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HostPID != os.Getpid() {
		t.Fatalf("dead stamp must be taken over: host pid %d, want %d", got.HostPID, os.Getpid())
	}
}
