package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/live"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/review"
	"github.com/brown-enterprises/be-code/internal/store"
	"github.com/brown-enterprises/be-code/internal/tui"
)

// hostStartTimeout bounds how long the launcher waits for the spawned host
// to open its socket. It has to cover buildAgent, which since the loader
// landed no longer loads a model to read its window (see applyModelParams):
// the slowest steps left are spawning MCP servers and scanning the
// workspace. The generous bound stays because a cold NFS checkout or a
// stalled MCP server is still slow, and failing a session start for
// impatience is worse than waiting. The wait is not silent: the host's log
// is streamed while it starts.
const hostStartTimeout = 5 * time.Minute

// quitWait is how long `sessions kill` waits quietly for a host to shut
// itself down after a quit frame, and quitGrace how much longer it waits
// after saying so. The two stages exist because a clean shutdown writes the
// handoff briefing with the model, which on a local model is routinely
// slower than the first stage — terminating there would throw away exactly
// the briefing this feature is for — while an unreachable backend must not
// hold the command forever.
// They are vars rather than consts only so the kill test can shrink them
// instead of genuinely waiting out a minute of grace.
var (
	quitWait  = 10 * time.Second
	quitGrace = 60 * time.Second
	// killWait is the short window after a SIGKILL. Nothing runs in the host
	// after that signal, so this only covers the kernel reaping it.
	killWait = 2 * time.Second
)

// runSessionHost is the detached process behind a served TUI session: it
// owns the agent, runs the TUI over a live.Host instead of a terminal, and
// advertises itself in ~/.be-code/live so terminals can attach by code.
// Started as `be-code --session-host <code>` by launchServed, which has
// already written the live record (and forwards -C/--resume/--provider/
// --model as ordinary flags).
func runSessionHost(code string) error {
	dir, err := live.Dir()
	if err != nil {
		return err
	}
	rec, err := live.Load(dir, code)
	if err != nil {
		return fmt.Errorf("live record for %s: %w", code, err)
	}
	// From here every exit path retires the record, so `be-code sessions`
	// never advertises a host that has gone.
	defer live.Remove(dir, code)
	rec.PID = os.Getpid() // the launcher wrote its own pid to keep the record alive across the handover
	if err := rec.Save(dir); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	p, ag, err := buildAgent(cfg, false)
	if err != nil {
		return err
	}
	if flagResume == "" {
		// The advertised attach code doubles as the resume code, so the
		// line finishSession prints is the code used to attach.
		ag.Session.Code = rec.Code
	}

	h := live.NewHost(rec.Token, os.Stderr) // stderr is the host's log file
	if err := h.Listen(rec.Socket); err != nil {
		return err
	}
	go h.Serve()
	defer ag.Tools.Close()
	defer ag.Checkpoints.Cleanup()
	var editor review.Editor
	if ideSession != nil {
		editor = ideSession.ReviewEditor()
		defer ideSession.Close()
	}
	s := tui.NewSession(cfg, ag, p)
	// Everything a dispatched sub-agent's own registry reads must be in
	// place before StartSubAgentsAsync below can possibly reach one — see
	// the matching comment in cmd/root.go's runInteractive, which this
	// mirrors exactly and where the hazard is spelled out in full: under
	// -y the gate returns instantly, and a sub-agent's first write reads
	// ReviewWrite/ReviewInvolvesEditor through a.Tools.Scoped before this
	// goroutine would otherwise have assigned them. This is the second time
	// ordering around StartSubAgentsAsync has bitten, so this block runs
	// first, deliberately, and h (the host) is already built above with
	// everything Clients() needs.
	//
	// Served: auto resolves per write from the roster — the editor alone
	// while VS Code's own terminal is the only one attached, both places as
	// soon as anyone else joins. Clients() only takes the host's lock to
	// copy the roster, and this runs on the agent goroutine (never inside
	// the program's Update), so it cannot deadlock the host.
	coord := review.New(reviewMode(cfg.IDE.Review), editor, s.ReviewTerminal(), func() []string {
		infos := h.Clients()
		labels := make([]string, 0, len(infos))
		for _, c := range infos {
			labels = append(labels, c.Label)
		}
		return labels
	})
	coord.SetEditorName(agent.EditorLabel(ag.IDEName))
	s.SetReview(coord)
	ag.Tools.ReviewWrite = coord.Decide
	// See root.go: the editor-side status note only for reviews that reach it.
	ag.Tools.ReviewInvolvesEditor = func() bool { return coord.Resolve() != review.ModeTUI && editor != nil }
	// NewSession has just wired Registry.Approve and Agent.Events; a dispatch
	// before that would ask consent of nobody and print to nobody here
	// either. But this goroutine still has to reach s.RunServed below, and a
	// synchronous StartSubAgents blocks on the resume ask's answer — the
	// attached client would render nothing and the host log would stay
	// empty until someone answered a question nothing had shown them yet.
	// StartSubAgentsAsync keeps the resume pass here (it never blocks) and
	// moves the gate and the first schedule to a goroutine of their own; see
	// runInteractive's own comment on this same call in cmd/root.go.
	ag.StartSubAgentsAsync()

	// The hosted case is the one that most needed this. Here stdio is the
	// host's log file, so a loader notice printed at startup is written
	// where nobody will read it, and a consent prompt had nobody at all to
	// ask. NewSession has now wired Registry.Approve, so re-running the
	// resolution puts the question on the shared modal every attached
	// terminal renders — including one that attaches after it was raised.
	ag.ResolveModel()

	// A signal must take the same route as a client's /quit, or the process
	// would die past its defers: no handoff briefing, no tool cleanup, and
	// MCP server children orphaned. `be-code sessions kill` falls back to
	// SIGTERM, so this is a normal path, not just a courtesy.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go func() {
		for range sigs {
			h.RequestQuit()
		}
	}()

	err = s.RunServed(context.Background(), h, rec, dir)
	// Order matters: the resume line goes to every attached terminal, so it
	// has to be written before Close says goodbye to them. Two details make
	// it actually readable there: every client is in raw mode with its own
	// alt screen (see live.Attach), so these lines are wrapped in a CRLF
	// translation — a bare LF would staircase them — and the clients are
	// taken back to the normal screen first, so the resume line survives
	// their restore instead of vanishing with the alt buffer.
	out := live.NewCRLFWriter(h.Output())
	fmt.Fprint(out, live.ExitAltScreen)
	fmt.Fprintln(out, "\nfinishing session (writing the handoff briefing)...")
	finishSession(ag, true, out)
	h.Close(live.ReasonEnded)
	return err
}

