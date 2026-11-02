package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	failureDelay   = 250 * time.Millisecond
	failureLockout = 20 // auth failures per minute before everyone waits
	maxBody        = 1 << 20
)

// Options wires a Server. Now, Sleep and AccessLog are injectable for tests;
// nil means the real clock, time.Sleep and io.Discard.
type Options struct {
	Backend   Backend
	Apps      *Registry
	Timeout   time.Duration
	Now       func() time.Time
	Sleep     func(time.Duration)
	AccessLog io.Writer
}

type ServerStats struct {
	Served   int
	LastApp  string
	LastAt   time.Time
	InFlight string // the app whose request is being answered now, "" when idle
}

// Server is the application API: one http.Server on a unix socket, the
// check order of spec §2.4 in front of every route.
type Server struct {
	o        Options
	mux      *http.ServeMux
	budgets  *budgets
	http     *http.Server
	closing  atomic.Bool
	closeCtx context.Context
	closeFn  context.CancelFunc

	failMu     sync.Mutex
	failWindow time.Time
	failures   int

	statsMu sync.Mutex
	stats   ServerStats
}

// call is one authenticated request, handed to a route handler.
type call struct {
	w            http.ResponseWriter
	r            *http.Request
	app          App
	sessionToken string // X-BE-Session when present and accepted
	route        string // the template, for the log
	sessionCode  string
}

type handler func(s *Server, c *call)

// routes is the table every endpoint is declared in: path template, tier
// ("" means any key), whether the path names a session, the handler.
// Tasks 4 and 5 replace the placeholder handlers.
var routes = []struct {
	method, template, tier string
	session                bool
	h                      handler
}{
	{"GET", "/v1/health", "", false, handleHealth},
	{"POST", "/v1/query", "query", false, handleQuery},
	{"POST", "/v1/chat/completions", "completions", false, handleCompletions},
	{"GET", "/v1/sessions/{token}", "", true, handleSessionInfo},
	{"POST", "/v1/sessions/{token}/query", "query", true, handleSessionQuery},
}

func New(o Options) *Server {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = time.Sleep
	}
	if o.AccessLog == nil {
		o.AccessLog = io.Discard
	}
	if o.Timeout <= 0 {
		o.Timeout = 120 * time.Second
	}
	s := &Server{o: o, mux: http.NewServeMux(), budgets: newBudgets(o.Now)}
	s.closeCtx, s.closeFn = context.WithCancel(context.Background())
	for _, rt := range routes {
		rt := rt
		s.mux.HandleFunc(rt.method+" "+rt.template, func(w http.ResponseWriter, r *http.Request) { s.serve(w, r, rt.template, rt.tier, rt.session, rt.h) })
	}
	s.http = &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 30 * time.Second}
	return s
}

// Handler is the whole API as an http.Handler (tests mount it on httptest).
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Browser refusal before anything else — and before the body is
		// touched: a page in the person's browser can reach loopback.
		if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Mode") != "" {
			// Close the connection: otherwise net/http reads the unread body
			// (up to 256 KB) before answering, which is the very thing refused.
			w.Header().Set("Connection", "close")
			s.answerEarly(w, r, Errorf(http.StatusForbidden, "browser_refused", "this API is for programs, not pages"))
			return
		}
		if s.closing.Load() {
			s.answerEarly(w, r, Errorf(http.StatusServiceUnavailable, "shutting_down", "the serving session is ending"))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		if _, pattern := s.mux.Handler(r); pattern == "" {
			// An unknown path and a wrong method are the same 404, and only
			// after authentication, so a caller learns nothing about routes.
			s.serve(w, r, "-", "", false, handleNotFound)
			return
		}
		s.mux.ServeHTTP(w, r)
	})
}

// answerEarly answers a request refused before routing and logs it: no app,
// the route template when the request matches one, else "-".
func (s *Server) answerEarly(w http.ResponseWriter, r *http.Request, e *Error) {
	route := "-"
	if _, p := s.mux.Handler(r); p != "" {
		if _, t, ok := strings.Cut(p, " "); ok {
			route = t
		} else {
			route = p
		}
	}
	writeError(w, e)
	s.logLine(&call{route: route}, e.Status, 0)
}

func handleNotFound(s *Server, c *call) {
	writeError(c.w, Errorf(http.StatusNotFound, "not_found", "no such endpoint"))
}

func (s *Server) Serve(l net.Listener) error { return s.http.Serve(l) }

