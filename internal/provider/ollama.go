package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Ollama wraps an OpenAICompat chat client with native Ollama management
// calls (list, pull, running models, keep-alive) against the /api endpoints.
type Ollama struct {
	*OpenAICompat
	APIBase string // e.g. http://localhost:11434
}

// NewOllama derives the native API base from the OpenAI-compatible base URL
// (http://host:11434/v1 -> http://host:11434).
func NewOllama(name, baseURL, apiKey string) *Ollama {
	api := strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
	return &Ollama{
		OpenAICompat: NewOpenAICompat(name, baseURL, apiKey),
		APIBase:      api,
	}
}

// ListModels uses the native endpoint to get size/family/quant detail.
func (p *Ollama) ListModels(ctx context.Context) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", p.APIBase+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama: HTTP %d listing models", resp.StatusCode)
	}
	var body struct {
		Models []struct {
			Name    string `json:"name"`
			Size    int64  `json:"size"`
			Details struct {
				Family            string `json:"family"`
				QuantizationLevel string `json:"quantization_level"`
			} `json:"details"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(body.Models))
	for _, m := range body.Models {
		out = append(out, ModelInfo{
			ID: m.Name, SizeBytes: m.Size,
			Family: m.Details.Family, Quantization: m.Details.QuantizationLevel,
		})
	}
	return out, nil
}

// Pull downloads a model, reporting progress lines via onProgress.
func (p *Ollama) Pull(ctx context.Context, model string, onProgress func(string)) error {
	body, _ := json.Marshal(map[string]any{"model": model, "stream": true})
	req, err := http.NewRequestWithContext(ctx, "POST", p.APIBase+"/api/pull", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama: HTTP %d pulling %s", resp.StatusCode, model)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	last := ""
	for sc.Scan() {
		var ev struct {
			Status    string `json:"status"`
			Error     string `json:"error"`
			Completed int64  `json:"completed"`
			Total     int64  `json:"total"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if ev.Error != "" {
			return fmt.Errorf("ollama: %s", ev.Error)
		}
		line := ev.Status
		if ev.Total > 0 {
			line = fmt.Sprintf("%s %d%%", ev.Status, ev.Completed*100/ev.Total)
		}
		if line != last && onProgress != nil {
			onProgress(line)
			last = line
		}
	}
	return sc.Err()
}

// Running lists models currently loaded in memory.
func (p *Ollama) Running(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", p.APIBase+"/api/ps", nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var body struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(body.Models))
	for _, m := range body.Models {
		out = append(out, m.Name)
	}
	return out, nil
}

// Chat routes NoThink requests through Ollama's native /api/chat, the only
// endpoint that can disable a thinking model's reasoning per request; all
// other requests use the OpenAI-compatible streaming path.
func (p *Ollama) Chat(ctx context.Context, req ChatRequest, onDelta StreamFunc) (*ChatResponse, error) {
	if !req.NoThink || len(req.Tools) > 0 {
		return p.OpenAICompat.Chat(ctx, req, onDelta)
	}
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	msgs := make([]msg, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, msg{Role: string(m.Role), Content: m.Content})
	}
	opts := map[string]any{"temperature": req.Temperature}
	if req.MaxTokens > 0 {
		opts["num_predict"] = req.MaxTokens
	}
	body, err := json.Marshal(map[string]any{
		"model": req.Model, "messages": msgs, "think": false, "stream": false, "options": opts,
	})
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.APIBase+"/api/chat", bytes.NewReader(body))
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
	var out struct {
		Message struct {
			Content  string `json:"content"`
			Thinking string `json:"thinking"`
		} `json:"message"`
		DoneReason      string `json:"done_reason"`
		PromptEvalCount int    `json:"prompt_eval_count"`
		EvalCount       int    `json:"eval_count"`
		Error           string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("%s: decode: %w", p.ProviderName, err)
	}
	if out.Error != "" {
		return nil, fmt.Errorf("%s: %s", p.ProviderName, out.Error)
	}
	if onDelta != nil && out.Message.Content != "" {
		onDelta(out.Message.Content)
	}
	fin := out.DoneReason
	if fin == "" {
		fin = "stop"
	}
	return &ChatResponse{
		Content:      out.Message.Content,
		Reasoning:    out.Message.Thinking,
		FinishReason: fin,
		Usage:        Usage{PromptTokens: out.PromptEvalCount, CompletionTokens: out.EvalCount},
	}, nil
}

// Status reports the live window and residency of model: loaded=true with
// the /api/ps window when resident, else loaded=false with the Modelfile
// num_ctx (0 if unknown).
func (p *Ollama) Status(ctx context.Context, model string) (int, bool, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", p.APIBase+"/api/ps", nil)
	if err != nil {
		return 0, false, err
	}
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return 0, false, err
	}
	var ps struct {
		Models []struct {
			Name          string `json:"name"`
			Model         string `json:"model"`
			ContextLength int    `json:"context_length"`
		} `json:"models"`
	}
	err = json.NewDecoder(resp.Body).Decode(&ps)
	resp.Body.Close()
	if err == nil {
		for _, m := range ps.Models {
			if m.Name == model || m.Model == model {
				return m.ContextLength, true, nil
			}
		}
	}
	n, err := p.ContextLength(ctx, model)
	return n, false, err
}

// KeepAlive extends the model's residency (see Warm).
func (p *Ollama) KeepAlive(ctx context.Context, model string, d time.Duration) error {
	return p.Warm(ctx, model, d)
}

// ContextLength reports the context window the server will actually use for
// model: the live value from /api/ps when loaded, else a Modelfile num_ctx
// from /api/show. 0 means unknown (Ollama's own default applies).
func (p *Ollama) ContextLength(ctx context.Context, model string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", p.APIBase+"/api/ps", nil)
	if err != nil {
		return 0, err
	}
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	var ps struct {
		Models []struct {
			Name          string `json:"name"`
			Model         string `json:"model"`
			ContextLength int    `json:"context_length"`
		} `json:"models"`
	}
	err = json.NewDecoder(resp.Body).Decode(&ps)
	resp.Body.Close()
	if err == nil {
		for _, m := range ps.Models {
			if (m.Name == model || m.Model == model) && m.ContextLength > 0 {
				return m.ContextLength, nil
			}
		}
	}
	body, _ := json.Marshal(map[string]any{"name": model, "model": model})
	req, err = http.NewRequestWithContext(ctx, "POST", p.APIBase+"/api/show", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err = p.HTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var show struct {
		Parameters string `json:"parameters"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&show); err != nil {
		return 0, nil
	}
	for _, line := range strings.Split(show.Parameters, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "num_ctx" {
			if n, err := strconv.Atoi(f[1]); err == nil {
				return n, nil
			}
		}
	}
	return 0, nil
}

// Warm loads a model into memory with an extended keep-alive so the first
// real request doesn't pay the load cost.
func (p *Ollama) Warm(ctx context.Context, model string, keepAlive time.Duration) error {
	body, _ := json.Marshal(map[string]any{
		"model": model, "keep_alive": keepAlive.String(),
	})
	req, err := http.NewRequestWithContext(ctx, "POST", p.APIBase+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama: HTTP %d warming %s", resp.StatusCode, model)
	}
	return nil
}

func (p *Ollama) Ping(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	models, err := p.ListModels(ctx)
	if err != nil {
		return "", err
	}
	running, _ := p.Running(ctx)
	return fmt.Sprintf("ok (%d models, %d loaded)", len(models), len(running)), nil
}