// launchServed starts a detached host for this session and attaches to it.
// The launcher deliberately builds no agent of its own: the host owns the
// session, the tools, the MCP servers and the editor bridge.
func launchServed(ctx context.Context, cfg *config.Config, flags *pflag.FlagSet) error {
	dir, err := live.Dir()
	if err != nil {
		return err
	}
	workspace, err := filepath.Abs(flagDir)
	if err != nil {
		return err
	}
	// Join, never fork: a session that is already live anywhere is attached
	// to, not loaded a second time. No question is asked — a second program
	// on one session file is never what the user meant, and --new is there
	// for the one case where they do want a second session.
	if rec, msg := decideStart(dir, workspace, flagResume, flagNew); rec != nil {
		fmt.Println(msg)
		joined, err := joinLive(ctx, rec, cfg)
		if err != nil {
			return err
		}
		if joined {
			return nil
		}
		// The host was on its way out as we arrived (it advertises its
		// record and answers on its socket until the very last moment of
		// finishSession). Joining must never refuse or fork, so this is not
		// an error: there is simply no live session after all, and the
		// fresh-host path below starts one — resuming the saved file when
		// --resume named it. Give the record a moment to be retired first,
		// or the guard below would see the host that has just gone.
		fmt.Printf("%s ended as you joined it; starting a session instead\n", rec.Code)
		gone(dir, rec.Code, 2*time.Second)
	}
	code := store.CodeFor(live.NewToken()) // fresh; the host stamps it on its session
	if flagResume != "" {
		s, err := store.Load(flagResume)
		if err != nil {
			return err
		}
		code = s.ResumeCode()
	}
	// Normally unreachable for --resume (decideStart has just joined any
	// live host for that code) and vanishingly unlikely for a fresh code,
	// but a second host on one session file is exactly what this whole path
	// exists to prevent, so the guard stays — and a join that missed by a
	// hair (above) comes through here with the departing host's own code.
	// findLive, not live.Load: a record left behind by a host that is gone
	// must not block a new session under the same code.
	if findLive(dir, code) != nil {
		return fmt.Errorf("session %s is already live; use: be-code attach %s", code, code)
	}
	model := provider.ResolveModel(cfg, flagProvider, flagModel)
	rec := live.Record{
		Code: code, PID: os.Getpid(), Socket: live.SocketPath(dir, code),
		Workspace: workspace, Model: model, StartedAt: time.Now(), Token: live.NewToken(),
	}
	// PID is ours until the host overwrites it with its own: a record whose
	// pid is dead is pruned by live.List, so a `be-code sessions` running
	// concurrently with this spawn must not see an unowned record.
	if err := rec.Save(dir); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	args := hostArgs(code, workspace, flags)
	logPath := filepath.Join(dir, code+".log")
	if _, err := live.SpawnHost(self, "--session-host", logPath, os.Environ(), args...); err != nil {
		live.Remove(dir, code)
		return fmt.Errorf("could not start the session host: %w (use --no-host to run in-process)", err)
	}
	fmt.Printf("starting session %s (log: %s)\n", code, logPath)
	stop := make(chan struct{})
	tailDone := make(chan struct{})
	go func() { defer close(tailDone); tailLog(logPath, stop, os.Stdout) }()
	err = waitForHost(dir, code, rec.Socket)
	// Wait for the tail to finish before attaching: once Attach owns the
	// screen, a late log line written underneath it would corrupt the
	// rendering.
	close(stop)
	<-tailDone
	if err != nil {
		return fmt.Errorf("%w (see %s; use --no-host to run in-process)", err, logPath)
	}
	return attachLive(ctx, &rec, false, cfg)
}

