package setup

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
)

// Wizard runs the first-launch flow on plain stdio (it executes before any
// UI starts, so it works identically under the TUI, the plain REPL, and
// SSH/BE-CLI sessions). It probes local backends, lets the user pick a
// backend and model, and writes the initial config.
func Wizard(ctx context.Context, in *bufio.Reader, out io.Writer) (*config.Config, error) {
	fmt.Fprintln(out, "BE-Code first-run setup")
	fmt.Fprintln(out, "Probing local inference backends...")

	found := Probe(ctx, DefaultCandidates(), 4*time.Second)

	if len(found) == 0 {
		fmt.Fprintln(out, `No local backends reachable (looked for Ollama :11434, llama.cpp :8080,
LM Studio :1234, vLLM :8000, BE AI Engine :9800).
Writing a default config pointing at Ollama — edit ~/.be-code/config.json
or start a backend and run 'be-code doctor'.`)
		cfg := BuildConfig("", "")
		return cfg, cfg.Save()
	}

	fmt.Fprintln(out, "\nReachable backends:")
	for i, f := range found {
		fmt.Fprintf(out, "  [%d] %-14s %s (%d models)\n", i+1, f.Name, f.Cfg.BaseURL, len(f.Models))
	}
	choice := pickNumber(in, out, "Choose a backend", 1, len(found))
	sel := found[choice-1]

	model := ""
	if len(sel.Models) > 0 {
		fmt.Fprintf(out, "\nModels on %s:\n", sel.Name)
		show := sel.Models
		if len(show) > 20 {
			show = show[:20]
			fmt.Fprintf(out, "  (showing first 20 of %d)\n", len(sel.Models))
		}
		for i, m := range show {
			size := ""
			if m.SizeBytes > 0 {
				size = fmt.Sprintf("  %.1fGB %s", float64(m.SizeBytes)/1e9, m.Quantization)
			}
			fmt.Fprintf(out, "  [%d] %s%s\n", i+1, m.ID, size)
		}
		mi := pickNumber(in, out, "Choose a model", 1, len(show))
		model = show[mi-1].ID
	} else {
		fmt.Fprint(out, "\nNo models listed. Enter a model name (e.g. qwen3:8b): ")
		line, _ := in.ReadString('\n')
		model = strings.TrimSpace(line)
	}

	cfg := BuildConfig(sel.Name, model)
	if err := cfg.Save(); err != nil {
		return nil, err
	}
	p, _ := config.Path()
	fmt.Fprintf(out, "\nSaved %s (provider=%s model=%s)\n\n", p, sel.Name, model)
	return cfg, nil
}

func pickNumber(in *bufio.Reader, out io.Writer, prompt string, lo, hi int) int {
	for {
		fmt.Fprintf(out, "%s [%d-%d, default %d]: ", prompt, lo, hi, lo)
		line, err := in.ReadString('\n')
		if err != nil {
			return lo
		}
		line = strings.TrimSpace(line)
		if line == "" {
			return lo
		}
		n, err := strconv.Atoi(line)
		if err == nil && n >= lo && n <= hi {
			return n
		}
		fmt.Fprintln(out, "invalid choice")
	}
}
