package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func ollamaStub(t *testing.T, ps, show string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ps":
			w.Write([]byte(ps))
		case "/api/show":
			w.Write([]byte(show))
		default:
			http.NotFound(w, r)
		}
	}))
}

// A loaded model reports its live window in /api/ps.
func TestOllamaContextLengthFromPs(t *testing.T) {
	srv := ollamaStub(t, `{"models":[{"name":"qwen3:8b","context_length":8192}]}`, `{}`)
	defer srv.Close()
	p := NewOllama("ollama", srv.URL+"/v1", "")
	n, err := p.ContextLength(context.Background(), "qwen3:8b")
	if err != nil || n != 8192 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

// An unloaded model with a Modelfile num_ctx reports that via /api/show.
func TestOllamaContextLengthFromShowParameters(t *testing.T) {
	srv := ollamaStub(t, `{"models":[]}`, `{"parameters":"stop \"<|im_end|>\"\nnum_ctx 16384\n"}`)
	defer srv.Close()
	p := NewOllama("ollama", srv.URL+"/v1", "")
	n, err := p.ContextLength(context.Background(), "qwen3:8b")
	if err != nil || n != 16384 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

// Unknown is reported as 0 without error so callers can fall back.
func TestOllamaContextLengthUnknown(t *testing.T) {
	srv := ollamaStub(t, `{"models":[]}`, `{"parameters":"stop \"x\"\n"}`)
	defer srv.Close()
	p := NewOllama("ollama", srv.URL+"/v1", "")
	n, err := p.ContextLength(context.Background(), "qwen3:8b")
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

// Auxiliary calls (compaction, handoff) ask for NoThink. Ollama can honor
// that only through its native /api/chat, so the Ollama provider must route
// such requests there with think=false and map the reply back.
func TestOllamaNoThinkUsesNativeChat(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b := make([]byte, 1<<16)
		n, _ := r.Body.Read(b)
		gotBody = string(b[:n])
		w.Write([]byte(`{"model":"m","message":{"role":"assistant","content":"briefing text"},"done":true,"prompt_eval_count":40,"eval_count":5}`))
	}))
	defer srv.Close()
	p := NewOllama("ollama", srv.URL+"/v1", "")
	resp, err := p.Chat(context.Background(), ChatRequest{Model: "m", NoThink: true,
		Messages: []Message{{Role: RoleSystem, Content: "sys"}, {Role: RoleUser, Content: "hi"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/chat" {
		t.Fatalf("path = %s, want /api/chat", gotPath)
	}
	if !strings.Contains(gotBody, `"think":false`) {
		t.Fatalf("think=false not sent: %s", gotBody)
	}
	if resp.Content != "briefing text" || resp.Usage.PromptTokens != 40 || resp.Usage.CompletionTokens != 5 {
		t.Fatalf("reply not mapped: %+v", resp)
	}
}

// Native /api/chat is now the normal path for every request, not only the
// NoThink ones: it is the only way to send num_ctx. (This test was
// TestOllamaDefaultStaysOnOpenAIPath and asserted the opposite; the OpenAI
// path survives only as the fallback for servers without /api/chat.)
func TestOllamaDefaultUsesNativeChat(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte(`{"message":{"content":"x"},"done":true,"done_reason":"stop"}` + "\n"))
	}))
	defer srv.Close()
	p := NewOllama("ollama", srv.URL+"/v1", "")
	if _, err := p.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: "hi"}}}, nil); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/chat" {
		t.Fatalf("path = %s", gotPath)
	}
}

// TestNativeChatStreamsAndCarriesOptions: the normal path is native, the
// window we were told to use is on the wire, and deltas arrive as they
// stream rather than in one lump at the end.
func TestNativeChatStreamsAndCarriesOptions(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		fl, _ := w.(http.Flusher)
		for _, line := range []string{
			`{"message":{"content":"hel"},"done":false}`,
			`{"message":{"content":"lo"},"done":false}`,
			`{"message":{"content":""},"done":true,"done_reason":"stop","prompt_eval_count":11,"eval_count":2}`,
		} {
			w.Write([]byte(line + "\n"))
			fl.Flush()
		}
	}))
	defer srv.Close()
	p := NewOllama("t", srv.URL, "")
	p.SetOptions(Options{NumCtx: 32768})
	var deltas []string
	resp, err := p.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: "hi"}}},
		func(d string) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "hello" || resp.FinishReason != "stop" || resp.Usage.PromptTokens != 11 {
		t.Fatalf("resp: %+v", resp)
	}
	if len(deltas) < 2 {
		t.Fatalf("not streamed: %v", deltas)
	}
	opts, _ := gotBody["options"].(map[string]any)
	if opts["num_ctx"] != float64(32768) {
		t.Fatalf("num_ctx not sent: %v", gotBody["options"])
	}
	if gotBody["stream"] != true {
		t.Fatal("stream not requested")
	}
}

