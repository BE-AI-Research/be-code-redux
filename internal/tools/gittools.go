package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Git-backed lookups: read-only, confined to the workspace, never prompt.
// They return the function, the hunk or the delta the model needs instead
// of whole files. Outside a repository lookup falls back to the walk-based
// search and the others say so.

const gitTimeout = 20 * time.Second

// BaselineFunc reports where the current task began: the HEAD hash and the
// porcelain status at that moment ("" when unknown).
type BaselineFunc func() (head, dirty string)

// NewGitTools builds lookup, history, show and changes (lookup only when minimal).
func NewGitTools(r *Registry, baseline BaselineFunc, minimal bool) []Tool {
	if baseline == nil {
		baseline = func() (string, string) { return "", "" }
	}
	ts := []Tool{&lookupTool{r: r}}
	if minimal {
		return ts
	}
	return append(ts, &historyTool{r: r}, &showTool{r: r}, &changesTool{r: r, baseline: baseline})
}

// git runs one git command as an argument vector, never as a shell line:
// queries, paths and revisions come from the model, and no quoting scheme
// is safe across sh and PowerShell. RunArgv hands the arguments to git
// untouched on every platform.
func (r *Registry) git(ctx context.Context, args ...string) (string, error) {
	out, err := RunArgv(ctx, r.Root, gitTimeout, "git", args...)
	return strings.TrimRight(out, "\n"), err
}

func (r *Registry) isRepo(ctx context.Context) bool {
	out, err := r.git(ctx, "rev-parse", "--is-inside-work-tree")
	return err == nil && strings.TrimSpace(out) == "true"
}

// relInRoot confines a user path to the workspace and returns it relative.
// A leading ":" is refused outright: git reads such a pathspec as magic
// (":/a.go" is repo-top-relative, ":(top)" and ":!x" likewise), which would
// walk straight out of the workspace even though the path itself cleans to
// something inside it.
func (r *Registry) relInRoot(p string) (string, error) {
	if strings.HasPrefix(strings.TrimSpace(p), ":") {
		return "", fmt.Errorf("bad path")
	}
	abs, err := r.resolve(p)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(r.Root, abs)
	if err != nil {
		return "", err
	}
	rel = filepath.ToSlash(rel)
	if strings.HasPrefix(rel, ":") {
		return "", fmt.Errorf("bad path")
	}
	return rel, nil
}

// badRev rejects a revision the model supplied that git would not read as
// a revision at all. A leading "-" would be taken for an option, and a
// leading ":" for pathspec magic (":(exclude)…", ":/…"), which escapes the
// workspace exactly as it does in relInRoot.
func badRev(rev string) bool {
	rev = strings.TrimSpace(rev)
	return strings.HasPrefix(rev, "-") || strings.HasPrefix(rev, ":")
}

// gitErr reports a failed git call. When git printed something before it
// failed, the partial output is returned with the cause appended, so a
// timeout or a killed process is never mistaken for git's own answer.
func gitErr(err error, out string) Result {
	msg := strings.TrimSpace(out)
	switch {
	case err == nil:
		return Result{IsError: true, Content: msg}
	case msg == "":
		return Result{IsError: true, Content: err.Error()}
	default:
		return Result{IsError: true, Content: msg + "\n(" + err.Error() + ")"}
	}
}

// ---- lookup ----------------------------------------------------------------

type lookupTool struct{ r *Registry }

