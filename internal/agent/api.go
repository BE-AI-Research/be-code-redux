package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/brown-enterprises/be-code/internal/api"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/packs"
	"github.com/brown-enterprises/be-code/internal/provider"
)

// APIModelFactory builds a provider for a model the serving session is not
// running, loaded as a reviewer is (cmd/root.go: FromConfig + secondaryLoad)
// so the primary's window is never touched. Nil means only "default" is
// served. Injected from cmd/root.go like ReviewerFactory.
var APIModelFactory func(ctx context.Context, cfg *config.Config, model string) (provider.Provider, error)

// APIBackend answers the application API from the running session (spec
// 2026-11-02-app-api-design.md §4–§5). Nothing here is a typed request: it
// never clears UntrustedWeb, never touches the inbox, the engine, the work
// ledger or History.Messages, and never writes.
type APIBackend struct {
	a       *Agent
	version string
	since   time.Time

	mu    sync.Mutex
	provs map[string]provider.Provider // named models, built once each
}

func NewAPIBackend(a *Agent, version string) *APIBackend {
	return &APIBackend{a: a, version: version, since: time.Now(), provs: map[string]provider.Provider{}}
}

func (b *APIBackend) Health() api.Health {
	code := ""
	if b.a.Session != nil {
		code = b.a.Session.ResumeCode()
	}
	return api.Health{Version: b.version, SessionCode: code, Model: b.a.Model, Since: b.since}
}

func (b *APIBackend) SessionInfo() api.SessionInfo {
	title := ""
	if b.a.Session != nil {
		title = b.a.Session.Title
	}
	return api.SessionInfo{Title: title, Workspace: b.a.Tools.Root, Model: b.a.Model}
}

// AcceptsSession compares under saveMu: Accepts may clear an expired grace
// token, and autosave marshals the same struct.
func (b *APIBackend) AcceptsSession(tok string) bool {
	if b.a.Session == nil {
		return false
	}
	b.a.saveMu.Lock()
	defer b.a.saveMu.Unlock()
	return b.a.Session.Accepts(tok)
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
	resp, model, err := b.chat(ctx, q.Model, provider.ChatRequest{Messages: msgs, MaxTokens: q.MaxTokens, NoThink: true}, nil)
	if err != nil {
		return api.QueryResponse{}, err
	}
	return api.QueryResponse{Text: strings.TrimSpace(resp.Content), Model: model, Pack: used, Usage: usageOf(resp)}, nil
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
	req := provider.ChatRequest{Messages: msgs, MaxTokens: c.MaxTokens, NoThink: true}
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
	if !a.turnMu.TryLock() {
		return api.QueryResponse{}, api.Errorf(http.StatusConflict, "session_busy", "the session is in the middle of a turn; try again shortly")
	}
	system := a.History.System.Content
	snapshot := make([]provider.Message, 0, len(a.History.Messages)+1)
	all := a.History.Messages
	for _, m := range all {
		m := m
		if webResult(m, all) {
			m.Content = "(page text is not shown)"
		} else {
			m.Content = StripHarnessState(m.Content)
		}
		snapshot = append(snapshot, m)
	}
	a.turnMu.Unlock()

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
	if err := a.checkSpendCap(ctx); err != nil {
		return nil, "", api.Errorf(http.StatusTooManyRequests, "spend_cap", "%v", err)
	}
	prov, name := a.Provider, a.Model
	primary := model == "" || model == "default"
	if !primary {
		p, err := b.providerFor(ctx, model)
		if err != nil {
			return nil, "", err
		}
		prov, name = p, model
	}
	req.Model = name
	if a.Profile.StripThink && onDelta != nil {
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
	resp, err := a.inLane(ctx, func() (*provider.ChatResponse, error) { return prov.Chat(ctx, req, onDelta) })
	if err != nil {
		return nil, "", err
	}
	if a.Profile.StripThink {
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

func (b *APIBackend) providerFor(ctx context.Context, model string) (provider.Provider, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if APIModelFactory == nil {
		return nil, api.Errorf(http.StatusForbidden, "model_not_allowed", "this session serves only its own model")
	}
	if p, ok := b.provs[model]; ok {
		return p, nil
	}
	p, err := APIModelFactory(ctx, b.a.Cfg, model)
	if err != nil {
		return nil, fmt.Errorf("model %s: %w", model, err)
	}
	b.provs[model] = p
	return p, nil
}

func usageOf(r *provider.ChatResponse) api.Usage {
	return api.Usage{PromptTokens: r.Usage.PromptTokens, CompletionTokens: r.Usage.CompletionTokens}
}

// SessionToken is the session's token, minted and saved if it had none.
func (a *Agent) SessionToken() (string, error) {
	if a.Session == nil {
		return "", errors.New("no session")
	}
	a.saveMu.Lock()
	tok := a.Session.AccessToken()
	a.saveMu.Unlock()
	return tok, a.saveSessionNow()
}

// RotateSessionToken replaces the token (spec §3.1) and saves.
func (a *Agent) RotateSessionToken(grace time.Duration) (string, error) {
	if a.Session == nil {
		return "", errors.New("no session")
	}
	a.saveMu.Lock()
	err := a.Session.Rotate(grace)
	tok := a.Session.Token
	a.saveMu.Unlock()
	if err != nil {
		return "", err
	}
	return tok, a.saveSessionNow()
}

// saveSessionNow persists the session's fields as autosave does, without a
// user input to title it from. Refused when another host owns the file.
func (a *Agent) saveSessionNow() error {
	if blocked, owner := a.SaveGuard(); blocked {
		return fmt.Errorf("session file is owned by live host %d", owner)
	}
	a.saveMu.Lock()
	defer a.saveMu.Unlock()
	a.Session.Model = a.Model
	a.Session.Messages = a.History.Messages
	return a.Session.Save()
}
