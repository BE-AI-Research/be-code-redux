package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/bench"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/ide"
	"github.com/brown-enterprises/be-code/internal/live"
	"github.com/brown-enterprises/be-code/internal/profiles"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/setup"
	"github.com/brown-enterprises/be-code/internal/store"
	"github.com/brown-enterprises/be-code/internal/tools"
	"github.com/brown-enterprises/be-code/internal/ui"
	"github.com/brown-enterprises/be-code/internal/verify"
)

// runCmd is the headless mode: prompt in, work done, summary out.
// Suitable for scripting and for driving BE-Code from other Continuum
// components (BE-PAP, schedulers, CI).
var runCmd = &cobra.Command{
	Use:   "run [prompt]",
	Short: "Run one task non-interactively (reads prompt from args or stdin)",
	RunE: func(cmd *cobra.Command, args []string) error {
		prompt := strings.TrimSpace(strings.Join(args, " "))
		if prompt == "" {
			data, err := readAllStdin()
			if err != nil {
				return err
			}
			prompt = strings.TrimSpace(data)
		}
		if prompt == "" {
			return fmt.Errorf("no prompt given (pass as arguments or pipe via stdin)")
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		_, ag, err := buildAgent(cfg, true)
		if err != nil {
			return err
		}
		if flagJSON {
			ag.Events = agent.Events{} // no streaming noise on stdout
		} else {
			ag.Events = ui.Events()
		}
		ag.Tools.Approve = headlessApprover(cfg)
		// Approve and Events are wired above; a dispatch before this point
		// would ask consent of nobody and print to nobody.
		ag.StartSubAgents()
		defer ag.Tools.Close()
		defer ag.Checkpoints.Cleanup()
		defer finishSession(ag, false, os.Stderr)
		if ideSession != nil {
			defer ideSession.Close()
		}
		answer, rep, err := ag.RunFull(cmd.Context(), prompt)
		// Sub-agents hand back into the queue; headless, nobody types the
		// next turn, so the run itself takes up to three of them (a
		// question, its answer, the hand-back).
		for round := 0; err == nil && ag.SubAgentsEnabled() && round < 3; round++ {
			ag.WaitSubAgents(cmd.Context())
			msgs := ag.DrainInbox()
			if len(msgs) == 0 {
				break
			}
			answer, rep, err = ag.RunFull(cmd.Context(), strings.Join(msgs, "\n\n"))
		}
		if err != nil {
			return err
		}
		verifyPassed := rep == nil || rep.Verify == nil || rep.Verify.Passed()
		if flagJSON {
			out := map[string]any{
				"answer":            answer,
				"verified":          rep != nil && rep.Verify != nil && rep.Verify.Passed(),
				"verification_ran":  rep != nil && rep.Verify != nil,
				"reviewed":          rep != nil && rep.Reviewed,
				"review_issues":     reviewIssues(rep),
				"changed_files":     changedFiles(ag),
				"model_requests":    ag.Usage().Requests,
				"tool_calls":        ag.Usage().ToolCalls,
				"prompt_tokens":     ag.Usage().PromptTokens,
				"completion_tokens": ag.Usage().CompletionTokens,
				"elapsed_seconds":   ag.Usage().Elapsed.Seconds(),
				// The server's own account (native Ollama; 0 elsewhere).
				"prompt_processing_seconds": ag.Usage().PromptTime.Seconds(),
				"model_loading_seconds":     ag.Usage().LoadTime.Seconds(),
				"uncached_prompt_reads":     ag.Usage().SlowReads,
				"sub_agents":                subAgentRows(ag),
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(out); err != nil {
				return err
			}
		} else {
			fmt.Println()
			if rep != nil && rep.Verify != nil {
				fmt.Fprintln(os.Stderr, rep.Verify.Human())
			}
			if rep != nil && rep.ReviewIssues != "" {
				fmt.Fprintln(os.Stderr, "reviewer raised issues (repair attempted):\n"+rep.ReviewIssues)
			}
		}
		if !verifyPassed {
			return fmt.Errorf("verification failed after repair attempts")
		}
		return nil
	},
}

func reviewIssues(rep *agent.ReviewedReport) string {
	if rep == nil {
		return ""
	}
	return rep.ReviewIssues
}

// subAgentRows is run --json's "sub_agents": every hand-back this run made,
// including one carried over from a co-worker's prior session, in the shape
// the flag's other rows already use.
func subAgentRows(ag *agent.Agent) []map[string]any {
	rows := []map[string]any{}
	for _, hb := range ag.SubAgentReport() {
		rows = append(rows, map[string]any{"node": hb.Node, "owner": hb.Owner, "status": hb.Status,
			"reason": hb.Reason, "elapsed_seconds": hb.Elapsed.Seconds(), "tool_calls": hb.Calls, "files": hb.Files})
	}
	return rows
}

func changedFiles(ag *agent.Agent) []string {
	if ag.Checkpoints == nil {
		return nil
	}
	return ag.Checkpoints.ChangedAll()
}

var benchCmd = &cobra.Command{
	Use:   "bench",
	Short: "Run the built-in offline eval suite against one or more models",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		p, err := provider.FromConfig(cfg, flagProvider)
		if err != nil {
			return err
		}
		models := []string{provider.ResolveModel(cfg, flagProvider, flagModel)}
		if flagBenchModels != "" {
			models = strings.Split(flagBenchModels, ",")
		}
		suite := bench.Suite()
		all := map[string][]bench.Result{}
		for _, model := range models {
			model = strings.TrimSpace(model)
			fmt.Fprintf(os.Stderr, "benchmarking %s (%d tasks)...\n", model, len(suite))
			var results []bench.Result
			for _, t := range suite {
				fmt.Fprintf(os.Stderr, "  %s...\n", t.Name)
				results = append(results, bench.RunTask(cmd.Context(), cfg, p, model, t))
			}
			all[model] = results
			if !flagJSON {
				fmt.Println(bench.Format(model, results))
			}
		}
		if flagJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(all)
		}
		return nil
	},
}

