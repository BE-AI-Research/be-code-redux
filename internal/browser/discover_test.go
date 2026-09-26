package browser

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/browser/browsertest"
)

// TestMain doubles as a fake browser executable: Launch runs this test
// binary with BE_CODE_FAKE_BROWSER set, and it behaves as that value says
// instead of running tests.
func TestMain(m *testing.M) {
	if mode := os.Getenv("BE_CODE_FAKE_BROWSER"); mode != "" {
		fakeBrowserMain(mode)
		return
	}
	os.Exit(m.Run())
}

func fakeBrowserMain(mode string) {
	profile := ""
	for _, a := range os.Args[1:] {
		if strings.HasPrefix(a, "--user-data-dir=") {
			profile = strings.TrimPrefix(a, "--user-data-dir=")
		}
	}
	os.WriteFile(filepath.Join(profile, "args"), []byte(strings.Join(os.Args[1:], "\n")), 0o600)
	switch mode {
	case "serve":
		// BE_CODE_FAKE_PORT/PATH point the "browser" at a browsertest
		// endpoint (Task 7's launch test); otherwise a port nobody is on.
		port, path := os.Getenv("BE_CODE_FAKE_PORT"), os.Getenv("BE_CODE_FAKE_PATH")
		if port == "" {
			port, path = "45678", "/devtools/browser/abc"
		}
		os.WriteFile(filepath.Join(profile, "DevToolsActivePort"), []byte(port+"\n"+path+"\n"), 0o600)
		time.Sleep(time.Minute)
	case "exit":
		fmt.Fprintln(os.Stderr, "profile directory is in use")
		os.Exit(21)
	case "silent":
		time.Sleep(time.Minute)
	}
	os.Exit(0)
}

func TestLaunchReadsDevToolsActivePort(t *testing.T) {
	t.Setenv("BE_CODE_FAKE_BROWSER", "serve")
	profile := t.TempDir()
	p, err := Launch(context.Background(), os.Args[0], profile, false)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Kill()
	if p.WSURL != "ws://127.0.0.1:45678/devtools/browser/abc" {
		t.Fatalf("WSURL %q", p.WSURL)
	}
	args, _ := os.ReadFile(filepath.Join(profile, "args"))
	for _, want := range []string{"--remote-debugging-port=0", "--user-data-dir=" + profile, "--no-first-run", "--no-default-browser-check", "about:blank"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("launch args lack %q:\n%s", want, args)
		}
	}
	if strings.Contains(string(args), "--headless") {
		t.Error("a visible launch passed --headless")
	}
}

func TestLaunchHeadless(t *testing.T) {
	t.Setenv("BE_CODE_FAKE_BROWSER", "serve")
	profile := t.TempDir()
	p, err := Launch(context.Background(), os.Args[0], profile, true)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Kill()
	args, _ := os.ReadFile(filepath.Join(profile, "args"))
	if !strings.Contains(string(args), "--headless=new") {
		t.Fatalf("headless launch lacks --headless=new:\n%s", args)
	}
}

func TestLaunchIgnoresAStalePortFile(t *testing.T) {
	t.Setenv("BE_CODE_FAKE_BROWSER", "silent")
	old := launchTimeout
	launchTimeout = 300 * time.Millisecond
	defer func() { launchTimeout = old }()
	profile := t.TempDir()
	os.WriteFile(filepath.Join(profile, "DevToolsActivePort"), []byte("1\n/devtools/browser/stale\n"), 0o600)
	_, err := Launch(context.Background(), os.Args[0], profile, false)
	if err == nil || !strings.Contains(err.Error(), "did not open its debugging port") {
		t.Fatalf("a stale DevToolsActivePort was used, or the wrong error: %v", err)
	}
}

func TestLaunchExitAtOnceNamesTheProfile(t *testing.T) {
	t.Setenv("BE_CODE_FAKE_BROWSER", "exit")
	profile := t.TempDir()
	_, err := Launch(context.Background(), os.Args[0], profile, false)
	if err == nil || !strings.Contains(err.Error(), "exited at once") || !strings.Contains(err.Error(), profile) ||
		!strings.Contains(err.Error(), "profile directory is in use") {
		t.Fatalf("error %v", err)
	}
}

func TestLaunchKillEndsTheProcess(t *testing.T) {
	t.Setenv("BE_CODE_FAKE_BROWSER", "serve")
	p, err := Launch(context.Background(), os.Args[0], t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	p.Kill()
	select {
	case <-p.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("Kill left the browser running")
	}
}

func TestFindExecutable(t *testing.T) {
	oldLook, oldStat, oldOS, oldEnv := lookPath, statFile, goos, getenv
	defer func() { lookPath, statFile, goos, getenv = oldLook, oldStat, oldOS, oldEnv }()

	goos = "linux"
	lookPath = func(name string) (string, error) {
		if name == "chromium" {
			return "/usr/bin/chromium", nil
		}
		return "", errors.New("not found")
	}
	if got := FindExecutable(); got != "/usr/bin/chromium" {
		t.Fatalf("linux: %q", got)
	}
	lookPath = func(string) (string, error) { return "", errors.New("not found") }
	if got := FindExecutable(); got != "" {
		t.Fatalf("nothing installed: %q", got)
	}

	goos = "windows"
	getenv = func(k string) string {
		if k == "LocalAppData" {
			return `C:\Users\ann\AppData\Local`
		}
		return ""
	}
	want := filepath.Join(`C:\Users\ann\AppData\Local`, "Microsoft", "Edge", "Application", "msedge.exe")
	statFile = func(p string) (fs.FileInfo, error) {
		if p == want {
			return os.Stat(os.Args[0]) // any real file will do
		}
		return nil, fs.ErrNotExist
	}
	if got := FindExecutable(); got != want {
		t.Fatalf("windows: %q, want %q", got, want)
	}
}

func TestHasDisplay(t *testing.T) {
	oldOS, oldEnv := goos, getenv
	defer func() { goos, getenv = oldOS, oldEnv }()
	env := map[string]string{}
	getenv = func(k string) string { return env[k] }
	goos = "linux"
	if HasDisplay() {
		t.Fatal("linux with no DISPLAY or WAYLAND_DISPLAY has a display")
	}
	env["WAYLAND_DISPLAY"] = "wayland-0"
	if !HasDisplay() {
		t.Fatal("WAYLAND_DISPLAY was not a display")
	}
	goos, env = "darwin", map[string]string{}
	if !HasDisplay() {
		t.Fatal("macOS always has a display")
	}
}

func TestFetchVersion(t *testing.T) {
	fb := browsertest.New(t)
	v, err := FetchVersion(context.Background(), fb.Addr())
	if err != nil {
		t.Fatal(err)
	}
	if v.Browser != "FakeChrome/1.0" || v.WebSocketURL != fb.WSURL() {
		t.Fatalf("version %+v", v)
	}
	if _, err := FetchVersion(context.Background(), "127.0.0.1:1"); err == nil {
		t.Fatal("nothing listening, yet a version came back")
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:9222": true, "localhost:9222": true, "[::1]:9222": true,
		"10.0.0.5:9222": false, "example.com:9222": false, "localhost": true,
	} {
		if got := IsLoopbackAddr(addr); got != want {
			t.Errorf("IsLoopbackAddr(%q) = %v", addr, got)
		}
	}
}
