package setup

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/credentials"
	"github.com/brown-enterprises/be-code/internal/provider"
)

// Choice is the provider a person is adding: a preset (name, URL and env
// var from provider.Presets) or a custom OpenAI-compatible endpoint.
type Choice struct {
	Name    string
	BaseURL string
	KeyEnv  string
	Preset  bool
}

// ErrKeyRejected (401/403) sends a UI back to the key step; ErrUnreachable
// (transport, non-JSON, any other non-200) offers retry or cancel.
var (
	ErrKeyRejected = errors.New("key rejected")
	ErrUnreachable = errors.New("endpoint unreachable or not OpenAI-compatible")
)

// ValidateTimeout bounds the one listing Validate makes.
const ValidateTimeout = 15 * time.Second

// ChoiceFor resolves what a person named. A preset's name is a promise
// about where it points: the preset's URL is used even when cfg's block
// of that name was hand-edited to another, and the note says so. A custom
// name must not be a preset's, and needs a URL.
func ChoiceFor(cfg *config.Config, name, baseURL string) (Choice, string, error) {
	name = strings.TrimSpace(name)
	baseURL = strings.TrimSpace(baseURL)
	if name == "" {
		return Choice{}, "", errors.New("a provider needs a name")
	}
	if pre, ok := provider.PresetByName(name); ok {
		if baseURL != "" && baseURL != pre.BaseURL {
			return Choice{}, "", fmt.Errorf("%s is a preset; a custom endpoint needs another name", name)
		}
		note := ""
		if pc, ok := cfg.Providers[name]; ok && pc.BaseURL != "" && pc.BaseURL != pre.BaseURL {
			note = fmt.Sprintf("%s is a preset; its URL is %s", name, pre.BaseURL)
		}
		return Choice{Name: name, BaseURL: pre.BaseURL, KeyEnv: pre.KeyEnv, Preset: true}, note, nil
	}
	// A custom name is new or an existing custom (openai-typed) entry
	// (spec §3): the flow writes an openai block, so reusing the name of
	// an ollama provider would turn the local backend into a cloud one.
	if pc, ok := cfg.Providers[name]; ok && pc.Type != "openai" && pc.Type != "" {
		return Choice{}, "", fmt.Errorf("%s is an existing %s provider; a custom endpoint needs another name", name, pc.Type)
	}
	if baseURL == "" {
		return Choice{}, "", fmt.Errorf("custom provider %s needs a base URL", name)
	}
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Choice{}, "", fmt.Errorf("base URL %q is not an http(s) URL", baseURL)
	}
	return Choice{Name: name, BaseURL: strings.TrimRight(baseURL, "/")}, "", nil
}

// onlineValidate is the wizard's seam over Validate; tests replace it.
var onlineValidate = Validate

// Validate lists models at the endpoint with key. The key is trimmed (a
// copy from a web page carries a newline).
func Validate(ctx context.Context, ch Choice, key string) ([]provider.ModelInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, ValidateTimeout)
	defer cancel()
	p := provider.NewOpenAICompat(ch.Name, ch.BaseURL, strings.TrimSpace(key))
	models, err := p.ListModels(ctx)
	if err == nil {
		return models, nil
	}
	var he *provider.HTTPError
	if errors.As(err, &he) {
		switch he.Code {
		case 401, 403:
			return nil, fmt.Errorf("%w by %s", ErrKeyRejected, ch.Name)
		case 404:
			return nil, nil // no listing route: a typed model name instead
		}
		return nil, fmt.Errorf("%w: %s answered HTTP %d", ErrUnreachable, ch.BaseURL, he.Code)
	}
	var ne net.Error
	if errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("%w: %s: %v", ErrUnreachable, ch.BaseURL, err)
	}
	// A 200 whose body is not the listing's JSON (an HTML login page).
	return nil, fmt.Errorf("%w: %s did not answer with a models listing", ErrUnreachable, ch.BaseURL)
}

// Added is what AddOnline did.
type Added struct {
	Model   string
	Default bool
	Note    string
}

// AddOnline stores the credential, then writes the provider block, then
// the default. Nothing here validates: callers run Validate first, so a
// cancel before this point leaves both files untouched.
func AddOnline(cfg *config.Config, store *credentials.Store, ch Choice, key, source, model string, makeDefault bool) (Added, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return Added{}, errors.New("no key to store")
	}
	if err := store.Put(ch.Name, credentials.Credential{Kind: credentials.KindAPIKey, Key: key, Source: source, Created: time.Now().UTC()}); err != nil {
		return Added{}, err
	}
	var pc config.ProviderConfig
	if ch.Preset {
		pre, _ := provider.PresetByName(ch.Name)
		pc = pre.ProviderConfig()
	} else {
		pc = config.ProviderConfig{Type: "openai", BaseURL: ch.BaseURL, Online: !config.LocalEndpoint(ch.BaseURL)}
	}
	if old, ok := cfg.Providers[ch.Name]; ok {
		// Keep a person's tuning; only identity fields are ours.
		pc.ContextWindow, pc.KeepAlive, pc.Options = old.ContextWindow, old.KeepAlive, old.Options
		if !ch.Preset {
			// A custom block's consent fields are the person's: a key
			// variable they named stays, and a hand-set online: true (a
			// LAN endpoint they treat as off-site) is never recomputed away
			// — that would silently drop the online-project gate.
			pc.APIKeyEnv = old.APIKeyEnv
			pc.Online = old.Online || !config.LocalEndpoint(ch.BaseURL)
		}
	}
	pc.DefaultModel = strings.TrimSpace(model)
	if cfg.Providers == nil {
		cfg.Providers = map[string]config.ProviderConfig{}
	}
	cfg.Providers[ch.Name] = pc
	if makeDefault {
		cfg.DefaultProvider = ch.Name
	}
	if err := cfg.Save(); err != nil {
		return Added{}, err
	}
	added := Added{Model: pc.DefaultModel, Default: makeDefault}
	if src, _ := cfg.KeySource(ch.Name); src == "env" {
		added.Note = fmt.Sprintf("note: $%s is set and takes precedence", pc.APIKeyEnv)
	}
	return added, nil
}
