package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/brown-enterprises/be-code/internal/api"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/packs"
	"github.com/brown-enterprises/be-code/internal/profiles"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/store"
)

// APIModelFactory builds a provider for a model the serving session is not
// running, on providerName — the session's *current* provider, never
// cfg.DefaultProvider, which --provider and /provider leave behind — loaded
// as a reviewer is (cmd/root.go: FromConfig + secondaryLoad) so the
// primary's window is never touched. Nil means only "default" is served.
// Injected from cmd/root.go like ReviewerFactory.
var APIModelFactory func(ctx context.Context, cfg *config.Config, providerName, model string) (provider.Provider, error)

// APIBackend answers the application API from the running session (spec
// 2026-11-02-app-api-design.md §4–§5). Nothing here is a typed request: it
// never clears UntrustedWeb, never touches the inbox, the engine, the work
// ledger or History.Messages, and never writes.
type APIBackend struct {
	a       *Agent
	version string
	since   time.Time

	mu    sync.Mutex
	provs map[string]provider.Provider // named models by provider + model, built once each
}

func NewAPIBackend(a *Agent, version string) *APIBackend {
	return &APIBackend{a: a, version: version, since: time.Now(), provs: map[string]provider.Provider{}}
}

func (b *APIBackend) Health() api.Health {
	code := ""
	if s := b.a.CurrentSession(); s != nil {
		code = s.ResumeCode()
	}
	_, model, _ := b.a.modelSnapshot()
	return api.Health{Version: b.version, SessionCode: code, Model: model, Since: b.since}
}

func (b *APIBackend) SessionInfo() api.SessionInfo {
	title := ""
	if s := b.a.CurrentSession(); s != nil {
		title = s.Title
	}
	_, model, _ := b.a.modelSnapshot()
	return api.SessionInfo{Title: title, Workspace: b.a.Tools.Root, Model: model}
}

// AcceptsSession compares under saveMu: Accepts may clear an expired grace
// token, and autosave marshals the same struct.
func (b *APIBackend) AcceptsSession(tok string) bool {
	s := b.a.CurrentSession()
	if s == nil {
		return false
	}
	b.a.saveMu.Lock()
	defer b.a.saveMu.Unlock()
	return s.Accepts(tok)
}

func (b *APIBackend) PackActive(name string) bool { return b.activePack(name) != nil }

func (b *APIBackend) activePack(name string) *packs.Pack {
	for _, src := range b.a.Tools.Packs() {
		if p, ok := src.(*packs.Pack); ok && p.Name == name {
			return p
		}
	}
	return nil
}

// Query is one tool-less chat with no history (spec §4.1).
func (b *APIBackend) Query(ctx context.Context, q api.QueryRequest) (api.QueryResponse, error) {
	system, used := b.systemFor(q)
	msgs := []provider.Message{}
	if system != "" {
		msgs = append(msgs, provider.Message{Role: provider.RoleSystem, Content: system})
	}
	msgs = append(msgs, provider.Message{Role: provider.RoleUser, Content: q.Prompt})
	req := provider.ChatRequest{Messages: msgs, MaxTokens: q.MaxTokens, NoThink: true, Format: rawOrNil(q.JSONSchema)}
	resp, model, err := b.chat(ctx, q.Model, req, nil)
	if err != nil {
		return api.QueryResponse{}, err
	}
	return api.QueryResponse{Text: strings.TrimSpace(resp.Content), Model: model, Pack: used, Usage: usageOf(resp)}, nil
}

// rawOrNil is r, or nil when it is absent or JSON null: "null" asks for no
// structured output, and must not reach a provider as a format.
func rawOrNil(r json.RawMessage) json.RawMessage {
	t := bytes.TrimSpace(r)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return nil
	}
	return r
}

