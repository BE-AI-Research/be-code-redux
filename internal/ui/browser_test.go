package ui

import (
	"strings"
	"testing"

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
	if got := BrowserLines(reg, []string{"bogus"}); len(got) != 1 || got[0] != "usage: /browser [close | forget <host>]" {
		t.Fatalf("usage: %q", got)
	}
}
