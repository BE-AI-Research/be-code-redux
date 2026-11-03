package setup

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/credentials"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/signin"
)

// probeBackends, listOnline and wizardSignIn are the wizard's network
// seams (onlineValidate, in online.go, is the fourth); tests replace them.
var (
	wizardSignIn   = signin.SignIn
	wizardOpenPage = signin.OpenPage
	probeBackends  = Probe
	listOnline     = func(ctx context.Context, p provider.Preset) ([]provider.ModelInfo, error) {
		return provider.NewOpenAICompat(p.Name, p.BaseURL, os.Getenv(p.KeyEnv)).ListModels(ctx)
	}
)

// Wizard runs the first-launch flow on plain stdio (it executes before any
// UI starts, so it works identically under the TUI, the plain REPL, and
// SSH/BE-CLI sessions). It probes local backends, lets the user pick a
// backend and model, and writes the initial config.
func Wizard(ctx context.Context, in *bufio.Reader, out io.Writer) (*config.Config, error) {
	fmt.Fprintln(out, "BE-Code first-run setup")
	fmt.Fprintln(out, "Probing local inference backends...")

	found := probeBackends(ctx, DefaultCandidates(), 4*time.Second)

	if len(found) == 0 {
		fmt.Fprintln(out, `No local backends reachable (looked for Ollama :11434, llama.cpp :8080,
LM Studio :1234, vLLM :8000, BE AI Engine :9800).
  [1] write a default config pointing at Ollama (edit ~/.be-code/config.json
      or start a backend and run 'be-code doctor')
  [2] use an online provider (OpenRouter, OpenAI, ...)`)
		if pickNumber(in, out, "Choose", 1, 2) == 2 {
			return wizardOnline(ctx, in, out, nil)
		}
		cfg := BuildConfig("", "")
		return cfg, cfg.Save()
	}

	fmt.Fprintln(out, "\nReachable backends:")
	for i, f := range found {
		fmt.Fprintf(out, "  [%d] %-14s %s (%d models)\n", i+1, f.Name, f.Cfg.BaseURL, len(f.Models))
	}
	fmt.Fprintf(out, "  [%d] use an online provider (OpenRouter, OpenAI, ...)\n", len(found)+1)
	choice := pickNumber(in, out, "Choose a backend", 1, len(found)+1)
	if choice == len(found)+1 {
		return wizardOnline(ctx, in, out, found)
	}
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

// MissingKeyError was the wizard's answer when the chosen online
// provider's key was not in the environment. Since 1.3.1 the wizard asks
// for the key instead (a pasted key or, for OpenRouter, a browser sign-in)
// and no longer returns it; the type stays because callers name it.
type MissingKeyError struct{ KeyEnv string }

func (e *MissingKeyError) Error() string {
	return fmt.Sprintf("set %s in your shell, then run be-code setup again", e.KeyEnv)
}

// errWizardCancelled is input that ended (or a sign-in that was abandoned)
// before anything was stored.
var errWizardCancelled = errors.New("setup cancelled; nothing was saved")

// wizardOnline is the online path: pick a preset, take its key from the
// environment or ask for one (pasted, or for OpenRouter a browser
// sign-in), pick a model from the provider's listing, and keep the first
// local backend as the helper. A key that was asked for is stored in the
// credential file, never in config.json; input that ends first saves
// nothing.
func wizardOnline(ctx context.Context, in *bufio.Reader, out io.Writer, found []Found) (*config.Config, error) {
	presets := provider.SignInOrder()
	fmt.Fprintln(out, "\nOnline providers:")
	for i, p := range presets {
		fmt.Fprintf(out, "  [%d] %-12s %s (key in %s)\n", i+1, p.Name, p.SignInHint(), p.KeyEnv)
	}
	pre := presets[pickNumber(in, out, "Choose a provider", 1, len(presets))-1]
	var helper *Found
	if len(found) > 0 {
		helper = &found[0]
	}
	if os.Getenv(pre.KeyEnv) == "" {
		return wizardOnlineKey(ctx, in, out, pre, helper)
	}
	fmt.Fprintf(out, "\nListing models on %s...\n", pre.Name)
	models, err := listOnline(ctx, pre)
	if err != nil {
		return nil, fmt.Errorf("listing %s models: %w", pre.Name, err)
	}
	model := ""
	if len(models) > 0 {
		show := models
		if len(show) > 20 {
			show = show[:20]
			fmt.Fprintf(out, "  (showing first 20 of %d)\n", len(models))
		}
		for i, m := range show {
			fmt.Fprintf(out, "  [%d] %s\n", i+1, m.ID)
		}
		model = show[pickNumber(in, out, "Choose a model", 1, len(show))-1].ID
	} else {
		fmt.Fprint(out, "No models listed. Enter a model name: ")
		line, _ := in.ReadString('\n')
		model = strings.TrimSpace(line)
	}
	cfg := BuildOnlineConfig(pre, model, helper)
	if err := cfg.Save(); err != nil {
		return nil, err
	}
	p, _ := config.Path()
	fmt.Fprintf(out, "\nSaved %s (provider=%s model=%s)\n", p, pre.Name, model)
	onlineSavedNotes(out, cfg)
	return cfg, nil
}

func onlineSavedNotes(out io.Writer, cfg *config.Config) {
	if cfg.LocalHelper.Model != "" {
		fmt.Fprintf(out, "Housekeeping (summaries, commit messages) will run on %s / %s.\n", cfg.LocalHelper.Provider, cfg.LocalHelper.Model)
	}
	fmt.Fprintln(out, "Each project asks once before its code goes to this provider.")
	fmt.Fprintln(out)
}

// wizardOnlineKey asks for the key the environment does not have: a
// browser sign-in (OpenRouter only) or a pasted key, validated against the
// provider's listing, then stored with AddOnline.
func wizardOnlineKey(ctx context.Context, in *bufio.Reader, out io.Writer, pre provider.Preset, helper *Found) (*config.Config, error) {
	ch := Choice{Name: pre.Name, BaseURL: pre.BaseURL, KeyEnv: pre.KeyEnv, Preset: true}
	fmt.Fprintf(out, "\n$%s is not set. How should BE-Code get the key?\n", pre.KeyEnv)
	browser := false
	if pre.Name == "openrouter" {
		fmt.Fprintln(out, "  [1] Sign in with browser\n  [2] Paste a key")
		n, ok := pickNumberOK(in, out, "Choose", 1, 2)
		if !ok {
			return nil, errWizardCancelled
		}
		browser = n == 1
	} else {
		fmt.Fprintln(out, "  Paste a key (it is stored in ~/.be-code/"+credentials.FileName+", mode 0600)")
		if pre.KeysURL != "" {
			// The "sign in" these providers can honestly offer: their own
			// keys page, opened here, then the masked field.
			fmt.Fprintln(out, KeysPageLine(pre.KeysURL, wizardOpenPage(pre.KeysURL)))
		}
	}
	var (
		key, source string
		models      []provider.ModelInfo
		noted       bool
	)
	for {
		if browser {
			k, err := wizardBrowser(ctx, in, out)
			if errors.Is(err, signin.ErrCancelled) {
				return nil, fmt.Errorf("%w; run be-code setup again", err)
			}
			if err != nil {
				fmt.Fprintln(out, err.Error())
				fmt.Fprintln(out, "Paste a key instead.")
				browser = false
				continue
			}
			key, source = k, "oauth"
		} else {
			if !noted {
				fmt.Fprintln(out, "(the key is not echoed on a terminal)")
				noted = true
			}
			k, ok := readKey(in, out, fmt.Sprintf("key for %s: ", pre.Name))
			if !ok {
				return nil, errWizardCancelled
			}
			if key = strings.TrimSpace(k); key == "" {
				continue
			}
			source = "pasted"
		}
		fmt.Fprintf(out, "checking %s...\n", pre.Name)
		ms, err := onlineValidate(ctx, ch, key)
		if errors.Is(err, ErrKeyRejected) {
			fmt.Fprintf(out, "key rejected by %s\n", pre.Name)
			browser = false
			continue
		}
		if err != nil {
			fmt.Fprintln(out, err.Error())
			fmt.Fprint(out, "retry? [Y/n] ")
			a, ok := readLine(in)
			if !ok || strings.HasPrefix(strings.ToLower(strings.TrimSpace(a)), "n") {
				return nil, errWizardCancelled
			}
			continue
		}
		models = ms
		break
	}
	model := ""
	if len(models) > 0 {
		show := models
		if len(show) > 20 {
			show = show[:20]
			fmt.Fprintf(out, "  (showing first 20 of %d)\n", len(models))
		}
		for i, m := range show {
			fmt.Fprintf(out, "  [%d] %s\n", i+1, m.ID)
		}
		n, ok := pickNumberOK(in, out, "Choose a model", 1, len(show))
		if !ok {
			return nil, errWizardCancelled
		}
		model = show[n-1].ID
	} else {
		fmt.Fprint(out, "No models listed. Enter a model name: ")
		line, ok := readLine(in)
		if !ok {
			return nil, errWizardCancelled
		}
		model = strings.TrimSpace(line)
	}
	dir, err := config.Dir()
	if err != nil {
		return nil, err
	}
	store, err := credentials.Open(dir)
	if err != nil {
		return nil, err
	}
	cfg := BuildOnlineConfig(pre, model, helper)
	added, err := AddOnline(cfg, store, ch, key, source, model, true)
	if err != nil {
		return nil, err
	}
	p, _ := config.Path()
	fmt.Fprintf(out, "\nSaved %s (provider=%s model=%s); the key is in %s\n", p, pre.Name, model, filepath.Join(dir, credentials.FileName))
	if added.Note != "" {
		fmt.Fprintln(out, added.Note)
	}
	onlineSavedNotes(out, cfg)
	return cfg, nil
}

// wizardBrowser runs the OpenRouter sign-in while a goroutine reads pasted
// redirect URLs from in (a box without a browser prints the URL). A read
// still parked when the sign-in ends is finished by an Enter, so it never
// swallows the next answer.
func wizardBrowser(ctx context.Context, in *bufio.Reader, out io.Writer) (string, error) {
	pasted := make(chan string)
	stop := make(chan struct{})
	exited := make(chan struct{})
	fmt.Fprintln(out, "waiting for the browser; or paste the redirect URL here and press Enter")
	go func() {
		defer close(exited)
		for {
			line, ok := readLine(in)
			select {
			case <-stop:
				return
			default:
			}
			if !ok {
				close(pasted)
				return
			}
			if strings.TrimSpace(line) == "" {
				continue
			}
			select {
			case pasted <- line:
			case <-stop:
				return
			}
		}
	}()
	k, err := wizardSignIn(ctx, signin.Options{
		Open:   signin.OpenBrowser,
		Show:   func(u string) { fmt.Fprintln(out, "open this URL to sign in: "+u) },
		Pasted: pasted,
	})
	close(stop)
	select {
	case <-exited:
	default:
		fmt.Fprint(out, "press Enter to continue ")
		<-exited
	}
	return k, err
}

// readKey reads a key without echo on a terminal, else one line of in.
func readKey(in *bufio.Reader, out io.Writer, prompt string) (string, bool) {
	fmt.Fprint(out, prompt)
	if term.IsTerminal(os.Stdin.Fd()) {
		b, err := ReadSecret(os.Stdin.Fd())
		fmt.Fprintln(out)
		if err != nil {
			return "", false
		}
		return string(b), true
	}
	line, ok := readLine(in)
	if ok {
		fmt.Fprintln(out)
	}
	return line, ok
}

// readLine is one line of in, false when input ended with nothing on it.
func readLine(in *bufio.Reader) (string, bool) {
	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		return "", false
	}
	return strings.TrimRight(line, "\r\n"), true
}

// pickNumberOK is pickNumber that tells an ended input from a choice, for
// the questions where defaulting would act (start a sign-in, store a key).
func pickNumberOK(in *bufio.Reader, out io.Writer, prompt string, lo, hi int) (int, bool) {
	for {
		fmt.Fprintf(out, "%s [%d-%d]: ", prompt, lo, hi)
		line, ok := readLine(in)
		if !ok {
			return 0, false
		}
		n, err := strconv.Atoi(strings.TrimSpace(line))
		if err == nil && n >= lo && n <= hi {
			return n, true
		}
		fmt.Fprintln(out, "invalid choice")
	}
}