// decideStart picks a live session to join, or nil to start a fresh host,
// and returns the line to print before joining. An explicit --resume names
// the session the user wants, so it is looked up by itself and the
// workspace's other live sessions are left alone; --new is the only way to
// ask for a second session on a workspace that already has one.
func decideStart(dir, workspace, resume string, fresh bool) (*live.Record, string) {
	if resume = strings.TrimSpace(resume); resume != "" {
		// The live registry first, and by the code as typed: a host's
		// session exists in its memory from the moment it starts, but the
		// file only appears on the first autosave — so a live session with
		// no turns yet is not loadable from the store, and going through it
		// would turn "join the session I can see running" into "no such
		// session". Resume codes are upper-case; the store gets the same
		// string untouched, because what --resume names may equally be a
		// session id or "last", neither of which survives upper-casing (and
		// store.Load upper-cases for itself when it falls back to a code).
		if rec := live.LiveCode(dir, strings.ToUpper(resume)); rec != nil {
			return rec, "joining live session " + rec.Code
		}
		s, err := store.Load(resume)
		if err != nil {
			// Not a session we can resolve: let the ordinary --resume path
			// report it.
			return nil, ""
		}
		// A saved session resumed by id (or "last") may still be live under
		// its code.
		if rec := live.LiveCode(dir, s.ResumeCode()); rec != nil {
			return rec, "joining live session " + rec.Code
		}
		return nil, ""
	}
	if fresh {
		return nil, ""
	}
	if rec := newestLiveIn(dir, workspace); rec != nil {
		return rec, fmt.Sprintf("joining live session %s (be-code --new starts a fresh one)", rec.Code)
	}
	return nil, ""
}

// newestLiveIn returns the most recently started live session serving
// workspace, or nil when there is none. live.List prunes records whose host
// is gone, so a stale record never joins a session that is not there.
func newestLiveIn(dir, workspace string) *live.Record {
	lives, _ := live.List(dir)
	var newest *live.Record
	for i := range lives {
		if lives[i].Workspace != workspace {
			continue
		}
		if newest == nil || lives[i].StartedAt.After(newest.StartedAt) {
			newest = &lives[i]
		}
	}
	return newest
}

