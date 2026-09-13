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
		return rp, c.Reviewer.Model, nil
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
	applyBackendWindow(cfg, p, ag, model)
	return p, ag, nil
}

// attachIDE connects to an editor bridge when one is advertised and wanted,
// registers its tools as ide_*, and wires context and review. Returns nil
// (and prints nothing beyond an explicit --ide failure) when there is no
// bridge to connect to, so ordinary terminal runs stay silent.
//
// Wanting an editor is decided in this order: --no-ide always wins; --ide
// then forces a connection attempt even when ide.enabled is false or the
// run is headless; otherwise config must allow it, the run must be
// interactive, and the terminal must be VS Code's own.
func attachIDE(cfg *config.Config, reg *tools.Registry, ag *agent.Agent, headless bool) *ide.Session {
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
		if os.Getenv("TERM_PROGRAM") != "vscode" {
			return nil
		}
	}
	dir, err := ide.LockDir()
	if err != nil {
		return nil
	}
	lock, err := ide.Discover(dir, reg.Root)
	if err != nil || lock == nil {
		if flagIDE {
			fmt.Fprintln(os.Stderr, "warn: --ide given but no editor bridge is listening (is the BE-Code extension installed and active?)")
		}
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
	ag.SetGuidance(agent.IDEGuidance)
	ag.RefreshSystem() // rebuilds the known-tool list (for embedded tool-call parsing) now that ide_* tools are attached, and recomposes the system prompt
	// The TUI prints this itself (a dimmed transcript line) because stderr
	// written before the alt screen opens is wiped; plain and headless
	// runs have no alt screen, so stderr is the right place there.
	if headless || usePlainUI(cfg) {
		fmt.Fprintf(os.Stderr, "VS Code connected: %d tools\n", len(names))
	}
	return sess
}

// usePlainUI reports whether the plain REPL (not the Bubble Tea TUI) will
// drive this session.
func usePlainUI(cfg *config.Config) bool {
	return flagPlain || strings.EqualFold(cfg.UI, "plain") || !stdoutIsTTY() || !stdinIsTTY()
}

// applyBackendWindow asks an Ollama backend what context window it will
// really use for the model and clamps the history budget to it. Ollama's
// OpenAI endpoint cannot set num_ctx per request and silently truncates
// oversized prompts, so a budget larger than the window means the model
// quietly loses its instructions and history.
func applyBackendWindow(cfg *config.Config, p provider.Provider, ag *agent.Agent, model string) {
	o, ok := p.(*provider.Ollama)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	n, err := o.ContextLength(ctx, model)
	if err != nil {
		return // unreachable backend: the first request will report it
	}
	if n == 0 {
		// Not loaded and no Modelfile num_ctx: load it now (the first
		// request would anyway) so /api/ps can report the live window.
		fmt.Fprintf(os.Stderr, "loading %s to read its context window...\n", model)
		keep := 30 * time.Minute
		if d, err := time.ParseDuration(cfg.KeepAlive); err == nil && d > 0 {
			keep = d
		}
		if werr := o.Warm(ctx, model, keep); werr == nil {
			n, _ = o.ContextLength(ctx, model)
		}
	}
	if n == 0 {
		fmt.Fprintf(os.Stderr, "warn: could not determine the backend context window; using context_tokens=%d\n", cfg.ContextTokens)
		return
	}
	if ag.ApplyWindow(n) {
		fmt.Fprintf(os.Stderr, "warn: backend context window is %d tokens, below context_tokens=%d; budget clamped to %d.\n"+
			"      Raise the window on the server (OLLAMA_CONTEXT_LENGTH=%d, or a Modelfile with PARAMETER num_ctx %d).\n",
			n, cfg.ContextTokens, n, cfg.ContextTokens, cfg.ContextTokens)
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
		repl.SetReview(coord)
		ag.Tools.ReviewWrite = coord.Decide
		// The "reviewing change in VS Code…" note belongs to reviews that
		// really reach the editor: mode "tui" (or no editor at all) resolves
		// in the terminal instead.
		ag.Tools.ReviewInvolvesEditor = func() bool { return coord.Resolve() != review.ModeTUI && editor != nil }
		return repl.Run(ctx)
	}
	m := tui.New(cfg, ag, p)
	coord := review.New(mode, editor, m.ReviewTerminal(), nil)
	m.SetReview(coord)
	ag.Tools.ReviewWrite = coord.Decide
	ag.Tools.ReviewInvolvesEditor = func() bool { return coord.Resolve() != review.ModeTUI && editor != nil }
	return m.Run(ctx)
}
