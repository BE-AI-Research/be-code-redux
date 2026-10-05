package browser

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

// AXValue is a protocol Accessibility.AXValue: a type tag and a JSON value.
type AXValue struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
}

// Str is the value as text: a JSON string unquoted, anything else as
// written ("true", "1").
func (v *AXValue) Str() string {
	if v == nil || len(v.Value) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(v.Value, &s) == nil {
		return s
	}
	return string(v.Value)
}

// AXProperty is one named accessibility property.
type AXProperty struct {
	Name  string  `json:"name"`
	Value AXValue `json:"value"`
}

// AXNode is one node of Accessibility.getFullAXTree.
type AXNode struct {
	NodeID           string       `json:"nodeId"`
	Ignored          bool         `json:"ignored"`
	Role             *AXValue     `json:"role"`
	Name             *AXValue     `json:"name"`
	Value            *AXValue     `json:"value"`
	Properties       []AXProperty `json:"properties"`
	ParentID         string       `json:"parentId"`
	ChildIDs         []string     `json:"childIds"`
	BackendDOMNodeID int          `json:"backendDOMNodeId"`
}

func (n *AXNode) prop(name string) string {
	for _, p := range n.Properties {
		if p.Name == name {
			return p.Value.Str()
		}
	}
	return ""
}

// Dialog is a pending confirm or prompt. It blocks the page, so it is shown
// instead of the page.
type Dialog struct {
	Type, Message         string
	AcceptRef, DismissRef string
}

// RenderInput is everything Render needs. Render calls nothing: it is a
// pure function over what the Page fetched, so it is tested without a
// browser.
type RenderInput struct {
	Title, URL string
	Nodes      []AXNode
	Refs       *RefTable
	// Sensitive maps the backend id of each password, one-time-code and card
	// field to its kind ("password", "one-time code", "card"). Such a field
	// is shown with its label and never its value (spec §3.4).
	Sensitive map[int]string
	// HideAllValues is set when the sensitive-field query failed: every
	// field's value is left out rather than risk one we could not check.
	HideAllValues bool
	// InView, when non-nil, holds the backend ids inside the viewport. It
	// enables the last rung of the budget ladder.
	InView  map[int]bool
	Budget  int
	Loading bool
	Dialog  *Dialog
}

// Rendered is a snapshot plus what the tool needs to name an element in an
// approval prompt.
type Rendered struct {
	Text string
	// NeedInView: over budget with InView nil. Fetch the viewport and
	// render again; Text meanwhile is a usable hard-cut fallback.
	NeedInView bool
	// Labels maps each ref to `role "name"`.
	Labels map[string]string
}

const (
	truncatedNote = "(snapshot truncated — scroll to see more)"
	loadingNote   = "(page still loading)"
	defaultBudget = 12000
)

var interactiveRoles = map[string]bool{
	"button": true, "link": true, "textbox": true, "searchbox": true, "combobox": true,
	"checkbox": true, "radio": true, "switch": true, "slider": true, "spinbutton": true,
	"menuitem": true, "menuitemcheckbox": true, "menuitemradio": true, "option": true,
	"tab": true, "treeitem": true, "listbox": true,
}

// valueRoles show their current value: ` = "…"`.
var valueRoles = map[string]bool{"textbox": true, "searchbox": true, "combobox": true, "spinbutton": true, "slider": true}

// structuralRoles are worth a line of their own even unnamed — except the
// quietUnnamed ones, which are noise without a name.
var structuralRoles = map[string]bool{
	"main": true, "navigation": true, "banner": true, "contentinfo": true, "complementary": true,
	"form": true, "search": true, "region": true, "list": true, "table": true, "row": true,
	"cell": true, "columnheader": true, "rowheader": true, "dialog": true, "alertdialog": true,
	"article": true, "tablist": true, "menu": true, "menubar": true, "toolbar": true,
	"tree": true, "grid": true, "group": true,
}

