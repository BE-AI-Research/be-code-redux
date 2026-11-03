// internal/ui/onlineadd.go
package ui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/credentials"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/setup"
	"github.com/brown-enterprises/be-code/internal/signin"
	"github.com/chzyer/readline"
)

var (
	onlineValidate    = setup.Validate
	onlineSignIn      = signin.SignIn
	onlineOpenBrowser = signin.OpenBrowser
	onlineOpenPage    = signin.OpenPage
)

// Prompter is one terminal's questions and lines for OnlineAdd.
type Prompter interface {
	Ask(ctx context.Context, q string) (string, bool)
	AskSecret(ctx context.Context, q string) (string, bool)
	Say(line string)
}

// unrecordedAsker is a Prompter that can ask a question whose answer stays
// out of the input history (the REPL's; spec §2 Secrecy). The pasted
// redirect URL during the browser wait carries the sign-in code.
type unrecordedAsker interface {
	AskUnrecorded(ctx context.Context, q string) (string, bool)
}

// OnlineAdd is /online add [name] on numbered prompts (spec §6).
func OnlineAdd(ctx context.Context, cfg *config.Config, p Prompter, name string) (string, string, error) {
	cancelled := func() (string, string, error) { p.Say("cancelled; nothing was stored"); return "", "", nil }
	presets := provider.SignInOrder()
	var ch setup.Choice
	if name == "" {
		p.Say("Online providers:")
		for i, pre := range presets {
			p.Say(fmt.Sprintf("  [%d] %-12s %s", i+1, pre.Name, pre.SignInHint()))
		}
		p.Say(fmt.Sprintf("  [%d] custom OpenAI-compatible endpoint", len(presets)+1))
		n, ok := askNumber(ctx, p, "Choose", 1, len(presets)+1)
		if !ok {
			return cancelled()
		}
		if n <= len(presets) {
			name = presets[n-1].Name
		}
	}
	if name == "" {
		for {
			n, ok := p.Ask(ctx, "name for the new provider: ")
			if !ok {
				return cancelled()
			}
			if _, isPreset := provider.PresetByName(strings.TrimSpace(n)); isPreset {
				p.Say(strings.TrimSpace(n) + " is a preset; a custom endpoint needs another name")
				continue
			}
			u, ok := p.Ask(ctx, "base URL (ending in /v1): ")
			if !ok {
				return cancelled()
			}
			c, note, err := setup.ChoiceFor(cfg, n, u)
			if err != nil {
				p.Say(err.Error())
				continue
			}
			ch = c
			if note != "" {
				p.Say(note)
			}
			break
		}
	} else {
		c, note, err := setup.ChoiceFor(cfg, name, "")
		if err != nil {
			return "", "", err
		}
		ch = c
		if note != "" {
			p.Say(note)
		}
	}

	key, source := "", "pasted"
	var models []provider.ModelInfo
	browser := false
	if pre, ok := provider.PresetByName(ch.Name); ok && pre.KeysURL != "" {
		// The "sign in" these providers can honestly offer: their own keys
		// page, opened here, then the masked prompt.
		p.Say(setup.KeysPageLine(pre.KeysURL, onlineOpenPage(pre.KeysURL)))
	}
	if ch.Name == "openrouter" {
		p.Say("  [1] Sign in with browser\n  [2] Paste a key")
		n, ok := askNumber(ctx, p, "Choose", 1, 2)
		if !ok {
			return cancelled()
		}
		browser = n == 1
	}
	keep := false // validate again with the same key (a workspace ID was added)
	for {
		if keep {
			keep = false
		} else if browser {
			pasted := make(chan string, 1)
			bctx, bcancel := context.WithCancel(ctx)
			done := make(chan struct{})
			go func() {
				defer close(done)
				// The person may paste the redirect URL while the browser is awaited.
				ask := p.Ask
				if u, ok := p.(unrecordedAsker); ok {
					ask = u.AskUnrecorded
				}
				if s, ok := ask(bctx, "waiting for the browser; or paste the redirect URL here: "); ok {
					select {
					case pasted <- s:
					default:
					}
				}
			}()
			k, err := onlineSignIn(ctx, signin.Options{Open: onlineOpenBrowser, Show: func(u string) { p.Say("open this URL to sign in: " + u) }, Pasted: pasted})
			bcancel()
			<-done
			if err != nil {
				if errors.Is(err, signin.ErrCancelled) {
					p.Say(err.Error())
					return "", "", nil
				}
				p.Say(err.Error())
				browser = false // paste-a-key offered instead
				continue
			}
			key, source = k, "oauth"
		} else {
			k, ok := p.AskSecret(ctx, fmt.Sprintf("key for %s: ", ch.Name))
			if !ok {
				return cancelled()
			}
			key = strings.TrimSpace(k)
			if key == "" {
				continue
			}
			p.Say("key entered (hidden)")
		}
		p.Say("checking " + ch.Name + "…")
		ms, err := onlineValidate(ctx, ch, key)
		if errors.Is(err, setup.ErrKeyRejected) {
			p.Say(setup.RejectionLine(err, ch.Name))
			browser = false
			continue
		}
		if errors.As(err, new(*setup.WorkspaceNeeded)) {
			// The key is fine; the request needs the workspace header. Ask
			// once and validate again with the same key.
			p.Say(setup.WorkspacePrompt)
			id, ok := p.Ask(ctx, "workspace ID: ")
			if !ok {
				return cancelled()
			}
			if ch.Headers == nil {
				ch.Headers = map[string]string{}
			}
			ch.Headers[setup.WorkspaceHeader] = strings.TrimSpace(id)
			keep = true
			continue
		}
		if err != nil {
			p.Say(err.Error())
			a, ok := p.Ask(ctx, "retry? [Y/n] ")
			if !ok || strings.HasPrefix(strings.ToLower(strings.TrimSpace(a)), "n") {
				return cancelled()
			}
			continue
		}
		models = ms
		break
	}

	model := ""
	if len(models) == 0 {
		p.Say(ch.Name + " lists no models")
		m, ok := p.Ask(ctx, "model name: ")
		if !ok {
			return cancelled()
		}
		model = strings.TrimSpace(m)
	} else {
		show := models
		if len(show) > 20 {
			show = show[:20]
			p.Say(fmt.Sprintf("  (showing first 20 of %d)", len(models)))
		}
		for i, m := range show {
			p.Say(fmt.Sprintf("  [%d] %s", i+1, m.ID))
		}
		n, ok := askNumber(ctx, p, "Choose a model", 1, len(show))
		if !ok {
			return cancelled()
		}
		model = show[n-1].ID
	}
	a, ok := p.Ask(ctx, fmt.Sprintf("make %s the default provider? [y/N] ", ch.Name))
	if !ok {
		return cancelled()
	}
	makeDefault := strings.HasPrefix(strings.ToLower(strings.TrimSpace(a)), "y")

	dir, err := config.Dir()
	if err != nil {
		return "", "", err
	}
	store, err := credentials.Open(dir)
	if err != nil {
		return "", "", err
	}
	added, err := setup.AddOnline(cfg, store, ch, key, source, model, makeDefault)
	if err != nil {
		return "", "", err
	}
	if added.Note != "" {
		p.Say(added.Note)
	}
	p.Say(fmt.Sprintf("%s added (model %s)", ch.Name, added.Model))
	return ch.Name, added.Model, nil
}

func askNumber(ctx context.Context, p Prompter, q string, lo, hi int) (int, bool) {
	for {
		a, ok := p.Ask(ctx, fmt.Sprintf("%s [%d-%d]: ", q, lo, hi))
		if !ok {
			return 0, false
		}
		n, err := strconv.Atoi(strings.TrimSpace(a))
		if err == nil && n >= lo && n <= hi {
			return n, true
		}
		p.Say(fmt.Sprintf("enter a number from %d to %d", lo, hi))
	}
}

// stdinIsTerminal is the /online add gate in plain mode.
func stdinIsTerminal() bool {
	// A real tty check: /dev/null is a character device but not a terminal.
	return readline.IsTerminal(int(os.Stdin.Fd()))
}