// headlessApprover approves per cfg flags; when stdin is a TTY it falls
// back to a simple y/N prompt, otherwise it denies (safe default for CI).
func headlessApprover(cfg *config.Config) tools.ApproveFunc {
	in := bufio.NewReader(os.Stdin)
	return func(action, detail string) bool {
		if action == "shell" && cfg.AutoApproveShell {
			return true
		}
		if action == "file_write" && !cfg.ApproveFileWrites {
			return true
		}
		if !stdinIsTTY() {
			fmt.Fprintf(os.Stderr, "denied %s (non-interactive; use -y to auto-approve): %.120s\n", action, detail)
			return false
		}
		if action == "file_write" {
			fmt.Fprintln(os.Stderr, ui.ColorizeDiff(detail, stdoutIsTTY()))
		} else {
			fmt.Fprintf(os.Stderr, "%s: %s\n", action, detail)
		}
		fmt.Fprint(os.Stderr, "approve? [y/N] ")
		line, _ := in.ReadString('\n')
		l := strings.ToLower(strings.TrimSpace(line))
		return l == "y" || l == "yes"
	}
}

var sessionsCmd = &cobra.Command{
	Use:   "sessions",
	Short: "List saved sessions (resume with --resume <code>, <id> or last)",
	RunE: func(cmd *cobra.Command, args []string) error {
		metas, err := store.List()
		if err != nil {
			return err
		}
		// Live hosts: a session being served right now is attachable rather
		// than resumable, and a brand-new one has no saved file yet, so it is
		// listed even when store.List does not know about it.
		liveByCode := map[string]live.Record{}
		if dir, derr := live.Dir(); derr == nil {
			recs, _ := live.List(dir)
			for _, r := range recs {
				liveByCode[r.Code] = r
			}
		}
		anyLive := len(liveByCode) > 0
		if len(metas) == 0 && !anyLive {
			fmt.Println("no saved sessions")
			return nil
		}
		fmt.Printf("%-6s  %-4s  %-19s  %-16s  %-5s  %s\n", "CODE", "LIVE", "ID", "UPDATED", "TURNS", "TITLE")
		for _, m := range metas {
			mark := "-"
			if _, ok := liveByCode[m.Code]; ok {
				mark = "live"
				delete(liveByCode, m.Code)
			}
			fmt.Printf("%-6s  %-4s  %-19s  %-16s  %5d  %s\n",
				m.Code, mark, m.ID, m.UpdatedAt.Format("2006-01-02 15:04"), m.Turns, m.Title)
		}
		codes := make([]string, 0, len(liveByCode))
		for code := range liveByCode {
			codes = append(codes, code)
		}
		sort.Strings(codes)
		for _, code := range codes {
			r := liveByCode[code]
			fmt.Printf("%-6s  %-4s  %-19s  %-16s  %5s  %s\n",
				r.Code, "live", "-", r.StartedAt.Format("2006-01-02 15:04"), "-", "(no saved turns yet) "+r.Workspace)
		}
		fmt.Println("\nresume with: be-code --resume <code>")
		if anyLive {
			fmt.Println("attach to a live one with: be-code attach <code>   (end it with: be-code sessions kill <code>)")
		}
		return nil
	},
}