var quietUnnamed = map[string]bool{"region": true, "row": true, "cell": true, "group": true}

// transparentRoles never get a line, named or not; their children move up.
var transparentRoles = map[string]bool{
	"RootWebArea": true, "WebArea": true, "generic": true, "none": true,
	"presentation": true, "paragraph": true, "Section": true,
}

// line is one rendered element. Parts are kept raw so each rung of the
// budget ladder can cut them differently.
type line struct {
	role, name, value, target, marker, states, ref, level string
	hasValue, heading, interactive, isText, listItem      bool
	backend                                               int
	children                                              []*line
}

func (l *line) format(cut int) string {
	switch {
	case l.isText:
		return "text " + quote(clip(l.name, cut))
	case l.heading:
		s := "heading"
		if l.level != "" {
			s += "[" + l.level + "]"
		}
		return s + " " + quote(clip(l.name, cut))
	}
	s := l.role
	if l.name != "" {
		s += " " + quote(clip(l.name, cut))
	}
	if l.marker != "" {
		s += " (" + l.marker + ")"
	}
	if l.ref != "" {
		s += " [" + l.ref + "]"
	}
	if l.hasValue {
		s += " = " + quote(clip(l.value, cut))
	}
	if l.target != "" {
		s += " → " + l.target
	}
	if l.states != "" {
		s += " (" + l.states + ")"
	}
	return s
}

type builder struct {
	in       RenderInput
	byID     map[string]*AXNode
	pageHost string
	labels   map[string]string
}

// buildLines turns the tree into lines once; every rung emits from them.
func buildLines(in RenderInput) ([]*line, map[string]string) {
	b := &builder{in: in, byID: map[string]*AXNode{}, labels: map[string]string{}}
	if u, err := url.Parse(in.URL); err == nil {
		b.pageHost = u.Hostname()
	}
	for i := range in.Nodes {
		b.byID[in.Nodes[i].NodeID] = &in.Nodes[i]
	}
	var roots []*line
	for i := range in.Nodes {
		if in.Nodes[i].ParentID == "" {
			roots = append(roots, b.walk(in.Nodes[i].NodeID, false)...)
		}
	}
	return roots, b.labels
}

// walk renders one node. skipText is set under an element whose name
// already carries its text (a link, a button, a heading).
func (b *builder) walk(id string, skipText bool) []*line {
	n := b.byID[id]
	if n == nil {
		return nil
	}
	if n.Ignored {
		return b.children(n, skipText)
	}
	role, name := n.Role.Str(), collapse(n.Name.Str())
	switch {
	case role == "InlineTextBox" || role == "LineBreak":
		return nil
	case role == "StaticText":
		if skipText || name == "" {
			return nil
		}
		return []*line{{isText: true, name: name, backend: n.BackendDOMNodeID}}
	case role == "heading":
		return []*line{{heading: true, name: name, level: n.prop("level"), backend: n.BackendDOMNodeID, children: b.children(n, true)}}
	case interactiveRoles[role]:
		return []*line{b.interactive(n, role, name)}
	case role == "image" || role == "img":
		if name == "" {
			return nil
		}
		return []*line{{role: "img", name: name, backend: n.BackendDOMNodeID}}
	case role == "listitem":
		kids := b.children(n, skipText)
		switch len(kids) {
		case 0:
			return nil
		case 1:
			kids[0].listItem = true
			return kids
		}
		return []*line{{role: "listitem", listItem: true, backend: n.BackendDOMNodeID, children: kids}}
	case transparentRoles[role]:
		return b.children(n, skipText)
	case structuralRoles[role] && !(name == "" && quietUnnamed[role]):
		return []*line{{role: role, name: name, backend: n.BackendDOMNodeID, children: b.children(n, skipText)}}
	case name != "" && !structuralRoles[role]:
		return []*line{{role: role, name: name, backend: n.BackendDOMNodeID, children: b.children(n, skipText)}}
	}
	return b.children(n, skipText)
}

