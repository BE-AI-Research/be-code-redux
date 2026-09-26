package browser

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/browser/browsertest"
)

func formNodes(t *testing.T) []AXNode {
	t.Helper()
	var tree struct {
		Nodes []AXNode `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(browsertest.FormTree), &tree); err != nil {
		t.Fatal(err)
	}
	return tree.Nodes
}

func formInput(t *testing.T) RenderInput {
	return RenderInput{
		Title: "Sign in — Acme", URL: "https://acme.test/login",
		Nodes: formNodes(t), Refs: NewRefTable(),
		Sensitive: map[int]string{60: "password", 130: "card"},
		Budget:    12000,
	}
}

func axStr(typ, s string) *AXValue {
	b, _ := json.Marshal(s)
	return &AXValue{Type: typ, Value: b}
}

func ax(id, parent, role, name string, backend int, props []AXProperty, children ...string) AXNode {
	return AXNode{NodeID: id, ParentID: parent, Role: axStr("role", role), Name: axStr("computedString", name),
		Properties: props, ChildIDs: children, BackendDOMNodeID: backend}
}

func axProps(kv ...string) []AXProperty {
	var out []AXProperty
	for i := 0; i+1 < len(kv); i += 2 {
		out = append(out, AXProperty{Name: kv[i], Value: *axStr("string", kv[i+1])})
	}
	return out
}

// listTree is a page whose main holds a heading and a list of n links.
// Link i has backend 2000+i.
func listTree(n int) []AXNode {
	var items []string
	for i := 1; i <= n; i++ {
		items = append(items, "li"+strconv.Itoa(i))
	}
	nodes := []AXNode{
		ax("1", "", "RootWebArea", "Results", 1, nil, "2"),
		ax("2", "1", "main", "", 2, nil, "3", "4"),
		ax("3", "2", "heading", "Checks", 3, axProps("level", "2")),
		ax("4", "2", "list", "Checks", 4, nil, items...),
	}
	for i := 1; i <= n; i++ {
		li, a := "li"+strconv.Itoa(i), "a"+strconv.Itoa(i)
		nodes = append(nodes,
			ax(li, "4", "listitem", "", 1000+i, nil, a),
			ax(a, li, "link", "Check "+strconv.Itoa(i), 2000+i, axProps("url", "https://acme.test/checks/"+strconv.Itoa(i))))
	}
	return nodes
}

func body(r Rendered) string {
	_, b, _ := strings.Cut(r.Text, "\n")
	return b
}

func TestRenderForm(t *testing.T) {
	got := Render(formInput(t)).Text
	want := `page: Sign in — Acme — acme.test/login
  main
    heading[1] "Sign in"
    textbox "Email" [e1] = "ann@example.com" (required)
    textbox "Password" (password) [e2]
    checkbox "Remember me" [e3] (checked)
    button "Sign in" [e4] (disabled)
    link "Forgot password?" [e5] → /reset?from=login
    text "Need an account? Ask your admin."
    textbox "Card number" (card) [e6]`
	if got != want {
		t.Fatalf("got:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestRenderNeverShowsSensitiveValues(t *testing.T) {
	r := Render(formInput(t))
	for _, secret := range []string{"hunter2", "4111"} {
		if strings.Contains(r.Text, secret) {
			t.Fatalf("snapshot shows %q:\n%s", secret, r.Text)
		}
	}
}

func TestRenderHideAllValuesFailsClosed(t *testing.T) {
	in := formInput(t)
	in.Sensitive, in.HideAllValues = nil, true
	r := Render(in)
	for _, v := range []string{"ann@example.com", "hunter2", "4111"} {
		if strings.Contains(r.Text, v) {
			t.Fatalf("HideAllValues still shows %q:\n%s", v, r.Text)
		}
	}
	if !strings.Contains(r.Text, `textbox "Email" [e1]`) {
		t.Fatalf("the field itself must still be shown:\n%s", r.Text)
	}
}

func TestRenderLabelsDescribeRefs(t *testing.T) {
	r := Render(formInput(t))
	if r.Labels["e4"] != `button "Sign in"` || r.Labels["e2"] != `textbox "Password"` {
		t.Fatalf("labels %v", r.Labels)
	}
}

func TestRefsStableAcrossRendersAndInsertions(t *testing.T) {
	in := formInput(t)
	first := Render(in).Text
	if Render(in).Text != first {
		t.Fatal("a second render of the same page renumbered it")
	}
	// A field inserted above Email keeps Email as e1 and takes the next ref.
	var nodes []AXNode
	for _, n := range in.Nodes {
		if n.NodeID == "2" {
			n.ChildIDs = append([]string{"99"}, n.ChildIDs...)
		}
		nodes = append(nodes, n)
	}
	nodes = append(nodes, ax("99", "2", "textbox", "Name", 35, nil))
	in.Nodes = nodes
	got := Render(in).Text
	if !strings.Contains(got, `textbox "Name" [e7]`) || !strings.Contains(got, `textbox "Email" [e1]`) {
		t.Fatalf("refs moved:\n%s", got)
	}
}

func TestRefTableResetForgetsRefs(t *testing.T) {
	rt := NewRefTable()
	r := rt.Ref(40)
	rt.Reset()
	if _, ok := rt.Lookup(r); ok {
		t.Fatal("a ref survived a reset")
	}
	if rt.Ref(99) != "e1" {
		t.Fatal("numbering did not restart")
	}
}

func TestNormalizeRef(t *testing.T) {
	for _, in := range []string{"e14", "E14", "14", "[e14]", "ref=e14", " e14 ", "[14]"} {
		if got := NormalizeRef(in); got != "e14" {
			t.Errorf("NormalizeRef(%q) = %q", in, got)
		}
	}
	if NormalizeRef("  ") != "" {
		t.Error("blank ref is not empty")
	}
}

func TestRenderEmptyTree(t *testing.T) {
	for _, nodes := range [][]AXNode{nil, {ax("1", "", "RootWebArea", "", 1, nil)}} {
		r := Render(RenderInput{URL: "about:blank", Nodes: nodes, Refs: NewRefTable()})
		if r.Text != "page: about:blank\n  (empty page)" {
			t.Fatalf("empty page rendered as %q", r.Text)
		}
	}
}

func TestRenderLoadingNote(t *testing.T) {
	in := formInput(t)
	in.Loading = true
	if !strings.HasPrefix(Render(in).Text, "page: Sign in — Acme — acme.test/login\n(page still loading)\n") {
		t.Fatal("no loading note under the page line")
	}
}

func TestRenderListKeepsFirstItems(t *testing.T) {
	in := RenderInput{Title: "Results", URL: "https://acme.test/checks", Nodes: listTree(20), Refs: NewRefTable(), Budget: 1 << 20}
	full := body(Render(in))
	in.Budget = len(full) - 1
	got := Render(in)
	if !strings.Contains(got.Text, `link "Check 5"`) || strings.Contains(got.Text, `link "Check 6"`) {
		t.Fatalf("list not cut to its first five items:\n%s", got.Text)
	}
	if !strings.Contains(got.Text, "(+15 more items)") {
		t.Fatalf("no count of the hidden items:\n%s", got.Text)
	}
}

func TestRenderCutsLongTextAtRungOne(t *testing.T) {
	long := strings.Repeat("word ", 80) // 400 characters
	nodes := []AXNode{
		ax("1", "", "RootWebArea", "Article", 1, nil, "2"),
		ax("2", "1", "StaticText", long, 2, nil),
	}
	in := RenderInput{URL: "https://acme.test/a", Nodes: nodes, Refs: NewRefTable(), Budget: 1 << 20}
	full := body(Render(in))
	in.Budget = len(full) - 1
	got := body(Render(in))
	if !strings.HasSuffix(got, `…"`) || len([]rune(got)) > 100 {
		t.Fatalf("long text not cut to 80 characters: %q", got)
	}
}

