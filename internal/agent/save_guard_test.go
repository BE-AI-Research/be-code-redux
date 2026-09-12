package agent

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/store"
)

// stubRegistry points the save guard's two injected lookups (see PIDAlive
// and LiveOwner, wired from cmd to internal/live) at maps the test drives:
// which pids are running, and which pid the live registry advertises as the
// host for a session code.
func stubRegistry(t *testing.T) (alive map[int]bool, hosts map[string]int) {
	t.Helper()
	alive, hosts = map[int]bool{}, map[string]int{}
	prevAlive, prevOwner := PIDAlive, LiveOwner
	t.Cleanup(func() { PIDAlive, LiveOwner = prevAlive, prevOwner })
	PIDAlive = func(pid int) bool { return alive[pid] }
	LiveOwner = func(code string) (int, bool) { pid, ok := hosts[code]; return pid, ok }
	return alive, hosts
}

// The save guard is the last line of defence against two programs owning
// one session file: before saving, the agent reloads the on-disk pid stamp
// and refuses to overwrite a file an advertised *live host* owns.
func TestAutosaveSkipsWhenALiveHostOwnsTheFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	alive, hosts := stubRegistry(t)
	a, dir := newTestAgent(t, &scriptedProvider{}, nil)
	a.Session = store.NewSession("test", "m", dir)
	var notices []string
	a.Events.OnNotice = func(s string) { notices = append(notices, s) }

	// A real other process stands in for the other host: its pid is alive,
	// and the registry advertises it as this code's host.
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
	alive[cmd.Process.Pid] = true
	hosts[a.Session.ResumeCode()] = cmd.Process.Pid

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

	// A new session is a different file with a different owner: /clear must
	// not inherit the latch (see SetSession).
	next := store.NewSession("test", "m", dir)
	// A distinct id: NewSession stamps ids to the millisecond, so one made
	// in the same instant as the first would be the same file.
	next.ID += "-clear"
	next.Code = store.CodeFor(next.ID)
	a.SetSession(next)
	a.autosave("a fresh start")
	fresh, err := store.Load(a.Session.ID)
	if err != nil || fresh.HostPID != os.Getpid() {
		t.Fatalf("a new session must save: %v %+v", err, fresh)
	}
	if blocked, _ := a.SaveGuard(); blocked {
		t.Fatal("the guard latch survived SetSession")
	}
}

// A pid alone is not ownership. Pids are recycled, so an unrelated process
// that happens to hold a crashed host's number must not lock the session
// out of its own file: the stamp only counts while the live registry still
// advertises that pid as this code's host.
func TestAutosaveTakesOverAStampNoLiveHostClaims(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	alive, hosts := stubRegistry(t)
	a, dir := newTestAgent(t, &scriptedProvider{}, nil)
	a.Session = store.NewSession("test", "m", dir)
	var notices []string
	a.Events.OnNotice = func(s string) { notices = append(notices, s) }

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skip("no sleep binary")
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	stamp := func(pid int) {
		other := *a.Session
		other.HostPID = pid
		if err := other.Save(); err != nil {
			t.Fatal(err)
		}
	}

	// Alive, but no live host advertises it for this code: a recycled pid.
	stamp(cmd.Process.Pid)
	alive[cmd.Process.Pid] = true
	a.autosave("mine")
	got, err := store.Load(a.Session.ID)
	if err != nil || got.HostPID != os.Getpid() {
		t.Fatalf("an unclaimed stamp must be taken over: %v %+v", err, got)
	}

	// A live host for the code, but a different pid than the stamp: the
	// stamp is a leftover, not that host's claim.
	stamp(cmd.Process.Pid)
	hosts[a.Session.ResumeCode()] = cmd.Process.Pid + 1
	alive[cmd.Process.Pid+1] = true
	a.autosave("still mine")
	if got, err = store.Load(a.Session.ID); err != nil || got.HostPID != os.Getpid() {
		t.Fatalf("a stamp the live host does not match must be taken over: %v %+v", err, got)
	}

	// A dead stamp is ignored even when the registry still names it, or a
	// crashed session could never be written again.
	stamp(cmd.Process.Pid)
	hosts[a.Session.ResumeCode()] = cmd.Process.Pid
	alive[cmd.Process.Pid] = false
	a.autosave("after the crash")
	if got, err = store.Load(a.Session.ID); err != nil || got.HostPID != os.Getpid() {
		t.Fatalf("a dead stamp must be taken over: %v %+v", err, got)
	}
	if len(notices) != 0 {
		t.Fatalf("no save was blocked, so nothing should have been reported: %q", notices)
	}
}
