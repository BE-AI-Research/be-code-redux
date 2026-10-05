package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
