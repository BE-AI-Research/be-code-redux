package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// OpenAICompat talks to any OpenAI-compatible /v1/chat/completions endpoint:
// Ollama, llama.cpp server, vLLM, LM Studio, and the BE AI Engine fabric.
type OpenAICompat struct {
	ProviderName string
	BaseURL      string // e.g. http://localhost:11434/v1
	APIKey       string
	HTTPClient   *http.Client
}

// NewOpenAICompat builds a client with a generous timeout suited to local
// inference (first-token latency on CPU/consumer GPUs can be long).
func NewOpenAICompat(name, baseURL, apiKey string) *OpenAICompat {
	return &OpenAICompat{
		ProviderName: name,
		BaseURL:      strings.TrimRight(baseURL, "/"),
		APIKey:       apiKey,
		HTTPClient:   &http.Client{Timeout: 30 * time.Minute},
	}
}

func (p *OpenAICompat) Name() string { return p.ProviderName }

// ---- wire types ------------------------------------------------------------

type oaTool struct {
	Type     string     `json:"type"`
	Function oaFunction `json:"function"`
}

type oaFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type oaToolCall struct {
	ID       string `json:"id,omitempty"`
	Index    *int   `json:"index,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type oaMessage struct {
	Role       string       `json:"role"`
	Content    string       `json:"content"`
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
	Name       string       `json:"name,omitempty"`
	// Reasoning fields are inbound only (deltas from thinking models).
	Reasoning        string `json:"reasoning,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type oaRequest struct {
	Model       string      `json:"model"`
	Messages    []oaMessage `json:"messages"`
	Tools       []oaTool    `json:"tools,omitempty"`
	Temperature float64     `json:"temperature"`
	MaxTokens   int         `json:"max_tokens,omitempty"`
	Stream      bool        `json:"stream"`
	// ReasoningEffort: OpenAI's field; Ollama's OpenAI endpoint passes it
	// to thinking models (verified: low cuts a Qwen3 reply's reasoning by
	// roughly six times), unknown elsewhere and harmlessly ignored.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	StreamOpts      *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
}

type oaChoiceDelta struct {
	Delta        oaMessage `json:"delta"`
	Message      oaMessage `json:"message"`
	FinishReason string    `json:"finish_reason"`
}

