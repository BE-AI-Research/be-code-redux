package ui

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/tools"
)

func TestBrowserLines(t *testing.T) {
	reg, err := tools.NewRegistry(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := BrowserLines(reg, nil); len(got) != 1 || !strings.HasPrefix(got[0], "the browser is off") {
		t.Fatalf("off: %q", got)
	}
	reg.AddTool(tools.NewBrowser(tools.BrowserConfig{Address: "127.0.0.1:1"}))
	if got := BrowserLines(reg, nil); got[0] != "browser: not connected (it starts on the model's first browser call)" {
		t.Fatalf("status: %q", got)
	}
	if got := BrowserLines(reg, []string{"forget", "x.test"}); len(got) != 1 || got[0] != "x.test was not allowed this session" {
		t.Fatalf("forget: %q", got)
	}
	if got := BrowserLines(reg, []string{"close"}); len(got) != 1 || !strings.HasPrefix(got[0], "browser: closing") {
		t.Fatalf("close: %q", got)
	}
	if got := BrowserLines(reg, []string{"bogus"}); len(got) != 1 || got[0] != browserUsage {
		t.Fatalf("usage: %q", got)
	}
	if got := BrowserLines(reg, []string{"untab", "all"}); len(got) != 1 || !strings.Contains(got[0], "/browser attach") {
		t.Fatalf("untab outside my-Chrome mode: %q", got)
	}
	if got := BrowserLines(reg, []string{"tab", "1"}); len(got) != 1 || !strings.Contains(got[0], "/browser attach") {
		t.Fatalf("tab outside my-Chrome mode: %q", got)
	}
	if got := BrowserLines(reg, []string{"tabs"}); len(got) != 1 || !strings.Contains(got[0], "/browser attach") {
		t.Fatalf("tabs outside my-Chrome mode: %q", got)
	}
}

func TestBrowserLinesAttachToMyChrome(t *testing.T) {
	reg, err := tools.NewRegistry(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg.Close)
	dir := t.TempDir() // no DevToolsActivePort: the attach fails, off this goroutine
	var mu sync.Mutex
	var notes []string
	// The outcome reaches the transcript (OnNotice), not only the status line.
	reg.OnNotice = func(m string) { mu.Lock(); notes = append(notes, m); mu.Unlock() }
	reg.AddTool(tools.NewBrowser(tools.BrowserConfig{Address: "127.0.0.1:1", ChromeUserDataDir: dir}))
	got := BrowserLines(reg, []string{"attach"})
	if len(got) != 1 || !strings.HasPrefix(got[0], "browser: attaching to your Chrome ("+dir+") for this session") {
		t.Fatalf("attach: %q", got)
	}
	if got := BrowserLines(reg, nil); got[0] != "browser: not connected (it attaches to your Chrome ("+dir+") on the model's first browser call)" {
		t.Fatalf("status after attach: %q", got)
	}
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		mu.Lock()
		n := len(notes)
		last := ""
		if n > 0 {
			last = notes[n-1]
		}
		mu.Unlock()
		if strings.Contains(last, "chrome://inspect/#remote-debugging") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("notes %q", notes)
		}
	}
	if got := BrowserLines(reg, []string{"tabs"}); len(got) != 1 || !strings.HasPrefix(got[0], "not attached to your Chrome yet") {
		t.Fatalf("tabs: %q", got)
	}
}

func TestBrowserLinesCloseCancelsAWaitingAttach(t *testing.T) {
	reg, err := tools.NewRegistry(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg.Close)
	reg.AddTool(tools.NewBrowser(tools.BrowserConfig{Address: "127.0.0.1:1", ChromeUserDataDir: t.TempDir()}))
	if got := BrowserLines(reg, []string{"close"}); got[0] != "browser: closing (the next browser call starts it again)" {
		t.Fatalf("close with no attach: %q", got)
	}
}
