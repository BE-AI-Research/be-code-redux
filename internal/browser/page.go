package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

// callTimeout bounds one protocol command inside a page action, so a page
// wedged behind a dialog or a hung renderer cannot hang a tool call.
var callTimeout = 15 * time.Second

// Options configures a Session and the Pages it opens.
type Options struct {
	Address       string        // attach here first; "" means 127.0.0.1:9222
	Launch        bool          // launch a browser when nothing answers at Address
	Executable    string        // "" means FindExecutable
	Profile       string        // the launched browser's --user-data-dir
	AllowRemote   bool          // permit a non-loopback Address
	SnapshotChars int           // the snapshot budget
	SettleTimeout time.Duration // bound on waiting for a page to settle; 0 means 10s
	ForceHeadless bool          // launch headless whatever the display; tests use it
	QuietWindow   time.Duration // network idle that counts as settled; 0 means 500ms
}

// sensitiveSelector finds the fields whose values the model must never see
// or write (spec §3.4).
const sensitiveSelector = `input[type=password i], [autocomplete~="current-password" i], [autocomplete~="new-password" i], ` +
	`[autocomplete~="one-time-code" i], [autocomplete~="cc-number" i], [autocomplete~="cc-csc" i], [autocomplete~="cc-exp" i]`

// StaleRefError is an action on an element that has left the page.
type StaleRefError struct{ Ref string }

func (e *StaleRefError) Error() string { return e.Ref + " is no longer on the page" }

// UnknownRefError is a ref no snapshot of this page has shown.
type UnknownRefError struct{ Ref string }

func (e *UnknownRefError) Error() string {
	return e.Ref + " is not an element on this page; take a snapshot and use a ref from it"
}

type pendingDialog struct{ Type, Message string }

// Page is the one tab the model drives: its events, its refs, its actions.
type Page struct {
	conn      *Conn
	targetID  string
	sessionID string
	opts      Options
	refs      *RefTable

	mu       sync.Mutex
	frameID  string
	loaderID string
	url      string
	title    string
	navGen   int // main-frame loads started
	loadGen  int // main-frame loads finished
	inflight map[string]bool
	lastNet  time.Time
	dialog   *pendingDialog
	notes    []string
	labels   map[string]string
	loading  bool
	unsub    func()
}

// attachPage opens a flat session on a tab and starts listening to it.
func attachPage(ctx context.Context, conn *Conn, targetID string, opts Options) (*Page, error) {
	var att struct {
		SessionID string `json:"sessionId"`
	}
	if err := conn.Call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}, &att); err != nil {
		return nil, err
	}
	p := &Page{conn: conn, targetID: targetID, sessionID: att.SessionID, opts: opts,
		refs: NewRefTable(), inflight: map[string]bool{}, lastNet: time.Now(), labels: map[string]string{}}
	p.unsub = conn.Subscribe(p.onEvent)
	for _, m := range []string{"Page.enable", "Network.enable", "DOM.enable"} {
		if err := p.call(ctx, m, nil, nil); err != nil {
			p.release()
			return nil, err
		}
	}
	var ft struct {
		FrameTree struct {
			Frame struct {
				ID       string `json:"id"`
				LoaderID string `json:"loaderId"`
				URL      string `json:"url"`
			} `json:"frame"`
		} `json:"frameTree"`
	}
	if err := p.call(ctx, "Page.getFrameTree", nil, &ft); err != nil {
		p.release()
		return nil, err
	}
	p.frameID, p.loaderID, p.url = ft.FrameTree.Frame.ID, ft.FrameTree.Frame.LoaderID, ft.FrameTree.Frame.URL
	return p, nil
}

func (p *Page) call(ctx context.Context, method string, params, result any) error {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	return p.conn.Call(ctx, p.sessionID, method, params, result)
}

