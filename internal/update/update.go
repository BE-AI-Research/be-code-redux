// Package update finds out whether a newer BE-Code release exists and
// installs it in place after checking it against the release's SHA256SUMS.
// It is stdlib only; every URL and the target path are injectable so tests
// never touch the network or a real binary.
package update

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Current is the running version; cmd sets it from its build-time Version.
var Current = "dev"

// APIBase is GitHub's API root (a test points it at an httptest server).
var APIBase = "https://api.github.com"

// Repo is the repository releases are published to.
const Repo = "BE-AI-Research/be-code-redux"

const installCmd = "curl -fsSL https://raw.githubusercontent.com/BE-AI-Research/be-code-redux/main/install.sh | sh"

// maxBinary bounds a download (the binaries are ~15 MB).
const maxBinary = 200 << 20

var (
	ErrUnverified = errors.New("could not be verified")
	ErrNoAsset    = errors.New("no build for this platform")
	// ErrFromSource is returned for a dev build, which never updates itself.
	ErrFromSource = errors.New("this BE-Code was built from source; update it with git pull and ./install.sh")
)

// NotWritableError says the binary's directory cannot be written.
type NotWritableError struct{ Dir string }

func (e *NotWritableError) Error() string {
	return fmt.Sprintf("cannot write %s; update with: %s", e.Dir, installCmd)
}

// Release is the newest published release.
type Release struct {
	Version string            // without the leading v
	HTMLURL string            // the release page
	Assets  map[string]string // asset name → download URL
}

// Latest reads the newest release.
func Latest(ctx context.Context, c *http.Client) (Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, APIBase+"/repos/"+Repo+"/releases/latest", nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.Do(req)
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("GitHub answered HTTP %d", resp.StatusCode)
	}
	var body struct {
		Tag    string `json:"tag_name"`
		URL    string `json:"html_url"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return Release{}, fmt.Errorf("unreadable release: %w", err)
	}
	r := Release{Version: strings.TrimPrefix(body.Tag, "v"), HTMLURL: body.URL, Assets: map[string]string{}}
	for _, a := range body.Assets {
		r.Assets[a.Name] = a.URL
	}
	if _, ok := parseVersion(r.Version); !ok {
		return Release{}, fmt.Errorf("unreadable release tag %q", body.Tag)
	}
	return r, nil
}

// parseVersion reads major.minor.patch; anything else (a pre-release
// suffix, "dev") is not a version this updater offers or compares.
func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// Newer reports whether latest is a newer release than current. A dev or
// unreadable current version is never offered an update.
func Newer(current, latest string) bool {
	c, ok1 := parseVersion(current)
	l, ok2 := parseVersion(latest)
	if !ok1 || !ok2 {
		return false
	}
	for i := 0; i < 3; i++ {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

// AssetName is the release asset for a platform (install.sh's names).
func AssetName(goos, goarch string) string {
	n := "be-code-" + goos + "-" + goarch
	if goos == "windows" {
		n += ".exe"
	}
	return n
}

// Target is the installed binary to replace, symlinks resolved. A variable
// so tests elsewhere can point it at a temp file.
var Target = func() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

// parseSums finds name in a sha256sum listing ("<hex>  name" or "<hex> *name").
func parseSums(sums, name string) (string, bool) {
	sc := bufio.NewScanner(strings.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return strings.ToLower(f[0]), true
		}
	}
	return "", false
}

func fetch(ctx context.Context, c *http.Client, url string, limit int64, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download answered HTTP %d", resp.StatusCode)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return fmt.Errorf("download larger than %d bytes", limit)
	}
	return nil
}

// Install downloads r's binary for goos/goarch, checks it against the
// release's SHA256SUMS and replaces target with it. Nothing is replaced
// unless the checksum matches.
func Install(ctx context.Context, c *http.Client, r Release, goos, goarch, target string) error {
	name := AssetName(goos, goarch)
	binURL, ok := r.Assets[name]
	if !ok {
		return fmt.Errorf("no v%s build for %s/%s; see %s: %w", r.Version, goos, goarch, r.HTMLURL, ErrNoAsset)
	}
	unverified := fmt.Errorf("v%s could not be verified; not installed: %w", r.Version, ErrUnverified)
	sumsURL, ok := r.Assets["SHA256SUMS"]
	if !ok {
		return unverified
	}
	var sums strings.Builder
	if err := fetch(ctx, c, sumsURL, 1<<16, &sums); err != nil {
		return unverified
	}
	want, ok := parseSums(sums.String(), name)
	if !ok {
		return unverified
	}
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".be-code-update-*")
	if err != nil {
		return &NotWritableError{Dir: dir}
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	h := sha256.New()
	if err := fetch(ctx, c, binURL, maxBinary, io.MultiWriter(tmp, h)); err != nil {
		tmp.Close()
		return fmt.Errorf("downloading v%s: %w", r.Version, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != want {
		return unverified
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	if runtime.GOOS == "windows" { // a running .exe cannot be overwritten
		old := target + ".old"
		os.Remove(old) // left by the previous update
		if err := os.Rename(target, old); err != nil {
			return &NotWritableError{Dir: dir}
		}
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return &NotWritableError{Dir: dir}
	}
	return nil
}