func (t *lookupTool) Name() string { return "lookup" }
func (t *lookupTool) Description() string {
	return "Find where something is defined or used, fast: git grep over tracked files. Returns file:line: text. symbol=true returns the whole enclosing function for each hit, which is usually what you needed the file for."
}
func (t *lookupTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
		"query":{"type":"string","description":"Text to find (literal unless regex=true)"},
		"path":{"type":"string","description":"Limit to a directory or file"},
		"symbol":{"type":"boolean","description":"Show the enclosing function for each hit"},
		"regex":{"type":"boolean","description":"Treat query as a regular expression"}},
		"required":["query"]}`)
}
func (t *lookupTool) Run(ctx context.Context, args map[string]any) Result {
	q := strings.TrimSpace(argString(args, "query", "pattern", "q", "text"))
	if q == "" {
		return Result{IsError: true, Content: "lookup needs a query"}
	}
	if !t.r.isRepo(ctx) {
		if s, ok := t.r.byName["search"]; ok {
			pat := q
			if !argBool(args, false, "regex") {
				pat = regexp.QuoteMeta(q)
			}
			fallback := map[string]any{"pattern": pat}
			if p := argString(args, "path", "dir"); p != "" {
				fallback["path"] = p
			}
			return s.Run(ctx, fallback)
		}
		return Result{IsError: true, Content: "not a git repository"}
	}
	// --untracked so a file the model just created is findable before it
	// is staged; git still skips anything .gitignore excludes.
	gargs := []string{"grep", "-n", "-I", "--no-color", "--untracked"}
	if !argBool(args, false, "regex") {
		gargs = append(gargs, "-F")
	} else {
		gargs = append(gargs, "-E")
	}
	if argBool(args, false, "symbol", "context", "function") {
		gargs = append(gargs, "-W")
	}
	gargs = append(gargs, "-e", q, "--")
	if p := argString(args, "path", "dir"); p != "" {
		rel, err := t.r.relInRoot(p)
		if err != nil {
			return Result{IsError: true, Content: err.Error()}
		}
		gargs = append(gargs, rel)
	} else {
		gargs = append(gargs, ".")
	}
	out, err := t.r.git(ctx, gargs...)
	if err != nil && strings.TrimSpace(out) == "" {
		return Result{Content: "no matches"}
	}
	if err != nil {
		return gitErr(err, out)
	}
	return Result{Content: truncate(out, t.r.MaxOutput())}
}

// ---- history ---------------------------------------------------------------

type historyTool struct{ r *Registry }

// lineRange is the only shape accepted for a -L line range.
var lineRange = regexp.MustCompile(`^\d+,\d+$`)

func (t *historyTool) Name() string { return "history" }
func (t *historyTool) Description() string {
	return "Why is the code this way: git history for one file. Give path plus one of symbol (that function's own history), lines \"a,b\" (that range's history), query (commits that added or removed the text), or blame=true with lines."
}
func (t *historyTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
		"path":{"type":"string","description":"File to look at"},
		"symbol":{"type":"string","description":"Function or type name: its own history"},
		"lines":{"type":"string","description":"Line range a,b"},
		"query":{"type":"string","description":"Text whose introduction or removal you want to find"},
		"blame":{"type":"boolean","description":"Who last touched each line in lines"},
		"limit":{"type":"integer","description":"Newest N entries (default 10)"}},
		"required":["path"]}`)
}
func (t *historyTool) Run(ctx context.Context, args map[string]any) Result {
	if !t.r.isRepo(ctx) {
		return Result{IsError: true, Content: "not a git repository"}
	}
	rel, err := t.r.relInRoot(argString(args, "path", "file"))
	if err != nil {
		return Result{IsError: true, Content: err.Error()}
	}
	limit := argInt(args, 10, "limit", "n")
	symbol, lines, query := argString(args, "symbol", "function"), argString(args, "lines", "range"), argString(args, "query", "text")
	// Both go inside a -L argument, where git's own syntax would otherwise
	// let a stray ":" or regex form address a different file or range.
	if lines != "" && !lineRange.MatchString(lines) {
		return Result{IsError: true, Content: "lines must be a,b"}
	}
	if strings.Contains(symbol, ":") {
		return Result{IsError: true, Content: "bad symbol"}
	}
	var gargs []string
	switch {
	case argBool(args, false, "blame") && lines != "":
		gargs = []string{"blame", "-L", lines, "--date=short", "--", rel}
	case symbol != "":
		gargs = []string{"log", "-L", ":" + symbol + ":" + rel, "--date=short", "--format=%h %ad %an %s", "-n", strconv.Itoa(limit)}
	case lines != "":
		gargs = []string{"log", "-L", lines + ":" + rel, "--date=short", "--format=%h %ad %an %s", "-n", strconv.Itoa(limit)}
	case query != "":
		gargs = []string{"log", "-S", query, "--date=short", "--format=%h %ad %an %s", "-n", strconv.Itoa(limit), "--", rel}
	default:
		return Result{IsError: true, Content: "history needs one of symbol, lines, query or blame with lines"}
	}
	out, err := t.r.git(ctx, gargs...)
	if err != nil {
		return gitErr(err, out)
	}
	if strings.TrimSpace(out) == "" {
		out = "no history"
	}
	return Result{Content: truncate(out, t.r.MaxOutput())}
}

// ---- show ------------------------------------------------------------------

type showTool struct{ r *Registry }

