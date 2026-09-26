package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Swappable for tests.
var (
	lookPath = exec.LookPath
	statFile = os.Stat
	goos     = runtime.GOOS
	getenv   = os.Getenv
)

// VersionInfo is what GET /json/version answers.
type VersionInfo struct {
	Browser      string `json:"Browser"`
	Protocol     string `json:"Protocol-Version"`
	WebSocketURL string `json:"webSocketDebuggerUrl"`
}

// FetchVersion asks addr (host:port) whether a browser is listening there,
// bounded to two seconds so an absent browser costs almost nothing.
func FetchVersion(ctx context.Context, addr string) (VersionInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/json/version", nil)
	if err != nil {
		return VersionInfo{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return VersionInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return VersionInfo{}, fmt.Errorf("%s/json/version: %s", addr, resp.Status)
	}
	var v VersionInfo
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return VersionInfo{}, err
	}
	if v.WebSocketURL == "" {
		return VersionInfo{}, errors.New("the endpoint named no webSocketDebuggerUrl")
	}
	return v, nil
}

// IsLoopbackAddr reports whether host:port (or a bare host) is on this
// machine.
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	return IsLoopbackHost(host)
}

// FindExecutable returns the first installed Chrome, Edge, Brave or
// Chromium, or "". A candidate with a path separator is a file to stat
// (macOS and Windows install locations); a bare name is looked up on PATH.
// Not filepath.IsAbs: a Windows path is not absolute to a Linux test.
func FindExecutable() string {
	for _, c := range candidates() {
		if strings.ContainsAny(c, `/\`) {
			if st, err := statFile(c); err == nil && !st.IsDir() {
				return c
			}
			continue
		}
		if p, err := lookPath(c); err == nil {
			return p
		}
	}
	return ""
}

func candidates() []string {
	switch goos {
	case "darwin":
		return []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}
	case "windows":
		var out []string
		for _, base := range []string{getenv("ProgramFiles"), getenv("ProgramFiles(x86)"), getenv("LocalAppData")} {
			if base == "" {
				continue
			}
			out = append(out,
				filepath.Join(base, "Google", "Chrome", "Application", "chrome.exe"),
				filepath.Join(base, "Microsoft", "Edge", "Application", "msedge.exe"),
				filepath.Join(base, "BraveSoftware", "Brave-Browser", "Application", "brave.exe"),
				filepath.Join(base, "Chromium", "Application", "chrome.exe"))
		}
		return out
	}
	return []string{"google-chrome", "google-chrome-stable", "microsoft-edge", "microsoft-edge-stable",
		"brave-browser", "chromium", "chromium-browser"}
}

// HasDisplay reports whether a visible browser window could open. macOS and
// Windows always can; elsewhere it takes an X or Wayland display — a VM
// over SSH has neither, and launches headless (spec §1.3).
func HasDisplay() bool {
	if goos == "darwin" || goos == "windows" {
		return true
	}
	return getenv("DISPLAY") != "" || getenv("WAYLAND_DISPLAY") != ""
}
