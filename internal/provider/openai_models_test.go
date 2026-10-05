package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListModelsParsesOpenRouterShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[
			{"id":"anthropic/claude-x","context_length":200000,"pricing":{"prompt":"0.000003","completion":"0.000015"}},
			{"id":"bare/model"},
			{"id":"num/model","context_length":8192,"pricing":{"prompt":0.000001,"completion":2e-6}}
		]}`))
	}))
	defer srv.Close()
	p := NewOpenAICompat("openrouter", srv.URL, "")
	ms, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 3 {
		t.Fatalf("got %d models", len(ms))
	}
	a := ms[0]
	if a.ID != "anthropic/claude-x" || a.ContextLength != 200000 || a.PromptPrice != 0.000003 || a.CompletionPrice != 0.000015 {
		t.Fatalf("first = %+v", a)
	}
	b := ms[1]
	if b.ID != "bare/model" || b.ContextLength != 0 || b.PromptPrice != 0 || b.CompletionPrice != 0 {
		t.Fatalf("second = %+v", b)
	}
	c := ms[2]
	if c.ContextLength != 8192 || c.PromptPrice != 0.000001 || c.CompletionPrice != 0.000002 {
		t.Fatalf("third = %+v", c)
	}
}