// bodyAtRung renders with one rung of the ladder, for sizing budgets in the
// tests below.
func bodyAtRung(in RenderInput, i int) string {
	roots, _ := buildLines(in)
	return emit(roots, 1, rungs[i], in.InView)
}

func TestRenderOffscreenAsksForViewportThenSummarises(t *testing.T) {
	in := RenderInput{Title: "Results", URL: "https://acme.test/checks", Nodes: listTree(20), Refs: NewRefTable()}
	in.Budget = len(bodyAtRung(in, 2)) - 1
	first := Render(in)
	if !first.NeedInView {
		t.Fatal("over budget with no viewport, but Render did not ask for one")
	}
	in.InView = map[int]bool{2: true, 2001: true, 2002: true, 2003: true}
	got := Render(in)
	if got.NeedInView {
		t.Fatal("asked for the viewport again")
	}
	if !strings.Contains(got.Text, `link "Check 3"`) || strings.Contains(got.Text, `link "Check 4"`) {
		t.Fatalf("off-screen items not summarised:\n%s", got.Text)
	}
	if !strings.Contains(got.Text, `heading[2] "Checks"`) || !strings.Contains(got.Text, "(17 more off screen)") {
		t.Fatalf("heading or off-screen count missing:\n%s", got.Text)
	}
	if len(body(got)) > in.Budget {
		t.Fatalf("rung three is %d bytes over a %d budget", len(body(got)), in.Budget)
	}
}

