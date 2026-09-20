// Package cmd wires the BE-Code CLI (cobra), following BE-CLI conventions.
package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/checkpoint"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/ide"
	"github.com/brown-enterprises/be-code/internal/live"
	"github.com/brown-enterprises/be-code/internal/loader"
	"github.com/brown-enterprises/be-code/internal/mcp"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/review"
	"github.com/brown-enterprises/be-code/internal/setup"
	"github.com/brown-enterprises/be-code/internal/store"
	"github.com/brown-enterprises/be-code/internal/tools"
	"github.com/brown-enterprises/be-code/internal/tui"
	"github.com/brown-enterprises/be-code/internal/ui"
)

var (
	flagProvider    string
	flagModel       string
	flagDir         string
	flagYes         bool
	flagPlain       bool
	flagResume      string
	flagJSON        bool
	flagBenchModels string
	flagIDE         bool
	flagNoIDE       bool
	flagNoHost      bool
	flagNew         bool
	flagView        bool
	flagSessionHost string
)

// ideSession is the live editor bridge connection (nil when none), set by
// attachIDE during buildAgent and closed by the caller (runInteractive or
// the headless run command) on exit.
var ideSession *ide.Session

var rootCmd = &cobra.Command{
	Use:   "be-code",
	Short: "BE-Code — offline-first agentic coding CLI for local LLMs",
	Long: `BE-Code is an agentic coding tool built for local models (Ollama,
llama.cpp, vLLM, LM Studio, BE AI Engine). It combines a Claude Code-style
tool loop with a verification pipeline (build/lint/test + auto-repair)
that compensates for smaller models. Runs fully offline.`,
	SilenceUsage: true,
	Version:      Version,
	RunE: func(cmd *cobra.Command, args []string) error {
		if flagSessionHost != "" {
			// We are the detached host process launchServed spawned, not a
			// terminal: serve the session instead of attaching to one.
			return runSessionHost(flagSessionHost)
		}
		return runInteractive(cmd)
	},
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&flagProvider, "provider", "p", "", "provider name from config (default: config default_provider)")
	rootCmd.PersistentFlags().StringVarP(&flagModel, "model", "m", "", "model to use")
	rootCmd.PersistentFlags().StringVarP(&flagDir, "dir", "C", ".", "workspace directory")
	rootCmd.PersistentFlags().BoolVarP(&flagYes, "yes", "y", false, "auto-approve shell commands and file writes (headless use)")
	rootCmd.Flags().BoolVar(&flagPlain, "plain", false, "use the inline REPL instead of the full-screen TUI")
	rootCmd.PersistentFlags().StringVar(&flagResume, "resume", "", "resume a saved session by code, id, or 'last'")
	rootCmd.PersistentFlags().BoolVar(&flagIDE, "ide", false, "connect to the editor bridge even outside an editor terminal")
	rootCmd.PersistentFlags().BoolVar(&flagNoIDE, "no-ide", false, "never connect to the editor bridge")
	rootCmd.PersistentFlags().BoolVar(&flagNoHost, "no-host", false, "run the session in this process instead of a detachable host")
	rootCmd.PersistentFlags().BoolVar(&flagNew, "new", false, "start a fresh session even when this workspace has a live one")
	rootCmd.PersistentFlags().StringVar(&flagSessionHost, "session-host", "", "internal: serve the live session with this code")
	_ = rootCmd.PersistentFlags().MarkHidden("session-host")
	attachCmd.Flags().BoolVar(&flagView, "view", false, "attach read-only: never send input to the session")
	runCmd.Flags().BoolVar(&flagJSON, "json", false, "emit a machine-readable JSON result on stdout")
	benchCmd.Flags().StringVar(&flagBenchModels, "models", "", "comma-separated models to benchmark (default: current model)")
	benchCmd.Flags().BoolVar(&flagJSON, "json", false, "emit JSON results")
	rootCmd.AddCommand(runCmd, modelsCmd, pullCmd, doctorCmd, verifyCmd, configCmd, sessionsCmd, setupCmd, benchCmd, attachCmd, initCmd)
	sessionsCmd.AddCommand(sessionsDeleteCmd, sessionsKillCmd)
	mcp.ClientVersion = Version
}

// Execute is the entry point called from main.
func Execute() {
	if err := rootCmd.ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// stdinIsTTY is a var so tests can stub it (e.g. initApprove's non-TTY
// denial path).
var stdinIsTTY = func() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}

