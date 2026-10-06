package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		cur, latest string
		want        bool
	}{
		{"1.2.0", "1.2.1", true}, {"1.2.0", "v1.2.1", true}, {"v1.9.0", "1.10.0", true},
		{"1.2.1", "1.2.1", false}, {"1.3.0", "1.2.9", false},
		{"dev", "9.9.9", false}, {"1.2.0", "garbage", false}, {"1.2.0", "v1.3.0-rc1", false},
	}
	for _, c := range cases {
		if got := Newer(c.cur, c.latest); got != c.want {
			t.Errorf("Newer(%q,%q)=%v", c.cur, c.latest, got)
		}
	}
}

func TestParseSums(t *testing.T) {
	sums := "aa11  be-code-linux-amd64\nbb22 *be-code-windows-amd64.exe\n\n"
	if h, ok := parseSums(sums, "be-code-linux-amd64"); !ok || h != "aa11" {
		t.Fatalf("%q %v", h, ok)
	}
	if h, ok := parseSums(sums, "be-code-windows-amd64.exe"); !ok || h != "bb22" {
		t.Fatalf("%q %v", h, ok)
	}
	if _, ok := parseSums(sums, "be-code-darwin-arm64"); ok {
		t.Fatal("absent name found")
	}
}

// fakeGitHub serves one release whose asset body is bin and whose SHA256SUMS
// line is sum (empty = no SHA256SUMS asset).
func fakeGitHub(t *testing.T, bin []byte, sum string) *httptest.Server {
	t.Helper()
	asset := AssetName(runtime.GOOS, runtime.GOARCH)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + Repo + "/releases/latest":
			assets := fmt.Sprintf(`{"name":%q,"browser_download_url":%q}`, asset, srv.URL+"/dl/"+asset)
			if sum != "" {
				assets += fmt.Sprintf(`,{"name":"SHA256SUMS","browser_download_url":%q}`, srv.URL+"/dl/SHA256SUMS")
			}
			fmt.Fprintf(w, `{"tag_name":"v9.9.9","html_url":"https://example/rel","assets":[%s]}`, assets)
		case "/dl/" + asset:
			w.Write(bin)
		case "/dl/SHA256SUMS":
			fmt.Fprintf(w, "%s  %s\n", sum, asset)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := APIBase
	APIBase = srv.URL
	t.Cleanup(func() { APIBase = old })
	return srv
}

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func TestLatestParsesRelease(t *testing.T) {
	fakeGitHub(t, []byte("new"), sha([]byte("new")))
	r, err := Latest(context.Background(), http.DefaultClient)
	if err != nil || r.Version != "9.9.9" || r.HTMLURL != "https://example/rel" || len(r.Assets) != 2 {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestLatestErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	}))
	defer srv.Close()
	old := APIBase
	APIBase = srv.URL
	defer func() { APIBase = old }()
	if _, err := Latest(context.Background(), http.DefaultClient); err == nil {
		t.Fatal("403 must be an error")
	}
}