type oaChunk struct {
	Choices []oaChoiceDelta `json:"choices"`
	Usage   *Usage          `json:"usage"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// ---- chat ------------------------------------------------------------------

func toWire(msgs []Message) []oaMessage {
	out := make([]oaMessage, 0, len(msgs))
	for _, m := range msgs {
		om := oaMessage{Role: string(m.Role), Content: m.Content, ToolCallID: m.ToolCallID, Name: m.Name}
		for _, tc := range m.ToolCalls {
			w := oaToolCall{ID: tc.ID, Type: "function"}
			w.Function.Name = tc.Name
			w.Function.Arguments = tc.Arguments
			om.ToolCalls = append(om.ToolCalls, w)
		}
		out = append(out, om)
	}
	return out
}

// Chat streams a completion, accumulating text and tool calls.
func (p *OpenAICompat) Chat(ctx context.Context, req ChatRequest, onDelta StreamFunc) (*ChatResponse, error) {
	wire := oaRequest{
		Model:       req.Model,
		Messages:    toWire(req.Messages),
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Stream:      true,
	}
	wire.ReasoningEffort = req.ReasoningEffort
	// Ask for usage in the final chunk; servers that don't know the field
	// ignore it, and the ones that do give us real token counts.
	wire.StreamOpts = &struct {
		IncludeUsage bool `json:"include_usage"`
	}{IncludeUsage: true}
	for _, t := range req.Tools {
		wire.Tools = append(wire.Tools, oaTool{Type: "function", Function: oaFunction(t)})
	}

	body, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.BaseURL+"/chat/completions", bytes.NewReader(body))
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
		return nil, fmt.Errorf("%s: HTTP %d: %s", p.ProviderName, resp.StatusCode, strings.TrimSpace(string(b)))
	}

	return p.consumeStream(resp.Body, onDelta, req.OnReasoning)
}

// consumeStream parses SSE chunks, merging tool-call fragments by index.
func (p *OpenAICompat) consumeStream(r io.Reader, onDelta, onReasoning StreamFunc) (*ChatResponse, error) {
	out := &ChatResponse{}
	var text, reasoning strings.Builder
	calls := map[int]*ToolCall{} // index -> accumulating call
	nextIdx := 0

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	sawData := false
	var other strings.Builder // non-SSE bytes, in case the server ignored stream:true
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			if other.Len() < 4*1024*1024 {
				other.WriteString(line)
			}
			continue
		}
		sawData = true
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk oaChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue // tolerate malformed keep-alives from lenient servers
		}
		if chunk.Error != nil {
			return nil, fmt.Errorf("%s: %s", p.ProviderName, chunk.Error.Message)
		}
		if chunk.Usage != nil {
			out.Usage = *chunk.Usage
		}
		for _, ch := range chunk.Choices {
			d := ch.Delta
			// Some non-streaming-strict servers put the whole message here.
			if d.Content == "" && len(d.ToolCalls) == 0 && ch.Message.Content != "" {
				d = ch.Message
			}
			if d.Content != "" {
				text.WriteString(d.Content)
				if onDelta != nil {
					onDelta(d.Content)
				}
			}
			if r := d.Reasoning + d.ReasoningContent; r != "" {
				reasoning.WriteString(r)
				if onReasoning != nil {
					onReasoning(r)
				}
			}
			for _, tc := range d.ToolCalls {
				idx := nextIdx
				if tc.Index != nil {
					idx = *tc.Index
				}
				cur, ok := calls[idx]
				if !ok {
					cur = &ToolCall{}
					calls[idx] = cur
					nextIdx = idx + 1
				}
				if tc.ID != "" {
					cur.ID = tc.ID
				}
				if tc.Function.Name != "" {
					cur.Name = tc.Function.Name
				}
				cur.Arguments += tc.Function.Arguments
			}
			if ch.FinishReason != "" {
				out.FinishReason = ch.FinishReason
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: stream read: %w", p.ProviderName, err)
	}
	if !sawData {
		// Some servers answer a stream request with one plain completion
		// object; accept it. Anything else is an empty reply we must not
		// pass off as an answer.
		var whole struct {
			Choices []oaChoiceDelta `json:"choices"`
			Usage   *Usage          `json:"usage"`
		}
		if json.Unmarshal([]byte(other.String()), &whole) == nil && len(whole.Choices) > 0 {
			m := whole.Choices[0].Message
			out.Content = m.Content
			out.Reasoning = m.Reasoning + m.ReasoningContent
			out.FinishReason = whole.Choices[0].FinishReason
			if whole.Usage != nil {
				out.Usage = *whole.Usage
			}
			for n, tc := range m.ToolCalls {
				id := tc.ID
				if id == "" {
					id = fmt.Sprintf("call_%d", n)
				}
				out.ToolCalls = append(out.ToolCalls, ToolCall{ID: id, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
			}
			if onDelta != nil && out.Content != "" {
				onDelta(out.Content)
			}
			return out, nil
		}
		return nil, fmt.Errorf("%s: empty response (no SSE data and no completion object); is %s an OpenAI-compatible chat endpoint?", p.ProviderName, p.BaseURL)
	}

	out.Content = text.String()
	out.Reasoning = reasoning.String()
	idxs := make([]int, 0, len(calls))
	for i := range calls {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for n, i := range idxs {
		c := calls[i]
		if c.ID == "" {
			c.ID = fmt.Sprintf("call_%d", n)
		}
		if c.Arguments == "" {
			c.Arguments = "{}"
		}
		out.ToolCalls = append(out.ToolCalls, *c)
	}
	return out, nil
}

// ---- models / ping ---------------------------------------------------------

func (p *OpenAICompat) ListModels(ctx context.Context) ([]ModelInfo, error) {
	httpReq, err := http.NewRequestWithContext(ctx, "GET", p.BaseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	if p.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	resp, err := p.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d listing models", p.ProviderName, resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(body.Data))
	for _, m := range body.Data {
		out = append(out, ModelInfo{ID: m.ID})
	}
	return out, nil
}

func (p *OpenAICompat) Ping(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	models, err := p.ListModels(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("ok (%d models)", len(models)), nil
}