func TestRenderHardCutEndsWithTheNote(t *testing.T) {
	in := RenderInput{URL: "https://acme.test/checks", Nodes: listTree(20), Refs: NewRefTable(), Budget: 20, InView: map[int]bool{}}
	got := body(Render(in))
	if !strings.HasSuffix(got, "\n"+truncatedNote) {
		t.Fatalf("no truncation note:\n%s", got)
	}
	if kept := strings.TrimSuffix(got, "\n"+truncatedNote); len(kept) > 20 {
		t.Fatalf("kept %d bytes of a 20-byte budget", len(kept))
	}
}

func TestRenderHugeTreeStaysInBudget(t *testing.T) {
	var kids []string
	nodes := []AXNode{ax("1", "", "RootWebArea", "Huge", 1, nil, "2")}
	for i := 0; i < 5000; i++ {
		kids = append(kids, "b"+strconv.Itoa(i))
	}
	nodes = append(nodes, ax("2", "1", "main", "", 2, nil, kids...))
	for i := 0; i < 5000; i++ {
		nodes = append(nodes, ax("b"+strconv.Itoa(i), "2", "button", "Button "+strconv.Itoa(i), 10+i, nil))
	}
	in := RenderInput{URL: "https://acme.test/huge", Nodes: nodes, Refs: NewRefTable(), Budget: 12000}
	limit := in.Budget + len(truncatedNote) + 1
	if r := Render(in); len(body(r)) > limit || !r.NeedInView {
		t.Fatalf("no viewport: %d bytes (limit %d), NeedInView=%v", len(body(r)), limit, r.NeedInView)
	}
	in.InView = map[int]bool{2: true}
	for i := 0; i < 10; i++ {
		in.InView[10+i] = true
	}
	r := Render(in)
	if len(body(r)) > limit || !strings.Contains(r.Text, "(4990 more off screen)") {
		t.Fatalf("with a viewport: %d bytes:\n%.400s", len(body(r)), r.Text)
	}
}

