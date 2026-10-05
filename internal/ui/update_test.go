package ui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/brown-enterprises/be-code/internal/update"
)

// fakeRelease serves a latest release at version with a verified asset and
// points update at it and at a temp target. It returns the target path and
// a request counter.
func fakeRelease(t *testing.T, version string) (string, *atomic.Int32) {
	t.Helper()
	bin := []byte("new binary")
	sum := sha256.Sum256(bin)
	asset := update.AssetName(runtime.GOOS, runtime.GOARCH)
	var hits atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/repos/" + update.Repo + "/releases/latest":
			fmt.Fprintf(w, `{"tag_name":"v%s","html_url":"https://example/rel","assets":[{"name":%q,"browser_download_url":%q},{"name":"SHA256SUMS","browser_download_url":%q}]}`,
				version, asset, srv.URL+"/dl/bin", srv.URL+"/dl/sums")
		case "/dl/bin":
			w.Write(bin)
		case "/dl/sums":
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), asset)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	target := filepath.Join(t.TempDir(), "be-code")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldBase, oldTarget, oldCur := update.APIBase, update.Target, update.Current
	update.APIBase = srv.URL
	update.Target = func() (string, error) { return target, nil }
	update.Current = "1.0.0"
	t.Cleanup(func() { update.APIBase, update.Target, update.Current = oldBase, oldTarget, oldCur })
	return target, &hits
}

func TestUpdateCommandLatest(t *testing.T) {
	fakeRelease(t, "1.0.0")
	asked := false
	got := UpdateCommand(context.Background(), http.DefaultClient, func(string) bool { asked = true; return true }, nil)
	if got != "BE-Code v1.0.0 is the latest" || asked {
		t.Fatalf("%q asked=%v", got, asked)
	}
}

func TestUpdateCommandInstallsAfterYes(t *testing.T) {
	target, _ := fakeRelease(t, "9.9.9")
	var detail string
	updated := false
	got := UpdateCommand(context.Background(), http.DefaultClient, func(d string) bool { detail = d; return true }, func() { updated = true })
	if detail != "Update BE-Code 1.0.0 → 9.9.9?" {
		t.Fatalf("ask %q", detail)
	}
	if got != "updated to v9.9.9; restart BE-Code to use it" || !updated {
		t.Fatalf("%q updated=%v", got, updated)
	}
	if b, _ := os.ReadFile(target); string(b) != "new binary" {
		t.Fatalf("target %q", b)
	}
}

func TestUpdateCommandNoLeavesTarget(t *testing.T) {
	target, _ := fakeRelease(t, "9.9.9")
	got := UpdateCommand(context.Background(), http.DefaultClient, func(string) bool { return false }, func() { t.Fatal("onUpdated on a no") })
	if got != "not updated" {
		t.Fatalf("%q", got)
	}
	if b, _ := os.ReadFile(target); string(b) != "old" {
		t.Fatal("target changed")
	}
}

func TestUpdateCommandDevBuild(t *testing.T) {
	_, hits := fakeRelease(t, "9.9.9")
	update.Current = "dev"
	got := UpdateCommand(context.Background(), http.DefaultClient, func(string) bool { t.Fatal("asked"); return true }, nil)
	if !strings.Contains(got, "built from source") || hits.Load() != 0 {
		t.Fatalf("%q hits=%d", got, hits.Load())
	}
}

func TestUpdateInSlashTableAndBusySafe(t *testing.T) {
	found := false
	for _, c := range SlashCommandTable {
		found = found || c.Name == "/update"
	}
	if !found || !busySafe["/update"] {
		t.Fatal("/update is listed and busy-safe")
	}
}

func TestREPLUpdateApprovalHasNoAlways(t *testing.T) {
	if got := approvePrompt("update"); got != "approve? [y/N] " {
		t.Fatalf("prompt %q", got)
	}
	r := newTestREPL(t)
	r.lines = make(chan lineEvent, 1)
	r.lines <- lineEvent{line: "a"}
	var ok bool
	out := capture(t, func() { ok = r.approveCtx(context.Background(), "update", "Update BE-Code 1.0.0 → 9.9.9?") })
	if ok {
		t.Fatal(`"a" is not an answer to an update prompt`)
	}
	if !strings.Contains(out, "update BE-Code:") {
		t.Fatalf("header:\n%s", out)
	}
}

// The owner's rule: an update never touches config.json. The binary is
// replaced; ~/.be-code/config.json is byte-for-byte what it was.
func TestUpdateNeverTouchesConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgPath := filepath.Join(home, ".be-code", "config.json")
	os.MkdirAll(filepath.Dir(cfgPath), 0o700)
	want := []byte(`{"default_provider":"ollama","model":"mine"}`)
	os.WriteFile(cfgPath, want, 0o600)
	before, _ := os.Stat(cfgPath)
	fakeRelease(t, "9.9.9")
	if got := UpdateCommand(context.Background(), http.DefaultClient, func(string) bool { return true }, nil); !strings.HasPrefix(got, "updated to v9.9.9") {
		t.Fatalf("%q", got)
	}
	after, _ := os.Stat(cfgPath)
	if b, _ := os.ReadFile(cfgPath); string(b) != string(want) || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("config.json changed: %q", b)
	}
}
