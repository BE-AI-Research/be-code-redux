// Package provider defines the inference backend abstraction for BE-Code.
//
// All backends (Ollama, llama.cpp server, vLLM, LM Studio, BE AI Engine)
// are reached through the OpenAI-compatible chat completions API, which is
// the de-facto universal adapter for local inference. Ollama additionally
// gets native management calls (list/pull/ps) via the Ollama type.
package provider

import (
	"context"
	"encoding/json"
	"time"
)

// Role identifies the author of a chat message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one turn in a conversation.
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// ToolCall is a model-requested tool invocation.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // raw JSON object
}

// ToolSpec describes a tool the model may call.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"` // JSON Schema
}

// ChatRequest is a provider-agnostic completion request.
type ChatRequest struct {
	Model       string
	Messages    []Message
	Tools       []ToolSpec
	Temperature float64
	MaxTokens   int
	// OnReasoning receives hidden reasoning deltas from thinking models
	// (Ollama "reasoning", llama.cpp/vLLM "reasoning_content"). Optional.
	OnReasoning StreamFunc
	// NoThink asks the backend to skip hidden reasoning for this call —
	// used for auxiliary work (summaries, briefings) where minutes of
	// deliberation buy nothing. Backends that cannot honor it ignore it.
	NoThink bool
	// ReasoningEffort asks a thinking model to think less or more:
	// "low", "medium" or "high" ("" leaves the backend's default). Sent as
	// OpenAI's reasoning_effort; backends that do not know it ignore it.
	ReasoningEffort string
}

// Usage reports token accounting when the backend supplies it.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	// PromptDuration is how long the server spent reading the prompt and
	// LoadDuration how long loading the model, where it says (native
	// Ollama). PromptTokens is the whole prompt whether or not the server's
	// prompt cache covered it, so the duration is the only evidence of a
	// cache miss: 19k tokens read in 0.3s was cached, in 43s was not.
	PromptDuration time.Duration `json:"-"`
	LoadDuration   time.Duration `json:"-"`
}

// ChatResponse is the final assembled result of a (streamed) completion.
type ChatResponse struct {
	Content      string
	Reasoning    string // hidden reasoning, when the backend exposes it separately
	ToolCalls    []ToolCall
	FinishReason string
	Usage        Usage
}

// StreamFunc receives incremental text deltas during generation.
type StreamFunc func(delta string)

// Provider is an inference backend.
type Provider interface {
	// Name returns the configured provider name (e.g. "ollama", "be-ai-engine").
	Name() string
	// Chat runs one completion. onDelta may be nil (non-streaming display).
	Chat(ctx context.Context, req ChatRequest, onDelta StreamFunc) (*ChatResponse, error)
	// ListModels enumerates models available on the backend.
	ListModels(ctx context.Context) ([]ModelInfo, error)
	// Ping checks reachability; returns a short human-readable status.
	Ping(ctx context.Context) (string, error)
}

// BackendStatus is implemented by providers that can say whether a model is
// resident and what context window it is currently loaded with (Ollama).
// The agent uses it to notice evictions and window changes caused by other
// clients sharing the server.
type BackendStatus interface {
	Status(ctx context.Context, model string) (window int, loaded bool, err error)
}

// NativeFallbacker is implemented by providers that prefer a native
// endpoint but can fall back to the OpenAI-compatible one against a server
// that does not serve it (Ollama). The agent reports the downgrade once,
// because the fallback silently loses what the native path was for.
type NativeFallbacker interface {
	NativeFallback() bool
}

// KeepAliver is implemented by providers that can extend a model's
// residency (Ollama keep_alive), so idle expiry between prompts does not
// evict it and force a slow reload plus prompt re-processing.
type KeepAliver interface {
	KeepAlive(ctx context.Context, model string, d time.Duration) error
}

// WindowClearer is implemented by providers that carry a context window on
// the wire (Ollama's num_ctx). Clearing it means "send no window", which is
// not the same as sending zero — and not safe either: a real Ollama runs a
// request that names no window at its own default, reloading a model held at
// another size. So a cleared wire is a state to wait out, never to send in
// (Agent.awaitWindow).
//
// The agent uses it for the gap between a model switch and the loader's
// answer for the new model. Options belong to the endpoint, so in that gap
// the provider is still carrying the previous model's window, and putting
// that on the wire would reload the new model without anybody being asked.
type WindowClearer interface {
	ClearWindow()
}

// ModelDetailer is implemented by providers that can say more about their
// models than a name and a size: the window each one is currently loaded
// with, and whether it is resident at all. That is what a model picker is
// really being asked — "what am I choosing between" — and it is the one
// place a user can see, before switching, that a model is already held by
// somebody else at a window our own config disagrees with.
type ModelDetailer interface {
	Details(ctx context.Context) ([]ModelDetail, error)
}

// ModelInfo describes an available model.
type ModelInfo struct {
	ID           string
	SizeBytes    int64
	Family       string
	Quantization string
}