// systemFor is the caller's system text with an allowed, active pack's
// rules in front; used names the pack, or is nil when none or it went
// inactive between the server's check and this call.
func (b *APIBackend) systemFor(q api.QueryRequest) (string, *string) {
	if q.Pack == "" {
		return q.System, nil
	}
	p := b.activePack(q.Pack)
	if p == nil {
		return q.System, nil
	}
	name := p.Name
	rules := packBlock(p)
	if q.System == "" {
		return rules, &name
	}
	return rules + "\n\n" + q.System, &name
}

// Complete is the chat-completions tier: the caller's messages as they are.
func (b *APIBackend) Complete(ctx context.Context, c api.CompletionRequest, onDelta func(string)) (api.CompletionResponse, error) {
	msgs := make([]provider.Message, 0, len(c.Messages))
	for _, m := range c.Messages {
		msgs = append(msgs, provider.Message{Role: provider.Role(m.Role), Content: m.Content})
	}
	// The caller's response_format goes to the provider unchanged (spec §4.2).
	req := provider.ChatRequest{Messages: msgs, MaxTokens: c.MaxTokens, NoThink: true, ResponseFormat: rawOrNil(c.ResponseFormat)}
	if c.Temperature != nil {
		req.Temperature = *c.Temperature
	}
	var stream provider.StreamFunc
	if onDelta != nil {
		stream = func(d string) { onDelta(d) }
	}
	resp, model, err := b.chat(ctx, c.Model, req, stream)
	if err != nil {
		return api.CompletionResponse{}, err
	}
	fr := resp.FinishReason
	if fr == "" {
		fr = "stop"
	}
	return api.CompletionResponse{Text: resp.Content, Model: model, FinishReason: fr, Usage: usageOf(resp)}, nil
}

// SessionQuery answers with a snapshot of the session's conversation as
// context (spec §4.3): taken only when no turn is running, harness state
// stripped, web page text withheld, never appended.
func (b *APIBackend) SessionQuery(ctx context.Context, q api.QueryRequest) (api.QueryResponse, error) {
	a := b.a
	system, snapshot, ok := a.apiSnapshot()
	if !ok {
		return api.QueryResponse{}, api.Errorf(http.StatusConflict, "session_busy", "the session is in the middle of a turn; try again shortly")
	}

	callerSystem, used := b.systemFor(q)
	if callerSystem != "" {
		system = strings.TrimSpace(system + "\n\n" + callerSystem)
	}
	msgs := []provider.Message{{Role: provider.RoleSystem, Content: system}}
	msgs = append(msgs, snapshot...)
	msgs = append(msgs, provider.Message{Role: provider.RoleUser, Content: q.Prompt})

	if w := a.Window(); w > 0 {
		need := q.MaxTokens
		for _, m := range msgs {
			need += a.History.MessageTokens(m)
		}
		if need > w {
			return api.QueryResponse{}, api.Errorf(http.StatusRequestEntityTooLarge, "context_too_large",
				"the session's conversation plus the prompt and max_tokens need about %d tokens; the window is %d", need, w)
		}
	}
	resp, model, err := b.chat(ctx, q.Model, provider.ChatRequest{Messages: msgs, MaxTokens: q.MaxTokens, NoThink: true}, nil)
	if err != nil {
		return api.QueryResponse{}, err
	}
	return api.QueryResponse{Text: strings.TrimSpace(resp.Content), Model: model, Pack: used, Usage: usageOf(resp)}, nil
}

// apiSnapshot copies the system prompt and the conversation, harness state
// stripped and web page text withheld, with the turn lock held (TryLock: ok
// is false while a turn runs). Both locks are released by defer, so a panic
// while copying — recovered by net/http — never leaves the session locked
// for the life of the process.
func (a *Agent) apiSnapshot() (system string, snapshot []provider.Message, ok bool) {
	if !a.turnMu.TryLock() {
		return "", nil, false
	}
	defer a.turnMu.Unlock()
	system = func() string {
		a.modelMu.Lock()
		defer a.modelMu.Unlock()
		return a.History.System.Content
	}()
	all := a.History.Messages
	snapshot = make([]provider.Message, 0, len(all)+1)
	for _, m := range all {
		m := m
		if webResult(m, all) {
			m.Content = "(page text is not shown)"
		} else {
			m.Content = StripHarnessState(m.Content)
		}
		snapshot = append(snapshot, m)
	}
	return system, snapshot, true
}

