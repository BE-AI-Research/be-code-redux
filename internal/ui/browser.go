package ui

import (
	"strconv"

	"github.com/brown-enterprises/be-code/internal/tools"
)

const browserUsage = "usage: /browser [close | forget <host> | attach | tabs | tab <n>]"

// BrowserLines is /browser (browser spec §4.2): the status, /browser forget
// <host>, /browser close, and for the person's own Chrome /browser attach,
// /browser tabs and /browser tab <n>. It never waits on the browser, so the
// TUI may call it from its Update goroutine.
func BrowserLines(reg *tools.Registry, args []string) []string {
	bt := reg.Browser()
	if bt == nil {
		return []string{`the browser is off (set "browser": {"enabled": true} in config; see README "Browser")`}
	}
	switch {
	case len(args) == 0:
		return bt.StatusLines()
	case len(args) == 1 && args[0] == "close":
		bt.CloseBrowser()
		return []string{"browser: closing (the next browser call starts it again)"}
	case len(args) == 1 && args[0] == "attach":
		return []string{bt.AttachMyChrome()}
	case len(args) == 1 && args[0] == "tabs":
		return bt.PersonTabLines()
	case len(args) == 2 && args[0] == "tab":
		n, err := strconv.Atoi(args[1])
		if err != nil {
			return []string{browserUsage}
		}
		return []string{bt.HandOver(n)}
	case len(args) == 2 && args[0] == "forget":
		if bt.Forget(args[1]) {
			return []string{"forgot " + args[1] + "; the next interaction there asks again"}
		}
		return []string{args[1] + " was not allowed this session"}
	}
	return []string{browserUsage}
}