func stdoutIsTTY() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}

// loadOrWizard runs the first-launch wizard when no config exists yet and
// we're on a real terminal; otherwise loads (writing defaults if needed).
func loadOrWizard(ctx context.Context) (*config.Config, error) {
	if !config.Exists() && stdinIsTTY() && stdoutIsTTY() {
		return setup.Wizard(ctx, bufio.NewReader(os.Stdin), os.Stdout)
	}
	return config.Load()
}

// buildAgent assembles registry + agent from config and flags. The caller
// wires the approval func and events (UI-specific). headless is true for
// scripted runs (`be-code run`), which stay off the editor bridge unless
// --ide asks for it explicitly.
func buildAgent(cfg *config.Config, headless bool) (provider.Provider, *agent.Agent, error) {
	if flagYes {
		cfg.AutoApproveShell = true
		cfg.ApproveFileWrites = false
		// Its own flag, not AutoApproveShell: the shell approval's "a" sets
		// that one too, and choosing to stop being asked about commands is
		// not consent to send the workspace to an online co-worker.
		cfg.AutoApproveConsult = true
	}
	p, err := provider.FromConfig(cfg, flagProvider)
	if err != nil {
		return nil, nil, err
	}
	model := provider.ResolveModel(cfg, flagProvider, flagModel)

	reg, err := tools.NewRegistry(flagDir, nil)
	if err != nil {
		return nil, nil, err
	}
	reg.ApproveWrites = true // approver funcs consult cfg for auto-approve
	reg.ShellAllow = cfg.ShellAllow
	reg.ShellDeny = cfg.ShellDeny
	reg.Hooks = cfg.Hooks

	// MCP servers: spawn configured servers and expose their tools.
	for name, sc := range cfg.MCPServers {
		client, err := mcp.Dial(context.Background(), name, sc.Command, sc.Args, sc.Env)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warn: mcp server %s: %v\n", name, err)
			continue
		}
		names := reg.AttachMCP(client)
		fmt.Fprintf(os.Stderr, "mcp %s: %d tools (%s)\n", name, len(names), strings.Join(names, ", "))
	}

	// Web search (opt-in): Google Programmable Search Engine.
	if cfg.WebSearch.Enabled() {
		reg.AddTool(tools.NewWebSearch(tools.WebSearchConfig{
			CX: cfg.WebSearch.CX, APIKeyEnv: cfg.WebSearch.APIKeyEnv, MaxResults: cfg.WebSearch.MaxResults,
		}))
		if cfg.WebSearch.AllowFetch {
			reg.AddTool(tools.NewWebFetch())
		}
		if os.Getenv(cfg.WebSearch.APIKeyEnv) == "" {
			fmt.Fprintf(os.Stderr, "warn: web_search configured but %s is not set; searches will fail until it is exported\n", cfg.WebSearch.APIKeyEnv)
		}
	}

	notes := loadProjectNotes(reg.Root)
	ag := agent.New(cfg, p, model, reg, notes)

	// Co-working models: the agent has already taken the usable ones from
	// the config; the warnings for the unusable ones belong here, printed
	// once, and the consult tool exists only when there is someone to ask.
	if cws, warns := cfg.ValidCoworkers(); len(cws) > 0 || len(warns) > 0 {
		for _, w := range warns {
			fmt.Fprintf(os.Stderr, "warn: %s\n", w)
		}
		if len(cws) > 0 {
			roster := make([]tools.CoworkerInfo, 0, len(cws))
			for _, cw := range cws {
				roster = append(roster, tools.CoworkerInfo{Name: cw.Name, Skills: cw.Skills})
			}
			reg.AddTool(tools.NewConsult(roster, func(ctx context.Context, a tools.ConsultArgs) (string, error) {
				// RecentContext reads agent-goroutine-only fields; this
				// runs inside dispatch, which is that goroutine.
				res, err := ag.Consult(ctx, agent.ConsultRequest{
					Who: a.Who, Question: a.Question, Files: a.Files,
					Origin: "tool", Recent: ag.RecentContext(),
				})
				if err != nil {
					return "", err
				}
				out := "co-worker " + res.Coworker + " replied:\n\n" + res.Answer
				if res.Partial {
					out += "\n\n(the co-worker was cut short; this is what it had)"
				}
				return out, nil
			}))
			ag.RefreshSystem() // the known-tool list and the prompt must see consult
		}
	}

	if sess := attachIDE(cfg, reg, ag, headless); sess != nil {
		ideSession = sess // package var; closed in runInteractive/run defers
	}

	// Checkpoints for turn-level undo. Undo is session-scoped, so each
	// session's snapshot dir is deleted on clean exit; sweepStale catches
	// leftovers from crashed sessions.
	if d, derr := config.Dir(); derr == nil {
		cpRoot := filepath.Join(d, "checkpoints")
		sweepStale(cpRoot, 7*24*time.Hour)
		sid := time.Now().Format("20060102-150405.000")
		if cp, cerr := checkpoint.New(reg.Root, filepath.Join(cpRoot, sid)); cerr == nil {
			ag.Checkpoints = cp
		}
	}

	wireLiveRegistry()

	// Reviewer factory (avoids an agent→provider-registry import cycle).
	agent.ReviewerFactory = func(c *config.Config) (provider.Provider, string, error) {
		pname := c.Reviewer.Provider
		if pname == "" {
			pname = c.DefaultProvider
		}
		rp, err := provider.FromConfig(c, pname)
		if err != nil {
			return nil, "", err
		}
		secondaryLoad(c, rp, c.Reviewer.Model, ag)
		return rp, c.Reviewer.Model, nil
	}

	// Co-worker factory (same import-cycle dodge as ReviewerFactory).
	agent.CoworkerFactory = func(c *config.Config, cw config.CoworkerConfig) (provider.Provider, error) {
		cp, err := provider.FromConfig(c, cw.Provider)
		if err != nil {
			return nil, err
		}
		secondaryLoad(c, cp, cw.Model, ag)
		return cp, nil
	}

	if flagResume != "" {
		s, err := store.Load(flagResume)
		if err != nil {
			return nil, nil, err
		}
		ag.Resume(s)
		if s.Handoff != "" {
			fmt.Fprintf(os.Stderr, "resumed %s (%s) with handoff briefing\n", s.ResumeCode(), s.Title)
		}
	} else {
		ag.SetSession(store.NewSession(p.Name(), model, reg.Root))
	}
	// The store is keyed by workspace and needs the session id, so it opens
	// here rather than with the registry.
	attachEngine(cfg, reg, ag, flagResume != "")
	applyModelParams(cfg, p, reg, ag, model)
	return p, ag, nil
}

