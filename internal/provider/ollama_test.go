package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
		resp, err := p.Chat(context.Background(), ChatRequest{Model: "m"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Content != "ok" || resp.FinishReason != "stop" {
			t.Fatalf("the fallback must answer, not just not fail: %+v", resp)
		}
	}
	if native != 1 || compat != 3 {
		t.Fatalf("native %d, compat %d; the fallback must latch", native, compat)
	}
	if !p.NativeFallback() {
		t.Fatal("a downgraded session must be able to say so")
	}
}

// The critical distinction the status code alone cannot make: a current
// Ollama answers a model it has not pulled with 404 too. A mistyped model
// name must not cost the session its native path — and with it num_ctx,
// the whole point of using /api/chat — for the rest of the run.
func TestModelNotFoundIsNotAnOldServer(t *testing.T) {
	var native, compat int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/chat":
			native++
			http.Error(w, `{"error":"model \"m\" not found, try pulling it first"}`, http.StatusNotFound)
		case "/v1/chat/completions":
			compat++
		}
	}))
	defer srv.Close()
	p := NewOllama("t", srv.URL, "")
	for i := 0; i < 2; i++ {
		_, err := p.Chat(context.Background(), ChatRequest{Model: "m"}, nil)
		if err == nil {
			t.Fatal("a missing model must be reported, not worked around")
		}
		if !strings.Contains(err.Error(), "try pulling it first") {
			t.Fatalf("err = %v", err)
		}
	}
	if native != 2 || compat != 0 {
		t.Fatalf("native %d, compat %d; a missing model is not a missing endpoint", native, compat)
	}
	if p.NativeFallback() {
		t.Fatal("the session must not be downgraded by a typo")
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
			http.Error(w, `{"error":"invalid options: num_ctx must be a positive integer"}`, http.StatusBadRequest)
		case "/v1/chat/completions":
			compat++
		}
	}))
	defer srv.Close()
	p := NewOllama("t", srv.URL, "")
	for i := 0; i < 2; i++ {
		if _, err := p.Chat(context.Background(), ChatRequest{Model: "m"}, nil); err == nil {
			t.Fatal("a failing request must be reported")
		} else if !strings.Contains(err.Error(), "num_ctx") {
			t.Fatalf("err = %v", err)
		}
	}
	if native != 2 || compat != 0 {
		t.Fatalf("native %d, compat %d; a 400 is not an old server", native, compat)
	}
}

// A 400 that happens to name think when the request sent no think key is
// somebody else's error: retrying it identically would only double it.
func TestThinkRetryOnlyWhenAThinkKeyWasSent(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, `{"error":"the think template for this model is broken"}`, http.StatusBadRequest)
	}))
	defer srv.Close()
	p := NewOllama("t", srv.URL, "")
	if _, err := p.Chat(context.Background(), ChatRequest{Model: "m"}, nil); err == nil {
		t.Fatal("want an error")
	}
	if calls != 1 {
		t.Fatalf("calls = %d; nothing was refused, so there is nothing to retry without", calls)
	}
}

