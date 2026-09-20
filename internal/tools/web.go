package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// Web access is opt-in (config web_search.cx) and is the one deliberate
// exception to "no network egress except inference endpoints": queries go
// to Google Programmable Search, page fetches to whatever URL the model
// picks from the results.

// WebSearchConfig configures the web_search tool (Google Programmable
// Search Engine, Custom Search JSON API).
type WebSearchConfig struct {
	CX         string // search engine id
	APIKeyEnv  string // env var holding the API key (never the key itself)
	MaxResults int    // 1-10
	Endpoint   string // override for tests; default is Google's API
}

const googlePSEEndpoint = "https://www.googleapis.com/customsearch/v1"

type webSearchTool struct {
	cfg    WebSearchConfig
	client *http.Client
	r      *Registry
}

// NewWebSearch builds the web_search tool.
func NewWebSearch(cfg WebSearchConfig) Tool {
	if cfg.Endpoint == "" {
		cfg.Endpoint = googlePSEEndpoint
	}
	if cfg.MaxResults <= 0 || cfg.MaxResults > 10 {
		cfg.MaxResults = 5
	}
	return &webSearchTool{cfg: cfg, client: &http.Client{Timeout: 20 * time.Second}}
}

func (t *webSearchTool) attach(r *Registry) { t.r = r }
func (t *webSearchTool) Name() string       { return "web_search" }
func (t *webSearchTool) Description() string {
	return "Search the web (Google Programmable Search). Returns numbered results with title, URL and snippet. Use it for library/API facts you are unsure of; use web_fetch to read a result page."
}
func (t *webSearchTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
		"query":{"type":"string","description":"Search query"},
		"num":{"type":"integer","description":"Number of results (1-10, default 5)"}},
		"required":["query"]}`)
}

func (t *webSearchTool) Run(ctx context.Context, args map[string]any) Result {
	q := strings.TrimSpace(argString(args, "query", "q", "search"))
	if q == "" {
		return Result{IsError: true, Content: "query is required"}
	}
	key := os.Getenv(t.cfg.APIKeyEnv)
	if key == "" {
		return Result{IsError: true, Content: fmt.Sprintf("web_search is configured but the API key env var %s is not set; export it and restart", EnvNameForDisplay(t.cfg.APIKeyEnv))}
	}
	n := argInt(args, t.cfg.MaxResults, "num", "count", "max_results")
	if n < 1 {
		n = 1
	}
	if n > 10 {
		n = 10
	}
	v := url.Values{}
	v.Set("key", key)
	v.Set("cx", t.cfg.CX)
	v.Set("q", q)
	v.Set("num", fmt.Sprint(n))
	req, err := http.NewRequestWithContext(ctx, "GET", t.cfg.Endpoint+"?"+v.Encode(), nil)
	if err != nil {
		return Result{IsError: true, Content: err.Error()}
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return Result{IsError: true, Content: "web_search: " + err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(body))
		var ge struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &ge) == nil && ge.Error.Message != "" {
			msg = ge.Error.Message
		}
		return Result{IsError: true, Content: fmt.Sprintf("web_search: HTTP %d: %.300s", resp.StatusCode, msg)}
	}
	var out struct {
		Items []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return Result{IsError: true, Content: "web_search: bad response: " + err.Error()}
	}
	if len(out.Items) == 0 {
		return Result{Content: "no results for: " + q}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "results for %q:\n", q)
	for i, it := range out.Items {
		fmt.Fprintf(&b, "%d. %s\n   %s\n   %s\n", i+1, strings.TrimSpace(it.Title), it.Link, strings.Join(strings.Fields(it.Snippet), " "))
	}
	return Result{Content: truncate(b.String(), t.maxOut())}
}

func (t *webSearchTool) maxOut() int {
	if t.r != nil && t.r.MaxOutput() > 0 {
		return t.r.MaxOutput()
	}
	return defaultMaxOutput
}

// ---- web_fetch ---------------------------------------------------------------

type webFetchTool struct {
	client *http.Client
	r      *Registry
}

// NewWebFetch builds the web_fetch tool (GET a URL, return readable text).
func NewWebFetch() Tool {
	return &webFetchTool{client: &http.Client{Timeout: 30 * time.Second}}
}

func (t *webFetchTool) attach(r *Registry) { t.r = r }
func (t *webFetchTool) Name() string       { return "web_fetch" }
func (t *webFetchTool) Description() string {
	return "Fetch a web page (http/https) and return its readable text with HTML removed. Long pages are truncated; use offset to read further."
}
func (t *webFetchTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
		"url":{"type":"string","description":"Absolute http(s) URL"},
		"offset":{"type":"integer","description":"Character offset to start from (for long pages)"}},
		"required":["url"]}`)
}

