package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sse serves a canned SSE stream.
func sse(t *testing.T, lines ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, l := range lines {
			w.Write([]byte("data: " + l + "\n\n"))
		}
		w.Write([]byte("data: [DONE]\n\n"))
	}))
}

func TestStreamTextAccumulation(t *testing.T) {
	srv := sse(t,
		`{"choices":[{"delta":{"role":"assistant","content":"Hel"}}]}`,
		`{"choices":[{"delta":{"content":"lo"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)
	defer srv.Close()
	p := NewOpenAICompat("test", srv.URL, "")
	var streamed strings.Builder
	resp, err := p.Chat(context.Background(), ChatRequest{Model: "m",
		Messages: []Message{{Role: RoleUser, Content: "hi"}}},
		func(d string) { streamed.WriteString(d) })
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "Hello" || streamed.String() != "Hello" {
		t.Fatalf("content=%q streamed=%q", resp.Content, streamed.String())
	}
	if resp.FinishReason != "stop" {
		t.Fatalf("finish=%q", resp.FinishReason)
	}
}

func TestStreamToolCallFragments(t *testing.T) {
	srv := sse(t,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"read_file","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.go\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	defer srv.Close()
	p := NewOpenAICompat("test", srv.URL, "")
	resp, err := p.Chat(context.Background(), ChatRequest{Model: "m",
		Messages: []Message{{Role: RoleUser, Content: "read a.go"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("calls: %+v", resp.ToolCalls)
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_9" || tc.Name != "read_file" || tc.Arguments != `{"path":"a.go"}` {
		t.Fatalf("bad call assembly: %+v", tc)
	}
}

func TestStreamMissingIndexToolCalls(t *testing.T) {
	// llama.cpp-style: no index field, whole call in one chunk.
	srv := sse(t,
		`{"choices":[{"delta":{"tool_calls":[{"id":"a","function":{"name":"shell","arguments":"{\"command\":\"ls\"}"}}]}}]}`,
	)
	defer srv.Close()
	p := NewOpenAICompat("test", srv.URL, "")
	resp, err := p.Chat(context.Background(), ChatRequest{Model: "m",
		Messages: []Message{{Role: RoleUser, Content: "x"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "shell" {
		t.Fatalf("calls: %+v", resp.ToolCalls)
	}
}

func TestHTTPErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"model not found"}}`, 404)
	}))
	defer srv.Close()
	p := NewOpenAICompat("test", srv.URL, "")
	_, err := p.Chat(context.Background(), ChatRequest{Model: "nope",
		Messages: []Message{{Role: RoleUser, Content: "x"}}}, nil)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected surfaced 404, got %v", err)
	}
}

// Reasoning models stream hidden thinking in a separate field (Ollama:
// "reasoning", llama.cpp/vLLM: "reasoning_content"). It must be surfaced so
// the UI can show activity, and never leak into Content.
func TestStreamCapturesReasoningSeparately(t *testing.T) {
	srv := sse(t,
		`{"choices":[{"delta":{"role":"assistant","reasoning":"let me think"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":" more"}}]}`,
		`{"choices":[{"delta":{"content":"42"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)
	defer srv.Close()
	p := NewOpenAICompat("test", srv.URL, "")
	var thought strings.Builder
	resp, err := p.Chat(context.Background(), ChatRequest{Model: "m",
		Messages:    []Message{{Role: RoleUser, Content: "hi"}},
		OnReasoning: func(d string) { thought.WriteString(d) }}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "42" {
		t.Fatalf("content polluted: %q", resp.Content)
	}
	if resp.Reasoning != "let me think more" || thought.String() != "let me think more" {
		t.Fatalf("reasoning=%q streamed=%q", resp.Reasoning, thought.String())
	}
}

// Without stream_options.include_usage most servers omit usage from the
// stream, leaving the harness to guess token counts.
func TestChatRequestsUsageInStream(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 1<<16)
		n, _ := r.Body.Read(b)
		body = string(b[:n])
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`data: {"choices":[{"delta":{"content":"x"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":1}}` + "\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()
	p := NewOpenAICompat("test", srv.URL, "")
	resp, err := p.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: "hi"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"include_usage":true`) {
		t.Fatalf("stream_options.include_usage not requested: %s", body)
	}
	if resp.Usage.PromptTokens != 7 {
		t.Fatalf("usage not parsed: %+v", resp.Usage)
	}
}

// A server that ignores stream:true and answers with one JSON object must
// still yield the content (previously: empty reply, no error).
func TestNonStreamingJSONBodyIsAccepted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"whole reply"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`))
	}))
	defer srv.Close()
	p := NewOpenAICompat("test", srv.URL, "")
	resp, err := p.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: "hi"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "whole reply" || resp.Usage.PromptTokens != 3 {
		t.Fatalf("non-streaming body not parsed: %+v", resp)
	}
}

