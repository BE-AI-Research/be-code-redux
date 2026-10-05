package ui

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/update"
)

// UpdateCommand is /update in both UIs: check for a newer release, ask
// (the `update` approval, which has no "always"), and install it in place.
// onUpdated runs after a successful install (the TUI clears its notice). It
// returns the line to show.
// checkTimeout bounds the release lookup; installTimeout the download. A
// person's time answering the question is bounded by neither.
var checkTimeout, installTimeout = 15 * time.Second, 10 * time.Minute

func UpdateCommand(ctx context.Context, c *http.Client, ask func(detail string) bool, onUpdated func()) string {
	cur := update.Current
	if !update.Newer("0.0.0", cur) { // a dev build never updates itself
		return update.ErrFromSource.Error()
	}
	checkCtx, cancelCheck := context.WithTimeout(ctx, checkTimeout)
	rel, err := update.Latest(checkCtx, c)
	cancelCheck()
	if err != nil {
		return "could not reach GitHub: " + err.Error()
	}
	if !update.Newer(cur, rel.Version) {
		return fmt.Sprintf("BE-Code v%s is the latest", cur)
	}
	if !ask(fmt.Sprintf("Update BE-Code %s → %s?", cur, rel.Version)) {
		return "not updated"
	}
	target, err := update.Target()
	if err != nil {
		return "cannot find the installed binary: " + err.Error()
	}
	installCtx, cancelInstall := context.WithTimeout(ctx, installTimeout)
	defer cancelInstall()
	if err := update.Install(installCtx, c, rel, runtime.GOOS, runtime.GOARCH, target); err != nil {
		return err.Error()
	}
	if onUpdated != nil {
		onUpdated()
	}
	return fmt.Sprintf("updated to v%s; restart BE-Code to use it", rel.Version)
}

// UpdateCheckCommand is /update check [on|off] (and the menu toggle): it
// sets update_check and saves config.json, so the next session honours it.
// No argument flips the current setting.
func UpdateCheckCommand(cfg *config.Config, arg string) string {
	on := !cfg.UpdateCheckOn()
	switch arg {
	case "":
	case "on":
		on = true
	case "off":
		on = false
	default:
		return "usage: /update check on|off"
	}
	cfg.UpdateCheck = &on
	msg := "update check on — BE-Code checks GitHub for a newer release at start"
	if !on {
		msg = "update check off — BE-Code makes no network call at start"
	}
	if err := cfg.Save(); err != nil {
		return msg + " (this session only: could not save config: " + err.Error() + ")"
	}
	return msg
}