var sessionsDeleteCmd = &cobra.Command{
	Use:   "delete <code|id>",
	Short: "Delete a saved session",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		s, err := store.Load(args[0])
		if err != nil {
			return err
		}
		if err := store.Delete(s.ID); err != nil {
			return err
		}
		fmt.Printf("deleted %s (%s)\n", s.ResumeCode(), s.Title)
		return nil
	},
}

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Re-run the first-launch setup wizard (probe backends, pick model)",
	RunE: func(cmd *cobra.Command, args []string) error {
		_, err := setup.Wizard(cmd.Context(), bufio.NewReader(os.Stdin), os.Stdout)
		return err
	},
}

func readAllStdin() (string, error) {
	fi, err := os.Stdin.Stat()
	if err != nil || (fi.Mode()&os.ModeCharDevice) != 0 {
		return "", nil // interactive stdin; nothing piped
	}
	var b strings.Builder
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		b.WriteString(sc.Text())
		b.WriteString("\n")
	}
	return b.String(), sc.Err()
}

var modelsCmd = &cobra.Command{
	Use:   "models",
	Short: "List models available on the configured backend",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		p, err := provider.FromConfig(cfg, flagProvider)
		if err != nil {
			return err
		}
		models, err := p.ListModels(cmd.Context())
		if err != nil {
			return err
		}
		for _, m := range models {
			line := m.ID
			if m.SizeBytes > 0 {
				line += fmt.Sprintf("\t%.1fGB\t%s\t%s", float64(m.SizeBytes)/1e9, m.Family, m.Quantization)
			}
			fmt.Println(line)
		}
		return nil
	},
}

var pullCmd = &cobra.Command{
	Use:   "pull <model>",
	Short: "Pull a model (Ollama backends only)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		p, err := provider.FromConfig(cfg, flagProvider)
		if err != nil {
			return err
		}
		ol, ok := p.(*provider.Ollama)
		if !ok {
			return fmt.Errorf("provider %q is not an Ollama backend; pull models with the backend's own tooling", p.Name())
		}
		return ol.Pull(cmd.Context(), args[0], func(s string) { fmt.Println(s) })
	},
}

// doctorCmd checks every configured backend — the offline equivalent of
// "is my environment sane" — plus workspace verification support.
var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check all configured providers and the workspace toolchain",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		fmt.Println("providers:")
		names := make([]string, 0, len(cfg.Providers))
		for name := range cfg.Providers {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			p, err := provider.FromConfig(cfg, name)
			if err != nil {
				fmt.Printf("  %-14s config error: %v\n", name, err)
				continue
			}
			start := time.Now()
			status, err := p.Ping(cmd.Context())
			if err != nil {
				fmt.Printf("  %-14s unreachable (%v)\n", name, compactErr(err))
				continue
			}
			fmt.Printf("  %-14s %s (%dms)\n", name, status, time.Since(start).Milliseconds())
			if o, ok := p.(*provider.Ollama); ok && name == cfg.DefaultProvider {
				reportWindow(cmd.Context(), o, cfg, name)
			}
		}
		model := provider.ResolveModel(cfg, cfg.DefaultProvider, "")
		if prof := profiles.Detect(model); prof.Notes != "" {
			fmt.Printf("model: %s → %s profile (%s)\n", model, prof.Family, prof.Notes)
		}
		if cfg.WebSearch.Enabled() {
			keyState := "key set"
			if os.Getenv(cfg.WebSearch.APIKeyEnv) == "" {
				keyState = "KEY MISSING: export " + tools.EnvNameForDisplay(cfg.WebSearch.APIKeyEnv)
			}
			fmt.Printf("web search: google pse cx=%s (%s)\n", cfg.WebSearch.CX, keyState)
		} else {
			fmt.Println("web search: off (set web_search.cx in config to enable)")
		}
		if dir, err := ide.LockDir(); err == nil {
			if lock, _ := ide.Discover(dir, mustAbs(flagDir)); lock != nil {
				fmt.Printf("editor bridge: %s v%s on port %d (workspace %s)\n", lock.IDEName, lock.Version, lock.Port, strings.Join(lock.WorkspaceFolders, ", "))
			} else {
				fmt.Println("editor bridge: none listening (install the BE-Code VS Code extension)")
			}
		}
		proj := verify.Detect(mustAbs(flagDir))
		fmt.Printf("workspace: %s project detected", proj.Kind)
		if len(proj.Checks) > 0 {
			names := make([]string, 0, len(proj.Checks))
			for _, c := range proj.Checks {
				names = append(names, c.Name)
			}
			fmt.Printf(" (checks: %s)", strings.Join(names, ", "))
		}
		fmt.Println()
		return nil
	},
}

