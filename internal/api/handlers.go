package api

import (
	"encoding/json"
	"net/http"
)

type queryBody struct {
	Prompt     string          `json:"prompt"`
	System     string          `json:"system"`
	Model      string          `json:"model"`
	Pack       string          `json:"pack"`
	JSONSchema json.RawMessage `json:"json_schema"`
	MaxTokens  int             `json:"max_tokens"`
}

func (b queryBody) request() QueryRequest {
	return QueryRequest{Prompt: b.Prompt, System: b.System, Model: b.Model, Pack: b.Pack, JSONSchema: b.JSONSchema, MaxTokens: b.MaxTokens}
}

// checkQuery applies spec §2.4 steps 7 and 8's request-level checks and
// fills the max_tokens default from the app's budget.
func checkQuery(s *Server, c *call, q *QueryRequest) bool {
	if q.Prompt == "" {
		writeError(c.w, Errorf(http.StatusBadRequest, "bad_request", "prompt is required"))
		return false
	}
	if !c.app.Profile.AllowsModel(q.Model) {
		writeError(c.w, Errorf(http.StatusForbidden, "model_not_allowed", "app %q may not use model %q", c.app.Name, q.Model))
		return false
	}
	if q.Pack != "" && (!c.app.Profile.AllowsPack(q.Pack) || !s.o.Backend.PackActive(q.Pack)) {
		writeError(c.w, Errorf(http.StatusForbidden, "pack_not_allowed", "pack %q is not in app %q's profile or not active in the serving session", q.Pack, c.app.Name))
		return false
	}
	if q.MaxTokens < 0 {
		writeError(c.w, Errorf(http.StatusBadRequest, "bad_request", "max_tokens must not be negative"))
		return false
	}
	if isNullSchema(q.JSONSchema) {
		q.JSONSchema = nil // JSON null is "no schema"
	}
	if len(q.JSONSchema) > 0 {
		if err := CompileSchema(q.JSONSchema); err != nil {
			writeError(c.w, Errorf(http.StatusBadRequest, "bad_request", "%v", err))
			return false
		}
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

// answerQuery runs one query through the given backend method and writes
// the reply, checking it against json_schema when one was given.
func answerQuery(s *Server, c *call, q QueryRequest, run func() (QueryResponse, error)) {
	resp, err := run()
	if err != nil {
		writeError(c.w, s.mapErr(err))
		return
	}
	if len(q.JSONSchema) > 0 {
		if verr := Validate(q.JSONSchema, []byte(resp.Text)); verr != nil {
			e := Errorf(http.StatusUnprocessableEntity, "schema_mismatch", "the reply does not match json_schema: %v", verr)
			e.Extra = map[string]any{"text": resp.Text}
			writeError(c.w, e)
			return
		}
	}
	writeJSON(c, map[string]any{"text": resp.Text, "model": resp.Model, "pack": resp.Pack, "usage": resp.Usage})
}

func handleQuery(s *Server, c *call) {
	var b queryBody
	if !readJSON(c, &b) {
		return
	}
	q := b.request()
	if !checkQuery(s, c, &q) {
		return
	}
	answerQuery(s, c, q, func() (QueryResponse, error) { return s.o.Backend.Query(c.r.Context(), q) })
}

func handleSessionQuery(s *Server, c *call) {
	var b queryBody
	if !readJSON(c, &b) {
		return
	}
	q := b.request()
	if !checkQuery(s, c, &q) {
		return
	}
	answerQuery(s, c, q, func() (QueryResponse, error) { return s.o.Backend.SessionQuery(c.r.Context(), q) })
}

func handleSessionInfo(s *Server, c *call) {
	i := s.o.Backend.SessionInfo()
	writeJSON(c, map[string]any{"title": i.Title, "workspace": i.Workspace, "model": i.Model, "live": true})
}