// chat runs one request inside the server's lane on the primary's own
// provider (model "" or "default") or on a named model's provider built by
// APIModelFactory. It deliberately does not go through chatFiltered: that
// streams into Events.OnDelta, the person's transcript, and an app's answer
// is not the person's conversation. What it keeps from chatFiltered: the
// spend check and accounting, think-stripping, and stats. Afterwards, with
// the primary idle, the person's prompt is re-read in the background (spec
// §5: the API call made the cache cold).
func (b *APIBackend) chat(ctx context.Context, model string, req provider.ChatRequest, onDelta provider.StreamFunc) (*provider.ChatResponse, string, error) {
	a := b.a
	primary := model == "" || model == "default"
	// The person's gate and cap, never a prompt: an app's request is refused
	// outright (spec §5), not put to the terminal. The gate covers named
	// models too: they are built on the same, current provider.
	if err := a.checkOnlineGate(ctx, false); err != nil {
		return nil, "", api.Errorf(http.StatusForbidden, "online_not_approved", "%v", err)
	}
	_, online := a.Online()
	if !primary && online {
		// Spend is counted and capped only at the primary's own pricing; a
		// named model's bill could be neither, and an uncapped bill is
		// worse than a refusal.
		return nil, "", api.Errorf(http.StatusForbidden, "model_not_allowed", "named models are not served while the session's provider is online; use the default model")
	}
	if primary {
		if online {
			if capUSD := a.SpendCap(); capUSD > 0 && a.Usage().SpendUSD >= capUSD {
				return nil, "", api.Errorf(http.StatusTooManyRequests, "spend_cap", "the session's spend cap of $%.2f is reached", capUSD)
			}
		}
		// Never with no window on the wire (the 0.12.1 incident): after a
		// /model the request waits for the resolution, as every primary
		// call site does. The model is read after the wait.
		a.awaitWindow(ctx)
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
	}
	prov, name, profile, provName := a.apiModelSnapshot()
	if !primary {
		p, err := b.providerFor(ctx, provName, model)
		if err != nil {
			return nil, "", err
		}
		prov, name = p, model
	}
	req.Model = name
	if profile.StripThink && onDelta != nil {
		f := &ThinkFilter{}
		inner := onDelta
		onDelta = func(d string) {
			if out := f.Feed(d); out != "" {
				inner(out)
			}
		}
		defer func() {
			if tail := f.Flush(); tail != "" {
				inner(tail)
			}
		}()
	}
	// A prefill never shares the server with a real request, and the lane
	// is the primary server's at background priority: the person's own
	// requests go first, and the API never runs alongside them (spec §5).
	a.stopPrefill()
	resp, err := inLaneWith(ctx, a.laneFor(provName, false), func() (*provider.ChatResponse, error) { return prov.Chat(ctx, req, onDelta) })
	if err != nil {
		return nil, "", err
	}
	if profile.StripThink {
		resp.Content = StripThink(resp.Content)
	}
	used := Stats{Requests: 1, PromptTokens: resp.Usage.PromptTokens, CompletionTokens: resp.Usage.CompletionTokens}
	if used.PromptTokens == 0 {
		for _, m := range req.Messages {
			used.PromptTokens += a.History.MessageTokens(m)
		}
		used.CompletionTokens = a.History.est(resp.Content)
	}
	if _, online := a.Online(); online && primary {
		if p := a.Pricing(); p.Known {
			used.SpendUSD = float64(used.PromptTokens)*p.Prompt + float64(used.CompletionTokens)*p.Completion
		}
	}
	a.addStats(used)
	if primary {
		a.StartPrefill()
	}
	return resp, name, nil
}