// A server that takes think only as a boolean rejects a level. That costs
// the level and nothing else: think:false must keep being sent, or every
// NoThink call (compaction, handoff, init) silently starts reasoning again
// — which is the one thing those calls exist to prevent.
func TestThinkLevelRefusalKeepsNoThinkWorking(t *testing.T) {
	var sent []any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		sent = append(sent, b["think"])
		if _, ok := b["think"].(string); ok {
			http.Error(w, `{"error":"json: cannot unmarshal string into Go struct field ChatRequest.think of type bool"}`, http.StatusBadRequest)
			return
		}
		w.Write([]byte(`{"message":{"content":"x"},"done":true,"done_reason":"stop"}` + "\n"))
	}))
	defer srv.Close()
	p := NewOllama("t", srv.URL, "")
	if _, err := p.Chat(context.Background(), ChatRequest{Model: "m", ReasoningEffort: "low"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Chat(context.Background(), ChatRequest{Model: "m", ReasoningEffort: "low"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Chat(context.Background(), ChatRequest{Model: "m", NoThink: true}, nil); err != nil {
		t.Fatal(err)
	}
	// level (refused) -> retry without it -> no level again -> think:false.
	want := []any{"low", nil, nil, false}
	if len(sent) != len(want) {
		t.Fatalf("think keys sent: %v, want %v", sent, want)
	}
	for i := range want {
		if sent[i] != want[i] {
			t.Fatalf("think keys sent: %v, want %v", sent, want)
		}
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

// TestRequestTemperatureIsNeverOverriddenByConfig: a deliberate 0 is a real
// setting — determinism — and the harness's own calls pick their temperature
// on purpose. Nothing on the provider may fill in over them.
func TestRequestTemperatureIsNeverOverriddenByConfig(t *testing.T) {
	var opts []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		o, _ := b["options"].(map[string]any)
		opts = append(opts, o)
		w.Write([]byte(`{"message":{"content":"ok"},"done":true,"done_reason":"stop"}` + "\n"))
	}))
	defer srv.Close()
	p := NewOllama("t", srv.URL, "")
	p.SetOptions(Options{NumCtx: 8192, Extra: map[string]any{"top_k": 40}})

	for _, want := range []float64{0, 0.1, 0.7} {
		if _, err := p.Chat(context.Background(), ChatRequest{Model: "m", Temperature: want}, nil); err != nil {
			t.Fatal(err)
		}
	}
	for i, want := range []float64{0, 0.1, 0.7} {
		if got := opts[i]["temperature"]; got != want {
			t.Fatalf("request %d asked for %v, sent %v", i, want, got)
		}
	}
	if opts[0]["top_k"] != float64(40) {
		t.Fatalf("passthrough options lost: %v", opts[0])
	}
}

// TestPassthroughOptionsWinOverTheHarness: a config that really does want to
// pin a value per endpoint says so in the options map, which is merged last.
func TestPassthroughOptionsWinOverTheHarness(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		got, _ = b["options"].(map[string]any)
		w.Write([]byte(`{"message":{"content":"ok"},"done":true,"done_reason":"stop"}` + "\n"))
	}))
	defer srv.Close()
	p := NewOllama("t", srv.URL, "")
	p.SetOptions(Options{Extra: map[string]any{"temperature": 0.6}})
	if _, err := p.Chat(context.Background(), ChatRequest{Model: "m", Temperature: 0.2}, nil); err != nil {
		t.Fatal(err)
	}
	if got["temperature"] != 0.6 {
		t.Fatalf("temperature = %v; the passthrough map is merged last", got["temperature"])
	}
}

// TestProxyErrorPageDowngradesTheSession: an nginx or Apache in front of a
// genuinely old Ollama answers /api/chat with an HTML 404. Refusing to
// downgrade on it means the session never works at all — and the body is the
// one thing that distinguishes it from Ollama's own JSON 404.
func TestProxyErrorPageDowngradesTheSession(t *testing.T) {
	for _, body := range []string{
		"<html><head><title>404 Not Found</title></head><body><center><h1>404 Not Found</h1></center><hr><center>nginx/1.24.0</center></body></html>",
		"<!DOCTYPE HTML PUBLIC \"-//IETF//DTD HTML 2.0//EN\">\n<html><head>\n<title>404 Not Found</title>\n</head></html>",
		"Not Found",
		"{}",
	} {
		var native, compat int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/chat":
				native++
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(body))
			case "/v1/chat/completions":
				compat++
				w.Header().Set("Content-Type", "text/event-stream")
				w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
			}
		}))
		p := NewOllama("t", srv.URL, "")
		for i := 0; i < 2; i++ {
			if _, err := p.Chat(context.Background(), ChatRequest{Model: "m"}, nil); err != nil {
				t.Fatalf("%q: %v", body, err)
			}
		}
		if native != 1 || compat != 2 {
			t.Fatalf("%q: native %d, compat %d; the old server must be probed once", body, native, compat)
		}
		if !p.NativeFallback() {
			t.Fatalf("%q: session not downgraded", body)
		}
		srv.Close()
	}
}