// An empty body with no SSE data is an error, not a silent empty answer.
func TestEmptyStreamIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
	}))
	defer srv.Close()
	p := NewOpenAICompat("test", srv.URL, "")
	if _, err := p.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: "hi"}}}, nil); err == nil {
		t.Fatal("expected an error for an empty response body")
	}
}

// reasoning_effort travels only when asked for: absent by default, so
// backends that know the field keep their own default.
func TestChatSendsReasoningEffortOnlyWhenSet(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 1<<16)
		n, _ := r.Body.Read(b)
		bodies = append(bodies, string(b[:n]))
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`data: {"choices":[{"delta":{"content":"x"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()
	p := NewOpenAICompat("test", srv.URL, "")
	msgs := []Message{{Role: RoleUser, Content: "hi"}}
	if _, err := p.Chat(context.Background(), ChatRequest{Model: "m", Messages: msgs}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Chat(context.Background(), ChatRequest{Model: "m", Messages: msgs, ReasoningEffort: "low"}, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(bodies[0], "reasoning_effort") {
		t.Fatalf("effort sent although unset: %s", bodies[0])
	}
	if !strings.Contains(bodies[1], `"reasoning_effort":"low"`) {
		t.Fatalf("effort not sent: %s", bodies[1])
	}
}

func TestAnthropicOmitsTemperature(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 1<<16)
		n, _ := r.Body.Read(b)
		bodies = append(bodies, string(b[:n]))
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`data: {"choices":[{"delta":{"content":"x"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()
	msgs := []Message{{Role: RoleUser, Content: "hi"}}
	p := NewOpenAICompat("test", srv.URL, "")
	if _, err := p.Chat(context.Background(), ChatRequest{Model: "m", Messages: msgs, Temperature: 0.2}, nil); err != nil {
		t.Fatal(err)
	}
	p.Anthropic = true
	if _, err := p.Chat(context.Background(), ChatRequest{Model: "m", Messages: msgs, Temperature: 0.2}, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bodies[0], `"temperature":0.2`) {
		t.Fatalf("temperature missing for a plain server: %s", bodies[0])
	}
	if strings.Contains(bodies[1], "temperature") || strings.Contains(bodies[1], "top_p") {
		t.Fatalf("temperature sent to Anthropic: %s", bodies[1])
	}
}

func TestStreamKeepsGeminiThoughtSignatureAndEchoesIt(t *testing.T) {
	const sig = `{"google":{"thought_signature":"EvEFCu4F"}}`
	srv := sse(t,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"list_dir","arguments":""},"extra_content":`+sig+`}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	defer srv.Close()
	p := NewOpenAICompat("gemini", srv.URL, "")
	resp, err := p.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: "ls"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || string(resp.ToolCalls[0].Extra) != sig {
		t.Fatalf("signature not kept: %+v", resp.ToolCalls)
	}
	// Saved with the session and back: the field survives a JSON round trip.
	b, _ := json.Marshal(Message{Role: RoleAssistant, ToolCalls: resp.ToolCalls})
	var back Message
	if err := json.Unmarshal(b, &back); err != nil || string(back.ToolCalls[0].Extra) != sig {
		t.Fatalf("round trip: %s %v", b, err)
	}
	// Replayed, the assistant message carries extra_content on the call.
	var body string
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<16)
		n, _ := r.Body.Read(buf)
		body = string(buf[:n])
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"))
	}))
	defer echo.Close()
	p2 := NewOpenAICompat("gemini", echo.URL, "")
	_, err = p2.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{
		{Role: RoleUser, Content: "ls"},
		back,
		{Role: RoleTool, ToolCallID: "call_1", Content: "a.go"},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"extra_content":`+sig) {
		t.Fatalf("signature not echoed: %s", body)
	}
	// A call with no signature sends no extra_content at all.
	if strings.Count(body, "extra_content") != 1 {
		t.Fatalf("extra_content count: %s", body)
	}
}

func TestNonStreamingKeepsThoughtSignature(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"},"extra_content":{"google":{"thought_signature":"abc"}}}]},"finish_reason":"tool_calls"}]}`))
	}))
	defer srv.Close()
	resp, err := NewOpenAICompat("gemini", srv.URL, "").Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: "x"}}}, nil)
	if err != nil || len(resp.ToolCalls) != 1 || !strings.Contains(string(resp.ToolCalls[0].Extra), "abc") {
		t.Fatalf("%+v %v", resp, err)
	}
}
