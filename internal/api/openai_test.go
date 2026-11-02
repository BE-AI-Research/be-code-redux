// internal/api/openai_test.go
package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCompletionsNonStreaming(t *testing.T) {
	h := newHarness(t)
	key := h.app(t, "app", Profile{Tiers: []string{"completions"}, Models: []string{"qwen"}})
	body := map[string]any{"model": "qwen", "messages": []any{
		map[string]any{"role": "system", "content": "be brief"},
		map[string]any{"role": "user", "content": "hi"},
	}, "temperature": 0.1, "max_tokens": 50, "ignored_field": 1}
	resp, out := h.do(t, "POST", "/v1/chat/completions", key, "", body, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	if out["object"] != "chat.completion" || out["model"] != "m" {
		t.Fatalf("shape: %v", out)
	}
	ch := out["choices"].([]any)[0].(map[string]any)
	if ch["message"].(map[string]any)["content"] != "hello there" || ch["finish_reason"] != "stop" {
		t.Fatalf("choice: %v", ch)
	}
	if u := out["usage"].(map[string]any); u["prompt_tokens"] != float64(3) || u["total_tokens"] != float64(5) {
		t.Fatalf("usage: %v", u)
	}
	req := h.back.complete[0]
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Model != "qwen" || req.MaxTokens != 50 || req.Temperature == nil || *req.Temperature != 0.1 {
		t.Fatalf("backend request: %+v", req)
	}
}

func TestCompletionsRefusals(t *testing.T) {
	h := newHarness(t)
	key := h.app(t, "app", Profile{Tiers: []string{"completions"}, Budget: Budget{Tokens: 100, PerHour: 0}})
	user := []any{map[string]any{"role": "user", "content": "hi"}}
	for name, tc := range map[string]struct {
		body   map[string]any
		status int
		code   string
	}{
		"tools":          {map[string]any{"messages": user, "tools": []any{map[string]any{}}}, 400, "not_supported"},
		"functions":      {map[string]any{"messages": user, "functions": []any{}}, 400, "not_supported"},
		"n above one":    {map[string]any{"messages": user, "n": 2}, 400, "not_supported"},
		"empty messages": {map[string]any{"messages": []any{}}, 400, "not_supported"},
		"array content":  {map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "x"}}}}}, 400, "not_supported"},
		"tool role":      {map[string]any{"messages": []any{map[string]any{"role": "tool", "content": "x"}}}, 400, "not_supported"},
		"over budget":    {map[string]any{"messages": user, "max_tokens": 101}, 400, "over_budget"},
		"model":          {map[string]any{"messages": user, "model": "gpt-4"}, 403, "model_not_allowed"},
		"default ok":     {map[string]any{"messages": user, "model": "default"}, 200, ""},
	} {
		resp, out := h.do(t, "POST", "/v1/chat/completions", key, "", tc.body, nil)
		if resp.StatusCode != tc.status || errCode(out) != tc.code {
			t.Errorf("%s: want %d %q, got %d %q (%v)", name, tc.status, tc.code, resp.StatusCode, errCode(out), out)
		}
	}
}

func TestCompletionsStreamEndsWithDone(t *testing.T) {
	h := newHarness(t)
	key := h.app(t, "app", Profile{Tiers: []string{"completions"}})
	b, _ := json.Marshal(map[string]any{"stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	req, _ := http.NewRequest("POST", h.ts.URL+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type %q", ct)
	}
	var text, last string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		last = strings.TrimPrefix(line, "data: ")
		if last == "[DONE]" {
			break
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(last), &chunk); err != nil {
			t.Fatalf("chunk %q: %v", last, err)
		}
		if chunk["object"] != "chat.completion.chunk" {
			t.Fatalf("chunk shape: %v", chunk)
		}
		delta := chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
		if s, ok := delta["content"].(string); ok {
			text += s
		}
	}
	if last != "[DONE]" || text != "hello there" {
		t.Fatalf("last=%q text=%q", last, text)
	}
}

// A client that goes away mid-stream cancels the backend call and frees the
// app's in-flight slot.
func TestStreamDisconnectCancelsAndReleases(t *testing.T) {
	h := newHarness(t)
	key := h.app(t, "app", Profile{Tiers: []string{"completions", "query"}})
	h.back.blockComplete = make(chan struct{}) // Complete waits on it or on ctx
	h.back.completeWaiting = make(chan struct{}, 1)
	b, _ := json.Marshal(map[string]any{"stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	req, _ := http.NewRequest("POST", h.ts.URL+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+key)
	go func() {
		if resp, err := h.ts.Client().Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-h.back.completeWaiting: // the backend call is in flight
	case <-time.After(5 * time.Second):
		t.Fatal("the backend call never started")
	}
	h.ts.CloseClientConnections() // the client hangs up mid-stream
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if resp, _ := h.do(t, "POST", "/v1/query", key, "", map[string]any{"prompt": "x"}, nil); resp.StatusCode == 200 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("after the client hung up the app's slot must be free again")
}