// waitForHost waits for the spawned host to open its socket. It gives up as
// soon as the host has retired its record, which is what a host that failed
// to start does (its own error went to the log), rather than sitting out the
// whole timeout — which exists only to cover a slow but working start, where
// buildAgent may be waiting on a model being loaded.
func waitForHost(dir, code, sock string) error {
	start := time.Now()
	deadline := start.Add(hostStartTimeout)
	nextNote := 10 * time.Second
	for {
		if err := live.WaitForSocket(sock, 200*time.Millisecond); err == nil {
			return nil
		}
		rec, err := live.Load(dir, code)
		if err != nil {
			return fmt.Errorf("the session host exited during startup")
		}
		// A host killed outright (SIGKILL, OOM) never retires its record, so
		// the record alone is not proof of life.
		if !live.Alive(rec.PID) {
			return fmt.Errorf("the session host died during startup (pid %d)", rec.PID)
		}
		if el := time.Since(start); el >= nextNote {
			fmt.Printf("still starting (%ds)...\n", int(el.Seconds()))
			nextNote = el.Truncate(10*time.Second) + 10*time.Second
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("the session host did not start within %s", hostStartTimeout)
		}
	}
}

// setResume turns on --resume from inside the program, for the attach
// command's fallback to a saved session. It has to go through the flag set
// rather than assign flagResume: the binding updates the variable either
// way, but only FlagSet.Set marks the flag Changed, and hostArgs forwards
// exactly the changed flags — so assigning the variable would leave the
// spawned host resuming nothing while the launcher believed it would.
func setResume(flags *pflag.FlagSet, code string) error {
	if err := flags.Set("resume", code); err != nil {
		return fmt.Errorf("--resume %s: %w", code, err)
	}
	return nil
}

// hostArgs builds the spawned host's argument list: the advertised code, the
// resolved workspace, and every root persistent flag the user actually
// changed. It forwards them wholesale rather than naming a few, because the
// host is the process that runs buildAgent — a flag that reaches the
// launcher but not the host (-y, --ide, --no-ide) would silently stop
// working now that hosting is the default, and a flag added later would be
// dropped in the same way.
func hostArgs(code, workspace string, flags *pflag.FlagSet) []string {
	// --dir is passed explicitly as the resolved absolute workspace: the
	// host has its own working directory, so the launcher's relative -C
	// would mean something else there (and often nothing at all).
	args := []string{code, "--dir=" + workspace}
	flags.VisitAll(func(f *pflag.Flag) {
		if !f.Changed {
			return
		}
		switch f.Name {
		case "dir": // already passed, resolved
			return
		case "session-host": // this is what marks the host; never forward it
			return
		case "no-host": // the launcher would not be here if it were set
			return
		}
		args = append(args, "--"+f.Name+"="+f.Value.String())
	})
	return args
}