// providerFor is the named model's provider on providerName, built once
// per provider and model: keyed by both, so a /provider switch never serves
// a provider built for the backend the session has left.
func (b *APIBackend) providerFor(ctx context.Context, providerName, model string) (provider.Provider, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if APIModelFactory == nil {
		return nil, api.Errorf(http.StatusForbidden, "model_not_allowed", "this session serves only its own model")
	}
	key := providerName + "\x00" + model
	if p, ok := b.provs[key]; ok {
		return p, nil
	}
	p, err := APIModelFactory(ctx, b.a.Cfg, providerName, model)
	if err != nil {
		return nil, fmt.Errorf("model %s: %w", model, err)
	}
	b.provs[key] = p
	return p, nil
}

// modelSnapshot reads the primary's provider, model and profile together
// under the lock their writers (SetModel, SetProvider) hold.
func (a *Agent) modelSnapshot() (provider.Provider, string, profiles.Profile) {
	a.modelMu.Lock()
	defer a.modelMu.Unlock()
	return a.Provider, a.Model, a.Profile
}

// apiModelSnapshot is modelSnapshot plus the provider's name, read together
// under modelMu so a /provider switch cannot pair one backend's name with
// another's model.
func (a *Agent) apiModelSnapshot() (provider.Provider, string, profiles.Profile, string) {
	a.modelMu.Lock()
	defer a.modelMu.Unlock()
	name := ""
	if a.Provider != nil {
		name = a.Provider.Name()
	}
	return a.Provider, a.Model, a.Profile, name
}

func usageOf(r *provider.ChatResponse) api.Usage {
	return api.Usage{PromptTokens: r.Usage.PromptTokens, CompletionTokens: r.Usage.CompletionTokens}
}

// errTurnRunning is the answer when a token change cannot take the turn lock.
var errTurnRunning = errors.New("the session is in the middle of a turn; try again after it finishes")

// SessionToken is the session's token, minted and saved if it had none. It
// holds the turn lock (TryLock) so the save never races a turn's appends.
func (a *Agent) SessionToken() (string, error) {
	return a.withTurnLock(func(s *store.Session) (string, error) {
		a.saveMu.Lock()
		tok := s.AccessToken()
		a.saveMu.Unlock()
		return tok, nil
	})
}

// RotateSessionToken replaces the token (spec §3.1) and saves.
func (a *Agent) RotateSessionToken(grace time.Duration) (string, error) {
	return a.withTurnLock(func(s *store.Session) (string, error) {
		a.saveMu.Lock()
		defer a.saveMu.Unlock()
		if err := s.Rotate(grace); err != nil {
			return "", err
		}
		return s.Token, nil
	})
}

// withTurnLock runs change with the turn lock held, then saves as autosave
// does (this pid stamped, the save guard honoured). If a turn holds the lock
// nothing is changed.
func (a *Agent) withTurnLock(change func(*store.Session) (string, error)) (string, error) {
	s := a.CurrentSession()
	if s == nil {
		return "", errors.New("no session")
	}
	if !a.turnMu.TryLock() {
		return "", errTurnRunning
	}
	defer a.turnMu.Unlock()
	if blocked, owner := a.SaveGuard(); blocked {
		return "", fmt.Errorf("session file is owned by live host %d", owner)
	}
	tok, err := change(s)
	if err != nil {
		return "", err
	}
	a.saveMu.Lock()
	defer a.saveMu.Unlock()
	s.HostPID = os.Getpid()
	_, s.Model, _ = a.modelSnapshot()
	s.Messages = a.History.Messages
	if err := s.Save(); err != nil {
		return tok, err
	}
	return tok, nil
}

// APIStatus is what /api and the header show.
type APIStatus struct {
	Serving     bool
	Socket      string
	ServedBy    string // another session's code when it serves instead
	ServedByPID int
	Served      int
	LastApp     string
	LastAt      time.Time
	InFlight    string // the app whose request is being answered now
	Err         string // why this session is not serving, when it tried
}

// APIControl is the serving side the UIs drive (/api on|off, the header).
// cmd/api.go implements it; nil means this session can never serve.
type APIControl interface {
	Start() error
	Stop()
	Status() APIStatus
}
