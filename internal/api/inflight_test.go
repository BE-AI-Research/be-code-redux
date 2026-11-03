package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gateBackend holds every Query until release is closed, says so on
// entered, and panics on the prompt "panic". Safe for concurrent calls.
type gateBackend struct {
	fakeBackend
	entered chan string
	release chan struct{}
}

func (g *gateBackend) Query(ctx context.Context, q QueryRequest) (QueryResponse, error) {
	if q.Prompt == "panic" {
		panic("handler blew up")
	}
	g.entered <- q.Prompt
	select {
	case <-g.release:
	case <-ctx.Done():
		return QueryResponse{}, ctx.Err()
	}
	return QueryResponse{Text: "ok", Model: "m"}, nil
}

func newGateServer(t *testing.T) (*Server, *httptest.Server, *Registry, *gateBackend) {
	t.Helper()
	g := &gateBackend{entered: make(chan string, 4), release: make(chan struct{})}
	reg := NewRegistry(filepath.Join(t.TempDir(), "apps"))
	srv := New(Options{Backend: g, Apps: reg, Timeout: 5 * time.Second, Sleep: func(time.Duration) {}})
	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.Config.ErrorLog = log.New(io.Discard, "", 0) // the panic test's stack
	ts.Start()
	t.Cleanup(ts.Close)
	return srv, ts, reg, g
}

func postQuery(ts *httptest.Server, key, prompt string) (int, error) {
	b, _ := json.Marshal(map[string]any{"prompt": prompt})
	req, _ := http.NewRequest("POST", ts.URL+"/v1/query", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := ts.Client().Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// Two apps in flight at once are both named; when both return the field is
// empty again.
func TestInFlightTracksEveryApp(t *testing.T) {
	srv, ts, reg, g := newGateServer(t)
	k1, _ := reg.Add("alpha", Profile{Tiers: []string{"query"}})
	k2, _ := reg.Add("beta", Profile{Tiers: []string{"query"}})
	done := make(chan int, 2)
	for _, k := range []string{k1, k2} {
		k := k
		go func() { st, _ := postQuery(ts, k, "x"); done <- st }()
	}
	<-g.entered
	<-g.entered
	got := srv.Stats().InFlight
	if got != "alpha,beta" {
		t.Fatalf("InFlight %q, want both apps", got)
	}
	close(g.release)
	for i := 0; i < 2; i++ {
		if st := <-done; st != 200 {
			t.Fatalf("status %d", st)
		}
	}
	if got := srv.Stats().InFlight; got != "" {
		t.Fatalf("InFlight after both returned: %q", got)
	}
}

// A handler that panics still clears its app from the in-flight set.
func TestInFlightClearedAfterAPanic(t *testing.T) {
	srv, ts, reg, _ := newGateServer(t)
	k, _ := reg.Add("alpha", Profile{Tiers: []string{"query"}})
	_, _ = postQuery(ts, k, "panic") // the connection is dropped; any outcome
	deadline := time.Now().Add(2 * time.Second)
	for srv.Stats().InFlight != "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := srv.Stats().InFlight; got != "" {
		t.Fatalf("InFlight after a panic: %q", got)
	}
}

// A wrapped deadline is still the request's own timeout, and a wrapped
// *Error keeps its status.
func TestToErrorUnwraps(t *testing.T) {
	if e := toError(fmt.Errorf("model x: %w", context.DeadlineExceeded)); e.Status != http.StatusGatewayTimeout || e.Code != "timeout" {
		t.Fatalf("wrapped deadline: %d %s", e.Status, e.Code)
	}
	inner := Errorf(http.StatusForbidden, "model_not_allowed", "no")
	if e := toError(fmt.Errorf("wrap: %w", inner)); e.Status != http.StatusForbidden || e.Code != "model_not_allowed" {
		t.Fatalf("wrapped *Error: %d %s", e.Status, e.Code)
	}
	if e := toError(errors.New("boom")); e.Status != http.StatusBadGateway || !strings.Contains(e.Message, "boom") {
		t.Fatalf("plain: %d %s", e.Status, e.Message)
	}
}