// chooseIDELock decides which live lock (if any) attachIDE should connect
// to, and whether the --ide "nothing listening" warning is due, without
// dialing anything. vscodeTerminal is TERM_PROGRAM=="vscode"; ideFlag is
// --ide.
//
// With --ide or a VS Code terminal, this is today's exact rule: ide.Discover
// (the lock covering workspace, else the newest live lock of any workspace),
// and the warning fires whenever --ide finds nothing.
//
// On the quiet path (neither), only a live lock that COVERS workspace and
// whose IDEName is "visualstudio" auto-attaches (spec §6) — a covering VS
// Code lock does not, since VS Code still needs its own terminal or --ide —
// and nothing is ever warned about, since silent terminals are the default.
func chooseIDELock(dir, workspace string, vscodeTerminal, ideFlag bool) (lock *ide.Lock, warnIfMissing bool, err error) {
	if ideFlag || vscodeTerminal {
		l, err := ide.Discover(dir, workspace)
		return l, ideFlag, err
	}
	locks, err := ide.DiscoverCovering(dir, workspace)
	if err != nil {
		return nil, false, err
	}
	for _, l := range locks {
		if l.IDEName == "visualstudio" {
			return l, false, nil
		}
	}
	return nil, false, nil
}

// ideLockToAttach decides, before any network dial, which live lock (if
// any) attachIDE should connect to. It owns every gate attachIDE used to
// apply inline: --no-ide always wins; --ide then forces a discovery attempt
// even when ide.enabled is false or the run is headless; otherwise
// ide.enabled must be on and the run must be interactive. From there it
// reads TERM_PROGRAM and hands the lock directory to chooseIDELock, which
// decides whether the terminal is VS Code's own (today's discovery,
// unchanged) or the quiet path, where only a covering Visual Studio lock
// attaches. It also owns the "--ide given but nothing is listening" warning
// (never printed on the quiet path) since that is part of the same
// before-dialling decision.
//
// Every interactive launch with ide.enabled on now reaches ide.LockDir()
// and prunes it — previously only a VS Code terminal or --ide did. That is
// safe: both the VS Code and Visual Studio extensions write their lock file
// atomically (temp file + rename), and the *.json suffix filter here can
// never match an in-progress temp file, so there is nothing to race with a
// partially written lock.
func ideLockToAttach(cfg *config.Config, headless bool, workspace string) *ide.Lock {
	if flagNoIDE {
		return nil
	}
	if !flagIDE {
		if !cfg.IDE.Enabled {
			return nil
		}
		// Scripted runs must be reproducible and never block on an editor:
		// no discovery unless --ide was passed on purpose.
		if headless {
			return nil
		}
	}
	dir, err := ide.LockDir()
	if err != nil {
		return nil
	}
	vscodeTerminal := os.Getenv("TERM_PROGRAM") == "vscode"
	lock, warnIfMissing, err := chooseIDELock(dir, workspace, vscodeTerminal, flagIDE)
	if err != nil || lock == nil {
		if warnIfMissing {
			fmt.Fprintln(os.Stderr, "warn: --ide given but no editor bridge is listening (is the BE-Code extension installed and active?)")
		}
		return nil
	}
	return lock
}

