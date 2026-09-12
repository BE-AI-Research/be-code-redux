package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/live"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/store"
	"github.com/brown-enterprises/be-code/internal/tui"
)

// hostStartTimeout bounds how long the launcher waits for the spawned host
// to open its socket. It has to cover buildAgent, whose slowest step is
// asking an Ollama backend for the model's context window — that loads the
// model when it is not resident, under a four-minute deadline of its own
// (see applyBackendWindow) — so this must be longer than that. The wait is
// not silent: the host's log is streamed while it starts.
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
	if ideSession != nil {
		ag.Tools.ReviewWrite = ideSession.ReviewWrite
		defer ideSession.Close()
	}

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

	err = tui.New(cfg, ag, p).RunServed(context.Background(), h)
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
	code := store.CodeFor(live.NewToken()) // fresh; the host stamps it on its session
	if flagResume != "" {
		s, err := store.Load(flagResume)
		if err != nil {
			return err
		}
		code = s.ResumeCode()
	}
	// Offer to attach to a live session for this workspace instead of
	// starting a second one on the same files. It is one question about one
	// session (the newest), never a walk through every record; and an
	// explicit --resume has already said which session the user wants, so the
	// offer only stands when the live one *is* that session.
	if r := newestLiveIn(dir, workspace); offerAttach(r, flagResume, code) {
		fmt.Printf("a live session for this workspace is running (%s, since %s). Attach to it? [Y/n] ",
			r.Code, r.StartedAt.Format("15:04"))
		var ans string
		fmt.Scanln(&ans)
		if ans == "" || strings.HasPrefix(strings.ToLower(ans), "y") {
			return attachLive(ctx, r, false)
		}
	}
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
	return attachLive(ctx, &rec, false)
}

// offerAttach reports whether to offer attaching to the live session r
// instead of starting a new one. An explicit --resume names the session the
// user wants, so the offer only stands when the live session is that one —
// otherwise the answer "yes" would silently attach them to a different
// session than the one they asked to resume.
func offerAttach(r *live.Record, resume, code string) bool {
	if r == nil {
		return false
	}
	return resume == "" || r.Code == code
}

// newestLiveIn returns the most recently started live session serving
// workspace, or nil when there is none. live.List prunes records whose host
// is gone, so a stale record never produces an offer to attach to nothing.
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

// attachLive runs this terminal as a client of rec's host until it detaches
// or the session ends.
func attachLive(ctx context.Context, rec *live.Record, view bool) error {
	opt := live.DefaultAttachOptions()
	opt.View = view
	reason, err := live.Attach(ctx, rec, opt)
	if err != nil {
		return err
	}
	if reason == "" {
		fmt.Printf("detached from %s (still running); be-code attach %s to return\n", rec.Code, rec.Code)
	} else {
		fmt.Printf("%s: %s\n", rec.Code, reason)
	}
	return nil
}

// findLive resolves a code (or "last") to a live record, or nil when there
// is no live host for it.
func findLive(dir, code string) *live.Record {
	lives, _ := live.List(dir)
	if code == "last" {
		var newest *live.Record
		for i := range lives {
			if newest == nil || lives[i].StartedAt.After(newest.StartedAt) {
				newest = &lives[i]
			}
		}
		return newest
	}
	for i := range lives {
		if lives[i].Code == code {
			return &lives[i]
		}
	}
	return nil
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
			return attachLive(cmd.Context(), rec, flagView)
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
	// reflows the session for the terminals that are really watching it. It
	// does briefly hold input (the host hands that to the newest client),
	// which is harmless for a connection whose only frame is "quit".
	hello := live.Hello{Token: rec.Token, Cols: 9999, Rows: 9999, Label: "sessions kill", UTF8: true}
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
