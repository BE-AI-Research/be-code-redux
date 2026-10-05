package ui

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"time"

	"github.com/brown-enterprises/be-code/internal/update"
)

// UpdateCommand is /update in both UIs: check for a newer release, ask
// (the `update` approval, which has no "always"), and install it in place.
// onUpdated runs after a successful install (the TUI clears its notice). It
// returns the line to show.
func UpdateCommand(ctx context.Context, c *http.Client, ask func(detail string) bool, onUpdated func()) string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cur := update.Current
	if !update.Newer("0.0.0", cur) { // a dev build never updates itself
		return update.ErrFromSource.Error()
	}
	rel, err := update.Latest(ctx, c)
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
	if err := update.Install(ctx, c, rel, runtime.GOOS, runtime.GOARCH, target); err != nil {
		return err.Error()
	}
	if onUpdated != nil {
		onUpdated()
	}
	return fmt.Sprintf("updated to v%s; restart BE-Code to use it", rel.Version)
}