// attachIDE connects to an editor bridge when one is advertised and wanted
// (ideLockToAttach), registers its tools as ide_*, and wires context and
// review. Returns nil when there is no bridge to connect to, so ordinary
// terminal runs stay silent.
func attachIDE(cfg *config.Config, reg *tools.Registry, ag *agent.Agent, headless bool) *ide.Session {
	lock := ideLockToAttach(cfg, headless, reg.Root)
	if lock == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess, err := ide.Connect(ctx, lock)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: editor bridge at port %d: %v\n", lock.Port, err)
		return nil
	}
	names := reg.AttachMCPPrefixed(sess.Client, "ide_")
	ag.IDEName = lock.IDEName
	reg.EditorName = agent.EditorLabel(lock.IDEName)
	if ag.IDEName == "" {
		ag.IDEName = "ide"
	}
	ag.IDETools = len(names)
	// A scripted run takes no interactive detours: no per-turn context
	// note (it would make the same prompt behave differently depending on
	// what happens to be open in the editor) and no ReviewWrite — the
	// caller wires that only for interactive sessions.
	if cfg.IDE.AutoContext && !headless {
		ag.ContextProvider = sess.ContextNote
	}
	ag.SetGuidance(agent.IDEGuidanceFor(lock.IDEName))
	ag.RefreshSystem() // rebuilds the known-tool list (for embedded tool-call parsing) now that ide_* tools are attached, and recomposes the system prompt
	// The TUI prints this itself (a dimmed transcript line) because stderr
	// written before the alt screen opens is wiped; plain and headless
	// runs have no alt screen, so stderr is the right place there.
	if headless || usePlainUI(cfg) {
		fmt.Fprintf(os.Stderr, "%s connected: %d tools\n", agent.EditorLabel(lock.IDEName), len(names))
	}
	return sess
}

// usePlainUI reports whether the plain REPL (not the Bubble Tea TUI) will
// drive this session.
func usePlainUI(cfg *config.Config) bool {
	return flagPlain || strings.EqualFold(cfg.UI, "plain") || !stdoutIsTTY() || !stdinIsTTY()
}

