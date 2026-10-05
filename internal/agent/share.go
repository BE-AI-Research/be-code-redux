package agent

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/brown-enterprises/be-code/internal/browser"
	"github.com/brown-enterprises/be-code/internal/provider"
)

// Page text already in the conversation when the main model goes online
// (online spec §2.2, owner's ruling in Task 8's review). Every read from now
// on asks share_page itself; what was read before — while the model was
// local, in a resumed session, or for another online provider — was never
// asked about for this one. So before the first request to an online main
// model, and again whenever the share gate is set or names another
// provider, each site found in the history's browser and web_fetch results
// is asked about once (in the order it first appears), and web_search
// results once in all; a site refused (or withdrawn, or unanswered) has
// only its own results replaced with a stub. A result whose site cannot be
// told is stubbed without asking. A local session is never touched.

// webSharedWith is the provider the history's page text was last settled
// for ("" while local); guarded by onlineMu, written under turnMu.

// settleEarlierWebText runs the pass described above when it is due. The
// caller holds turnMu: it rewrites History.Messages, which requestFor's
// rewrittenSinceSent then notices like any other rewrite. The questions are
// asked under turnMu too, exactly as a tool's own approval is.
func (a *Agent) settleEarlierWebText(ctx context.Context) {
	if a.Tools == nil || a.History == nil {
		return
	}
	key := a.Tools.ShareProvider()
	a.onlineMu.Lock()
	due := key != a.webSharedWith
	a.webSharedWith = key
	a.onlineMu.Unlock()
	if !due || key == "" {
		return
	}
	msgs := a.History.Messages
	type found struct {
		idx    int
		tool   string
		host   string
		search bool
	}
	var items []found
	var order []string
	seen := map[string]bool{}
	fromBrowser := map[string]bool{}
	searchSeen := false
	for i, m := range msgs {
		tool, n := webToolOf(m, msgs)
		if tool == "" {
			continue
		}
		it := found{idx: i, tool: tool}
		switch {
		case n > 1:
			// Several web results in one embedded message are judged whole
			// (a page can print its own </tool_result>): unknown site.
		case tool == "web_search":
			it.search, searchSeen = true, true
		default:
			it.host = webResultHost(tool, m)
		}
		if it.host != "" && !seen[it.host] {
			seen[it.host] = true
			order = append(order, it.host)
		}
		if it.host != "" && tool == "browser" {
			fromBrowser[it.host] = true
		}
		items = append(items, it)
	}
	if len(items) == 0 {
		return
	}
	shared := map[string]bool{}
	for _, h := range order {
		shared[h] = a.Tools.ShareEarlier(ctx, h, fromBrowser[h])
	}
	searchOK := searchSeen && a.Tools.ShareEarlierSearch(ctx)
	stubbed := 0
	for _, it := range items {
		var stub string
		switch {
		case it.search && !searchOK:
			stub = fmt.Sprintf("(web search results are not shown; they were not shared with %s)", key)
		case it.search:
			continue
		case it.host == "":
			stub = fmt.Sprintf("(page text is not shown; it was not shared with %s)", key)
		case !shared[it.host]:
			stub = fmt.Sprintf("(page text from %s is not shown; it was not shared with %s)", it.host, key)
		default:
			continue
		}
		m := &a.History.Messages[it.idx]
		if m.Role == provider.RoleTool {
			m.Content = stub
		} else {
			m.Content = fmt.Sprintf("<tool_result name=%q status=%q>\n%s\n</tool_result>", it.tool, "withheld", stub)
		}
		stubbed++
	}
	if stubbed > 0 {
		a.notice("%d earlier web result(s) not shared with %s are withheld from it", stubbed, key)
	}
}

// earlierWebUnsettled reports whether page text in the history has not yet
// been settled for the online provider now in force: the model-written
// handoff then leaves every web result out, as the pass above would have
// done for any site nobody was asked about.
func (a *Agent) earlierWebUnsettled() bool {
	if a.Tools == nil {
		return false
	}
	key := a.Tools.ShareProvider()
	a.onlineMu.Lock()
	defer a.onlineMu.Unlock()
	return key != "" && key != a.webSharedWith
}

// webToolOf names the web tool m is a result of ("" for none) and, for an
// embedded <tool_result> message, how many web results it carries.
func webToolOf(m provider.Message, msgs []provider.Message) (string, int) {
	if !webResult(m, msgs) {
		return "", 0
	}
	names := []string{"browser", "web_fetch", "web_search"}
	if m.Role == provider.RoleTool {
		for _, n := range names {
			if resultFrom(m, msgs, n) {
				return n, 1
			}
		}
		return "", 0
	}
	tool, count := "", 0
	for _, n := range names {
		if c := strings.Count(m.Content, `<tool_result name="`+n+`"`); c > 0 {
			tool, count = n, count+c
		}
	}
	return tool, count
}

// webResultHost is the site a browser or web_fetch result's text came from,
// read from the line the tool itself writes first — web_fetch's address
// header, the browser's "page: <title> — <host/path>" line — or "" when it
// cannot be told (a tab list, an error, a page with no address).
func webResultHost(tool string, m provider.Message) string {
	body := m.Content
	if m.Role != provider.RoleTool {
		if i := strings.Index(body, `<tool_result name="`+tool+`"`); i >= 0 {
			body = body[i:]
			if nl := strings.IndexByte(body, '\n'); nl >= 0 {
				body = body[nl+1:]
			} else {
				return ""
			}
		}
	}
	switch tool {
	case "web_fetch":
		line, _, _ := strings.Cut(body, "\n")
		i := strings.LastIndex(line, " (")
		if i < 0 || !strings.HasSuffix(line, " chars total)") {
			return ""
		}
		u, err := url.Parse(line[:i])
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return ""
		}
		return browser.NormalizeHost(u.Host)
	case "browser":
		for _, line := range strings.Split(body, "\n") {
			rest, ok := strings.CutPrefix(line, "page: ")
			if !ok {
				continue
			}
			if i := strings.LastIndex(rest, " — "); i >= 0 {
				rest = rest[i+len(" — "):]
			}
			host := rest
			if i := strings.IndexAny(host, "/?#"); i >= 0 {
				host = host[:i]
			}
			host = browser.NormalizeHost(host)
			// A title alone ("page: Dashboard") names no site.
			if host == "" || strings.ContainsAny(host, " \t") ||
				!(strings.Contains(host, ".") || host == "localhost") {
				return ""
			}
			return host
		}
	}
	return ""
}
