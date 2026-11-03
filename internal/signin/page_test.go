package signin

import "testing"

func TestOpenPageReportsWhetherTheBrowserOpened(t *testing.T) {
	orig := launch
	t.Cleanup(func() { launch = orig })
	got := ""
	launch = func(u string) error { got = u; return nil }
	if !OpenPage("https://example.test/keys") || got != "https://example.test/keys" {
		t.Fatalf("opened=%v launched=%q", got != "", got)
	}
	launch = func(string) error { return errTest }
	if OpenPage("https://example.test/keys") {
		t.Fatal("a failed launch must report not opened")
	}
}

var errTest = errorString("no browser")

type errorString string

func (e errorString) Error() string { return string(e) }
