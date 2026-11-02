package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// The OpenAI chat-completions wire shapes, reduced to what spec §4.2
// honours. Unknown fields are ignored on the way in, as the OpenAI API
// ignores them; the few that would change the meaning of the request
// (tools, functions, n) are refused by name.
type chatCompletionRequest struct {
	Model          string            `json:"model"`
	Messages       []json.RawMessage `json:"messages"`
	Stream         bool              `json:"stream"`
	Temperature    *float64          `json:"temperature"`
	MaxTokens      int               `json:"max_tokens"`
	ResponseFormat json.RawMessage   `json:"response_format"`
	N              *int              `json:"n"`
	Tools          json.RawMessage   `json:"tools"`
	Functions      json.RawMessage   `json:"functions"`
	ToolChoice     json.RawMessage   `json:"tool_choice"`
}

type wireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type chatCompletionResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   wireUsage    `json:"usage"`
}

type chatChoice struct {
	Index        int          `json:"index"`
	Message      *wireContent `json:"message,omitempty"`
	Delta        *wireContent `json:"delta,omitempty"`
	FinishReason *string      `json:"finish_reason"`
}

type wireContent struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content"`
}

type wireUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type chatChunk struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
}

func completionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "chatcmpl-" + hex.EncodeToString(b[:])
}

// parseCompletion turns the wire request into a backend request or a 400.
func parseCompletion(c *call, in chatCompletionRequest) (CompletionRequest, *Error) {
	for name, raw := range map[string]json.RawMessage{"tools": in.Tools, "functions": in.Functions, "tool_choice": in.ToolChoice} {
		if len(raw) > 0 && string(raw) != "null" {
			return CompletionRequest{}, Errorf(http.StatusBadRequest, "not_supported", "%s is not supported: this API serves inference without tools", name)
		}
	}
	if in.N != nil && *in.N > 1 {
		return CompletionRequest{}, Errorf(http.StatusBadRequest, "not_supported", "n above 1 is not supported")
	}
	if len(in.Messages) == 0 {
		return CompletionRequest{}, Errorf(http.StatusBadRequest, "not_supported", "messages must have at least one message")
	}
	if in.MaxTokens < 0 {
		return CompletionRequest{}, Errorf(http.StatusBadRequest, "bad_request", "max_tokens must not be negative")
	}
	out := CompletionRequest{Model: in.Model, Temperature: in.Temperature, MaxTokens: in.MaxTokens, ResponseFormat: in.ResponseFormat}
	for i, raw := range in.Messages {
		var m wireMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return CompletionRequest{}, Errorf(http.StatusBadRequest, "bad_request", "messages[%d] is not an object: %v", i, err)
		}
		switch m.Role {
		case "system", "user", "assistant":
		default:
			return CompletionRequest{}, Errorf(http.StatusBadRequest, "not_supported", "messages[%d]: role %q is not supported (system, user, assistant)", i, m.Role)
		}
		var text string
		if string(m.Content) == "null" || json.Unmarshal(m.Content, &text) != nil {
			return CompletionRequest{}, Errorf(http.StatusBadRequest, "not_supported", "messages[%d]: content must be a string; content parts are not supported", i)
		}
		out.Messages = append(out.Messages, ChatMessage{Role: m.Role, Content: text})
	}
	if !c.app.Profile.AllowsModel(out.Model) {
		return CompletionRequest{}, Errorf(http.StatusForbidden, "model_not_allowed", "app %q may not use model %q", c.app.Name, out.Model)
	}
	cap := c.app.Profile.Budget.Tokens
	if out.MaxTokens == 0 {
		out.MaxTokens = cap
	}
	if cap > 0 && out.MaxTokens > cap {
		return CompletionRequest{}, Errorf(http.StatusBadRequest, "over_budget", "max_tokens %d is over app %q's budget of %d", out.MaxTokens, c.app.Name, cap)
	}
	return out, nil
}

func handleCompletions(s *Server, c *call) {
	var in chatCompletionRequest
	if !readJSON(c, &in) {
		return
	}
	req, e := parseCompletion(c, in)
	if e != nil {
		writeError(c.w, e)
		return
	}
	id, created := completionID(), time.Now().Unix()
	if !in.Stream {
		resp, err := s.o.Backend.Complete(c.r.Context(), req, nil)
		if err != nil {
			writeError(c.w, s.mapErr(err))
			return
		}
		fr := resp.FinishReason
		writeJSON(c, chatCompletionResponse{
			ID: id, Object: "chat.completion", Created: created, Model: resp.Model,
			Choices: []chatChoice{{Message: &wireContent{Role: "assistant", Content: resp.Text}, FinishReason: &fr}},
			Usage:   wireUsage{resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.PromptTokens + resp.Usage.CompletionTokens},
		})
		return
	}

	// Streaming: headers first, then one chunk per delta, then the finish
	// chunk and [DONE]. An error before the first delta is still a JSON
	// error; after it, the stream ends without [DONE], which is how the
	// OpenAI clients report a broken stream.
	flusher, _ := c.w.(http.Flusher)
	started := false
	send := func(v any) {
		if !started {
			c.w.Header().Set("Content-Type", "text/event-stream")
			c.w.Header().Set("Cache-Control", "no-cache")
			c.w.WriteHeader(http.StatusOK)
			started = true
		}
		b, _ := json.Marshal(v)
		fmt.Fprintf(c.w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	chunk := func(delta *wireContent, finish *string) chatChunk {
		return chatChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model, Choices: []chatChoice{{Delta: delta, FinishReason: finish}}}
	}
	resp, err := s.o.Backend.Complete(c.r.Context(), req, func(delta string) {
		send(chunk(&wireContent{Role: "assistant", Content: delta}, nil))
	})
	if err != nil {
		if !started {
			writeError(c.w, s.mapErr(err))
		}
		return
	}
	fr := resp.FinishReason
	send(chunk(&wireContent{}, &fr))
	fmt.Fprint(c.w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}
