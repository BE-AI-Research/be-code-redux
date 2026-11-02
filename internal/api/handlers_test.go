package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestQueryPrependsOnlyAnAllowedActivePack(t *testing.T) {
	h := newHarness(t)
	key := h.app(t, "app", Profile{Tiers: []string{"query"}, Packs: []string{"ue5"}})
	h.back.packs = map[string]bool{"ue5": true}
	resp, body := h.do(t, "POST", "/v1/query", key, "", map[string]any{"prompt": "hi", "pack": "ue5", "system": "be brief"}, nil)
	if resp.StatusCode != 200 || body["text"] != "hello there" {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	q := h.back.queries[len(h.back.queries)-1]
	if q.Pack != "ue5" || q.System != "be brief" || q.MaxTokens != DefaultTokens {
		t.Fatalf("the backend gets the pack, the system text and the default cap: %+v", q)
	}
	h.back.packs["ue5"] = false
	if resp, body := h.do(t, "POST", "/v1/query", key, "", map[string]any{"prompt": "hi", "pack": "ue5"}, nil); resp.StatusCode != 403 || errCode(body) != "pack_not_allowed" {
		t.Fatalf("an inactive pack is refused: %d %v", resp.StatusCode, body)
	}
}

func TestQuerySchemaMismatchIs422WithTheText(t *testing.T) {
	h := newHarness(t)
	key := h.app(t, "app", Profile{Tiers: []string{"query"}})
	h.back.answer = `{"name": 7}`
	schema := map[string]any{"type": "object", "required": []string{"name"}, "properties": map[string]any{"name": map[string]any{"type": "string"}}}
	resp, body := h.do(t, "POST", "/v1/query", key, "", map[string]any{"prompt": "hi", "json_schema": schema}, nil)
	if resp.StatusCode != 422 || errCode(body) != "schema_mismatch" {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	if e := body["error"].(map[string]any); e["text"] != `{"name": 7}` {
		t.Fatalf("the raw text travels with the refusal: %v", e)
	}
	h.back.answer = `{"name": "ok"}`
	if resp, body := h.do(t, "POST", "/v1/query", key, "", map[string]any{"prompt": "hi", "json_schema": schema}, nil); resp.StatusCode != 200 || body["text"] != `{"name": "ok"}` {
		t.Fatalf("a conforming reply passes: %d %v", resp.StatusCode, body)
	}
}

func TestSessionInfoNeverShowsIdOrCode(t *testing.T) {
	h := newHarness(t)
	key := h.app(t, "app", Profile{Sessions: "named"})
	tok := h.back.token
	resp, body := h.do(t, "GET", "/v1/sessions/"+tok, key, tok, nil, nil)
	if resp.StatusCode != 200 || body["title"] != "t" || body["workspace"] != "/ws" || body["live"] != true {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "abc123") || strings.Contains(string(raw), `"id"`) || strings.Contains(string(raw), `"code"`) {
		t.Fatalf("id or code leaked: %s", raw)
	}
}

func TestSessionQueryGoesToSessionQuery(t *testing.T) {
	h := newHarness(t)
	key := h.app(t, "app", Profile{Tiers: []string{"query"}, Sessions: "named"})
	tok := h.back.token
	resp, _ := h.do(t, "POST", "/v1/sessions/"+tok+"/query", key, tok, map[string]any{"prompt": "what am I doing?"}, nil)
	if resp.StatusCode != 200 || h.back.sessionQueries != 1 {
		t.Fatalf("status %d, sessionQueries %d", resp.StatusCode, h.back.sessionQueries)
	}
}

func TestBackendErrorsKeepTheirStatus(t *testing.T) {
	h := newHarness(t)
	key := h.app(t, "app", Profile{Tiers: []string{"query"}, Sessions: "named"})
	h.back.err = Errorf(413, "context_too_large", "the session's %d tokens plus the prompt do not fit %d", 30000, 32768)
	resp, body := h.do(t, "POST", "/v1/sessions/"+h.back.token+"/query", key, h.back.token, map[string]any{"prompt": "x"}, nil)
	if resp.StatusCode != 413 || errCode(body) != "context_too_large" {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	h.back.err = errorString("connection refused")
	if resp, body := h.do(t, "POST", "/v1/query", key, "", map[string]any{"prompt": "x"}, nil); resp.StatusCode != 502 || errCode(body) != "backend_error" {
		t.Fatalf("an untyped error is 502: %d %v", resp.StatusCode, body)
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }

func TestNonCanonicalPathIsNeverRedirectedNorLogged(t *testing.T) {
	h := newHarness(t)
	key := h.app(t, "app", Profile{Tiers: []string{"query"}, Sessions: "named"})
	tok := h.back.token
	path := "//v1/sessions/" + tok + "/query"
	resp, body := h.do(t, "POST", path, key, "", map[string]any{"prompt": "x"}, map[string]string{"Origin": "http://evil.example"})
	if resp.StatusCode != 403 || errCode(body) != "browser_refused" {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	resp, body = h.do(t, "POST", path, key, "", map[string]any{"prompt": "x"}, nil)
	if resp.StatusCode != 404 || errCode(body) != "not_found" {
		t.Fatalf("a non-canonical path is 404, never a redirect: %d %v", resp.StatusCode, body)
	}
	if log := h.log.String(); strings.Count(log, "\n") != 2 || strings.Contains(log, tok) || strings.Contains(log, "/v1/sessions") {
		t.Fatalf("the log must carry neither token nor path: %q", log)
	}
}