// release stops listening and detaches, leaving the tab open.
func (p *Page) release() {
	if p.unsub != nil {
		p.unsub()
		p.unsub = nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	p.conn.Call(ctx, "", "Target.detachFromTarget", map[string]any{"sessionId": p.sessionID}, nil)
}

// onEvent runs on the connection's reader goroutine: it only records, and
// never calls the browser (a dialog is answered from a goroutine of its own).
func (p *Page) onEvent(ev Event) {
	if ev.SessionID != p.sessionID {
		return
	}
	switch ev.Method {
	case "Network.requestWillBeSent":
		var e struct {
			RequestID string `json:"requestId"`
			Type      string `json:"type"`
		}
		if json.Unmarshal(ev.Params, &e) != nil {
			return
		}
		p.mu.Lock()
		p.lastNet = time.Now()
		// A socket or event stream stays open by design: it must not keep
		// the page from ever counting as settled.
		if e.Type != "WebSocket" && e.Type != "EventSource" {
			p.inflight[e.RequestID] = true
		}
		p.mu.Unlock()
	case "Network.loadingFinished", "Network.loadingFailed":
		var e struct {
			RequestID string `json:"requestId"`
		}
		json.Unmarshal(ev.Params, &e)
		p.mu.Lock()
		delete(p.inflight, e.RequestID)
		p.lastNet = time.Now()
		p.mu.Unlock()
	case "Page.frameStartedLoading":
		var e struct {
			FrameID string `json:"frameId"`
		}
		json.Unmarshal(ev.Params, &e)
		p.mu.Lock()
		if e.FrameID == p.frameID {
			p.navGen++
		}
		p.mu.Unlock()
	case "Page.loadEventFired":
		p.mu.Lock()
		p.loadGen = p.navGen
		p.mu.Unlock()
	case "Page.frameNavigated":
		var e struct {
			Frame struct {
				ID       string `json:"id"`
				ParentID string `json:"parentId"`
				LoaderID string `json:"loaderId"`
				URL      string `json:"url"`
			} `json:"frame"`
		}
		if json.Unmarshal(ev.Params, &e) != nil || e.Frame.ParentID != "" {
			return
		}
		p.mu.Lock()
		p.frameID, p.url = e.Frame.ID, e.Frame.URL
		newDoc := e.Frame.LoaderID != p.loaderID
		p.loaderID = e.Frame.LoaderID
		p.mu.Unlock()
		if newDoc {
			p.refs.Reset()
		}
	case "Page.navigatedWithinDocument":
		var e struct {
			FrameID string `json:"frameId"`
			URL     string `json:"url"`
		}
		json.Unmarshal(ev.Params, &e)
		p.mu.Lock()
		if e.FrameID == p.frameID {
			p.url = e.URL
		}
		p.mu.Unlock()
	case "Page.javascriptDialogOpening":
		var e struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		json.Unmarshal(ev.Params, &e)
		switch e.Type {
		case "alert", "beforeunload":
			if e.Type == "alert" {
				p.mu.Lock()
				p.notes = append(p.notes, "alert: "+quote(clip(e.Message, 200)))
				p.mu.Unlock()
			}
			go p.answerDialog(true)
		default:
			p.mu.Lock()
			p.dialog = &pendingDialog{Type: e.Type, Message: e.Message}
			p.mu.Unlock()
		}
	case "Page.javascriptDialogClosed":
		p.mu.Lock()
		p.dialog = nil
		p.mu.Unlock()
		p.refs.ClearDialog()
	}
}

func (p *Page) answerDialog(accept bool) {
	p.call(context.Background(), "Page.handleJavaScriptDialog", map[string]any{"accept": accept}, nil)
}

// settle waits for the page after an action: any navigation's load event,
// then the network quiet for QuietWindow — or quiet for twice that with a
// request still open (a long poll never finishes) — bounded by
// SettleTimeout. On timeout the page is marked loading, never an error.
func (p *Page) settle(ctx context.Context) {
	quiet := p.opts.QuietWindow
	if quiet <= 0 {
		quiet = 500 * time.Millisecond
	}
	timeout := p.opts.SettleTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	start := time.Now()
	deadline := start.Add(timeout)
	for {
		p.mu.Lock()
		loaded := p.loadGen >= p.navGen
		idle := time.Since(p.lastNet)
		n := len(p.inflight)
		blocked := p.dialog != nil
		p.mu.Unlock()
		switch {
		case blocked:
			// A dialog blocks the page: nothing more loads until it is answered.
			p.setLoading(false)
			return
		case loaded && time.Since(start) >= quiet && ((n == 0 && idle >= quiet) || idle >= 2*quiet):
			p.setLoading(false)
			return
		case time.Now().After(deadline):
			p.setLoading(true)
			return
		}
		select {
		case <-ctx.Done():
			p.setLoading(true)
			return
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (p *Page) setLoading(v bool) {
	p.mu.Lock()
	p.loading = v
	p.mu.Unlock()
}

// Info asks the browser for the page's title and URL, falling back to what
// the page's own events said.
func (p *Page) Info(ctx context.Context) (title, pageURL string) {
	var ti struct {
		TargetInfo struct {
			Title string `json:"title"`
			URL   string `json:"url"`
		} `json:"targetInfo"`
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if err := p.conn.Call(cctx, "", "Target.getTargetInfo", map[string]any{"targetId": p.targetID}, &ti); err == nil {
		p.mu.Lock()
		p.title = ti.TargetInfo.Title
		if ti.TargetInfo.URL != "" {
			p.url = ti.TargetInfo.URL
		}
		p.mu.Unlock()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.title, p.url
}

// URL is the page's address as last seen.
func (p *Page) URL() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.url
}

// Title is the page's title as last seen.
func (p *Page) Title() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.title
}

// TargetID names the tab.
func (p *Page) TargetID() string { return p.targetID }

// Snapshot renders the page (spec §2.2), or the dialog blocking it.
func (p *Page) Snapshot(ctx context.Context) (string, error) {
	title, pageURL := p.Info(ctx)
	p.mu.Lock()
	dlg, loading := p.dialog, p.loading
	p.mu.Unlock()
	in := RenderInput{Title: title, URL: pageURL, Refs: p.refs, Budget: p.opts.SnapshotChars, Loading: loading}
	if dlg != nil {
		a, d := p.refs.DialogRefs()
		in.Dialog = &Dialog{Type: dlg.Type, Message: dlg.Message, AcceptRef: a, DismissRef: d}
		r := Render(in)
		p.setLabels(r.Labels)
		return r.Text, nil
	}
	var tree struct {
		Nodes []AXNode `json:"nodes"`
	}
	if err := p.call(ctx, "Accessibility.getFullAXTree", nil, &tree); err != nil {
		return "", fmt.Errorf("reading the page: %w", err)
	}
	in.Nodes = tree.Nodes
	if sens, err := p.sensitive(ctx); err != nil {
		in.HideAllValues = true // fail closed (spec §3.4)
	} else {
		in.Sensitive = sens
	}
	r := Render(in)
	if r.NeedInView {
		if view, err := p.inView(ctx); err == nil {
			in.InView = view
			r = Render(in)
		}
	}
	p.setLabels(r.Labels)
	return r.Text, nil
}

func (p *Page) setLabels(l map[string]string) {
	p.mu.Lock()
	for k, v := range l {
		p.labels[k] = v
	}
	p.mu.Unlock()
}

// Describe names an element for an approval prompt: `button "Merge" [e14]`.
func (p *Page) Describe(ref string) string {
	r := NormalizeRef(ref)
	p.mu.Lock()
	defer p.mu.Unlock()
	if l := p.labels[r]; l != "" {
		return l + " [" + r + "]"
	}
	return r
}

// TakeNotes returns and clears what happened in the background (an alert).
func (p *Page) TakeNotes() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := p.notes
	p.notes = nil
	return n
}

// sensitive finds the password, one-time-code and card fields, by backend
// id, with their kind — pure protocol, nothing run in the page.
func (p *Page) sensitive(ctx context.Context) (map[int]string, error) {
	var doc struct {
		Root struct {
			NodeID int `json:"nodeId"`
		} `json:"root"`
	}
	if err := p.call(ctx, "DOM.getDocument", map[string]any{"depth": 0}, &doc); err != nil {
		return nil, err
	}
	var q struct {
		NodeIDs []int `json:"nodeIds"`
	}
	if err := p.call(ctx, "DOM.querySelectorAll", map[string]any{"nodeId": doc.Root.NodeID, "selector": sensitiveSelector}, &q); err != nil {
		return nil, err
	}
	out := map[int]string{}
	for _, id := range q.NodeIDs {
		var d struct {
			Node struct {
				BackendNodeID int      `json:"backendNodeId"`
				Attributes    []string `json:"attributes"`
			} `json:"node"`
		}
		if err := p.call(ctx, "DOM.describeNode", map[string]any{"nodeId": id}, &d); err != nil {
			return nil, err
		}
		out[d.Node.BackendNodeID] = sensitiveKind(d.Node.Attributes)
	}
	return out, nil
}

func sensitiveKind(attrs []string) string {
	kind := "password"
	for i := 0; i+1 < len(attrs); i += 2 {
		name, val := strings.ToLower(attrs[i]), strings.ToLower(attrs[i+1])
		if name == "type" && val == "password" {
			return "password"
		}
		if name == "autocomplete" {
			switch {
			case strings.Contains(val, "one-time-code"):
				kind = "one-time code"
			case strings.Contains(val, "cc-"):
				kind = "card"
			}
		}
	}
	return kind
}

// IsSensitive reports whether ref is a password, one-time-code or card
// field. It fails closed: when the check itself cannot run, the answer is
// yes. An unknown ref is returned as its error.
func (p *Page) IsSensitive(ctx context.Context, ref string) (bool, error) {
	r := NormalizeRef(ref)
	backend, ok := p.refs.Lookup(r)
	if !ok {
		return false, &UnknownRefError{Ref: r}
	}
	set, err := p.sensitive(ctx)
	if err != nil {
		return true, nil
	}
	_, yes := set[backend]
	return yes, nil
}

// inView is the set of backend ids laid out inside the viewport, for the
// last rung of the budget ladder.
func (p *Page) inView(ctx context.Context) (map[int]bool, error) {
	var lm struct {
		V struct {
			PageX        float64 `json:"pageX"`
			PageY        float64 `json:"pageY"`
			ClientWidth  float64 `json:"clientWidth"`
			ClientHeight float64 `json:"clientHeight"`
		} `json:"cssLayoutViewport"`
	}
	if err := p.call(ctx, "Page.getLayoutMetrics", nil, &lm); err != nil {
		return nil, err
	}
	var snap struct {
		Documents []struct {
			Nodes struct {
				BackendNodeID []int `json:"backendNodeId"`
			} `json:"nodes"`
			Layout struct {
				NodeIndex []int       `json:"nodeIndex"`
				Bounds    [][]float64 `json:"bounds"`
			} `json:"layout"`
		} `json:"documents"`
	}
	if err := p.call(ctx, "DOMSnapshot.captureSnapshot", map[string]any{"computedStyles": []string{}}, &snap); err != nil {
		return nil, err
	}
	if len(snap.Documents) == 0 {
		return nil, errors.New("no document in the layout snapshot")
	}
	d := snap.Documents[0]
	vx0, vy0 := lm.V.PageX, lm.V.PageY
	vx1, vy1 := vx0+lm.V.ClientWidth, vy0+lm.V.ClientHeight
	out := map[int]bool{}
	for i, ni := range d.Layout.NodeIndex {
		if i >= len(d.Layout.Bounds) || ni < 0 || ni >= len(d.Nodes.BackendNodeID) || len(d.Layout.Bounds[i]) < 4 {
			continue
		}
		b := d.Layout.Bounds[i]
		if b[0]+b[2] > vx0 && b[0] < vx1 && b[1]+b[3] > vy0 && b[1] < vy1 {
			out[d.Nodes.BackendNodeID[ni]] = true
		}
	}
	return out, nil
}

// callOn runs fn with `this` bound to the element, returning its value.
func (p *Page) callOn(ctx context.Context, backend int, fn string, args ...any) (any, error) {
	var obj struct {
		Object struct {
			ObjectID string `json:"objectId"`
		} `json:"object"`
	}
	if err := p.call(ctx, "DOM.resolveNode", map[string]any{"backendNodeId": backend}, &obj); err != nil {
		return nil, err
	}
	params := map[string]any{"objectId": obj.Object.ObjectID, "functionDeclaration": fn, "returnByValue": true}
	if len(args) > 0 {
		var ca []map[string]any
		for _, a := range args {
			ca = append(ca, map[string]any{"value": a})
		}
		params["arguments"] = ca
	}
	var r struct {
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
		Exception *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := p.call(ctx, "Runtime.callFunctionOn", params, &r); err != nil {
		return nil, err
	}
	if r.Exception != nil {
		return nil, errors.New(r.Exception.Text)
	}
	return r.Result.Value, nil
}

// resolve turns a ref into a live element, or says why it cannot.
func (p *Page) resolve(ctx context.Context, ref string) (int, error) {
	r := NormalizeRef(ref)
	if r == "" {
		return 0, errors.New("this action needs a ref from the snapshot, like e14")
	}
	backend, ok := p.refs.Lookup(r)
	if !ok {
		return 0, &UnknownRefError{Ref: r}
	}
	if v, err := p.callOn(ctx, backend, "function(){return this.isConnected}"); err != nil || v != true {
		return 0, &StaleRefError{Ref: r}
	}
	return backend, nil
}

// Click clicks an element with real mouse input at its centre — or answers
// the pending dialog when ref is one of its refs.
func (p *Page) Click(ctx context.Context, ref string) error {
	if accept, ok := p.refs.DialogAction(ref); ok {
		if err := p.call(ctx, "Page.handleJavaScriptDialog", map[string]any{"accept": accept}, nil); err != nil {
			return err
		}
		p.settle(ctx)
		return nil
	}
	backend, err := p.resolve(ctx, ref)
	if err != nil {
		return err
	}
	_ = p.call(ctx, "DOM.scrollIntoViewIfNeeded", map[string]any{"backendNodeId": backend}, nil)
	var box struct {
		Model struct {
			Content []float64 `json:"content"`
		} `json:"model"`
	}
	if err := p.call(ctx, "DOM.getBoxModel", map[string]any{"backendNodeId": backend}, &box); err != nil || len(box.Model.Content) < 8 {
		return fmt.Errorf("%s has no visible area to click (hidden or zero-sized)", NormalizeRef(ref))
	}
	c := box.Model.Content
	x, y := (c[0]+c[2]+c[4]+c[6])/4, (c[1]+c[3]+c[5]+c[7])/4
	for _, typ := range []string{"mouseMoved", "mousePressed", "mouseReleased"} {
		ev := map[string]any{"type": typ, "x": x, "y": y}
		if typ != "mouseMoved" {
			ev["button"], ev["clickCount"] = "left", 1
		}
		if err := p.call(ctx, "Input.dispatchMouseEvent", ev, nil); err != nil {
			return err
		}
	}
	p.settle(ctx)
	return nil
}

const selectAllJS = `function(){ if (typeof this.select === 'function') { this.select(); return } ` +
	`const r = document.createRange(); r.selectNodeContents(this); const s = getSelection(); s.removeAllRanges(); s.addRange(r) }`

// Type replaces a field's content with text; submit presses Enter after.
// An empty text clears the field.
func (p *Page) Type(ctx context.Context, ref, text string, submit bool) error {
	backend, err := p.resolve(ctx, ref)
	if err != nil {
		return err
	}
	if err := p.call(ctx, "DOM.focus", map[string]any{"backendNodeId": backend}, nil); err != nil {
		return fmt.Errorf("%s cannot take text (is it a field?)", NormalizeRef(ref))
	}
	if _, err := p.callOn(ctx, backend, selectAllJS); err != nil {
		return err
	}
	if text == "" {
		if err := p.key(ctx, keys["delete"]); err != nil {
			return err
		}
	} else if err := p.call(ctx, "Input.insertText", map[string]any{"text": text}, nil); err != nil {
		return err
	}
	if submit {
		return p.Press(ctx, "Enter")
	}
	p.settle(ctx)
	return nil
}

const selectJS = `function(v){ if (this.tagName !== 'SELECT') return 'not a dropdown list (open a custom one with click, then click the option)'; ` +
	`const o = Array.from(this.options).find(o => o.value === v || o.label === v || o.text.trim() === v); ` +
	`if (!o) return 'no option ' + JSON.stringify(v) + '; the options are: ' + Array.from(this.options).map(o => o.text.trim()).join(', '); ` +
	`this.value = o.value; this.dispatchEvent(new Event('input', {bubbles: true})); this.dispatchEvent(new Event('change', {bubbles: true})); return '' }`

// Select chooses an option of a <select> by its value or visible text.
func (p *Page) Select(ctx context.Context, ref, value string) error {
	backend, err := p.resolve(ctx, ref)
	if err != nil {
		return err
	}
	v, err := p.callOn(ctx, backend, selectJS, value)
	if err != nil {
		return err
	}
	if msg, _ := v.(string); msg != "" {
		return errors.New(msg)
	}
	p.settle(ctx)
	return nil
}

type keyDef struct {
	key, code string
	vk        int
	text      string
}

var keys = map[string]keyDef{
	"enter": {"Enter", "Enter", 13, "\r"}, "return": {"Enter", "Enter", 13, "\r"},
	"tab":       {"Tab", "Tab", 9, ""},
	"escape":    {"Escape", "Escape", 27, ""}, "esc": {"Escape", "Escape", 27, ""},
	"backspace": {"Backspace", "Backspace", 8, ""},
	"delete":    {"Delete", "Delete", 46, ""},
	"space":     {" ", "Space", 32, " "},
	"arrowup":   {"ArrowUp", "ArrowUp", 38, ""}, "up": {"ArrowUp", "ArrowUp", 38, ""},
	"arrowdown": {"ArrowDown", "ArrowDown", 40, ""}, "down": {"ArrowDown", "ArrowDown", 40, ""},
	"arrowleft": {"ArrowLeft", "ArrowLeft", 37, ""}, "left": {"ArrowLeft", "ArrowLeft", 37, ""},
	"arrowright": {"ArrowRight", "ArrowRight", 39, ""}, "right": {"ArrowRight", "ArrowRight", 39, ""},
	"home": {"Home", "Home", 36, ""}, "end": {"End", "End", 35, ""},
	"pageup": {"PageUp", "PageUp", 33, ""}, "pagedown": {"PageDown", "PageDown", 34, ""},
}

// Press presses a named key in whatever has focus.
func (p *Page) Press(ctx context.Context, key string) error {
	k, ok := keys[strings.ToLower(strings.ReplaceAll(strings.TrimSpace(key), " ", ""))]
	if !ok {
		return fmt.Errorf("unknown key %q; use one of: Enter, Tab, Escape, Backspace, Delete, Space, ArrowUp, ArrowDown, ArrowLeft, ArrowRight, Home, End, PageUp, PageDown", key)
	}
	if err := p.key(ctx, k); err != nil {
		return err
	}
	p.settle(ctx)
	return nil
}

func (p *Page) key(ctx context.Context, k keyDef) error {
	down := map[string]any{"type": "keyDown", "key": k.key, "code": k.code, "windowsVirtualKeyCode": k.vk, "nativeVirtualKeyCode": k.vk}
	if k.text != "" {
		down["text"], down["unmodifiedText"] = k.text, k.text
	}
	if err := p.call(ctx, "Input.dispatchKeyEvent", down, nil); err != nil {
		return err
	}
	return p.call(ctx, "Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": k.key, "code": k.code,
		"windowsVirtualKeyCode": k.vk, "nativeVirtualKeyCode": k.vk}, nil)
}

// Scroll moves the viewport (up, down, top, bottom) or brings ref into view.
func (p *Page) Scroll(ctx context.Context, direction, ref string) error {
	if NormalizeRef(ref) != "" {
		backend, err := p.resolve(ctx, ref)
		if err != nil {
			return err
		}
		if err := p.call(ctx, "DOM.scrollIntoViewIfNeeded", map[string]any{"backendNodeId": backend}, nil); err != nil {
			return err
		}
		p.settle(ctx)
		return nil
	}
	var lm struct {
		V struct {
			ClientWidth  float64 `json:"clientWidth"`
			ClientHeight float64 `json:"clientHeight"`
		} `json:"cssLayoutViewport"`
	}
	if err := p.call(ctx, "Page.getLayoutMetrics", nil, &lm); err != nil {
		return err
	}
	dy := 0.8 * lm.V.ClientHeight
	switch strings.ToLower(strings.TrimSpace(direction)) {
	case "", "down":
	case "up":
		dy = -dy
	case "top":
		dy = -1e7
	case "bottom":
		dy = 1e7
	default:
		return fmt.Errorf("scroll direction %q: use up, down, top or bottom, or give a ref", direction)
	}
	if err := p.call(ctx, "Input.dispatchMouseEvent", map[string]any{"type": "mouseWheel",
		"x": lm.V.ClientWidth / 2, "y": lm.V.ClientHeight / 2, "deltaX": 0, "deltaY": dy}, nil); err != nil {
		return err
	}
	p.settle(ctx)
	return nil
}

// Back goes one step back in this tab's history.
func (p *Page) Back(ctx context.Context) error {
	var h struct {
		CurrentIndex int `json:"currentIndex"`
		Entries      []struct {
			ID int `json:"id"`
		} `json:"entries"`
	}
	if err := p.call(ctx, "Page.getNavigationHistory", nil, &h); err != nil {
		return err
	}
	if h.CurrentIndex <= 0 || h.CurrentIndex > len(h.Entries) {
		return errors.New("there is no earlier page in this tab's history")
	}
	if err := p.call(ctx, "Page.navigateToHistoryEntry", map[string]any{"entryId": h.Entries[h.CurrentIndex-1].ID}, nil); err != nil {
		return err
	}
	p.settle(ctx)
	return nil
}

// Navigate opens a web address in this tab.
func (p *Page) Navigate(ctx context.Context, raw string) error {
	u, err := NormalizeURL(raw)
	if err != nil {
		return err
	}
	var r struct {
		ErrorText string `json:"errorText"`
	}
	if err := p.call(ctx, "Page.navigate", map[string]any{"url": u}, &r); err != nil {
		return err
	}
	if r.ErrorText != "" {
		return fmt.Errorf("could not open %s: %s", u, r.ErrorText)
	}
	p.settle(ctx)
	return nil
}

// NormalizeURL accepts an address the way a model writes it — no scheme,
// a bare host:port — and refuses anything but a web page (spec §2.1,
// amended): file:, javascript:, data:, mailto: and every other scheme.
func NormalizeURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("open needs a url")
	}
	if s == "about:blank" {
		return s, nil
	}
	if !strings.Contains(s, "://") {
		// "scheme:rest" without "//" is another scheme unless rest is a port.
		if i := strings.Index(s, ":"); i > 0 {
			if scheme := s[:i]; isScheme(scheme) && !startsWithDigit(s[i+1:]) {
				return "", fmt.Errorf("only http and https pages can be opened, not %s:", strings.ToLower(scheme))
			}
		}
		host := s
		if i := strings.IndexAny(host, "/?#"); i >= 0 {
			host = host[:i]
		}
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if IsLoopbackHost(host) {
			s = "http://" + s
		} else {
			s = "https://" + s
		}
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("%q is not a web address", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("only http and https pages can be opened, not %s:", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%q is not a web address", raw)
	}
	return u.String(), nil
}

func isScheme(s string) bool {
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case i > 0 && (r >= '0' && r <= '9' || r == '+' || r == '-' || r == '.'):
		default:
			return false
		}
	}
	return s != ""
}

func startsWithDigit(s string) bool { return s != "" && s[0] >= '0' && s[0] <= '9' }

// Read returns the page's main text — main, else article, else the body —
// from an isolated world, so the page's own scripts cannot rewrite what is
// read. Form field values are never part of innerText.
func (p *Page) Read(ctx context.Context) (string, error) {
	p.mu.Lock()
	frame := p.frameID
	p.mu.Unlock()
	var w struct {
		ID int `json:"executionContextId"`
	}
	if err := p.call(ctx, "Page.createIsolatedWorld", map[string]any{"frameId": frame, "worldName": "be-code"}, &w); err != nil {
		return "", err
	}
	var r struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	expr := `(() => { const m = document.querySelector('main, [role=main], article') || document.body; return m ? m.innerText : '' })()`
	if err := p.call(ctx, "Runtime.evaluate", map[string]any{"expression": expr, "contextId": w.ID, "returnByValue": true}, &r); err != nil {
		return "", err
	}
	return tidyText(r.Result.Value), nil
}

// tidyText trims each line and keeps at most one blank line in a row.
func tidyText(s string) string {
	var out []string
	blank := false
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			if !blank && len(out) > 0 {
				out = append(out, "")
			}
			blank = true
			continue
		}
		blank = false
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
