package provider

import (
	"context"
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

// Without NoThink the Ollama provider keeps using the OpenAI-compatible
// path (tool calling, streaming) untouched.
func TestOllamaDefaultStaysOnOpenAIPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()
	p := NewOllama("ollama", srv.URL+"/v1", "")
	if _, err := p.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: "hi"}}}, nil); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %s", gotPath)
	}
}
