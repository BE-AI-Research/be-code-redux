package agent

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/brown-enterprises/be-code/internal/browser"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/tools"
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
		if n == 1 && contentFree(tool, resultBody(tool, m)) {
			continue // nothing of a page in it, or already stubbed
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
// been settled for the online provider now in force (the next request to it
// runs the pass above first).
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

// stubRe matches the stubs this pass writes, so a result already stubbed is
// never rewritten, asked about or counted again.
var stubRe = regexp.MustCompile(`^\((page text( from \S+)? is not shown; it was not shared with .+|web search results are not shown; they were not shared with .+)\)$`)

// resultBody is what tool wrote for a single result m: a native message's
// content, or the inside of an embedded message's one <tool_result> block,
// without the harness's own state block.
func resultBody(tool string, m provider.Message) string {
	body := strings.TrimSpace(StripHarnessState(m.Content))
	if m.Role == provider.RoleTool {
		return body
	}
	if !strings.HasPrefix(body, `<tool_result name="`+tool+`"`) {
		return ""
	}
	nl := strings.IndexByte(body, '\n')
	if nl < 0 {
		return ""
	}
	body = body[nl+1:]
	if i := strings.LastIndex(body, "</tool_result>"); i >= 0 {
		body = body[:i]
	}
	return strings.TrimSpace(body)
}

// contentFree reports a result that holds no page text to settle: a stub
// already written, a web_fetch or web_search error or empty answer (only a
// success begins with the tool's own header), or a browser result the tool
// itself withheld — its reason straight after the header, and no page line
// anywhere (a page's own text never comes before the page line, and every
// result that shows a page has one).
func contentFree(tool, body string) bool {
	if stubRe.MatchString(body) {
		return true
	}
	first, _, _ := strings.Cut(body, "\n")
	switch tool {
	case "web_fetch":
		return fetchHeaderHost(first) == "" && !strings.HasSuffix(first, " chars total)")
	case "web_search":
		return !strings.HasPrefix(body, "results for ")
	case "browser":
		lines := strings.Split(body, "\n")
		if len(lines) < 2 || lines[0] != tools.WebHeader ||
			!strings.HasPrefix(lines[1], "the page on ") || !strings.Contains(lines[1], " is not shown") {
			return false
		}
		for _, l := range lines {
			if strings.HasPrefix(l, "page: ") {
				return false
			}
		}
		return true
	}
	return false
}

// webResultHost is the site a browser or web_fetch result's text came from,
// read only from the line the tool itself writes first — web_fetch's
// address header, the browser's page line straight after its fixed header —
// or "" when it cannot be told (a tab list, an error, a page with no
// address, or an older result whose page line came after a note, which a
// page could have forged).
func webResultHost(tool string, m provider.Message) string {
	lines := strings.SplitN(resultBody(tool, m), "\n", 3)
	switch tool {
	case "web_fetch":
		return fetchHeaderHost(lines[0])
	case "browser":
		if len(lines) < 2 || lines[0] != tools.WebHeader {
			return ""
		}
		return pageLineHost(lines[1])
	}
	return ""
}

// fetchHeaderHost is the host of web_fetch's "<url> (N chars total)" line.
func fetchHeaderHost(line string) string {
	i := strings.LastIndex(line, " (")
	if i < 0 || !strings.HasSuffix(line, " chars total)") {
		return ""
	}
	u, err := url.Parse(line[:i])
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return browser.NormalizeHost(u.Host)
}

// hostRe is a host as PageLine writes it: a name or address, maybe a port.
var hostRe = regexp.MustCompile(`^(\[[0-9a-fA-F:.]+\]|[A-Za-z0-9.-]+)(:[0-9]+)?$`)

// pageLineHost is the host of a "page: <title> — <host/path>" line, as
// browser.PageLine writes it for an http(s) page; "" for anything else.
func pageLineHost(line string) string {
	rest, ok := strings.CutPrefix(line, "page: ")
	if !ok {
		return ""
	}
	i := strings.LastIndex(rest, " — ")
	if i < 0 {
		return "" // a title alone, or an address alone: no site to name for sure
	}
	host := rest[i+len(" — "):]
	if j := strings.IndexAny(host, "/?#"); j >= 0 {
		host = host[:j]
	}
	if !hostRe.MatchString(host) {
		return ""
	}
	h := browser.NormalizeHost(host)
	if !strings.Contains(h, ".") && h != "localhost" && !strings.Contains(h, ":") {
		return ""
	}
	return h
}
