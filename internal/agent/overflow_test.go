package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
)

// The error as the owner's Ollama 0.34 returned it on 2026-09-20, verbatim.
const overflowBody = `ollama: HTTP 400: {"error":"{\"error\":{\"code\":400,\"message\":\"request (11320 tokens) exceeds the available context size (8192 tokens), try increasing it\",\"type\":\"exceed_context_size_error\",\"n_prompt_tokens\":11320,\"n_ctx\":8192}}"}`

func TestContextOverflowIsRecognised(t *testing.T) {
	prompt, window, ok := contextOverflow(errors.New(overflowBody))
	if !ok || prompt != 11320 || window != 8192 {
		t.Fatalf("got prompt=%d window=%d ok=%v", prompt, window, ok)
	}
	for _, s := range []string{
		`HTTP 400: {"error":{"code":"context_length_exceeded","message":"This model's maximum context length is 8192 tokens"}}`,
		`the request exceeds the available context size`,
	} {
		if _, _, ok := contextOverflow(errors.New(s)); !ok {
			t.Errorf("not recognised: %s", s)
		}
	}
	for _, s := range []string{`HTTP 400: {"error":"model not found"}`, `connection refused`} {
		if _, _, ok := contextOverflow(errors.New(s)); ok {
			t.Errorf("wrongly recognised: %s", s)
		}
	}
	if _, _, ok := contextOverflow(nil); ok {
		t.Error("nil is not an overflow")
	}
}

// The server says the model is smaller than we believed. The loader is asked
// again — it may hold a consent that never reached the wire, or find the model
// free to load at our size — and the request is retried once at its answer.
func TestOverflowReResolvesTheWindowAndRetriesOnce(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { c.ContextTokens = 0 })
	l := &fakeLoader{window: 32768}
	ag.SetLoader(l)
	ag.ApplyWindow(8192)
	var calls atomic.Int64
	ag.Provider = &funcProvider{fn: func(provider.ChatRequest) (*provider.ChatResponse, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New(overflowBody)
		}
		return &provider.ChatResponse{Content: "done"}, nil
	}}
	notices := collectNotices(ag)

	out, err := ag.Run(context.Background(), "hello")
	if err != nil || out != "done" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected one retry, saw %d calls", calls.Load())
	}
	if ag.Window() != 32768 || len(l.appliedModels()) != 1 {
		t.Fatalf("window %d, loader applied %v", ag.Window(), l.appliedModels())
	}
	if !strings.Contains(strings.Join(*notices, "\n"), "8192-token window") {
		t.Fatalf("the rejection was never explained: %q", *notices)
	}
}

// Nothing can be done: the loader keeps the server's window and the fixed part
// of the prompt is what does not fit. One explained error, no retry loop, and
// not the server's raw JSON.
func TestOverflowThatCannotBeFixedIsExplained(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { c.ContextTokens = 0 })
	ag.SetLoader(&fakeLoader{window: 8192})
	ag.ApplyWindow(8192)
	var calls atomic.Int64
	ag.Provider = &funcProvider{fn: func(provider.ChatRequest) (*provider.ChatResponse, error) {
		calls.Add(1)
		return nil, errors.New(overflowBody)
	}}

	_, err := ag.Run(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, want := range []string{"11320", "8192-token window", "ollama stop", "/clear"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not mention %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, `\"n_ctx\"`) {
		t.Errorf("raw server JSON leaked into the message: %s", msg)
	}
	if calls.Load() > 2 {
		t.Fatalf("retried %d times", calls.Load())
	}
}