func TestRenderDialogReplacesThePage(t *testing.T) {
	in := formInput(t)
	a, d := in.Refs.DialogRefs()
	in.Dialog = &Dialog{Type: "confirm", Message: "Delete this repository?", AcceptRef: a, DismissRef: d}
	r := Render(in)
	want := "page: Sign in — Acme — acme.test/login\n  dialog confirm \"Delete this repository?\"\n    accept [" + a + "]\n    dismiss [" + d + "]"
	if r.Text != want {
		t.Fatalf("got:\n%s\nwant:\n%s", r.Text, want)
	}
	if r.Labels[a] != `accept dialog "Delete this repository?"` {
		t.Fatalf("labels %v", r.Labels)
	}
	if acc, ok := in.Refs.DialogAction(a); !ok || !acc {
		t.Fatal("the accept ref does not resolve to accept")
	}
	if acc, ok := in.Refs.DialogAction(d); !ok || acc {
		t.Fatal("the dismiss ref does not resolve to dismiss")
	}
}

func TestRenderLinkToAnotherHost(t *testing.T) {
	nodes := []AXNode{
		ax("1", "", "RootWebArea", "Docs", 1, nil, "2"),
		ax("2", "1", "link", "API", 5, axProps("url", "https://docs.other.test/api#auth")),
	}
	got := Render(RenderInput{URL: "https://acme.test/", Nodes: nodes, Refs: NewRefTable()}).Text
	if !strings.Contains(got, `link "API" [e1] → docs.other.test/api`) {
		t.Fatalf("got:\n%s", got)
	}
}

// expiryCombobox is a combobox "Expiry month" (backend 300) with two
// option children "01" and "02", the second selected.
func expiryCombobox() []AXNode {
	return []AXNode{
		ax("1", "", "RootWebArea", "", 1, nil, "2"),
		ax("2", "1", "combobox", "Expiry month", 300, nil, "3", "4"),
		ax("3", "2", "option", "01", 301, nil),
		ax("4", "2", "option", "02", 302, axProps("selected", "true")),
	}
}

func TestRenderSensitiveSubtreeIsHidden(t *testing.T) {
	in := RenderInput{URL: "https://acme.test/pay", Nodes: expiryCombobox(), Refs: NewRefTable(),
		Sensitive: map[int]string{300: "card"}}
	got := Render(in).Text
	if !strings.Contains(got, `combobox "Expiry month" (card) [e1]`) {
		t.Fatalf("sensitive combobox line missing:\n%s", got)
	}
	for _, s := range []string{`"01"`, `"02"`, "selected"} {
		if strings.Contains(got, s) {
			t.Fatalf("sensitive combobox subtree leaked %q:\n%s", s, got)
		}
	}
}

func TestRenderHideAllValuesHidesOptions(t *testing.T) {
	in := RenderInput{URL: "https://acme.test/pay", Nodes: expiryCombobox(), Refs: NewRefTable(), HideAllValues: true}
	got := Render(in).Text
	if !strings.Contains(got, `combobox "Expiry month" [e1]`) {
		t.Fatalf("combobox line missing:\n%s", got)
	}
	for _, s := range []string{`"01"`, `"02"`, "selected"} {
		if strings.Contains(got, s) {
			t.Fatalf("HideAllValues still shows %q:\n%s", s, got)
		}
	}
}

func TestRenderOrdinaryComboboxShowsOptions(t *testing.T) {
	in := RenderInput{URL: "https://acme.test/pay", Nodes: expiryCombobox(), Refs: NewRefTable()}
	got := Render(in).Text
	if !strings.Contains(got, `"01"`) || !strings.Contains(got, `"02"`) || !strings.Contains(got, "selected") {
		t.Fatalf("ordinary combobox lost its options:\n%s", got)
	}
}

func TestPageLine(t *testing.T) {
	cases := map[[2]string]string{
		{"PR", "https://github.com/acme/api/pull/42"}: "page: PR — github.com/acme/api/pull/42",
		{"", "about:blank"}:                             "page: about:blank",
		{"", ""}:                                        "page: (blank)",
		{"Search", "https://acme.test/s?q=go"}:          "page: Search — acme.test/s?q=go",
	}
	for in, want := range cases {
		if got := PageLine(in[0], in[1]); got != want {
			t.Errorf("PageLine(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}
