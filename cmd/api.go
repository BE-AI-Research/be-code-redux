package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/api"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
)

// apiService is the application API as one interactive session serves it:
// built in buildAgent, started by a UI once approvals are wired (like
// StartSchedules), stopped first in finishSession. It implements
// agent.APIControl for /api on|off and the header.
type apiService struct {
	cfg *config.Config
	ag  *agent.Agent
	dir string

	mu     sync.Mutex
	srv    *api.Server
	ln     net.Listener
	logf   *os.File
	status agent.APIStatus
}

// buildAPI never starts anything.
func buildAPI(cfg *config.Config, ag *agent.Agent) *apiService {
	dir, err := api.Dir()
	s := &apiService{cfg: cfg, ag: ag, dir: dir}
	if err != nil {
		s.status.Err = err.Error()
	}
	ag.API = s
	return s
}

// StartIfEnabled is the startup call: nothing happens with api.enabled off.
func (s *apiService) StartIfEnabled() {
	if !s.cfg.API.Enabled {
		return
	}
	if err := s.Start(); err != nil {
		msg := "api: " + err.Error()
		if !s.ag.Notice(msg) {
			s.ag.QueueNotice(msg)
		}
	}
}

// Start begins serving. A failed Listen leaves s.srv nil so a later /api on
// can retry.
func (s *apiService) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv != nil {
		return nil
	}
	if s.dir == "" {
		if s.status.Err != "" {
			return errors.New(s.status.Err)
		}
		return errors.New("no api directory")
	}
	timeout, err := time.ParseDuration(s.cfg.API.Timeout)
	if err != nil || timeout <= 0 {
		timeout = 120 * time.Second
	}
	code := ""
	if cs := s.ag.CurrentSession(); cs != nil {
		code = cs.ResumeCode()
	}
	ln, err := api.Listen(s.dir, api.Record{PID: os.Getpid(), SessionCode: code})
	if err != nil {
		var se *api.ServedElsewhere
		if errors.As(err, &se) {
			s.status = agent.APIStatus{ServedBy: se.Record.SessionCode, ServedByPID: se.Record.PID, Socket: se.Record.Socket, Err: err.Error()}
		} else {
			s.status = agent.APIStatus{Err: err.Error()}
		}
		return err
	}
	reg := api.NewRegistry(filepath.Join(s.dir, "apps"))
	reg.Warn = func(msg string) {
		if !s.ag.Notice(msg) {
			s.ag.QueueNotice(msg)
		}
	}
	logf, _ := os.OpenFile(filepath.Join(s.dir, "access.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	opts := api.Options{Backend: agent.NewAPIBackend(s.ag, Version), Apps: reg, Timeout: timeout}
	if logf != nil {
		opts.AccessLog = logf
	}
	srv := api.New(opts)
	s.srv, s.ln, s.logf = srv, ln, logf
	s.status = agent.APIStatus{Serving: true, Socket: api.SocketPath(s.dir)}
	go func() {
		_ = srv.Serve(ln)
	}()
	return nil
}

// Stop is idempotent. The fields come out under the lock and the slow
// shutdown (Close waits up to 2 s) runs outside it, so Status, which every
// render calls, never waits on it.
func (s *apiService) Stop() {
	s.mu.Lock()
	srv, ln, logf := s.srv, s.ln, s.logf
	if srv == nil {
		s.mu.Unlock()
		return
	}
	s.srv, s.ln, s.logf = nil, nil, nil
	s.status = agent.APIStatus{}
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Close(ctx)
	_ = ln.Close()
	if logf != nil {
		logf.Close()
	}
	api.Release(s.dir)
}

// Status is cheap on purpose: the header and bottom line call it on every
// render. Who else serves is learned in Start's failure path.
func (s *apiService) Status() agent.APIStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.status
	if s.srv != nil {
		x := s.srv.Stats()
		st.Served, st.LastApp, st.LastAt, st.InFlight = x.Served, x.LastApp, x.LastAt, x.InFlight
	}
	return st
}

// apiDoctorLines is doctor's report: switch, socket and mode, who serves,
// registered apps, revoked ones still on disk.
func apiDoctorLines(cfg *config.Config) []string {
	dir, err := api.Dir()
	if err != nil {
		return []string{"api: " + err.Error()}
	}
	state := "off"
	if cfg.API.Enabled {
		state = "on"
	}
	out := []string{fmt.Sprintf("api: %s (api.enabled); socket %s", state, api.SocketPath(dir))}
	if err := api.CheckDir(dir); err != nil {
		out = append(out, "  "+err.Error())
	}
	if rec, ok := api.Serving(dir); ok {
		out = append(out, fmt.Sprintf("  served by session %s (pid %d) since %s", rec.SessionCode, rec.PID, rec.Started.Format(time.Kitchen)))
	} else {
		out = append(out, "  nobody is serving")
	}
	apps, _ := api.NewRegistry(filepath.Join(dir, "apps")).List()
	out = append(out, fmt.Sprintf("  apps registered: %d", len(apps)))
	for _, a := range apps {
		if a.Revoked != nil {
			out = append(out, fmt.Sprintf("  %s: revoked %s; `be-code apps remove %s` deletes the file", a.Name, a.Revoked.Format("2006-01-02"), a.Name))
		}
	}
	return out
}

// apiModelFactory serves a named model the way a reviewer is built: its own
// provider from the default provider's config, loaded through a loader with
// no approver so the primary's window is never touched.
func apiModelFactory(ag *agent.Agent) func(ctx context.Context, c *config.Config, model string) (provider.Provider, error) {
	return func(ctx context.Context, c *config.Config, model string) (provider.Provider, error) {
		p, err := provider.FromConfig(c, c.DefaultProvider)
		if err != nil {
			return nil, err
		}
		secondaryLoad(ctx, c, p, model, ag)
		return p, nil
	}
}