func (b *builder) interactive(n *AXNode, role, name string) *line {
	l := &line{role: role, name: name, interactive: true, backend: n.BackendDOMNodeID}
	if n.BackendDOMNodeID != 0 && b.in.Refs != nil {
		l.ref = b.in.Refs.Ref(n.BackendDOMNodeID)
		label := role
		if name != "" {
			label += " " + quote(clip(name, 80))
		}
		b.labels[l.ref] = label
	}
	kind, sensitive := b.in.Sensitive[n.BackendDOMNodeID]
	if sensitive {
		l.marker = kind
	}
	if valueRoles[role] && !sensitive && !b.in.HideAllValues {
		l.value, l.hasValue = collapse(n.Value.Str()), true
	}
	if role == "link" {
		l.target = b.shortTarget(n.prop("url"))
	}
	l.states = states(n)
	switch {
	case sensitive:
		// A sensitive field's value can otherwise leak through its
		// descendants — a card-expiry <select>'s chosen option, for
		// instance — so it renders with no subtree at all (spec §3.4).
	case (role == "combobox" || role == "listbox") && b.in.HideAllValues:
		// The sensitive-field query failed, so any control might be
		// sensitive: a combobox/listbox's options carry its value, so
		// they are withheld along with every other value (spec §3.4).
	default:
		l.children = b.children(n, true) // a listbox's options still render; text does not
	}
	return l
}

func (b *builder) children(n *AXNode, skipText bool) []*line {
	var out []*line
	for _, c := range n.ChildIDs {
		out = append(out, b.walk(c, skipText)...)
	}
	return out
}

func states(n *AXNode) string {
	var s []string
	for _, p := range n.Properties {
		v := p.Value.Str()
		switch p.Name {
		case "disabled", "required", "focused", "selected", "pressed":
			if v == "true" {
				s = append(s, p.Name)
			}
		case "checked":
			switch v {
			case "true":
				s = append(s, "checked")
			case "mixed":
				s = append(s, "mixed")
			}
		case "expanded":
			switch v {
			case "true":
				s = append(s, "expanded")
			case "false":
				s = append(s, "collapsed")
			}
		case "invalid":
			if v != "" && v != "false" {
				s = append(s, "invalid")
			}
		}
	}
	return strings.Join(s, ", ")
}

// shortTarget writes a link's target briefly: a path on this page's host,
// host and path elsewhere, the fragment dropped.
func (b *builder) shortTarget(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return clip(raw, 60)
	}
	s := u.Host + u.EscapedPath()
	if strings.EqualFold(u.Hostname(), b.pageHost) {
		s = u.EscapedPath()
		if s == "" {
			s = "/"
		}
	}
	if u.RawQuery != "" {
		s += "?" + u.RawQuery
	}
	return clip(s, 60)
}

// rung is one step of the budget ladder (spec §2.2): cut text, then cut
// lists, then summarise what is off screen.
type rung struct {
	cut       int
	listKeep  int
	offscreen bool
}

var rungs = []rung{{cut: 300}, {cut: 80}, {cut: 80, listKeep: 5}, {cut: 80, listKeep: 5, offscreen: true}}