// applyModelParams resolves the model's parameters through the loader and
// budgets the session against the window it actually gets.
//
// This replaces the startup probe that used to live here, which asked an
// Ollama backend what window it would use and, when nothing could answer,
// *loaded the model* to find out — a multi-minute stall before the first
// prompt, under a four-minute deadline. A configured context_window now
// means no probe at all; an unconfigured one costs two cheap reads.
//
// Consent is read through the registry at the moment it is needed, not
// captured now. Nothing has wired an approver during buildAgent, which is
// deliberate: reloading a model on a shared server evicts whatever else is
// using it, and a session must not be able to do that before anyone is
// watching. Startup therefore keeps whatever window the server already has.
func applyModelParams(cfg *config.Config, p provider.Provider, reg *tools.Registry, ag *agent.Agent, model string) {
	// One way to build a loader, used both now and again if /provider
	// moves this session to another backend — a loader speaks for exactly
	// one server, and its consent record is about that server's users.
	//
	// Notices go to the transcript when there is one, and to stderr only
	// while there is not. Under a TUI stderr is wiped by the alt screen,
	// and in a hosted session it is a log file nobody opens — which is
	// where every explanation of a refused reload used to end up.
	newLoader := func(c *config.Config, prov provider.Provider) *loader.Loader {
		l := loader.New(prov, c, nil, func(s string) {
			if !ag.Notice(s) {
				fmt.Fprintf(os.Stderr, "warn: %s\n", s)
			}
		})
		l.SetApprover(func() tools.ApproveFunc { return reg.Approve })
		// Preferred when the UI offers it: it lets a resolution's deadline
		// close the question it raised, on every attached terminal.
		l.SetApproverCtx(func() tools.ApproveCtxFunc { return reg.ApproveCtx })
		return l
	}
	agent.LoaderFactory = func(c *config.Config, prov provider.Provider) agent.ModelLoader {
		return newLoader(c, prov)
	}
	// Spec §10.1: the loader is the only path to a model's parameters, so
	// the agent holds it for every later request — a /model switch, a pick
	// from /models, a recovery after the backend-status check trips. It is
	// reached through Agent.Loader, not a package var: /provider replaces it
	// from a UI goroutine.
	ld := newLoader(cfg, p)
	ag.SetLoader(ld)

	n, err := ld.Apply(context.Background(), model)
	if _, isOllama := p.(*provider.Ollama); !isOllama {
		return // nothing to set and nothing to read: no window to report
	}
	configured := ld.Params(model).Window > 0
	if err != nil || n == 0 {
		// Only news when nothing was configured; with a window in config the
		// loader has already said why it could not be used.
		if !configured {
			budget := cfg.ContextTokens
			if budget <= 0 {
				budget, _, _ = ag.History.Scalars()
			}
			startupWarn(ag, fmt.Sprintf("could not determine the backend context window; using a budget of %d tokens. "+
				"Set \"context_window\" for this model in config to say what it really is.", budget))
		}
		return
	}
	// The clamp warning is only news when the server won. A window the user
	// configured is the answer they chose, and the loader has already said
	// so if the server refused to give it up.
	//
	// The budget is read *before* ApplyWindow because ApplyWindow overwrites
	// it: the number worth naming in the advice is the one the session was
	// going to use, not the one it has been cut down to, or the line reads
	// "budget clamped to 4096 ... start the server with
	// OLLAMA_CONTEXT_LENGTH=4096".
	wanted, _, _ := ag.History.Scalars()
	if ag.ApplyWindow(n) && !configured {
		startupWarn(ag, fmt.Sprintf("model %s runs with a %d-token window; budget clamped to %d. "+
			"Set \"context_window\" for this model in config, or start the server with OLLAMA_CONTEXT_LENGTH=%d.",
			model, n, n, wanted))
	}
	// The other direction, and the one that used to say nothing at all:
	// context_tokens is below the window, so most of a window the user went
	// to the trouble of configuring simply goes unused. ApplyWindow reports
	// no clamp here — the budget was already under the window — so without
	// this line the loss is invisible, which is the complaint that started
	// this work.
	if cfg.ContextTokens > 0 && n > cfg.ContextTokens {
		startupWarn(ag, fmt.Sprintf("model %s has a %d-token window but context_tokens=%d caps the budget; %d tokens go unused. "+
			"Remove \"context_tokens\" from config to use the whole window, or raise it.",
			model, n, cfg.ContextTokens, n-cfg.ContextTokens))
	}
}

