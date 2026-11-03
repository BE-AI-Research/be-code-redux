package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPErrorKeepsText(t *testing.T) {
	e := &HTTPError{Provider: "openrouter", Code: 429, Body: "slow down", RetryAfter: 3 * time.Second}
	if got, want := e.Error(), "openrouter: HTTP 429: slow down"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

func TestRetryAfterParsesDateAndClamps(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	if d := ParseRetryAfter("12", now); d != 12*time.Second {
		t.Fatalf("seconds: %v", d)
	}
	if d := ParseRetryAfter(" 12 ", now); d != 12*time.Second {
		t.Fatalf("padded seconds: %v", d)
	}
	date := now.Add(20 * time.Second).Format(http.TimeFormat)
	if d := ParseRetryAfter(date, now); d < 19*time.Second || d > 21*time.Second {
		t.Fatalf("http-date: %v", d)
	}
	past := now.Add(-time.Minute).Format(http.TimeFormat)
	if d := ParseRetryAfter(past, now); d != 0 {
		t.Fatalf("past date: %v", d)
	}
	for _, h := range []string{"-5", "", "soon", "1.5x"} {
		if d := ParseRetryAfter(h, now); d != 0 {
			t.Fatalf("%q: %v, want 0", h, d)
		}
	}
	// Absurdly large values are not an error here; the agent clamps them.
	if d := ParseRetryAfter("99999999999", now); d <= 0 {
		t.Fatalf("huge: %v", d)
	}
}

func TestOpenAIErrorCarriesStatusAndRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer srv.Close()
	p := NewOpenAICompat("openrouter", srv.URL, "k")
	_, err := p.Chat(context.Background(), ChatRequest{Model: "m"}, nil)
	var he *HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("err %T %v is not *HTTPError", err, err)
	}
	if he.Code != 429 || he.RetryAfter != 7*time.Second || he.Provider != "openrouter" {
		t.Fatalf("got %+v", he)
	}
	if he.Error() != `openrouter: HTTP 429: {"error":"rate limited"}` {
		t.Fatalf("text = %q", he.Error())
	}
}

func TestHTTPErrorProviderMessage(t *testing.T) {
	cases := map[string]string{
		`listing models: {"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`:                                                      "authentication_error: invalid x-api-key",
		`{"error":{"message":"Incorrect API key provided: sk-proj-abc***xyz. You can find your key at https://platform.openai.com.","type":"invalid_request_error"}}`: "invalid_request_error: Incorrect API key provided: [redacted]. You can find your key at https://platform.openai.com.",
		`{"error":"plain string error"}`: "plain string error",
		`<html>nope</html>`:              "",
		``:                               "",
	}
	for body, want := range cases {
		he := &HTTPError{Provider: "p", Code: 401, Body: body}
		if got := he.ProviderMessage(); got != want {
			t.Errorf("%q:\n got %q\nwant %q", body, got, want)
		}
	}
	long := &HTTPError{Code: 400, Body: `{"error":{"message":"` + strings.Repeat("x\x1b[31m", 100) + `"}}`}
	got := long.ProviderMessage()
	if strings.ContainsRune(got, '\x1b') || len(got) > 163 {
		t.Fatalf("control chars or length: %q", got)
	}
}

func TestListModelsReadsAnthropicsWindow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"claude-x","max_input_tokens":200000,"display_name":"Claude X"},{"id":"or-y","context_length":131072}]}`))
	}))
	defer srv.Close()
	ms, err := NewOpenAICompat("t", srv.URL+"/v1", "k").ListModels(context.Background())
	if err != nil || len(ms) != 2 || ms[0].ContextLength != 200000 || ms[1].ContextLength != 131072 {
		t.Fatalf("%v %v", ms, err)
	}
}

func TestExtraHeadersRideEveryRequest(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path+" "+r.Header.Get("anthropic-workspace-id")+" "+r.Header.Get("Authorization"))
		if r.URL.Path == "/v1/models" {
			w.Write([]byte(`{"data":[{"id":"m"}]}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer srv.Close()
	p := NewOpenAICompat("t", srv.URL+"/v1", "k")
	p.Headers = map[string]string{"anthropic-workspace-id": "wrkspc_1"}
	if _, err := p.ListModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: "x"}}}, nil); err != nil {
		t.Fatal(err)
	}
	for _, g := range got {
		if !strings.Contains(g, "wrkspc_1 Bearer k") {
			t.Fatalf("header missing on %q", g)
		}
	}
	if len(got) != 2 {
		t.Fatalf("requests: %v", got)
	}
}

func TestAnthropicHostUsesItsOwnHeaders(t *testing.T) {
	if !IsAnthropicHost("https://api.anthropic.com/v1") || IsAnthropicHost("https://openrouter.ai/api/v1") || IsAnthropicHost("http://127.0.0.1:11434/v1") {
		t.Fatal("host rule")
	}
	if !NewOpenAICompat("anthropic", "https://api.anthropic.com/v1", "k").Anthropic || NewOpenAICompat("or", "https://openrouter.ai/api/v1", "k").Anthropic {
		t.Fatal("constructor flag")
	}
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Write([]byte(`{"data":[{"id":"m"}]}`))
	}))
	defer srv.Close()
	p := NewOpenAICompat("anthropic", srv.URL+"/v1", "sk-ant-k")
	p.Anthropic = true
	if _, err := p.ListModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got.Get("x-api-key") != "sk-ant-k" || got.Get("anthropic-version") != AnthropicVersion || got.Get("Authorization") != "" {
		t.Fatalf("headers: %v", got)
	}
}