// tailLog copies whatever the starting host writes to its log through to w
// until stop is closed, so a slow start (a model being loaded) or a warning
// is visible instead of a silent wait.
func tailLog(path string, stop <-chan struct{}, w io.Writer) {
	var off int64
	buf := make([]byte, 4096)
	for {
		if f, err := os.Open(path); err == nil {
			if _, err := f.Seek(off, io.SeekStart); err == nil {
				for {
					n, rerr := f.Read(buf)
					if n > 0 {
						w.Write(buf[:n])
						off += int64(n)
					}
					if rerr != nil || n == 0 {
						break
					}
				}
			}
			f.Close()
		}
		select {
		case <-stop:
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// attachOptions is live.DefaultAttachOptions, indirected so tests can run
// an attach without a real terminal to put in raw mode.
var attachOptions = live.DefaultAttachOptions

// attachLive runs this terminal as a client of rec's host until it detaches
// or the session ends. A "switch:CODE" bye reason hands the terminal to
// another live session instead of ending the attach: the loop reattaches to
// the named record and only returns once there is nowhere left to go.
func attachLive(ctx context.Context, rec *live.Record, view bool, cfg *config.Config) error {
	_, err := attachOrJoin(ctx, rec, view, false, cfg)
	return err
}

// joinLive is attachLive on a launcher's join path, where an attach that
// never joined is not a failure but a fresh session waiting to be started:
// it reports whether this terminal actually joined rec's session (see
// joinMissed). Only the first attach can miss — once the session has
// rendered here, a later switch is an ordinary attach.
func joinLive(ctx context.Context, rec *live.Record, cfg *config.Config) (bool, error) {
	return attachOrJoin(ctx, rec, false, true, cfg)
}

// joinMissed reports whether an attach on a join path never joined at all:
// the host's socket could not be dialled, or the host said the session had
// ended before it had rendered a single frame to this terminal. Both mean
// the record we found was a host in its shutdown window (it retires the
// record last of all), not a session to join.
func joinMissed(reason string, joined bool, err error) bool {
	if err != nil {
		return errors.Is(err, live.ErrDial)
	}
	return !joined && reason == live.ReasonEnded
}

func attachOrJoin(ctx context.Context, rec *live.Record, view, join bool, cfg *config.Config) (bool, error) {
	dir, err := live.Dir()
	if err != nil {
		return false, err
	}
	for {
		opt := attachOptions()
		opt.View = view
		// This device's declared chat identity (chat.name), so the host's
		// roster — and so the room's join/leave lines — can name it without
		// falling back to the device label.
		opt.User = cfg.Chat.Name
		var joined atomic.Bool
		opt.Joined = &joined
		reason, err := live.Attach(ctx, rec, opt)
		if join {
			// Only the first attach of a join can miss; after that this is
			// an ordinary attach loop following switches.
			join = false
			if joinMissed(reason, joined.Load(), err) {
				return false, nil
			}
		}
		if err != nil {
			return true, err
		}
		next, msg := nextAttach(dir, rec.Code, reason)
		if msg != "" {
			fmt.Println(msg)
		}
		if next == nil {
			return true, nil
		}
		rec = next
	}
}

// nextAttach interprets a bye reason: a switch names the record to attach
// next (silently, no message — the terminal is handed straight over);
// everything else ends the attach with a line for the user.
func nextAttach(dir, code, reason string) (*live.Record, string) {
	if target, ok := live.SwitchTarget(reason); ok {
		if rec := findLive(dir, target); rec != nil {
			return rec, ""
		}
		return nil, fmt.Sprintf("%s ended before you could join it", target)
	}
	// "" is this terminal's own Ctrl+] d; ReasonDetached is the host having
	// detached it (a `/detach` typed inside the session). Both leave the
	// session running, so both get the line that says how to come back.
	if reason == "" || reason == live.ReasonDetached {
		return nil, fmt.Sprintf("detached from %s (still running); be-code attach %s to return", code, code)
	}
	return nil, fmt.Sprintf("%s: %s", code, reason)
}

// findLive resolves a code (or "last") to a live record, or nil when there
// is no live host for it.
func findLive(dir, code string) *live.Record {
	if code != "last" {
		return live.LiveCode(dir, code)
	}
	lives, _ := live.List(dir)
	var newest *live.Record
	for i := range lives {
		if newest == nil || lives[i].StartedAt.After(newest.StartedAt) {
			newest = &lives[i]
		}
	}
	return newest
}

var attachCmd = &cobra.Command{
	Use:   "attach <code|last>",
	Short: "Attach this terminal to a live session (see: be-code sessions)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := live.Dir()
		if err != nil {
			return err
		}
		code := args[0]
		if rec := findLive(dir, code); rec != nil {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			return attachLive(cmd.Context(), rec, flagView, cfg)
		}
		// Nothing live under that code: the obvious intent is to pick the
		// saved session back up, which is what --resume does.
		fmt.Fprintf(os.Stderr, "no live session %s; resuming the saved one\n", code)
		if flagView {
			fmt.Fprintln(os.Stderr, "note: --view only applies to a live session; this is a normal resumed session")
		}
		if err := setResume(cmd.Root().PersistentFlags(), code); err != nil {
			return err
		}
		return runInteractive(cmd)
	},
}

var sessionsKillCmd = &cobra.Command{
	Use:   "kill <code>",
	Short: "End a live session host (asks it to quit, then terminates it)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := live.Dir()
		if err != nil {
			return err
		}
		code := args[0]
		rec := findLive(dir, code)
		if rec == nil {
			return fmt.Errorf("no live session %s", code)
		}
		if err := requestQuit(rec); err != nil {
			fmt.Fprintf(os.Stderr, "warn: could not ask %s to quit (%v)\n", rec.Code, err)
		} else {
			if gone(dir, rec.Code, quitWait) {
				fmt.Printf("%s ended\n", rec.Code)
				return nil
			}
			fmt.Fprintf(os.Stderr, "%s is still finishing (writing its handoff briefing); waiting up to %s\n", rec.Code, quitGrace)
			if gone(dir, rec.Code, quitGrace) {
				fmt.Printf("%s ended\n", rec.Code)
				return nil
			}
			fmt.Fprintf(os.Stderr, "warn: %s did not shut down; terminating it (its handoff briefing may be incomplete)\n", rec.Code)
		}
		if err := live.Terminate(rec.PID); err != nil {
			fmt.Fprintf(os.Stderr, "warn: terminating pid %d: %v\n", rec.PID, err)
		}
		// The host turns a signal into the same shutdown a quit frame asks
		// for, so give it one more short window to retire its own record.
		if gone(dir, rec.Code, quitWait) {
			fmt.Printf("%s ended\n", rec.Code)
			return nil
		}
		// Still there: escalate rather than tidy the record away and call it
		// killed. A record removed under a live host leaves the session
		// unreachable (no code to attach by) but still holding the workspace,
		// the MCP children and the socket.
		if live.Alive(rec.PID) {
			fmt.Fprintf(os.Stderr, "warn: %s ignored the signal; killing pid %d (no handoff briefing)\n", rec.Code, rec.PID)
			if err := live.Kill(rec.PID); err != nil {
				fmt.Fprintf(os.Stderr, "warn: killing pid %d: %v\n", rec.PID, err)
			}
			gone(dir, rec.Code, killWait)
			if live.Alive(rec.PID) {
				return fmt.Errorf("%s (pid %d) is still running; its record is left in place", rec.Code, rec.PID)
			}
		}
		if err := live.Remove(dir, rec.Code); err != nil {
			return err
		}
		fmt.Printf("killed %s\n", rec.Code)
		return nil
	},
}

// gone waits up to d for the host's record to be retired, which it does on
// every exit path, and reports whether it was.
func gone(dir, code string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := live.Load(dir, code); err != nil {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// requestQuit connects to a host as a throwaway client and asks it to quit,
// which is the same path /quit takes: the session writes its handoff and
// every attached terminal sees the resume line.
func requestQuit(rec *live.Record) error {
	conn, err := net.Dial("unix", rec.Socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	// Oversized dimensions so this client never becomes the smallest one and
	// reflows the session for the terminals that are really watching it.
	// Its keystrokes would be delivered tagged like any other client's, but
	// it sends none: the only frame it writes is "quit".
	hello := live.Hello{Token: rec.Token, Cols: 9999, Rows: 9999, Label: "sessions kill", UTF8: true, Control: true}
	if err := live.WriteJSON(conn, live.FHello, hello); err != nil {
		return err
	}
	if err := live.WriteFrame(conn, live.FQuit, nil); err != nil {
		return err
	}
	// A rejected hello is answered with a bye and nothing else, while an
	// accepted one is answered with the client roster first — so a bye as the
	// very first frame means the quit never reached the session, and saying so
	// now beats waiting out the shutdown windows for a host that never heard it.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	typ, payload, err := live.ReadFrame(conn)
	if err == nil && typ == live.FBye {
		var b live.Bye
		json.Unmarshal(payload, &b)
		if b.Reason == "" {
			b.Reason = "rejected"
		}
		return fmt.Errorf("host refused the connection: %s", b.Reason)
	}
	return nil
}