// finishSession runs on every exit path: writes the handoff briefing so the
// next session can pick up without loss of fidelity, saves, and prints the
// resume code. withModel=false keeps headless runs fast.
func finishSession(ag *agent.Agent, withModel bool, out io.Writer) {
	s := ag.Session
	if s == nil || len(ag.History.Messages) == 0 {
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if withModel {
		fmt.Fprintln(os.Stderr, "writing session handoff for resume (Ctrl-C to skip the model summary)...")
	}
	// Working memory outlives the process: everything observed this run is
	// on disk before the briefing is written.
	if ag.Engine != nil {
		if err := ag.Engine.Flush(); err != nil {
			fmt.Fprintf(os.Stderr, "warn: engine: %v\n", err)
		}
	}
	if _, err := ag.WriteHandoff(ctx, withModel); err != nil {
		fmt.Fprintf(os.Stderr, "warn: handoff: %v\n", err)
	}
	// The same guard autosave runs: a session file a live host owns is that
	// program's to write, and the briefing just composed must not land on
	// top of its transcript. The transcript is still this run's work, so it
	// is written to a session of its own rather than dropped.
	if blocked, owner := ag.SaveGuard(); blocked {
		fmt.Fprintf(os.Stderr, "warn: session %s is owned by live host %d; this transcript was not written to it\n",
			s.ResumeCode(), owner)
		alt := store.NewSession(s.Provider, ag.Model, s.Workspace)
		// Session ids are stamped to the millisecond, so a fresh one can
		// collide with an existing file — including the very file this
		// branch exists to protect. Take the first id nothing answers to.
		for base, n := alt.ID, 1; ; n++ {
			if _, err := store.Load(alt.ID); err != nil {
				break
			}
			alt.ID = fmt.Sprintf("%s-%d", base, n)
			alt.Code = store.CodeFor(alt.ID)
		}
		alt.Title = s.Title
		alt.Handoff = s.Handoff
		alt.Messages = ag.History.Messages
		alt.HostPID = os.Getpid()
		if err := alt.Save(); err != nil {
			fmt.Fprintf(os.Stderr, "warn: session save: %v\n", err)
			return
		}
		fmt.Fprintf(out, "saved as a new session: be-code --resume %s   (%s)\n", alt.ResumeCode(), alt.Title)
		return
	}
	s.Messages = ag.History.Messages
	s.Model = ag.Model
	s.HostPID = os.Getpid()
	if err := s.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "warn: session save: %v\n", err)
		return
	}
	fmt.Fprintf(out, "resume: be-code --resume %s   (%s)\n", s.ResumeCode(), s.Title)
}

// wireLiveRegistry injects the live-session lookups the agent's save guard
// needs, for the same reason as ReviewerFactory: internal/agent must not
// import the registry. Together they answer "does an advertised live host
// own this session file?" — see agent.SaveGuard.
func wireLiveRegistry() {
	agent.PIDAlive = live.Alive
	agent.LiveOwner = func(code string) (int, bool) {
		dir, err := live.Dir()
		if err != nil {
			return 0, false
		}
		rec := live.LiveCode(dir, code)
		if rec == nil {
			return 0, false
		}
		return rec.PID, true
	}
}

// sweepStale removes checkpoint dirs older than maxAge (crashed sessions).
func sweepStale(root string, maxAge time.Duration) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, ierr := e.Info()
		if ierr == nil && info.ModTime().Before(cutoff) {
			_ = os.RemoveAll(filepath.Join(root, e.Name()))
		}
	}
}

// loadProjectNotes reads BECODE.md from the workspace root — the project
// memory file, equivalent to CLAUDE.md in Claude Code.
func loadProjectNotes(root string) string {
	for _, name := range []string{"BECODE.md", "becode.md", "CLAUDE.md"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err == nil {
			// Trimmed at a line boundary (never mid-rune) by the one helper
			// every notes path shares.
			return agent.TrimProjectNotes(string(data))
		}
	}
	return ""
}

// reviewMode reads ide.review, warning on stderr about a value the
// coordinator cannot use rather than silently reviewing somewhere the user
// did not ask for. An unset value is simply the default.
func reviewMode(v string) review.Mode {
	if strings.TrimSpace(v) == "" {
		return review.ModeAuto
	}
	m, err := review.Normalize(review.Mode(v))
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: ide.review %q is not one of auto|editor|tui|both; using auto\n", v)
	}
	return m
}