// Render turns an accessibility tree into the outline the model reads.
func Render(in RenderInput) Rendered {
	budget := in.Budget
	if budget <= 0 {
		budget = defaultBudget
	}
	head := PageLine(in.Title, in.URL)
	if in.Loading {
		head += "\n" + loadingNote
	}
	if d := in.Dialog; d != nil {
		labels := map[string]string{
			d.AcceptRef:  "accept dialog " + quote(clip(d.Message, 80)),
			d.DismissRef: "dismiss dialog " + quote(clip(d.Message, 80)),
		}
		text := head + "\n  dialog " + d.Type + " " + quote(clip(d.Message, 300)) +
			"\n    accept [" + d.AcceptRef + "]\n    dismiss [" + d.DismissRef + "]"
		return Rendered{Text: text, Labels: labels}
	}
	roots, labels := buildLines(in)
	if len(roots) == 0 {
		return Rendered{Text: head + "\n  (empty page)", Labels: labels}
	}
	var body string
	for _, r := range rungs {
		if r.offscreen && in.InView == nil {
			return Rendered{Text: head + "\n" + hardCut(body, budget), NeedInView: true, Labels: labels}
		}
		body = emit(roots, 1, r, in.InView)
		if len(body) <= budget {
			return Rendered{Text: head + "\n" + body, Labels: labels}
		}
	}
	return Rendered{Text: head + "\n" + hardCut(body, budget), Labels: labels}
}

func emit(ls []*line, depth int, r rung, inView map[int]bool) string {
	var sb strings.Builder
	emitInto(&sb, ls, depth, r, inView)
	return strings.TrimRight(sb.String(), "\n")
}

func emitInto(sb *strings.Builder, ls []*line, depth int, r rung, inView map[int]bool) {
	pad := strings.Repeat("  ", depth)
	items, hidden, offscreen := 0, 0, 0
	for _, l := range ls {
		if r.offscreen && !keep(l, inView) {
			offscreen += count(l)
			continue
		}
		if l.listItem && r.listKeep > 0 {
			items++
			if items > r.listKeep {
				hidden++
				continue
			}
		}
		sb.WriteString(pad + l.format(r.cut) + "\n")
		emitInto(sb, l.children, depth+1, r, inView)
	}
	if hidden > 0 {
		fmt.Fprintf(sb, "%s(+%d more items)\n", pad, hidden)
	}
	if offscreen > 0 {
		fmt.Fprintf(sb, "%s(%d more off screen)\n", pad, offscreen)
	}
}

// keep decides the off-screen rung: headings always survive, a leaf or an
// interactive element survives when it is in view, and a container
// survives when anything under it does.
func keep(l *line, inView map[int]bool) bool {
	if l.heading {
		return true
	}
	if (l.interactive || l.isText || len(l.children) == 0) && inView[l.backend] {
		return true
	}
	for _, c := range l.children {
		if keep(c, inView) {
			return true
		}
	}
	return false
}

func count(l *line) int {
	n := 1
	for _, c := range l.children {
		n += count(c)
	}
	return n
}

// hardCut is the last resort: cut at a line boundary inside the budget
// (or a rune boundary, for one enormous line) and say so.
func hardCut(body string, budget int) string {
	if len(body) <= budget {
		return body
	}
	i := strings.LastIndexByte(body[:budget], '\n')
	if i <= 0 {
		i = budget
		for i > 0 && !utf8.RuneStart(body[i]) {
			i--
		}
	}
	return body[:i] + "\n" + truncatedNote
}

// PageLine is the "page:" line: the title and the URL without its scheme.
func PageLine(title, rawURL string) string {
	u := rawURL
	if p, err := url.Parse(rawURL); err == nil && (p.Scheme == "http" || p.Scheme == "https") {
		u = p.Host + p.EscapedPath()
		if p.RawQuery != "" {
			u += "?" + p.RawQuery
		}
	}
	t := collapse(title)
	switch {
	case t == "" && u == "":
		return "page: (blank)"
	case t == "":
		return "page: " + clip(u, 120)
	case u == "":
		// No address: the separator the host is read back by must not come
		// from the page's own title.
		return "page: " + clip(strings.ReplaceAll(t, " — ", " - "), 120)
	}
	return "page: " + clip(t, 120) + " — " + clip(u, 120)
}

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// clip collapses whitespace and cuts to n runes, marking the cut with "…".
func clip(s string, n int) string {
	s = collapse(s)
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

func quote(s string) string { return strconv.Quote(s) }
