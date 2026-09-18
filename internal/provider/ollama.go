package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

// Ollama talks to an Ollama server over its native /api/chat endpoint, which
// is the only one that can carry an options block (num_ctx above all) and a
// keep_alive. The embedded OpenAICompat client stays as the fallback for
// servers too old to serve /api/chat, and supplies the shared HTTP client,
// name and credentials.
type Ollama struct {
	*OpenAICompat
	APIBase string // e.g. http://localhost:11434

	mu        sync.RWMutex
	opts      Options
	keepAlive string
	// noThink names the models the server has told us cannot think at all,
	// so the think key is dropped for them rather than failing every
	// request. Keyed by model, because a session can switch models.
	noThink map[string]bool

	// nativeBroken latches once a server has told us /api/chat is not there,
	// so an old server is probed once per session rather than every request.
	nativeBroken atomic.Bool
}

// Options carries what the harness controls on the native path. Zero fields
// are omitted, so a zero Options sends {} and the server's defaults stand.
type Options struct {
	NumCtx      int            `json:"num_ctx,omitempty"`
	Temperature float64        `json:"temperature,omitempty"`
	NumPredict  int            `json:"num_predict,omitempty"`
	Extra       map[string]any `json:"-"` // merged in last, from config
}

// NewOllama derives the native API base from the configured base URL, which
// may be given either way round (http://host:11434 or http://host:11434/v1):
// the native calls use the bare host and the OpenAI fallback uses /v1.
func NewOllama(name, baseURL, apiKey string) *Ollama {
	api := strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
	return &Ollama{
		OpenAICompat: NewOpenAICompat(name, api+"/v1", apiKey),
		APIBase:      api,
	}
}

// SetOptions replaces the options sent with every native request. The
// config loader is the only caller; it may run while a request is in
// flight, so the fields are guarded.
func (p *Ollama) SetOptions(o Options) {
	if o.Extra != nil {
		extra := make(map[string]any, len(o.Extra))
		for k, v := range o.Extra {
			extra[k] = v
		}
		o.Extra = extra
	}
	p.mu.Lock()
	p.opts = o
	p.mu.Unlock()
}

// Options returns the options currently sent with native requests. Extra is
// copied, so a caller reading them cannot race a later SetOptions.
func (p *Ollama) Options() Options {
	p.mu.RLock()
	defer p.mu.RUnlock()
	o := p.opts
	if o.Extra != nil {
		extra := make(map[string]any, len(o.Extra))
		for k, v := range o.Extra {
			extra[k] = v
		}
		o.Extra = extra
	}
	return o
}

// SetKeepAlive sets the keep_alive sent with every native request (a Go
// duration string, "" to leave it to the server).
func (p *Ollama) SetKeepAlive(d string) {
	p.mu.Lock()
	p.keepAlive = d
	p.mu.Unlock()
}

// optionsMap builds the request's options block: the configured values,
// overridden by anything the request itself asks for, with the passthrough
// map merged in last so config can reach keys the harness knows nothing of.
func (p *Ollama) optionsMap(req ChatRequest) map[string]any {
	p.mu.RLock()
	o := p.opts
	p.mu.RUnlock()
	m := map[string]any{}
	if o.NumCtx > 0 {
		m["num_ctx"] = o.NumCtx
	}
	switch {
	case req.Temperature != 0:
		m["temperature"] = req.Temperature
	case o.Temperature != 0:
		m["temperature"] = o.Temperature
	}
	switch {
	case req.MaxTokens > 0:
		m["num_predict"] = req.MaxTokens
	case o.NumPredict > 0:
		m["num_predict"] = o.NumPredict
	}
	for k, v := range o.Extra {
		m[k] = v
	}
	return m
}

// ---- native wire types -----------------------------------------------------

type nativeToolCall struct {
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"` // an object, not a string
	} `json:"function"`
}

type nativeMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []nativeToolCall `json:"tool_calls,omitempty"`
	ToolName  string           `json:"tool_name,omitempty"`
}

// nativeChunk is one NDJSON line of a streamed /api/chat response.
type nativeChunk struct {
	Message struct {
		Content   string           `json:"content"`
		Thinking  string           `json:"thinking"`
		ToolCalls []nativeToolCall `json:"tool_calls"`
	} `json:"message"`
	Done            bool   `json:"done"`
	DoneReason      string `json:"done_reason"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
	Error           string `json:"error"`
}

// nativeMessages converts history to Ollama's message shape. Tool call
// arguments travel as an object there, not as the JSON string the rest of
// the harness carries, and a tool result is tied to its call by name.
func nativeMessages(msgs []Message) []nativeMessage {
	out := make([]nativeMessage, 0, len(msgs))
	for _, m := range msgs {
		nm := nativeMessage{Role: string(m.Role), Content: m.Content}
		if m.Role == RoleTool {
			nm.ToolName = m.Name
		}
		for _, tc := range m.ToolCalls {
			var w nativeToolCall
			w.Function.Name = tc.Name
			args := strings.TrimSpace(tc.Arguments)
			if args == "" || !json.Valid([]byte(args)) {
				args = "{}"
			}
			w.Function.Arguments = json.RawMessage(args)
			nm.ToolCalls = append(nm.ToolCalls, w)
		}
		out = append(out, nm)
	}
	return out
}

func nativeTools(tools []ToolSpec) []oaTool {
	out := make([]oaTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, oaTool{Type: "function", Function: oaFunction(t)})
	}
	return out
}

// ---- chat ------------------------------------------------------------------

// httpError carries the status code of a rejected request so the caller can
// tell "this endpoint is not here" from "this request failed".
type httpError struct {
	Provider string
	Code     int
	Body     string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("%s: HTTP %d: %s", e.Provider, e.Code, e.Body)
}

// isNativeUnsupported reports whether err means the server has no /api/chat
// at all, which is the one reason to spend the rest of the session on the
// OpenAI path. Only the status codes that mean "no such endpoint" count: a
// 400, a 500 or a transport failure is an ordinary error — a bad request or
// a model that genuinely failed — and downgrading the session over one would
// hide it and quietly cost the user their num_ctx for the rest of the run.
func isNativeUnsupported(err error) bool {
	var he *httpError
	if !errors.As(err, &he) {
		return false
	}
	switch he.Code {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return true
	}
	return false
}

// isThinkUnsupported reports whether err is Ollama refusing the think key —
// because the model has no reasoning ("<model> does not support thinking")
// or because an older server does not know the level form. That is about
// the key, not the endpoint, so it costs the key rather than the native
// path. Any 400 that names think qualifies: the only thing it buys is one
// retry without a key that request did not need.
func isThinkUnsupported(err error) bool {
	var he *httpError
	if !errors.As(err, &he) || he.Code != http.StatusBadRequest {
		return false
	}
	return strings.Contains(strings.ToLower(he.Body), "think")
}

// Chat runs one completion over native /api/chat, falling back to the
// OpenAI-compatible endpoint (once, for the rest of the session) against a
// server that does not serve it.
func (p *Ollama) Chat(ctx context.Context, req ChatRequest, onDelta StreamFunc) (*ChatResponse, error) {
	if p.nativeBroken.Load() {
		return p.OpenAICompat.Chat(ctx, req, onDelta)
	}
	resp, err := p.nativeChat(ctx, req, onDelta)
	switch {
	case err != nil && isNativeUnsupported(err):
		// An older server is a reason to degrade, not to fail.
		p.nativeBroken.Store(true)
		return p.OpenAICompat.Chat(ctx, req, onDelta)
	case err != nil && isThinkUnsupported(err) && !p.thinkRefused(req.Model):
		// This model cannot think; ask again without the key, once, and
		// remember it for the rest of the session.
		p.refuseThink(req.Model)
		return p.nativeChat(ctx, req, onDelta)
	}
	return resp, err
}

func (p *Ollama) thinkRefused(model string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.noThink[model]
}

func (p *Ollama) refuseThink(model string) {
	p.mu.Lock()
	if p.noThink == nil {
		p.noThink = map[string]bool{}
	}
	p.noThink[model] = true
	p.mu.Unlock()
}

func (p *Ollama) nativeChat(ctx context.Context, req ChatRequest, onDelta StreamFunc) (*ChatResponse, error) {
	body := map[string]any{
		"model":    req.Model,
		"messages": nativeMessages(req.Messages),
		"stream":   true,
		"options":  p.optionsMap(req),
	}
	// think is what the OpenAI path expressed as reasoning_effort (Ollama's
	// own compatibility layer translates one into the other), so the native
	// path must carry it or a thinking model loses the harness's control of
	// its reasoning budget. It is never sent as a bare true: leaving the key
	// out gives every model its own default, and a model that cannot think
	// at all is remembered below rather than asked twice.
	if !p.thinkRefused(req.Model) {
		switch {
		case req.NoThink:
			body["think"] = false
		case req.ReasoningEffort != "":
			body["think"] = req.ReasoningEffort
		}
	}
	if len(req.Tools) > 0 {
		body["tools"] = nativeTools(req.Tools)
	}
	p.mu.RLock()
	ka := p.keepAlive
	p.mu.RUnlock()
	if ka != "" {
		body["keep_alive"] = ka
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.APIBase+"/api/chat", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	resp, err := p.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.ProviderName, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &httpError{Provider: p.ProviderName, Code: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	return p.consumeNative(resp.Body, onDelta, req.OnReasoning)
}

// consumeNative reads the NDJSON stream: one complete JSON object per line,
// not SSE. Tool calls arrive whole and without ids, so there is nothing to
// merge by index — only ids to synthesize, the way the OpenAI path does for
// servers that send none, so the agent can pair a result with its call.
func (p *Ollama) consumeNative(r io.Reader, onDelta, onReasoning StreamFunc) (*ChatResponse, error) {
	out := &ChatResponse{}
	var text, reasoning strings.Builder
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	saw := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var chunk nativeChunk
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			continue // tolerate anything that is not a chunk
		}
		saw = true
		if chunk.Error != "" {
			return nil, fmt.Errorf("%s: %s", p.ProviderName, chunk.Error)
		}
		if c := chunk.Message.Content; c != "" {
			text.WriteString(c)
			if onDelta != nil {
				onDelta(c)
			}
		}
		if th := chunk.Message.Thinking; th != "" {
			reasoning.WriteString(th)
			if onReasoning != nil {
				onReasoning(th)
			}
		}
		for _, tc := range chunk.Message.ToolCalls {
			args := strings.TrimSpace(string(tc.Function.Arguments))
			if args == "" || args == "null" {
				args = "{}"
			}
			out.ToolCalls = append(out.ToolCalls, ToolCall{
				ID:        fmt.Sprintf("call_%d", len(out.ToolCalls)),
				Name:      tc.Function.Name,
				Arguments: args,
			})
		}
		if chunk.PromptEvalCount > 0 {
			out.Usage.PromptTokens = chunk.PromptEvalCount
		}
		if chunk.EvalCount > 0 {
			out.Usage.CompletionTokens = chunk.EvalCount
		}
		if chunk.Done {
			out.FinishReason = chunk.DoneReason
			if out.FinishReason == "" {
				out.FinishReason = "stop"
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: stream read: %w", p.ProviderName, err)
	}
	if !saw {
		return nil, fmt.Errorf("%s: empty response from %s/api/chat", p.ProviderName, p.APIBase)
	}
	out.Content = text.String()
	out.Reasoning = reasoning.String()
	return out, nil
}
