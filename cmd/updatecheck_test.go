package cmd

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/update"
)

// fakeLatest serves only the latest-release endpoint at version (status
// other than 200 when code != 200) and points update.APIBase at it.
func fakeLatest(t *testing.T, version string, code int) *atomic.Int32 {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if code != http.StatusOK {
			w.WriteHeader(code)
			w.Write([]byte(`{"message":"API rate limit exceeded"}`))
			return
		}
		fmt.Fprintf(w, `{"tag_name":"v%s","html_url":"https://example/rel","assets":[]}`, version)
	}))
	t.Cleanup(srv.Close)
	oldBase, oldCur := update.APIBase, update.Current
	update.APIBase, update.Current = srv.URL, "1.0.0"
	t.Cleanup(func() { update.APIBase, update.Current = oldBase, oldCur })
	return &hits
}

func TestStartCheckFindsNewer(t *testing.T) {
	fakeLatest(t, "9.9.9", http.StatusOK)
	got := make(chan string, 1)
	done := startUpdateCheck(config.Default(), http.DefaultClient, func(v string) { got <- v })
	defer func() { <-done }()
	select {
	case v := <-got:
		if v != "9.9.9" {
			t.Fatalf("found %q", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no notice")
	}
}

func TestStartCheckSilentOnFailure(t *testing.T) {
	for name, code := range map[string]int{"rate limited": http.StatusForbidden, "latest": http.StatusOK} {
		t.Run(name, func(t *testing.T) {
			fakeLatest(t, "1.0.0", code)
			found := make(chan string, 1)
			start := time.Now()
			done := startUpdateCheck(config.Default(), http.DefaultClient, func(v string) { found <- v })
			defer func() { <-done }()
			if time.Since(start) > 50*time.Millisecond {
				t.Fatal("the check blocked its caller")
			}
			select {
			case v := <-found:
				t.Fatalf("notice %q", v)
			case <-time.After(300 * time.Millisecond):
			}
		})
	}
}

func TestStartCheckOff(t *testing.T) {
	hits := fakeLatest(t, "9.9.9", http.StatusOK)
	cfg := config.Default()
	off := false
	cfg.UpdateCheck = &off
	<-startUpdateCheck(cfg, http.DefaultClient, func(string) { t.Error("notice with the check off") })
	if hits.Load() != 0 {
		t.Fatalf("%d requests with update_check off", hits.Load())
	}
}

func TestDoctorUpdateLine(t *testing.T) {
	fakeLatest(t, "9.9.9", http.StatusOK)
	if got := updateDoctorLine(context.Background(), config.Default(), http.DefaultClient); got != "update: v1.0.0 (latest v9.9.9)" {
		t.Fatalf("%q", got)
	}
	update.Current = "9.9.9"
	if got := updateDoctorLine(context.Background(), config.Default(), http.DefaultClient); got != "update: v9.9.9 is the latest" {
		t.Fatalf("%q", got)
	}
	update.Current = "dev"
	if got := updateDoctorLine(context.Background(), config.Default(), http.DefaultClient); got != "update: built from source" {
		t.Fatalf("%q", got)
	}
	update.Current = "1.0.0"
	hits := fakeLatest(t, "9.9.9", http.StatusForbidden)
	if got := updateDoctorLine(context.Background(), config.Default(), http.DefaultClient); !strings.HasPrefix(got, "update: could not reach GitHub") {
		t.Fatalf("%q", got)
	}
	cfg := config.Default()
	off := false
	cfg.UpdateCheck = &off
	before := hits.Load()
	if got := updateDoctorLine(context.Background(), cfg, http.DefaultClient); got != "update check off" || hits.Load() != before {
		t.Fatalf("%q (requests %d)", got, hits.Load()-before)
	}
}