func target(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "be-code")
	if err := os.WriteFile(p, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInstallVerifiedReplaces(t *testing.T) {
	fakeGitHub(t, []byte("new"), sha([]byte("new")))
	r, _ := Latest(context.Background(), http.DefaultClient)
	p := target(t)
	if err := Install(context.Background(), http.DefaultClient, r, runtime.GOOS, runtime.GOARCH, p); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "new" {
		t.Fatalf("target holds %q", b)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm()&0o100 == 0 {
		t.Fatal("not executable")
	}
}

func TestInstallRefusesUnverified(t *testing.T) {
	for name, sum := range map[string]string{"mismatch": sha([]byte("other")), "no sums": ""} {
		t.Run(name, func(t *testing.T) {
			fakeGitHub(t, []byte("new"), sum)
			r, _ := Latest(context.Background(), http.DefaultClient)
			p := target(t)
			err := Install(context.Background(), http.DefaultClient, r, runtime.GOOS, runtime.GOARCH, p)
			if !errors.Is(err, ErrUnverified) || !strings.Contains(err.Error(), "v9.9.9 could not be verified; not installed") {
				t.Fatalf("err=%v", err)
			}
			if b, _ := os.ReadFile(p); string(b) != "old" {
				t.Fatal("target changed")
			}
			if ents, _ := os.ReadDir(filepath.Dir(p)); len(ents) != 1 {
				t.Fatalf("temp file left behind: %v", ents)
			}
		})
	}
}

func TestInstallNoAssetForPlatform(t *testing.T) {
	fakeGitHub(t, []byte("new"), sha([]byte("new")))
	r, _ := Latest(context.Background(), http.DefaultClient)
	err := Install(context.Background(), http.DefaultClient, r, "plan9", "mips", target(t))
	if !errors.Is(err, ErrNoAsset) || !strings.Contains(err.Error(), "no v9.9.9 build for plan9/mips; see https://example/rel") {
		t.Fatalf("err=%v", err)
	}
}

func TestInstallUnwritableDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	fakeGitHub(t, []byte("new"), sha([]byte("new")))
	r, _ := Latest(context.Background(), http.DefaultClient)
	p := target(t)
	dir := filepath.Dir(p)
	os.Chmod(dir, 0o555)
	defer os.Chmod(dir, 0o755)
	err := Install(context.Background(), http.DefaultClient, r, runtime.GOOS, runtime.GOARCH, p)
	if err == nil || !strings.Contains(err.Error(), "cannot write "+dir+"; update with: curl -fsSL") {
		t.Fatalf("err=%v", err)
	}
}

func TestInstallReplacesSymlinkTarget(t *testing.T) {
	fakeGitHub(t, []byte("new"), sha([]byte("new")))
	r, _ := Latest(context.Background(), http.DefaultClient)
	real := target(t)
	link := filepath.Join(t.TempDir(), "be-code")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("no symlinks here")
	}
	resolved, _ := filepath.EvalSymlinks(link)
	if err := Install(context.Background(), http.DefaultClient, r, runtime.GOOS, runtime.GOARCH, resolved); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(link); string(b) != "new" {
		t.Fatal("symlink does not reach the new binary")
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced by a file")
	}
}

// An update replaces the be-code binary and nothing else: a target that is
// not a be-code binary file (a config file, a directory) is refused untouched.
func TestInstallRefusesATargetThatIsNotTheBinary(t *testing.T) {
	fakeGitHub(t, []byte("new"), sha([]byte("new")))
	r, _ := Latest(context.Background(), http.DefaultClient)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	os.WriteFile(cfg, []byte(`{"model":"x"}`), 0o600)
	for _, target := range []string{cfg, dir} {
		if err := Install(context.Background(), http.DefaultClient, r, runtime.GOOS, runtime.GOARCH, target); err == nil {
			t.Fatalf("installed over %s", target)
		}
	}
	if b, _ := os.ReadFile(cfg); string(b) != `{"model":"x"}` {
		t.Fatalf("config changed: %q", b)
	}
}

// Windows renames the running binary aside before moving the new one in. If
// that second rename fails, the old binary is put back: never no be-code.
func TestInstallWindowsRollsBackOnFailedSwap(t *testing.T) {
	fakeGitHub(t, []byte("new"), sha([]byte("new")))
	r, _ := Latest(context.Background(), http.DefaultClient)
	p := target(t)
	oldAside, oldRename := renameAside, rename
	renameAside = true
	calls := 0
	rename = func(from, to string) error {
		calls++
		if calls == 2 {
			return errors.New("file in use")
		}
		return os.Rename(from, to)
	}
	t.Cleanup(func() { renameAside, rename = oldAside, oldRename })
	if err := Install(context.Background(), http.DefaultClient, r, runtime.GOOS, runtime.GOARCH, p); err == nil {
		t.Fatal("the failed swap was not reported")
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "old" {
		t.Fatalf("binary not restored: %q %v", b, err)
	}
}

// The "update it yourself" command names the installer for the platform:
// Windows has no sh, so it gets the PowerShell one-liner.
func TestInstallHintForPlatform(t *testing.T) {
	if got := installHintFor("windows"); got != "irm https://raw.githubusercontent.com/BE-AI-Research/be-code-redux/main/install.ps1 | iex" {
		t.Fatalf("windows: %q", got)
	}
	if got := installHintFor("linux"); got != "curl -fsSL https://raw.githubusercontent.com/BE-AI-Research/be-code-redux/main/install.sh | sh" {
		t.Fatalf("linux: %q", got)
	}
}
