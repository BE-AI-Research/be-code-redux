package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Backend is what the server asks the serving session for. It is
// implemented over the running agent in internal/agent (APIBackend) and by
// a fake in this package's tests. Nothing here may change the session: a
// query is answered, never appended.
type Backend interface {
	Health() Health
	Query(ctx context.Context, q QueryRequest) (QueryResponse, error)
	Complete(ctx context.Context, c CompletionRequest, onDelta func(string)) (CompletionResponse, error)
	SessionInfo() SessionInfo
	AcceptsSession(token string) bool
	SessionQuery(ctx context.Context, q QueryRequest) (QueryResponse, error)
	PackActive(name string) bool
}

type Health struct {
	Version     string
	SessionCode string
	Model       string
	Since       time.Time
}

type SessionInfo struct{ Title, Workspace, Model string }

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// QueryRequest is one tool-less question (spec §4.1). Model "" or "default"
// is the serving session's model. Pack names a pack whose core rules go in
// front of System; JSONSchema, when set, is the structured-output format.
type QueryRequest struct {
	Prompt     string
	System     string
	Model      string
	Pack       string
	JSONSchema json.RawMessage
	MaxTokens  int
}

type QueryResponse struct {
	Text  string
	Model string
	Pack  *string // the pack used, nil when none or it went inactive
	Usage Usage
}

type ChatMessage struct {
	Role    string
	Content string
}

// CompletionRequest is the OpenAI chat-completions request reduced to what
// the backend needs (spec §4.2).
type CompletionRequest struct {
	Model          string
	Messages       []ChatMessage
	Temperature    *float64
	MaxTokens      int
	ResponseFormat json.RawMessage
}

type CompletionResponse struct {
	Text         string
	Model        string
	FinishReason string
	Usage        Usage
}

// Error is a failure the backend wants reported with a particular status
// and code. Any other error from a Backend is 502 backend_error.
type Error struct {
	Status  int
	Code    string
	Message string
	Extra   map[string]any // merged into the error object (e.g. "text" for schema_mismatch)
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func Errorf(status int, code, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// writeError sends the spec's error body: {"error": {"code", "message", ...}}.
func writeError(w http.ResponseWriter, e *Error) {
	body := map[string]any{"code": e.Code, "message": e.Message}
	for k, v := range e.Extra {
		body[k] = v
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": body})
}

// toError maps any backend error to an *Error. A context deadline is the
// request's own timeout; a cancellation is the client gone.
func toError(err error) *Error {
	if e, ok := err.(*Error); ok {
		return e
	}
	if err == context.DeadlineExceeded {
		return Errorf(http.StatusGatewayTimeout, "timeout", "the request did not finish within api.timeout")
	}
	return Errorf(http.StatusBadGateway, "backend_error", "%v", err)
}