const webFetchMaxBody = 2 << 20

func (t *webFetchTool) Run(ctx context.Context, args map[string]any) Result {
	raw := strings.TrimSpace(argString(args, "url", "link", "href"))
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Result{IsError: true, Content: "url must be an absolute http(s) URL"}
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return Result{IsError: true, Content: err.Error()}
	}
	req.Header.Set("User-Agent", "be-code/1.0 (+local coding agent)")
	req.Header.Set("Accept", "text/html, text/plain, application/json;q=0.9, */*;q=0.5")
	resp, err := t.client.Do(req)
	if err != nil {
		return Result{IsError: true, Content: "web_fetch: " + err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, webFetchMaxBody))
	if resp.StatusCode != http.StatusOK {
		return Result{IsError: true, Content: fmt.Sprintf("web_fetch: HTTP %d for %s", resp.StatusCode, u)}
	}
	text := string(body)
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "html") || (ct == "" && strings.Contains(strings.ToLower(text[:min(len(text), 512)]), "<html")) {
		text = htmlToText(text)
	}
	off := argInt(args, 0, "offset")
	if off > 0 && off < len(text) {
		text = text[off:]
	} else if off >= len(text) {
		return Result{Content: "(offset past end of page)"}
	}
	max := defaultMaxOutput
	if t.r != nil && t.r.MaxOutput() > 0 {
		max = t.r.MaxOutput()
	}
	header := fmt.Sprintf("%s (%d chars total)\n", u, len(text)+off)
	return Result{Content: header + truncate(text, max)}
}

var (
	reMainRegion  = regexp.MustCompile(`(?is)<(main|article)\b[^>]*>(.*?)</\s*(main|article)\s*>`)
	reScriptStyle = regexp.MustCompile(`(?is)<(script|style|noscript|svg)\b[^>]*>.*?</\s*(script|style|noscript|svg)\s*>`)
	reComments    = regexp.MustCompile(`(?s)<!--.*?-->`)
	reBlockTags   = regexp.MustCompile(`(?i)</?(p|div|br|li|ul|ol|h[1-6]|tr|td|th|table|section|article|header|footer|pre|blockquote|hr|dt|dd)\b[^>]*>`)
	reTags        = regexp.MustCompile(`(?s)<[^>]+>`)
	reBlankLines  = regexp.MustCompile(`\n\s*\n+`)
	reSpaces      = regexp.MustCompile(`[ \t]+`)
)

// htmlToText is a small readable-text extractor: drops scripts/styles and
// comments, turns block tags into newlines, strips the rest, unescapes
// entities, and collapses whitespace.
func htmlToText(h string) string {
	// Prefer the document body over chrome when the page marks it up.
	if m := reMainRegion.FindStringSubmatch(h); m != nil {
		h = m[2]
	}
	h = reScriptStyle.ReplaceAllString(h, " ")
	h = reComments.ReplaceAllString(h, " ")
	h = reBlockTags.ReplaceAllString(h, "\n")
	h = reTags.ReplaceAllString(h, " ")
	h = html.UnescapeString(h)
	h = reSpaces.ReplaceAllString(h, " ")
	lines := strings.Split(h, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	h = strings.Join(lines, "\n")
	h = reBlankLines.ReplaceAllString(h, "\n\n")
	return strings.TrimSpace(h)
}
