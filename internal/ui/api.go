package ui

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/api"
	"github.com/brown-enterprises/be-code/internal/store"
)

// APICommand is /api, /api on and /api off: the serving state of the
// application API for this session, and the per-session switch (it never
// writes config).
func APICommand(ag *agent.Agent, fields []string) string {
	if ag.API == nil {
		return "this session cannot serve the application API (headless runs never do)"
	}
	if len(fields) > 1 {
		switch fields[1] {
		case "on":
			if err := ag.API.Start(); err != nil {
				return "api: " + err.Error()
			}
			return "api: serving on " + ag.API.Status().Socket
		case "off":
			ag.API.Stop()
			return "api: stopped for this session"
		default:
			return "usage: /api [on|off]"
		}
	}
	st := ag.API.Status()
	var b strings.Builder
	switch {
	case st.Serving:
		fmt.Fprintf(&b, "api: serving on %s\n", st.Socket)
		fmt.Fprintf(&b, "  %d requests this session", st.Served)
		if st.LastApp != "" {
			fmt.Fprintf(&b, "; last from %s at %s", st.LastApp, st.LastAt.Format("15:04:05"))
		}
		if st.InFlight != "" {
			fmt.Fprintf(&b, "; %s is in flight", st.InFlight)
		}
	case st.ServedBy != "":
		fmt.Fprintf(&b, "api: not serving; session %s (pid %d) serves on %s", st.ServedBy, st.ServedByPID, st.Socket)
	default:
		if dir, derr := api.Dir(); derr == nil {
			if rec, ok := api.Serving(dir); ok {
				fmt.Fprintf(&b, "api: not serving; session %s (pid %d) serves on %s", rec.SessionCode, rec.PID, rec.Socket)
				return b.String()
			}
		}
		b.WriteString("api: not serving (api.enabled is off; /api on serves from this session)")
		if st.Err != "" {
			b.WriteString("\n  " + st.Err)
		}
	}
	return b.String()
}

// AppsCommand is /apps: the registered apps and what each may do.
func AppsCommand() string {
	dir, err := api.Dir()
	if err != nil {
		return "apps: " + err.Error()
	}
	apps, err := api.NewRegistry(filepath.Join(dir, "apps")).List()
	if err != nil {
		return "apps: " + err.Error()
	}
	if len(apps) == 0 {
		return "no apps registered; `be-code apps add <name>` registers one"
	}
	var b strings.Builder
	for _, a := range apps {
		state := strings.Join(a.Profile.Tiers, ",")
		if a.Revoked != nil {
			state = "revoked"
		}
		fmt.Fprintf(&b, "%-20s %-20s sessions: %s\n", a.Name, state, a.Profile.Sessions)
	}
	return strings.TrimRight(b.String(), "\n")
}

// SessionCommand is /session token and /session rotate [grace]. The secret
// comes back on its own so a UI can show it without ever storing it; the
// note is safe to render anywhere.
func SessionCommand(ag *agent.Agent, fields []string) (secret, note string, err error) {
	if len(fields) < 2 {
		return "", "", errors.New("usage: /session token | /session rotate [grace]")
	}
	switch fields[1] {
	case "token":
		tok, err := ag.SessionToken()
		if err != nil {
			return "", "", err
		}
		return tok, "this session's API token (apps send it as X-BE-Session); /session rotate replaces it", nil
	case "rotate":
		grace := time.Duration(0)
		if len(fields) > 2 {
			d, perr := time.ParseDuration(fields[2])
			if perr != nil {
				return "", "", fmt.Errorf("grace %q: %w", fields[2], perr)
			}
			grace = d
		}
		if grace > store.MaxTokenGrace {
			return "", "", fmt.Errorf("grace %s is over the 24h limit", grace)
		}
		tok, err := ag.RotateSessionToken(grace)
		if err != nil {
			return "", "", err
		}
		if grace > 0 {
			return tok, fmt.Sprintf("token rotated; the old one keeps working for %s", grace), nil
		}
		return tok, "token rotated; the old one has stopped", nil
	}
	return "", "", fmt.Errorf("unknown /session verb %q (token, rotate)", fields[1])
}