// Close refuses new requests, answers in-flight ones 503 by cancelling
// their contexts, and shuts the listener.
func (s *Server) Close(ctx context.Context) error {
	s.closing.Store(true)
	s.closeFn()
	return s.http.Shutdown(ctx)
}

// mapErr is toError, except that a cancellation while the server is closing
// is the server's doing: 503 shutting_down.
func (s *Server) mapErr(err error) *Error {
	if s.closing.Load() && errors.Is(err, context.Canceled) {
		return Errorf(http.StatusServiceUnavailable, "shutting_down", "the serving session is ending")
	}
	return toError(err)
}

func (s *Server) Stats() ServerStats {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	return s.stats
}

// serve is the check order of spec §2.4 steps 2–8 and the access log.
func (s *Server) serve(w http.ResponseWriter, r *http.Request, template, tier string, session bool, h handler) {
	start := s.o.Now()
	c := &call{w: w, r: r, route: template}
	status := &statusWriter{ResponseWriter: w, status: 200}
	c.w = status
	defer func() {
		s.logLine(c, status.status, s.o.Now().Sub(start))
	}()

	if s.lockedOut() {
		writeError(c.w, Errorf(http.StatusTooManyRequests, "rate_limited", "too many failed authentications; try again in a minute"))
		return
	}
	app, ok := s.authenticate(r)
	if !ok {
		s.failed(c.w, Errorf(http.StatusUnauthorized, "unauthorized", "unknown, revoked or malformed app key"))
		return
	}
	c.app = app
	if tier != "" && !app.Profile.AllowsTier(tier) {
		writeError(c.w, Errorf(http.StatusForbidden, "tier_not_allowed", "app %q may not call the %s tier", app.Name, tier))
		return
	}
	hdrTok := r.Header.Get("X-BE-Session")
	if hdrTok != "" && app.Profile.Sessions != "named" {
		writeError(c.w, Errorf(http.StatusForbidden, "sessions_not_allowed", "app %q may not address a session", app.Name))
		return
	}
	if session {
		pathTok := r.PathValue("token")
		if hdrTok != "" && hdrTok != pathTok {
			writeError(c.w, Errorf(http.StatusBadRequest, "session_mismatch", "the session in the path and in X-BE-Session differ"))
			return
		}
		if app.Profile.Sessions != "named" {
			writeError(c.w, Errorf(http.StatusForbidden, "sessions_not_allowed", "app %q may not address a session", app.Name))
			return
		}
		if !s.o.Backend.AcceptsSession(pathTok) {
			s.failed(c.w, Errorf(http.StatusNotFound, "session_not_found", "no such session"))
			return
		}
		c.sessionToken = pathTok
		c.sessionCode = s.o.Backend.Health().SessionCode
	}
	// The in-flight slot first: a request refused for it must not spend the
	// hour, or a client obeying Retry-After would burn its budget on refusals.
	release, ok := s.budgets.begin(app.Name)
	if !ok {
		c.w.Header().Set("Retry-After", "1")
		writeError(c.w, Errorf(http.StatusTooManyRequests, "rate_limited", "app %q already has a request in flight", app.Name))
		return
	}
	defer release()
	if retry, ok := s.budgets.take(app.Name, app.Profile.Budget.PerHour); !ok {
		c.w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		writeError(c.w, Errorf(http.StatusTooManyRequests, "rate_limited", "app %q is over its %d requests per hour", app.Name, app.Profile.Budget.PerHour))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.o.Timeout)
	stop := context.AfterFunc(s.closeCtx, cancel)
	defer stop()
	defer cancel()
	c.r = r.WithContext(ctx)
	s.statsMu.Lock()
	s.stats.InFlight = app.Name
	s.statsMu.Unlock()
	h(s, c)
	s.statsMu.Lock()
	s.stats.InFlight = ""
	s.statsMu.Unlock()
	s.statsMu.Lock()
	s.stats.Served++
	s.stats.LastApp, s.stats.LastAt = app.Name, s.o.Now()
	s.statsMu.Unlock()
}