func (t *showTool) Name() string { return "show" }
func (t *showTool) Description() string {
	return "Read a file as it was at a revision (default HEAD), without touching the working tree. Use offset/limit like read_file."
}
func (t *showTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
		"path":{"type":"string","description":"File path"},
		"rev":{"type":"string","description":"Commit, branch or tag (default HEAD)"},
		"offset":{"type":"integer","description":"1-based first line"},
		"limit":{"type":"integer","description":"Max lines (default 400)"}},
		"required":["path"]}`)
}
func (t *showTool) Run(ctx context.Context, args map[string]any) Result {
	if !t.r.isRepo(ctx) {
		return Result{IsError: true, Content: "not a git repository"}
	}
	rel, err := t.r.relInRoot(argString(args, "path", "file"))
	if err != nil {
		return Result{IsError: true, Content: err.Error()}
	}
	rev := argString(args, "rev", "revision", "commit")
	if rev == "" {
		rev = "HEAD"
	}
	if badRev(rev) {
		return Result{IsError: true, Content: "bad revision"}
	}
	// "rev:path" is resolved against the repository top, not the working
	// directory, so a workspace nested inside a larger repo would read the
	// top-level file of the same name. "./" makes it cwd-relative. The
	// terminating "--" stops git reading the argument as a pathspec when
	// it is not a valid object name.
	out, err := t.r.git(ctx, "show", rev+":./"+rel, "--")
	if err != nil {
		return gitErr(err, out)
	}
	lines := strings.Split(out, "\n")
	offset := argInt(args, 1, "offset", "start")
	limit := argInt(args, 400, "limit", "count")
	if offset < 1 {
		offset = 1
	}
	if offset > len(lines) {
		return Result{IsError: true, Content: fmt.Sprintf("offset %d beyond end of file (%d lines)", offset, len(lines))}
	}
	end := offset - 1 + limit
	if end > len(lines) {
		end = len(lines)
	}
	var b strings.Builder
	for i := offset - 1; i < end; i++ {
		fmt.Fprintf(&b, "%5d\t%s\n", i+1, lines[i])
	}
	if end < len(lines) {
		fmt.Fprintf(&b, "... (%d more lines; call show again with offset=%d)\n", len(lines)-end, end+1)
	}
	return Result{Content: truncate(b.String(), t.r.MaxOutput())}
}

// ---- changes ---------------------------------------------------------------

type changesTool struct {
	r        *Registry
	baseline BaselineFunc
}

func (t *changesTool) Name() string { return "changes" }
func (t *changesTool) Description() string {
	return "What this task has changed so far: a diff stat against the point where the task began (or since=<rev>), and with path the full diff of one file. Use it before verifying or reviewing instead of re-reading whole files."
}
func (t *changesTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
		"since":{"type":"string","description":"Revision to compare against (default: where the task began)"},
		"path":{"type":"string","description":"Show the full diff of this file"}},
		"required":[]}`)
}
func (t *changesTool) Run(ctx context.Context, args map[string]any) Result {
	if !t.r.isRepo(ctx) {
		return Result{IsError: true, Content: "not a git repository"}
	}
	since := argString(args, "since", "rev", "from")
	head, dirty := "", ""
	if since == "" {
		head, dirty = t.baseline()
		since = head
	}
	if since == "" {
		since = "HEAD"
	}
	if badRev(since) {
		return Result{IsError: true, Content: "bad revision"}
	}
	var out string
	var err error
	if p := argString(args, "path", "file"); p != "" {
		rel, rerr := t.r.relInRoot(p)
		if rerr != nil {
			return Result{IsError: true, Content: rerr.Error()}
		}
		out, err = t.r.git(ctx, "diff", "--no-color", since, "--", rel)
	} else {
		// "." keeps the stat inside the workspace: without a pathspec git
		// diffs the whole repository, which a nested workspace must not see.
		out, err = t.r.git(ctx, "diff", "--stat", "--no-color", since, "--", ".")
	}
	if err != nil {
		return gitErr(err, out)
	}
	if strings.TrimSpace(out) == "" {
		out = "no changes since " + since
	}
	if dirty != "" {
		var pre []string
		for _, l := range strings.Split(dirty, "\n") {
			if len(l) > 3 {
				pre = append(pre, strings.TrimSpace(l[3:]))
			}
		}
		if len(pre) > 0 {
			out += "\n(already modified before this task began: " + strings.Join(pre, ", ") + ")"
		}
	}
	return Result{Content: truncate(out, t.r.MaxOutput())}
}
