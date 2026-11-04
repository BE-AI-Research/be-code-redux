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
	// Headers ride every request to the endpoint (config
	// providers.<name>.headers): the workspace a key not scoped to one
	// must name, set when Validate answers WorkspaceNeeded.
	Headers map[string]string
}

// WorkspaceHeader is Anthropic's header naming the workspace a request
// runs in; a personal or service-account key not scoped to one workspace
// is refused (HTTP 400) without it.
const WorkspaceHeader = "anthropic-workspace-id"

// WorkspacePrompt is the question every UI asks when Validate answers
// WorkspaceNeeded.
const WorkspacePrompt = "this key is not scoped to a workspace; paste its workspace ID from https://platform.claude.com/settings/workspaces (wrkspc_…):"

// WorkspaceNeeded is Validate's answer to Anthropic's 400 for a key that
// is not scoped to a workspace: the key is fine, the request needs the
// workspace header. The UI asks for the ID, sets Choice.Headers and
// validates again.
type WorkspaceNeeded struct{ Provider string }

func (e *WorkspaceNeeded) Error() string {
	return e.Provider + ": this key is not scoped to a workspace; a workspace ID is needed"
}

// ErrKeyRejected (401/403) sends a UI back to the key step; ErrUnreachable
// (transport, non-JSON, any other non-200) offers retry or cancel.
var (
	ErrKeyRejected = errors.New("key rejected")
	ErrUnreachable = errors.New("endpoint unreachable or not OpenAI-compatible")
)

// KeyRejected is Validate's 401/403: the provider's own message rides
// along (sanitised by provider.HTTPError.ProviderMessage, never a key),
// because "rejected" alone leaves a person guessing between a typo, a key
// for the wrong product and an account-side rule.
type KeyRejected struct {
	Provider, Detail string
}

func (e *KeyRejected) Error() string   { return RejectionLine(e, e.Provider) }
func (e *KeyRejected) Is(t error) bool { return t == ErrKeyRejected }

// RejectionLine is what every UI prints for a rejected key.
func RejectionLine(err error, name string) string {
	var kr *KeyRejected
	if errors.As(err, &kr) && kr.Detail != "" {
		return fmt.Sprintf("key rejected by %s: %s", name, kr.Detail)
	}
	return "key rejected by " + name
}

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
	p.Headers = ch.Headers
	models, err := p.ListModels(ctx)
	if err == nil {
		return models, nil
	}
	var he *provider.HTTPError
	if errors.As(err, &he) {
		msg := he.ProviderMessage()
		switch he.Code {
		case 401, 403:
			return nil, &KeyRejected{Provider: ch.Name, Detail: msg}
		case 404:
			return nil, nil // no listing route: a typed model name instead
		case 400:
			if strings.Contains(msg, "not scoped to a workspace") {
				return nil, &WorkspaceNeeded{Provider: ch.Name}
			}
		}
		if msg != "" {
			return nil, fmt.Errorf("%w: %s answered HTTP %d: %s", ErrUnreachable, ch.BaseURL, he.Code, msg)
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

// KeysPageLine is what a UI says after trying to open a provider's keys
// page: where the key is made, and that the masked field comes next.
func KeysPageLine(url string, opened bool) string {
	if opened {
		return fmt.Sprintf("opened %s in your browser; create a key there and paste it below", url)
	}
	return fmt.Sprintf("create a key at %s and paste it below", url)
}

// Added is what AddOnline did.
type Added struct {
	Model   string
	Default bool
	Note    string
}

// AddOnline stores the credential, then writes the provider block, then
// the default, saving cfg whole: the wizard's, built for this purpose and
// not on disk yet. Nothing here validates: callers run Validate first, so
// a cancel before this point leaves both files untouched.
func AddOnline(cfg *config.Config, store *credentials.Store, ch Choice, key, source, model string, makeDefault bool) (Added, error) {
	return addOnline(cfg, store, ch, key, source, model, makeDefault, func(apply func(*config.Config)) error {
		apply(cfg)
		return cfg.Save()
	})
}

// AddOnlineInSession is AddOnline for a running session's Config: the
// change is applied to it in memory and written through config.Update,
// never by saving the session's copy — which carries the "a" key's
// session-only approvals under keys the file has.
func AddOnlineInSession(cfg *config.Config, store *credentials.Store, ch Choice, key, source, model string, makeDefault bool) (Added, error) {
	return addOnline(cfg, store, ch, key, source, model, makeDefault, func(apply func(*config.Config)) error {
		apply(cfg)
		return config.Update(apply)
	})
}

// addOnline is both: save applies the provider change to whatever copy is
// to be written and writes it. The block is derived from the copy it is
// applied to (an existing block's tuning is kept), so applying it to the
// file's own copy keeps what the file has.
func addOnline(cfg *config.Config, store *credentials.Store, ch Choice, key, source, model string, makeDefault bool, save func(apply func(*config.Config)) error) (Added, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return Added{}, errors.New("no key to store")
	}
	if err := store.Put(ch.Name, credentials.Credential{Kind: credentials.KindAPIKey, Key: key, Source: source, Created: time.Now().UTC()}); err != nil {
		return Added{}, err
	}
	model = strings.TrimSpace(model)
	apply := func(c *config.Config) {
		var pc config.ProviderConfig
		if ch.Preset {
			pre, _ := provider.PresetByName(ch.Name)
			pc = pre.ProviderConfig()
		} else {
			pc = config.ProviderConfig{Type: "openai", BaseURL: ch.BaseURL, Online: !config.LocalEndpoint(ch.BaseURL)}
		}
		if old, ok := c.Providers[ch.Name]; ok {
			// Keep a person's tuning; only identity fields are ours.
			pc.ContextWindow, pc.KeepAlive, pc.Options, pc.Headers = old.ContextWindow, old.KeepAlive, old.Options, old.Headers
			if !ch.Preset {
				// A custom block's consent fields are the person's: a key
				// variable they named stays, and a hand-set online: true (a
				// LAN endpoint they treat as off-site) is never recomputed away
				// — that would silently drop the online-project gate.
				pc.APIKeyEnv = old.APIKeyEnv
				pc.Online = old.Online || !config.LocalEndpoint(ch.BaseURL)
			}
		}
		if len(ch.Headers) > 0 {
			pc.Headers = ch.Headers
		}
		pc.DefaultModel = model
		if c.Providers == nil {
			c.Providers = map[string]config.ProviderConfig{}
		}
		c.Providers[ch.Name] = pc
		if makeDefault {
			c.DefaultProvider = ch.Name
		}
	}
	if err := save(apply); err != nil {
		return Added{}, err
	}
	pc := cfg.Providers[ch.Name]
	added := Added{Model: pc.DefaultModel, Default: makeDefault}
	if src, _ := cfg.KeySource(ch.Name); src == "env" {
		added.Note = fmt.Sprintf("note: $%s is set and takes precedence", pc.APIKeyEnv)
	}
	return added, nil
}