// One formatter for both model pickers, so the TUI's list and plain mode's
// /models cannot drift apart. The row has to answer what a person picking a
// model is actually weighing: size, quantization, the window it is loaded
// with, and whether loading it costs anything at all.
func TestModelDetailDescribe(t *testing.T) {
	cases := []struct {
		name string
		d    ModelDetail
		want []string
		not  []string
	}{
		{"resident", ModelDetail{SizeBytes: 17_000_000_000, Family: "qwen3", Quantization: "Q4_K_XL", Window: 32768, Resident: true},
			[]string{"17.0GB", "qwen3", "Q4_K_XL", "32k ctx", "· loaded"}, nil},
		{"on disk only", ModelDetail{SizeBytes: 4_000_000_000, Family: "llama", Quantization: "Q4_0"},
			[]string{"4.0GB", "ctx unknown"}, []string{"loaded"}},
		{"tiny window", ModelDetail{SizeBytes: 1_000_000_000, Family: "x", Quantization: "q", Window: 512},
			[]string{"512 ctx"}, []string{"k ctx"}},
		{"names only", ModelDetail{ID: "gpt-ish", Window: 8192},
			[]string{"8k ctx"}, []string{"0.0GB"}},
	}
	for _, c := range cases {
		got := c.d.Describe()
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q missing %q", c.name, got, w)
			}
		}
		for _, n := range c.not {
			if strings.Contains(got, n) {
				t.Errorf("%s: %q should not contain %q", c.name, got, n)
			}
		}
	}
}

// A backend with no richer listing still fills a picker: ModelDetails falls
// back to ListModels rather than leaving the list empty.
func TestModelDetailsFallsBackToListModels(t *testing.T) {
	p := NewOpenAICompat("x", "http://127.0.0.1:1/v1", "")
	if _, ok := any(p).(ModelDetailer); ok {
		t.Fatal("this test is about a provider that is NOT a detailer")
	}
	// The call itself will fail against a dead endpoint; what matters is
	// that it went to ListModels and reported that error rather than
	// silently returning nothing.
	if _, err := ModelDetails(context.Background(), p); err == nil {
		t.Fatal("a failing listing must be reported, not swallowed")
	}
}

// Warm is /api/generate with an empty prompt, and /api/generate *loads* the
// model when it is not resident — which is exactly the case a post-request
// keep-alive refresh runs into, since an eviction between requests is what
// the refresh exists for. Without options that load comes up at the
// server's default window, outside the loader entirely, which spec §10.1
// says is the only path that loads a model or sends num_ctx.
func TestWarmCarriesTheResolvedWindow(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			http.NotFound(w, r)
			return
		}
		json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	p := NewOllama("t", srv.URL, "")
	p.SetOptions(Options{NumCtx: 32768, Extra: map[string]any{"top_k": 40}})
	if err := p.Warm(context.Background(), "m", 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	opts, _ := body["options"].(map[string]any)
	if opts == nil {
		t.Fatalf("warm sent no options; the model would load at the server's default: %v", body)
	}
	if n, _ := opts["num_ctx"].(float64); int(n) != 32768 {
		t.Fatalf("num_ctx %v", opts["num_ctx"])
	}
	if k, _ := opts["top_k"].(float64); int(k) != 40 {
		t.Fatalf("the passthrough options were dropped: %v", opts)
	}
	if body["keep_alive"] != "30m0s" {
		t.Fatalf("keep_alive %v", body["keep_alive"])
	}
}

// With nothing resolved there is nothing to say, and the key is omitted
// rather than sent as an empty object.
func TestWarmOmitsAnEmptyOptionsBlock(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	p := NewOllama("t", srv.URL, "")
	if err := p.Warm(context.Background(), "m", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["options"]; ok {
		t.Fatalf("sent an options block with nothing in it: %v", body)
	}
}