// authenticate: Bearer with the key prefix, well-formed, hashed and found.
func (s *Server) authenticate(r *http.Request) (App, bool) {
	auth := r.Header.Get("Authorization")
	key, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok || !strings.HasPrefix(key, keyPrefix) {
		return App{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(key, keyPrefix))
	if err != nil || len(raw) != 24 {
		return App{}, false
	}
	return s.o.Apps.Lookup(key)
}

// failed answers a 401 or 404 after the delay, closing the connection, and
// counts it toward the lockout.
func (s *Server) failed(w http.ResponseWriter, e *Error) {
	s.failMu.Lock()
	now := s.o.Now()
	if now.Sub(s.failWindow) >= time.Minute {
		s.failWindow, s.failures = now, 0
	}
	s.failures++
	s.failMu.Unlock()
	s.o.Sleep(failureDelay)
	w.Header().Set("Connection", "close")
	writeError(w, e)
}

func (s *Server) lockedOut() bool {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	if s.o.Now().Sub(s.failWindow) >= time.Minute {
		return false
	}
	return s.failures >= failureLockout
}

// logLine: time, app when known, session code when named, route template
// (never the path, which carries the token), status, duration. No secret
// can reach it because none is a field.
func (s *Server) logLine(c *call, status int, d time.Duration) {
	app, sess := "-", "-"
	if c.app.Name != "" {
		app = c.app.Name
	}
	if c.sessionCode != "" {
		sess = c.sessionCode
	}
	fmt.Fprintf(s.o.AccessLog, "%s app=%s session=%s route=%s status=%d dur=%s\n",
		s.o.Now().Format(time.RFC3339), app, sess, c.route, status, d.Round(time.Millisecond))
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) { w.status = code; w.ResponseWriter.WriteHeader(code) }
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// readJSON decodes the body into v; a bad body is 400 bad_request.
func readJSON(c *call, v any) bool {
	dec := json.NewDecoder(c.r.Body)
	if err := dec.Decode(v); err != nil {
		writeError(c.w, Errorf(http.StatusBadRequest, "bad_request", "the body is not the JSON this endpoint takes: %v", err))
		return false
	}
	return true
}

func writeJSON(c *call, v any) {
	c.w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(c.w).Encode(v)
}

func handleHealth(s *Server, c *call) {
	h := s.o.Backend.Health()
	writeJSON(c, map[string]any{
		"version":       h.Version,
		"session":       map[string]string{"code": h.SessionCode, "model": h.Model},
		"serving_since": h.Since,
		"apps_seen":     s.Stats().Served,
	})
}

// Placeholders until Tasks 4 and 5; handleQuery already does the model,
// pack and max_tokens checks the Task 3 tests need.
func handleQuery(s *Server, c *call) {
	var q struct {
		Prompt, System, Model, Pack string
		JSONSchema                  json.RawMessage `json:"json_schema"`
		MaxTokens                   int             `json:"max_tokens"`
	}
	if !readJSON(c, &q) {
		return
	}
	req := QueryRequest{Prompt: q.Prompt, System: q.System, Model: q.Model, Pack: q.Pack, JSONSchema: q.JSONSchema, MaxTokens: q.MaxTokens}
	if !checkQuery(s, c, &req) {
		return
	}
	resp, err := s.o.Backend.Query(c.r.Context(), req)
	if err != nil {
		writeError(c.w, s.mapErr(err))
		return
	}
	writeJSON(c, map[string]any{"text": resp.Text, "model": resp.Model, "pack": resp.Pack, "usage": resp.Usage})
}

// checkQuery applies spec §2.4 steps 7 and 8's request-level checks.
func checkQuery(s *Server, c *call, q *QueryRequest) bool {
	if !c.app.Profile.AllowsModel(q.Model) {
		writeError(c.w, Errorf(http.StatusForbidden, "model_not_allowed", "app %q may not use model %q", c.app.Name, q.Model))
		return false
	}
	if q.Pack != "" && (!c.app.Profile.AllowsPack(q.Pack) || !s.o.Backend.PackActive(q.Pack)) {
		writeError(c.w, Errorf(http.StatusForbidden, "pack_not_allowed", "pack %q is not in app %q's profile or not active in the serving session", q.Pack, c.app.Name))
		return false
	}
	cap := c.app.Profile.Budget.Tokens
	if q.MaxTokens == 0 {
		q.MaxTokens = cap
	}
	if cap > 0 && q.MaxTokens > cap {
		writeError(c.w, Errorf(http.StatusBadRequest, "over_budget", "max_tokens %d is over app %q's budget of %d", q.MaxTokens, c.app.Name, cap))
		return false
	}
	return true
}

func handleCompletions(s *Server, c *call) { // replaced in Task 5
	writeError(c.w, Errorf(http.StatusNotImplemented, "not_supported", "completions are not wired yet"))
}
func handleSessionInfo(s *Server, c *call) { // replaced in Task 4
	i := s.o.Backend.SessionInfo()
	writeJSON(c, map[string]any{"title": i.Title, "workspace": i.Workspace, "model": i.Model, "live": true})
}
func handleSessionQuery(s *Server, c *call) { // replaced in Task 4
	handleQuery(s, c)
}