// TestNativeToolCallsDecodeObjectArguments: Ollama sends arguments as an
// object; the agent expects a JSON string. This is the conversion that
// breaks tool calling silently if it is wrong.
func TestNativeToolCallsDecodeObjectArguments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"message":{"tool_calls":[{"function":{"name":"read_file","arguments":{"path":"a.go"}}}]},"done":true,"done_reason":"stop"}` + "\n"))
	}))
	defer srv.Close()
	p := NewOllama("t", srv.URL, "")
	resp, err := p.Chat(context.Background(), ChatRequest{Model: "m",
		Tools: []ToolSpec{{Name: "read_file", Parameters: json.RawMessage(`{"type":"object"}`)}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "read_file" {
		t.Fatalf("tool calls: %+v", resp.ToolCalls)
	}
	if resp.ToolCalls[0].Arguments != `{"path":"a.go"}` {
		t.Fatalf("arguments: %q", resp.ToolCalls[0].Arguments)
	}
	if resp.ToolCalls[0].ID == "" {
		t.Fatal("a tool call with no id cannot be paired with its result")
	}
}

// TestFallsBackToOpenAIOnceWhenNativeIsUnsupported: an older server must
// degrade, not fail, and must not be re-probed on every request.
func TestFallsBackToOpenAIOnceWhenNativeIsUnsupported(t *testing.T) {
	var native, compat int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/chat":
			native++
			http.Error(w, "404 page not found", http.StatusNotFound)
		case "/v1/chat/completions":
			compat++
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
		}
	}))
	defer srv.Close()
	p := NewOllama("t", srv.URL, "")
	for i := 0; i < 3; i++ {
		if _, err := p.Chat(context.Background(), ChatRequest{Model: "m"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if native != 1 || compat != 3 {
		t.Fatalf("native %d, compat %d; the fallback must latch", native, compat)
	}
}

// The OpenAI path expressed the thinking budget as reasoning_effort; the
// native path must carry it as Ollama's own think level, or a thinking
// model loses the harness's control of how long it deliberates. A bare
// think:true is never sent.
func TestNativeCarriesReasoningEffortAsThink(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		w.Write([]byte(`{"message":{"content":"x"},"done":true,"done_reason":"stop"}` + "\n"))
	}))
	defer srv.Close()
	p := NewOllama("t", srv.URL, "")
	for _, req := range []ChatRequest{
		{Model: "m", ReasoningEffort: "low"},
		{Model: "m", NoThink: true, ReasoningEffort: "low"},
		{Model: "m"},
	} {
		if _, err := p.Chat(context.Background(), req, nil); err != nil {
			t.Fatal(err)
		}
	}
	if bodies[0]["think"] != "low" {
		t.Fatalf("effort not carried: %v", bodies[0]["think"])
	}
	if bodies[1]["think"] != false {
		t.Fatalf("NoThink must win: %v", bodies[1]["think"])
	}
	if _, ok := bodies[2]["think"]; ok {
		t.Fatalf("think must be omitted when nothing asked for it: %v", bodies[2]["think"])
	}
}

// A model with no reasoning at all makes Ollama reject the think key. That
// costs the key for that model, not the native path, and is not re-probed.
func TestNativeDropsThinkWhenTheModelCannotThink(t *testing.T) {
	var withThink, without int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		if _, ok := b["think"]; ok {
			withThink++
			http.Error(w, `{"error":"m does not support thinking"}`, http.StatusBadRequest)
			return
		}
		without++
		w.Write([]byte(`{"message":{"content":"x"},"done":true,"done_reason":"stop"}` + "\n"))
	}))
	defer srv.Close()
	p := NewOllama("t", srv.URL, "")
	for i := 0; i < 3; i++ {
		resp, err := p.Chat(context.Background(), ChatRequest{Model: "m", NoThink: true}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Content != "x" {
			t.Fatalf("resp: %+v", resp)
		}
	}
	if withThink != 1 || without != 3 {
		t.Fatalf("with %d, without %d; the refusal must latch per model", withThink, without)
	}
}

// An ordinary 400 is the request's own fault and must be reported, not
// turned into a session-long downgrade to the OpenAI path.
func TestNativeBadRequestDoesNotDowngradeTheSession(t *testing.T) {
	var native, compat int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/chat":
			native++
			http.Error(w, `{"error":"model \"m\" not found"}`, http.StatusBadRequest)
		case "/v1/chat/completions":
			compat++
		}
	}))
	defer srv.Close()
	p := NewOllama("t", srv.URL, "")
	for i := 0; i < 2; i++ {
		if _, err := p.Chat(context.Background(), ChatRequest{Model: "m"}, nil); err == nil {
			t.Fatal("a failing request must be reported")
		} else if !strings.Contains(err.Error(), "not found") {
			t.Fatalf("err = %v", err)
		}
	}
	if native != 2 || compat != 0 {
		t.Fatalf("native %d, compat %d; a 400 is not an old server", native, compat)
	}
}

// TestDetailsMergeTagsAndResident: /models needs size, quantization and
// whether the model is already loaded.
func TestDetailsMergeTagsAndResident(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			w.Write([]byte(`{"models":[{"name":"m","size":4700000000,"details":{"family":"qwen3","quantization_level":"Q4_K_M"}}]}`))
		case "/api/ps":
			w.Write([]byte(`{"models":[{"name":"m","model":"m","context_length":32768}]}`))
		}
	}))
	defer srv.Close()
	d, err := NewOllama("t", srv.URL, "").Details(context.Background())
	if err != nil || len(d) != 1 {
		t.Fatalf("details %+v err %v", d, err)
	}
	if !d[0].Resident || d[0].Window != 32768 || d[0].Quantization != "Q4_K_M" {
		t.Fatalf("row: %+v", d[0])
	}
}