// reportWindow is doctor's account of the one number that decides whether a
// session is running in the window it thinks it is.
//
// It reports context_window — the num_ctx the harness actually sends — not
// context_tokens, which is only a cap on the budget and says nothing about
// the server. The comparison that matters is between what config asks for
// and what the model is loaded with, because they differing is what makes
// Ollama reload the model and evict whoever else was using it. That is the
// question reload_on_mismatch answers, so doctor names its setting too.
func reportWindow(ctx context.Context, o *provider.Ollama, cfg *config.Config, providerName string) {
	model := provider.ResolveModel(cfg, providerName, "")
	want := 0
	if pc, ok := cfg.Providers[providerName]; ok {
		want = pc.ContextWindow
	}
	if mc, ok := cfg.Models[model]; ok && mc.ContextWindow > 0 {
		want = mc.ContextWindow
	}
	asks := "context_window unset (the harness fits itself to the server)"
	if want > 0 {
		asks = fmt.Sprintf("context_window=%d", want)
	}
	fmt.Printf("  %-14s %s: %s\n", "", model, asks)

	loadedWindow, resident, err := o.Resident(ctx, model)
	switch {
	case err != nil:
		fmt.Printf("  %-14s could not read loaded models (%v); the window in use is unknown\n", "", compactErr(err))
		return
	case resident && loadedWindow <= 0:
		fmt.Printf("  %-14s loaded, but this server does not report the window it was loaded with\n", "")
		return
	case resident && want > 0 && want != loadedWindow:
		fmt.Printf("  %-14s loaded with %d tokens — MISMATCH: reaching %d reloads the model and evicts other users (reload_on_mismatch=%s)\n",
			"", loadedWindow, want, cfg.ReloadOnMismatch)
	case resident:
		fmt.Printf("  %-14s loaded with %d tokens (ok)\n", "", loadedWindow)
	default:
		n, _ := o.ContextLength(ctx, model)
		if n > 0 {
			fmt.Printf("  %-14s not loaded; the Modelfile says %d tokens\n", "", n)
		} else {
			fmt.Printf("  %-14s not loaded, and no Modelfile num_ctx to go on\n", "")
		}
	}
	if cfg.ContextTokens > 0 {
		fmt.Printf("  %-14s context_tokens=%d caps the prompt budget below whatever window is in use\n", "", cfg.ContextTokens)
	}
}

var verifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Run the verification checks for the workspace and exit non-zero on failure",
	RunE: func(cmd *cobra.Command, args []string) error {
		root := mustAbs(flagDir)
		proj := verify.Detect(root)
		rep := verify.RunChecks(cmd.Context(), root, proj)
		fmt.Println(rep.Human())
		if len(rep.Results) > 0 && !rep.Passed() {
			fmt.Println()
			fmt.Println(rep.ModelSummary())
			return fmt.Errorf("verification failed")
		}
		return nil
	},
}

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Print the config file path (creating a default config on first run)",
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := config.Load(); err != nil {
			return err
		}
		p, err := config.Path()
		if err != nil {
			return err
		}
		fmt.Println(p)
		return nil
	},
}

func compactErr(err error) string {
	s := err.Error()
	if len(s) > 120 {
		s = s[:120] + "..."
	}
	return s
}

func mustAbs(p string) string {
	abs, err := os.Getwd()
	if p == "." && err == nil {
		return abs
	}
	if a, err := absPath(p); err == nil {
		return a
	}
	return p
}

func absPath(p string) (string, error) {
	if strings.HasPrefix(p, "~") {
		home, err := os.UserHomeDir()
		if err == nil {
			p = home + p[1:]
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		return p, err
	}
	if !strings.HasPrefix(p, "/") && !strings.Contains(p, ":\\") {
		p = wd + string(os.PathSeparator) + p
	}
	return p, nil
}
