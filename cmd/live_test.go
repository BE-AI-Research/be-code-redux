package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/live"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/store"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// testFlags mirrors the root command's persistent flags, so the argument
// builder is exercised over the same shapes (string and bool) it sees in
// production without mutating the real, global flag set.
func testFlags() *pflag.FlagSet {
	fs := pflag.NewFlagSet("root", pflag.ContinueOnError)
	fs.String("provider", "", "")
	fs.String("model", "", "")
	fs.String("dir", ".", "")
	fs.Bool("yes", false, "")
	fs.String("resume", "", "")
	fs.Bool("ide", false, "")
	fs.Bool("no-ide", false, "")
	fs.Bool("no-host", false, "")
	fs.Bool("new", false, "")
	fs.String("session-host", "", "")
	return fs
}

func TestHostArgsForwardsOnlyChangedFlags(t *testing.T) {
	fs := testFlags()
	got := hostArgs("A1B2C3", "/work/space", fs)
	want := []string{"A1B2C3", "--dir=/work/space"}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

func TestHostArgsForwardsEveryChangedFlag(t *testing.T) {
	fs := testFlags()
	// The flags a served session would otherwise lose: approvals and the
	// editor bridge are decided in the host, not the launcher.
	for name, val := range map[string]string{
		"yes":      "true",
		"no-ide":   "true",
		"provider": "lan",
		"model":    "qwen3:8b",
		"resume":   "ZZ9QQ9",
	} {
		if err := fs.Set(name, val); err != nil {
			t.Fatal(err)
		}
	}
	got := hostArgs("A1B2C3", "/work/space", fs)
	if got[0] != "A1B2C3" {
		t.Fatalf("first arg %q, want the session code", got[0])
	}
	for _, want := range []string{
		"--dir=/work/space", "--yes=true", "--no-ide=true",
		"--provider=lan", "--model=qwen3:8b", "--resume=ZZ9QQ9",
	} {
		if !contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if len(got) != 7 {
		t.Errorf("got %d args %q, want 7 (code + dir + 5 changed)", len(got), got)
	}
}

func TestHostArgsNeverForwardsLauncherOnlyFlags(t *testing.T) {
	for _, name := range []string{"session-host", "no-host", "dir"} {
		fs2 := testFlags()
		val := "true"
		if fs2.Lookup(name).Value.Type() == "string" {
			val = "LAUNCHER"
		}
		if err := fs2.Set(name, val); err != nil {
			t.Fatal(err)
		}
		for _, arg := range hostArgs("A1B2C3", "/work/space", fs2) {
			if strings.HasPrefix(arg, "--"+name+"=") && arg != "--dir=/work/space" {
				t.Errorf("%s forwarded as %q", name, arg)
			}
		}
	}
}

// TestRootHasTheFlagsHostArgsSkips guards the skip list against a rename:
// if one of these flags disappears, the skip is silently meaningless.
func TestRootHasTheFlagsHostArgsSkips(t *testing.T) {
	fs := rootCmd.PersistentFlags()
	for _, name := range []string{"dir", "session-host", "no-host"} {
		if fs.Lookup(name) == nil {
			t.Errorf("root has no persistent flag %q, but hostArgs skips it", name)
		}
	}
	// And the flags the finding was about must exist to be forwarded.
	for _, name := range []string{"yes", "ide", "no-ide"} {
		if fs.Lookup(name) == nil {
			t.Errorf("root has no persistent flag %q", name)
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// The attach command's fallback to a saved session must reach the spawned
// host: assigning flagResume leaves the flag unchanged, and hostArgs forwards
// only changed flags, so the host would resume nothing while the launcher
// derived the live record's code from the saved session — two saved sessions
// then share one resume code.
func TestSetResumeIsForwardedToTheHost(t *testing.T) {
	fs := testFlags()
	if contains(hostArgs("A1B2C3", "/work/space", fs), "--resume=ZZ9QQ9") {
		t.Fatal("resume forwarded while the flag is unchanged")
	}
	for _, arg := range hostArgs("A1B2C3", "/work/space", fs) {
		if strings.HasPrefix(arg, "--resume") {
			t.Fatalf("unchanged resume flag forwarded as %q", arg)
		}
	}
	if err := setResume(fs, "ZZ9QQ9"); err != nil {
		t.Fatal(err)
	}
	if !fs.Lookup("resume").Changed {
		t.Error("setResume did not mark the flag changed")
	}
	if got := fs.Lookup("resume").Value.String(); got != "ZZ9QQ9" {
		t.Errorf("resume value %q, want ZZ9QQ9", got)
	}
	if !contains(hostArgs("A1B2C3", "/work/space", fs), "--resume=ZZ9QQ9") {
		t.Errorf("--resume=ZZ9QQ9 not forwarded: %q", hostArgs("A1B2C3", "/work/space", fs))
	}
}

// setResume goes through the real root flag set in production, so the flag it
// names must exist there and its binding must write flagResume.
func TestSetResumeBindsTheRootFlagVariable(t *testing.T) {
	fs := rootCmd.PersistentFlags()
	f := fs.Lookup("resume")
	if f == nil {
		t.Fatal("root has no persistent --resume flag")
	}
	prev, prevChanged := flagResume, f.Changed
	defer func() {
		f.Value.Set(prev)
		f.Changed = prevChanged
		flagResume = prev
	}()
	if err := setResume(fs, "QQ2222"); err != nil {
		t.Fatal(err)
	}
	if flagResume != "QQ2222" {
		t.Errorf("flagResume is %q, want QQ2222 (the flag is not bound to it)", flagResume)
	}
	if !f.Changed {
		t.Error("root --resume not marked changed")
	}
}

// newestLiveIn is what an unflagged `be-code` joins: the newest live session
// serving this workspace, never a record whose host is gone (live.List prunes
// those).
func TestNewestLiveInPicksTheNewestAndSkipsDeadHosts(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	recs := []live.Record{
		{Code: "OLD111", PID: os.Getpid(), Socket: filepath.Join(dir, "OLD111.sock"), Workspace: "/ws", StartedAt: now.Add(-time.Hour)},
		{Code: "NEW222", PID: os.Getpid(), Socket: filepath.Join(dir, "NEW222.sock"), Workspace: "/ws", StartedAt: now},
		{Code: "OTHER3", PID: os.Getpid(), Socket: filepath.Join(dir, "OTHER3.sock"), Workspace: "/elsewhere", StartedAt: now.Add(time.Hour)},
		{Code: "DEAD44", PID: deadPID(t), Socket: filepath.Join(dir, "DEAD44.sock"), Workspace: "/ws", StartedAt: now.Add(time.Minute)},
	}
	for _, r := range recs {
		if err := r.Save(dir); err != nil {
			t.Fatal(err)
		}
	}
	got := newestLiveIn(dir, "/ws")
	if got == nil || got.Code != "NEW222" {
		t.Fatalf("newestLiveIn = %+v, want NEW222", got)
	}
	if r := newestLiveIn(dir, "/nothing-here"); r != nil {
		t.Fatalf("newestLiveIn for an unknown workspace = %+v, want nil", r)
	}
}

// TestNextAttach covers nextAttach's interpretation of every bye reason
// attachLive's loop can see: a switch to a still-live session hands over the
// record silently, a switch to a session that is gone (or was never live)
// ends the attach with an explanatory line, a local/host-initiated detach
// prints the "still running" line, and any other reason (host close, "session
// ended" included) is reported verbatim.
func TestNextAttach(t *testing.T) {
	dir := t.TempDir()
	target := live.Record{Code: "ABC123", PID: os.Getpid(), Socket: filepath.Join(dir, "ABC123.sock"), Workspace: "/ws", StartedAt: time.Now()}
	if err := target.Save(dir); err != nil {
		t.Fatal(err)
	}

	if rec, msg := nextAttach(dir, "OLD001", "switch:ABC123"); rec == nil || rec.Code != "ABC123" || msg != "" {
		t.Fatalf("switch to a live target = %+v, %q; want the ABC123 record and no message", rec, msg)
	}
	if rec, msg := nextAttach(dir, "OLD001", "switch:GONE99"); rec != nil || msg != "GONE99 ended before you could join it" {
		t.Fatalf("switch to a dead target = %+v, %q", rec, msg)
	}
	if rec, msg := nextAttach(dir, "OLD001", ""); rec != nil || msg != "detached from OLD001 (still running); be-code attach OLD001 to return" {
		t.Fatalf("local detach = %+v, %q", rec, msg)
	}
	if rec, msg := nextAttach(dir, "OLD001", live.ReasonDetached); rec != nil || msg != "detached from OLD001 (still running); be-code attach OLD001 to return" {
		t.Fatalf("host detach = %+v, %q", rec, msg)
	}
	if rec, msg := nextAttach(dir, "OLD001", live.ReasonEnded); rec != nil || msg != "OLD001: session ended" {
		t.Fatalf("ended = %+v, %q", rec, msg)
	}
}

// deadPID returns a pid that is certainly not running: a child started and
// reaped, so its pid is free (and not a zombie, which would still look alive).
func deadPID(t *testing.T) int {
	t.Helper()
	c := exec.Command("sh", "-c", "exit 0")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	_ = c.Wait()
	return c.Process.Pid
}

// TestSessionsKillEscalatesAndOnlyThenRetiresTheRecord covers the report that
// used to be a lie: a host that ignores SIGTERM was left running while the
// command removed its record and printed "killed <code>" — the session then
// held the workspace, the socket and its MCP children with no code left to
// reach it by.
func TestSessionsKillEscalatesAndOnlyThenRetiresTheRecord(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no SIGTERM on windows; Terminate is already the abrupt kill")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir, err := live.Dir()
	if err != nil {
		t.Fatal(err)
	}
	// The real waits are a quiet minute of grace for a handoff briefing.
	defer func(q, g, k time.Duration) { quitWait, quitGrace, killWait = q, g, k }(quitWait, quitGrace, killWait)
	quitWait, quitGrace, killWait = 300*time.Millisecond, 300*time.Millisecond, 2*time.Second

	// A "host" that ignores SIGTERM, reaped in the background so that its pid
	// is genuinely free once it dies (a zombie still answers kill(pid, 0)).
	child := exec.Command("sh", "-c", "trap '' TERM; while :; do sleep 0.2; done")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan struct{})
	go func() { defer close(waited); _ = child.Wait() }()
	t.Cleanup(func() {
		_ = live.Kill(child.Process.Pid)
		<-waited
	})
	pid := child.Process.Pid

	rec := live.Record{
		Code: "KILL01", PID: pid, Socket: live.SocketPath(dir, "KILL01"),
		Workspace: home, Model: "m", StartedAt: time.Now(), Token: "tok",
	}
	if err := rec.Save(dir); err != nil {
		t.Fatal(err)
	}
	// No socket is listening, so the polite quit frame cannot be delivered:
	// straight to the signal fallback.
	if err := sessionsKillCmd.RunE(sessionsKillCmd, []string{"KILL01"}); err != nil {
		t.Fatalf("sessions kill: %v", err)
	}
	<-waited
	if live.Alive(pid) {
		t.Fatalf("pid %d survived `sessions kill`", pid)
	}
	if _, err := live.Load(dir, "KILL01"); err == nil {
		t.Fatal("the record is still there after a successful kill")
	}
}

// --new must reach the host like any other changed root flag: it is
// harmless there (the host never runs the join decision), but hostArgs
// forwards flags wholesale and a silently dropped one is the bug that
// wholesale forwarding exists to prevent.
func TestHostArgsForwardsNew(t *testing.T) {
	fs := testFlags()
	if err := fs.Set("new", "true"); err != nil {
		t.Fatal(err)
	}
	got := hostArgs("A1B2C3", "/work/space", fs)
	if !contains(got, "--new=true") {
		t.Fatalf("hostArgs = %q, want it to include --new=true", got)
	}
}

// decideStart is the "join, never fork" decision the launcher makes before
// it spawns anything: a live session for this workspace is joined with no
// prompt, an explicit --resume of a live code joins that code, --new skips
// the join, and anything not live starts a fresh host (nil record).
func TestDecideStart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	dir := t.TempDir()

	// Two saved sessions: one live, one not.
	liveSess := store.NewSession("ollama", "m", "/ws")
	if err := liveSess.Save(); err != nil {
		t.Fatal(err)
	}
	coldSess := store.NewSession("ollama", "m", "/ws")
	// A distinct id: NewSession's ids are millisecond-stamped, so two made
	// in the same instant would be one session saved twice.
	coldSess.ID += "-cold"
	coldSess.Code = store.CodeFor(coldSess.ID)
	if err := coldSess.Save(); err != nil {
		t.Fatal(err)
	}
	rec := live.Record{
		Code: liveSess.ResumeCode(), PID: os.Getpid(),
		Socket:    filepath.Join(dir, liveSess.ResumeCode()+".sock"),
		Workspace: "/ws", StartedAt: time.Now(),
	}
	if err := rec.Save(dir); err != nil {
		t.Fatal(err)
	}
	join := "joining live session " + rec.Code

	if got, msg := decideStart(dir, "/ws", "", false); got == nil || got.Code != rec.Code ||
		msg != join+" (be-code --new starts a fresh one)" {
		t.Fatalf("workspace with a live session = %+v, %q", got, msg)
	}
	if got, msg := decideStart(dir, "/ws", "", true); got != nil || msg != "" {
		t.Fatalf("--new must start a fresh session, got %+v, %q", got, msg)
	}
	if got, msg := decideStart(dir, "/elsewhere", "", false); got != nil || msg != "" {
		t.Fatalf("workspace with no live session = %+v, %q", got, msg)
	}
	if got, msg := decideStart(dir, "/elsewhere", liveSess.ResumeCode(), false); got == nil ||
		got.Code != rec.Code || msg != join {
		t.Fatalf("--resume of a live code = %+v, %q", got, msg)
	}
	// --resume wins over --new: the user named the session they want.
	if got, _ := decideStart(dir, "/ws", liveSess.ResumeCode(), true); got == nil || got.Code != rec.Code {
		t.Fatalf("--resume of a live code with --new = %+v", got)
	}
	// A saved session that is not live is resumed by a fresh host, even
	// though this workspace has another live session.
	if got, msg := decideStart(dir, "/ws", coldSess.ResumeCode(), false); got != nil || msg != "" {
		t.Fatalf("--resume of a cold code = %+v, %q", got, msg)
	}
	if got, msg := decideStart(dir, "/ws", "NOSUCH", false); got != nil || msg != "" {
		t.Fatalf("--resume of an unknown code = %+v, %q", got, msg)
	}
}

// A host's session lives in its memory from the moment it starts; the file
// only appears on its first autosave. Joining must not go through the
// store, or `be-code --resume CODE` against a live session that has not
// finished a turn would report "no such session" and start a second one.
func TestDecideStartJoinsALiveCodeWithNoSessionFileYet(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	dir := t.TempDir()
	rec := live.Record{
		Code: "ABC123", PID: os.Getpid(), Socket: filepath.Join(dir, "ABC123.sock"),
		Workspace: "/ws", StartedAt: time.Now(),
	}
	if err := rec.Save(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("ABC123"); err == nil {
		t.Fatal("this case is only meaningful while the session file does not exist")
	}
	for _, typed := range []string{"ABC123", "abc123", " ABC123 "} {
		got, msg := decideStart(dir, "/elsewhere", typed, false)
		if got == nil || got.Code != "ABC123" || msg != "joining live session ABC123" {
			t.Fatalf("--resume %q = %+v, %q", typed, got, msg)
		}
	}
}

// wireLiveRegistry is how the agent's save guard reaches the live records
// without importing them.
func TestWireLiveRegistryAnswersFromTheRecords(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	dir, err := live.Dir()
	if err != nil {
		t.Fatal(err)
	}
	rec := live.Record{
		Code: "ABC123", PID: os.Getpid(), Socket: filepath.Join(dir, "ABC123.sock"),
		Workspace: "/ws", StartedAt: time.Now(),
	}
	if err := rec.Save(dir); err != nil {
		t.Fatal(err)
	}
	wireLiveRegistry()
	if pid, ok := agent.LiveOwner("ABC123"); !ok || pid != os.Getpid() {
		t.Fatalf("LiveOwner = %d, %v; want %d, true", pid, ok, os.Getpid())
	}
	if pid, ok := agent.LiveOwner("NOPE11"); ok {
		t.Fatalf("LiveOwner for a code with no host = %d, %v", pid, ok)
	}
	if !agent.PIDAlive(os.Getpid()) || agent.PIDAlive(deadPID(t)) {
		t.Fatal("PIDAlive is not wired to the real process check")
	}
}

// A session file a live host owns is that host's to write. The transcript
// this run produced is still real work, so it is written to a session of
// its own rather than dropped with a warning.
func TestFinishSessionSavesElsewhereWhenALiveHostOwnsTheFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	owner := exec.Command("sleep", "30")
	if err := owner.Start(); err != nil {
		t.Skip("no sleep binary")
	}
	t.Cleanup(func() { _ = owner.Process.Kill(); _ = owner.Wait() })

	reg, err := tools.NewRegistry(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.RepoMap = false
	ag := agent.New(cfg, stubProvider{}, "m", reg, "")
	s := store.NewSession("ollama", "m", "/ws")
	s.Title = "shared work"
	s.HostPID = owner.Process.Pid
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	ag.SetSession(s)
	ag.History.Messages = []provider.Message{
		{Role: provider.RoleUser, Content: "hi"}, {Role: provider.RoleAssistant, Content: "hello"},
	}
	// The live record is what turns the pid stamp into ownership.
	dir, err := live.Dir()
	if err != nil {
		t.Fatal(err)
	}
	rec := live.Record{
		Code: s.ResumeCode(), PID: owner.Process.Pid, Socket: filepath.Join(dir, s.ResumeCode()+".sock"),
		Workspace: "/ws", StartedAt: time.Now(),
	}
	if err := rec.Save(dir); err != nil {
		t.Fatal(err)
	}
	wireLiveRegistry()

	var out strings.Builder
	quiet(t, func() { finishSession(ag, false, &out) })

	onDisk, err := store.Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.HostPID != owner.Process.Pid || len(onDisk.Messages) != 0 {
		t.Fatalf("the live host's file was written: %+v", onDisk)
	}
	metas, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 2 {
		t.Fatalf("want the owned session plus a new one, got %d: %+v", len(metas), metas)
	}
	var alt store.Meta
	for _, mt := range metas {
		if mt.ID != s.ID {
			alt = mt
		}
	}
	if !strings.Contains(out.String(), "saved as a new session: be-code --resume "+alt.Code) {
		t.Fatalf("finishSession printed:\n%s\nwant the new session's code %s", out.String(), alt.Code)
	}
	saved, err := store.Load(alt.ID)
	if err != nil || len(saved.Messages) != 2 || saved.HostPID != os.Getpid() {
		t.Fatalf("the transcript was not carried over: %v %+v", err, saved)
	}
}

// quiet runs fn with os.Stderr on the null device: finishSession reports
// the blocked save there by design, and a passing test should print
// nothing.
func quiet(t *testing.T, fn func()) {
	t.Helper()
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		fn()
		return
	}
	prev := os.Stderr
	os.Stderr = f
	defer func() { os.Stderr = prev; f.Close() }()
	fn()
}

// stubProvider is enough for the agent constructor; no test here talks to a
// backend.
type stubProvider struct{}

func (stubProvider) Name() string { return "stub" }
func (stubProvider) Chat(context.Context, provider.ChatRequest, provider.StreamFunc) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{}, nil
}
func (stubProvider) ListModels(context.Context) ([]provider.ModelInfo, error) { return nil, nil }
func (stubProvider) Ping(context.Context) (string, error)                     { return "ok", nil }