// runInteractive drives an interactive session. It takes the cobra command
// rather than a bare context so the served path can forward the root's
// persistent flags to the host it spawns (see hostArgs) without cmd/live.go
// having to reach back to rootCmd, which would be an initialization cycle.
func runInteractive(cmd *cobra.Command) error {
	ctx := cmd.Context()
	cfg, err := loadOrWizard(ctx)
	if err != nil {
		return err
	}
	// A TUI session normally lives in its own detached host process that
	// this terminal attaches to, so it survives the terminal and other
	// terminals can join it. Decide before building anything: the launcher
	// must not own a session it does not host (no tool registry, no MCP
	// servers, no editor bridge, no handoff on exit — those belong to the
	// host). Plain and non-TTY runs stay in-process.
	if !usePlainUI(cfg) && !flagNoHost && cfg.HostSessions {
		return launchServed(ctx, cfg, cmd.Root().PersistentFlags())
	}
	p, ag, err := buildAgent(cfg, false)
	if err != nil {
		return err
	}

	defer ag.Tools.Close()
	defer ag.Checkpoints.Cleanup()
	defer finishSession(ag, true, os.Stdout)
	// The editor is one of the two places a file change can be reviewed; the
	// UI below is the other. The coordinator picks between them (ide.review,
	// /review) and owns Registry.ReviewWrite for the whole session.
	mode := reviewMode(cfg.IDE.Review)
	var editor review.Editor
	if ideSession != nil {
		editor = ideSession.ReviewEditor()
		defer ideSession.Close()
	}
	usePlain := usePlainUI(cfg)
	if usePlain {
		if strings.EqualFold(cfg.Theme, "mono") {
			ui.SetMono()
		}
		ag.Events = ui.Events()
		repl, err := ui.NewREPL(cfg, ag, p)
		if err != nil {
			return err
		}
		// In-process: no client roster, so auto resolves to the editor.
		coord := review.New(mode, editor, repl.ReviewTerminal(), nil)
		coord.SetEditorName(agent.EditorLabel(ag.IDEName))
		repl.SetReview(coord)
		ag.Tools.ReviewWrite = coord.Decide
		// The "reviewing change in VS Code…" note belongs to reviews that
		// really reach the editor: mode "tui" (or no editor at all) resolves
		// in the terminal instead.
		ag.Tools.ReviewInvolvesEditor = func() bool { return coord.Resolve() != review.ModeTUI && editor != nil }
		// Plain mode answers on the one input stream its own loop reads,
		// so its half of the deferred consent runs inline, on the REPL
		// goroutine, after the reader is up and before the first line is
		// taken. The REPL owns the bounding and the prompt context (see
		// underPrompt), because a switch typed later needs exactly the
		// same treatment.
		repl.OnStart = func() { repl.ResolveModelParams(ctx) }
		return repl.Run(ctx)
	}
	s := tui.NewSession(cfg, ag, p)
	coord := review.New(mode, editor, s.ReviewTerminal(), nil)
	coord.SetEditorName(agent.EditorLabel(ag.IDEName))
	s.SetReview(coord)
	ag.Tools.ReviewWrite = coord.Decide
	ag.Tools.ReviewInvolvesEditor = func() bool { return coord.Resolve() != review.ModeTUI && editor != nil }
	// Now that NewSession has wired Registry.Approve, the question startup
	// could not put to anybody can be asked: it goes through the shared
	// approval modal, which is the only place under a TUI a person can see
	// it. A terminal that attaches after it is raised is shown it too
	// (Session.NewView), so this is safe to run before the program starts.
	ag.ResolveModel()
	return s.RunLocal(ctx)
}

// startupWarn reports a warning raised while the session is still being built.
// It goes to stderr, which is all a headless or plain run has, and is queued
// on the agent so a TUI or hosted session — where stderr is wiped or is a log
// file — shows it in the transcript once a UI exists.
func startupWarn(ag *agent.Agent, msg string) {
	fmt.Fprintf(os.Stderr, "warn: %s\n", msg)
	if ag != nil {
		ag.QueueNotice(msg)
	}
}

// secondaryLoad puts a reviewer's or co-worker's provider through a loader of
// its own before it is used. These providers are built fresh by the factories
// above and used to skip the loader entirely, so their requests carried no
// num_ctx at all — and on Ollama an absent num_ctx means the server default,
// not "whatever is loaded": a review of the primary's own model would reload
// it at 8192 and back again, evicting whoever else shares the server, with
// nobody asked. The loader has no approver here, which it reads as a refusal:
// a secondary model is never worth reloading someone else's. It keeps the
// window the server already holds and puts that on the wire.
func secondaryLoad(c *config.Config, prov provider.Provider, model string, ag *agent.Agent) {
	if _, ok := prov.(*provider.Ollama); !ok || model == "" {
		return
	}
	l := loader.New(prov, c, nil, func(msg string) {
		if ag == nil || !ag.Notice(msg) {
			fmt.Fprintf(os.Stderr, "warn: %s\n", msg)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = l.Apply(ctx, model)
}
