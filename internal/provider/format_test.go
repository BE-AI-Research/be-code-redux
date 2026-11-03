package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

const testSchema = `{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`

func nativeBodies(t *testing.T, reqs ...ChatRequest) []map[string]any {
	t.Helper()
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		w.Write([]byte(`{"message":{"content":"x"},"done":true,"done_reason":"stop"}` + "\n"))
	}))
	defer srv.Close()
	p := NewOllama("t", srv.URL, "")
	for _, req := range reqs {
		if _, err := p.Chat(context.Background(), req, nil); err != nil {
			t.Fatal(err)
		}
	}
	return bodies
}

func openAIBodies(t *testing.T, reqs ...ChatRequest) []map[string]any {
	t.Helper()
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(`data: {"choices":[{"delta":{"content":"x"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()
	p := NewOpenAICompat("t", srv.URL, "")
	for _, req := range reqs {
		if _, err := p.Chat(context.Background(), req, nil); err != nil {
			t.Fatal(err)
		}
	}
	return bodies
}

func canon(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var x any
	_ = json.Unmarshal(b, &x)
	b, _ = json.Marshal(x)
	return string(b)
}

// Native Ollama: a schema goes out as "format"; nothing asked, no key.
func TestNativeCarriesFormat(t *testing.T) {
	b := nativeBodies(t,
		ChatRequest{Model: "m", Format: json.RawMessage(testSchema)},
		ChatRequest{Model: "m"},
		ChatRequest{Model: "m", ResponseFormat: json.RawMessage(`{"type":"json_schema","json_schema":{"name":"x","schema":` + testSchema + `}}`)},
		ChatRequest{Model: "m", ResponseFormat: json.RawMessage(`{"type":"json_object"}`)},
		ChatRequest{Model: "m", ResponseFormat: json.RawMessage(`{"type":"text"}`)},
	)
	if got := canon(t, b[0]["format"]); got != canon(t, json.RawMessage(testSchema)) {
		t.Fatalf("format: %s", got)
	}
	if _, ok := b[1]["format"]; ok {
		t.Fatalf("format must be omitted when nil: %v", b[1]["format"])
	}
	if got := canon(t, b[2]["format"]); got != canon(t, json.RawMessage(testSchema)) {
		t.Fatalf("json_schema response_format → its schema: %s", got)
	}
	if b[3]["format"] != "json" {
		t.Fatalf("json_object response_format → \"json\": %v", b[3]["format"])
	}
	if _, ok := b[4]["format"]; ok {
		t.Fatalf("a text response_format asks for nothing: %v", b[4]["format"])
	}
}

// OpenAI: a schema is wrapped as response_format json_schema; a caller's
// response_format goes out unchanged; nothing asked, no key.
func TestOpenAICarriesResponseFormat(t *testing.T) {
	rf := `{"type":"json_schema","json_schema":{"name":"mine","strict":true,"schema":` + testSchema + `}}`
	b := openAIBodies(t,
		ChatRequest{Model: "m", Format: json.RawMessage(testSchema)},
		ChatRequest{Model: "m"},
		ChatRequest{Model: "m", ResponseFormat: json.RawMessage(rf), Format: json.RawMessage(`{"type":"object"}`)},
	)
	want := `{"type":"json_schema","json_schema":{"name":"response","schema":` + testSchema + `}}`
	if got := canon(t, b[0]["response_format"]); got != canon(t, json.RawMessage(want)) {
		t.Fatalf("response_format: %s", got)
	}
	if _, ok := b[1]["response_format"]; ok {
		t.Fatalf("response_format must be omitted when nil: %v", b[1]["response_format"])
	}
	if got := canon(t, b[2]["response_format"]); got != canon(t, json.RawMessage(rf)) {
		t.Fatalf("a caller's response_format passes unchanged: %s", got)
	}
}
