package cmd

import (
	"context"
	"net/http"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/update"
)

// startUpdateCheck runs one background check for a newer release (5 s) when
// update_check is on, and calls found only when one exists. Failures are
// silent and it never blocks the caller: it only ever lights the notice;
// installing is /update's, and the person's choice. The returned channel
// closes when the check is over (at once when it is off); callers ignore it,
// tests wait on it.
func startUpdateCheck(cfg *config.Config, c *http.Client, found func(version string)) <-chan struct{} {
	done := make(chan struct{})
	if !cfg.UpdateCheckOn() {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		defer func() { recover() }() // a check must never take the session down
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rel, err := update.Latest(ctx, c)
		if err == nil && update.Newer(update.Current, rel.Version) {
			found(rel.Version)
		}
	}()
	return done
}

// updateDoctorLine is doctor's one line about updates.
func updateDoctorLine(ctx context.Context, cfg *config.Config, c *http.Client) string {
	if !cfg.UpdateCheckOn() {
		return "update check off"
	}
	cur := update.Current
	if !update.Newer("0.0.0", cur) {
		return "update: built from source"
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rel, err := update.Latest(ctx, c)
	if err != nil {
		return "update: could not reach GitHub (" + err.Error() + ")"
	}
	if update.Newer(cur, rel.Version) {
		return "update: v" + cur + " (latest v" + rel.Version + ")"
	}
	return "update: v" + cur + " is the latest"
}
