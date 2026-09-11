// Package cmd wires the BE-Code CLI (cobra), following BE-CLI conventions.
package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/checkpoint"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/mcp"
	"github.com/brown-enterprises/be-code/internal/provider"
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
)

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
		return runInteractive(cmd.Context())
	},
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&flagProvider, "provider", "p", "", "provider name from config (default: config default_provider)")
	rootCmd.PersistentFlags().StringVarP(&flagModel, "model", "m", "", "model to use")
	rootCmd.PersistentFlags().StringVarP(&flagDir, "dir", "C", ".", "workspace directory")
	rootCmd.PersistentFlags().BoolVarP(&flagYes, "yes", "y", false, "auto-approve shell commands and file writes (headless use)")
	rootCmd.Flags().BoolVar(&flagPlain, "plain", false, "use the inline REPL instead of the full-screen TUI")
	rootCmd.PersistentFlags().StringVar(&flagResume, "resume", "", "resume a saved session by code, id, or 'last'")
	runCmd.Flags().BoolVar(&flagJSON, "json", false, "emit a machine-readable JSON result on stdout")
	benchCmd.Flags().StringVar(&flagBenchModels, "models", "", "comma-separated models to benchmark (default: current model)")
	benchCmd.Flags().BoolVar(&flagJSON, "json", false, "emit JSON results")
	rootCmd.AddCommand(runCmd, modelsCmd, pullCmd, doctorCmd, verifyCmd, configCmd, sessionsCmd, setupCmd, benchCmd)
	sessionsCmd.AddCommand(sessionsDeleteCmd)
	mcp.ClientVersion = Version
}

// Execute is the entry point called from main.
func Execute() {
	if err := rootCmd.ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func stdinIsTTY() bool {
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
// wires the approval func and events (UI-specific).
func buildAgent(cfg *config.Config) (provider.Provider, *agent.Agent, error) {
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
		ag.Session = store.NewSession(p.Name(), model, reg.Root)
	}
	applyBackendWindow(cfg, p, ag, model)
	return p, ag, nil
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
func finishSession(ag *agent.Agent, withModel bool, out *os.File) {
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
	s.Messages = ag.History.Messages
	s.Model = ag.Model
	if err := s.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "warn: session save: %v\n", err)
		return
	}
	fmt.Fprintf(out, "resume: be-code --resume %s   (%s)\n", s.ResumeCode(), s.Title)
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
			const limit = 8 * 1024
			if len(data) > limit {
				data = data[:limit]
			}
			return string(data)
		}
	}
	return ""
}

func runInteractive(ctx context.Context) error {
	cfg, err := loadOrWizard(ctx)
	if err != nil {
		return err
	}
	p, ag, err := buildAgent(cfg)
	if err != nil {
		return err
	}

	defer ag.Tools.Close()
	defer ag.Checkpoints.Cleanup()
	defer finishSession(ag, true, os.Stdout)
	usePlain := flagPlain || strings.EqualFold(cfg.UI, "plain") || !stdoutIsTTY() || !stdinIsTTY()
	if usePlain {
		if strings.EqualFold(cfg.Theme, "mono") {
			ui.SetMono()
		}
		ag.Events = ui.Events()
		repl, err := ui.NewREPL(cfg, ag, p)
		if err != nil {
			return err
		}
		return repl.Run(ctx)
	}
	return tui.New(cfg, ag, p).Run(ctx)
}
