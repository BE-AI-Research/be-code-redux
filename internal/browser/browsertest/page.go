package browsertest

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// EmptyTree is the accessibility tree of a blank tab.
const EmptyTree = `{"nodes":[]}`

// Target is one scripted tab.
type Target struct {
	ID, URL, Title string
	Tree           string // an Accessibility.getFullAXTree result
	Opener         string
	history        []string
	index          int
	navs           int
}

// PageScript scripts a browser whose tabs are Targets: target listing,
// attach and info; the frame tree; the accessibility tree; the
// sensitive-field query; element resolution and box models; input;
// navigation and history; layout; dialogs; and page text. Page sessions
// are "S-"+target id and frames "F-"+target id. Change the exported fields
// between Lock and Unlock; read what was recorded with Inputs.
type PageScript struct {
	B *Browser

	mu      sync.Mutex
	targets []*Target
	inputs  []string

	// Sensitive maps a backend id to its attributes as name, value pairs;
	// these nodes are what the sensitive-field query finds.
	Sensitive map[int][]string
	// ShadowSensitive is like Sensitive, but its nodes are placed inside a
	// shadow root in the DOM.getDocument tree: reachable only by a query
	// that pierces shadow DOM.
	ShadowSensitive map[int][]string
	// Disconnected backends answer isConnected false: a stale ref.
	Disconnected map[int]bool
	// SelectResult is what the <select> function returns ("" = chosen).
	SelectResult string
	// ReadText is the page text Runtime.evaluate returns.
	ReadText string
	// InView lists the backends laid out inside the viewport.
	InView []int
	// FailSensitiveQuery makes DOM.getDocument fail.
	FailSensitiveQuery bool
	// FocusLost makes the select-all step report that the element is no
	// longer document.activeElement (it lost focus before typing).
	FocusLost bool
	// NoLoadEvent: navigations never fire Page.loadEventFired.
	NoLoadEvent bool
	// StopWithoutLoad: a navigation fires Page.frameStoppedLoading but
	// never Page.loadEventFired (a load that never signals completion the
	// normal way).
	StopWithoutLoad bool
	// LoadDelay delays loadEventFired after a navigation starts.
	LoadDelay time.Duration
	// Pages maps a URL to the title and tree a navigation there shows.
	Pages map[string][2]string
}

// NewPage scripts b as a browser with one tab, T1, showing url.
func NewPage(b *Browser, url, title, tree string) *PageScript {
	s := &PageScript{B: b, Sensitive: map[int][]string{},
		ShadowSensitive: map[int][]string{}, Disconnected: map[int]bool{}, Pages: map[string][2]string{}}
	s.targets = []*Target{{ID: "T1", URL: url, Title: title, Tree: tree, history: []string{url}}}
	s.install()
	return s
}

func (s *PageScript) Lock()   { s.mu.Lock() }
func (s *PageScript) Unlock() { s.mu.Unlock() }

// Inputs is everything recorded, in order: "mouse <type> x,y",
// "wheel <deltaY>", "text <t>", "key <type> <key>", "focus <backend>",
// "select-all", `select "<value>"`, "scroll-into-view <backend>",
// "dialog accept=<bool>".
func (s *PageScript) Inputs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.inputs...)
}

// AddTarget adds a tab (opener "" for none).
func (s *PageScript) AddTarget(id, url, title, tree, opener string) {
	s.mu.Lock()
	s.targets = append(s.targets, &Target{ID: id, URL: url, Title: title, Tree: tree, Opener: opener, history: []string{url}})
	s.mu.Unlock()
}

// RemoveTarget closes a tab.
func (s *PageScript) RemoveTarget(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.targets {
		if t.ID == id {
			s.targets = append(s.targets[:i], s.targets[i+1:]...)
			return
		}
	}
}

func (s *PageScript) record(format string, a ...any) {
	s.inputs = append(s.inputs, fmt.Sprintf(format, a...))
}

func (s *PageScript) targetLocked(id string) *Target {
	for _, t := range s.targets {
		if t.ID == id {
			return t
		}
	}
	return nil
}

func (s *PageScript) bySessionLocked(sid string) *Target {
	return s.targetLocked(strings.TrimPrefix(sid, "S-"))
}

func arg[T any](p json.RawMessage) T {
	var v T
	json.Unmarshal(p, &v)
	return v
}

func (s *PageScript) install() {
	b := s.B
	b.Handle("Browser.getVersion", func(string, json.RawMessage) (any, error) {
		return map[string]any{"product": "FakeChrome/1.0"}, nil
	})
	b.Handle("Target.getTargets", func(string, json.RawMessage) (any, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		infos := []map[string]any{}
		for _, t := range s.targets {
			infos = append(infos, map[string]any{"targetId": t.ID, "type": "page", "title": t.Title, "url": t.URL, "attached": false})
		}
		return map[string]any{"targetInfos": infos}, nil
	})
	b.Handle("Target.attachToTarget", func(_ string, p json.RawMessage) (any, error) {
		a := arg[struct {
			TargetID string `json:"targetId"`
		}](p)
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.targetLocked(a.TargetID) == nil {
			return nil, errors.New("No target with given id found")
		}
		return map[string]any{"sessionId": "S-" + a.TargetID}, nil
	})
	b.Handle("Target.getTargetInfo", func(_ string, p json.RawMessage) (any, error) {
		a := arg[struct {
			TargetID string `json:"targetId"`
		}](p)
		s.mu.Lock()
		defer s.mu.Unlock()
		t := s.targetLocked(a.TargetID)
		if t == nil {
			return nil, errors.New("No target with given id found")
		}
		return map[string]any{"targetInfo": map[string]any{"targetId": t.ID, "type": "page", "title": t.Title, "url": t.URL}}, nil
	})
	b.Handle("Target.createTarget", func(_ string, p json.RawMessage) (any, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		id := "T" + strconv.Itoa(len(s.targets)+100)
		s.targets = append(s.targets, &Target{ID: id, URL: "about:blank", Tree: EmptyTree, history: []string{"about:blank"}})
		return map[string]any{"targetId": id}, nil
	})
	b.Handle("Page.getFrameTree", func(sid string, _ json.RawMessage) (any, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		t := s.bySessionLocked(sid)
		if t == nil {
			return nil, errors.New("No target")
		}
		return map[string]any{"frameTree": map[string]any{"frame": map[string]any{"id": "F-" + t.ID, "loaderId": "L0", "url": t.URL}}}, nil
	})
	b.Handle("Accessibility.getFullAXTree", func(sid string, _ json.RawMessage) (any, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		t := s.bySessionLocked(sid)
		if t == nil {
			return nil, errors.New("No target")
		}
		return json.RawMessage(t.Tree), nil
	})
	b.Handle("DOM.getDocument", func(string, json.RawMessage) (any, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.FailSensitiveQuery {
			return nil, errors.New("Could not find node with given id")
		}
		return map[string]any{"root": s.documentTreeLocked()}, nil
	})
	b.Handle("DOM.resolveNode", func(_ string, p json.RawMessage) (any, error) {
		a := arg[struct {
			BackendNodeID int `json:"backendNodeId"`
		}](p)
		return map[string]any{"object": map[string]any{"objectId": "obj-" + strconv.Itoa(a.BackendNodeID)}}, nil
	})
	b.Handle("Runtime.callFunctionOn", func(_ string, p json.RawMessage) (any, error) {
		a := arg[struct {
			ObjectID  string `json:"objectId"`
			Decl      string `json:"functionDeclaration"`
			Arguments []struct {
				Value any `json:"value"`
			} `json:"arguments"`
		}](p)
		backend, _ := strconv.Atoi(strings.TrimPrefix(a.ObjectID, "obj-"))
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case strings.Contains(a.Decl, "isConnected"):
			return map[string]any{"result": map[string]any{"type": "boolean", "value": !s.Disconnected[backend]}}, nil
		case strings.Contains(a.Decl, "options"):
			v := ""
			if len(a.Arguments) > 0 {
				v, _ = a.Arguments[0].Value.(string)
			}
			s.record("select %q", v)
			return map[string]any{"result": map[string]any{"type": "string", "value": s.SelectResult}}, nil
		case strings.Contains(a.Decl, "selectNodeContents"):
			s.record("select-all")
			return map[string]any{"result": map[string]any{"type": "boolean", "value": !s.FocusLost}}, nil
		}
		return map[string]any{"result": map[string]any{"type": "undefined"}}, nil
	})
	b.Handle("DOM.getBoxModel", func(string, json.RawMessage) (any, error) {
		return map[string]any{"model": map[string]any{"content": []float64{10, 10, 90, 10, 90, 30, 10, 30}}}, nil
	})
	b.Handle("DOM.scrollIntoViewIfNeeded", func(_ string, p json.RawMessage) (any, error) {
		a := arg[struct {
			BackendNodeID int `json:"backendNodeId"`
		}](p)
		s.mu.Lock()
		s.record("scroll-into-view %d", a.BackendNodeID)
		s.mu.Unlock()
		return map[string]any{}, nil
	})
	b.Handle("DOM.focus", func(_ string, p json.RawMessage) (any, error) {
		a := arg[struct {
			BackendNodeID int `json:"backendNodeId"`
		}](p)
		s.mu.Lock()
		s.record("focus %d", a.BackendNodeID)
		s.mu.Unlock()
		return map[string]any{}, nil
	})
	b.Handle("Input.dispatchMouseEvent", func(_ string, p json.RawMessage) (any, error) {
		a := arg[struct {
			Type   string  `json:"type"`
			X      float64 `json:"x"`
			Y      float64 `json:"y"`
			DeltaY float64 `json:"deltaY"`
		}](p)
		s.mu.Lock()
		defer s.mu.Unlock()
		if a.Type == "mouseWheel" {
			s.record("wheel %g", a.DeltaY)
		} else {
			s.record("mouse %s %g,%g", a.Type, a.X, a.Y)
		}
		return map[string]any{}, nil
	})
	b.Handle("Input.insertText", func(_ string, p json.RawMessage) (any, error) {
		a := arg[struct {
			Text string `json:"text"`
		}](p)
		s.mu.Lock()
		s.record("text %s", a.Text)
		s.mu.Unlock()
		return map[string]any{}, nil
	})
	b.Handle("Input.dispatchKeyEvent", func(_ string, p json.RawMessage) (any, error) {
		a := arg[struct {
			Type string `json:"type"`
			Key  string `json:"key"`
		}](p)
		s.mu.Lock()
		s.record("key %s %s", a.Type, a.Key)
		s.mu.Unlock()
		return map[string]any{}, nil
	})
	b.Handle("Page.navigate", func(sid string, p json.RawMessage) (any, error) {
		a := arg[struct {
			URL string `json:"url"`
		}](p)
		s.mu.Lock()
		t := s.bySessionLocked(sid)
		if t == nil {
			s.mu.Unlock()
			return nil, errors.New("No target")
		}
		if a.URL == "https://unreachable.test/" {
			s.mu.Unlock()
			return map[string]any{"frameId": "F-" + t.ID, "errorText": "net::ERR_NAME_NOT_RESOLVED"}, nil
		}
		t.history = append(t.history[:t.index+1], a.URL)
		t.index = len(t.history) - 1
		loader := s.showLocked(t, a.URL)
		s.mu.Unlock()
		s.emitNav(sid, t, loader)
		return map[string]any{"frameId": "F-" + t.ID, "loaderId": loader}, nil
	})
	b.Handle("Page.getNavigationHistory", func(sid string, _ json.RawMessage) (any, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		t := s.bySessionLocked(sid)
		entries := []map[string]any{}
		for i, u := range t.history {
			entries = append(entries, map[string]any{"id": i + 1, "url": u, "title": ""})
		}
		return map[string]any{"currentIndex": t.index, "entries": entries}, nil
	})
	b.Handle("Page.navigateToHistoryEntry", func(sid string, p json.RawMessage) (any, error) {
		a := arg[struct {
			EntryID int `json:"entryId"`
		}](p)
		s.mu.Lock()
		t := s.bySessionLocked(sid)
		t.index = a.EntryID - 1
		loader := s.showLocked(t, t.history[t.index])
		s.mu.Unlock()
		s.emitNav(sid, t, loader)
		return map[string]any{}, nil
	})
	b.Handle("Page.getLayoutMetrics", func(string, json.RawMessage) (any, error) {
		return map[string]any{"cssLayoutViewport": map[string]any{"pageX": 0, "pageY": 0, "clientWidth": 800, "clientHeight": 600}}, nil
	})
	b.Handle("DOMSnapshot.captureSnapshot", func(string, json.RawMessage) (any, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		idx, bounds := []int{}, [][]float64{}
		for i := range s.InView {
			idx = append(idx, i)
			bounds = append(bounds, []float64{10, 10, 100, 20})
		}
		return map[string]any{"documents": []map[string]any{{
			"nodes":  map[string]any{"backendNodeId": s.InView},
			"layout": map[string]any{"nodeIndex": idx, "bounds": bounds},
		}}}, nil
	})
	b.Handle("Page.createIsolatedWorld", func(string, json.RawMessage) (any, error) {
		return map[string]any{"executionContextId": 7}, nil
	})
	b.Handle("Runtime.evaluate", func(string, json.RawMessage) (any, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		return map[string]any{"result": map[string]any{"type": "string", "value": s.ReadText}}, nil
	})
	b.Handle("Page.handleJavaScriptDialog", func(sid string, p json.RawMessage) (any, error) {
		a := arg[struct {
			Accept bool `json:"accept"`
		}](p)
		s.mu.Lock()
		s.record("dialog accept=%v", a.Accept)
		s.mu.Unlock()
		s.B.EmitSoon(sid, "Page.javascriptDialogClosed", map[string]any{"result": a.Accept})
		return map[string]any{}, nil
	})
}

// documentTreeLocked builds a DOM.getDocument {"depth":-1,"pierce":true}
// tree: Sensitive fields as ordinary children of the document, and
// ShadowSensitive fields nested inside one host element's shadow root — so
// a query that pierces shadow DOM finds both, and one that does not finds
// only the first.
func (s *PageScript) documentTreeLocked() map[string]any {
	children := []any{}
	for backend, attrs := range s.Sensitive {
		children = append(children, sensitiveDOMNode(backend, attrs))
	}
	if len(s.ShadowSensitive) > 0 {
		shadowChildren := []any{}
		for backend, attrs := range s.ShadowSensitive {
			shadowChildren = append(shadowChildren, sensitiveDOMNode(backend, attrs))
		}
		children = append(children, map[string]any{
			"backendNodeId": 900, "nodeName": "CUSTOM-FIELD", "localName": "custom-field",
			"shadowRoots": []any{
				map[string]any{"backendNodeId": 901, "nodeName": "#document-fragment", "children": shadowChildren},
			},
		})
	}
	return map[string]any{"backendNodeId": 1, "nodeName": "#document", "children": children}
}

func sensitiveDOMNode(backend int, attrs []string) map[string]any {
	return map[string]any{"backendNodeId": backend, "nodeName": "INPUT", "localName": "input", "attributes": attrs}
}

// showLocked points t at url, taking its title and tree from Pages when
// scripted, and returns the new document's loader id.
func (s *PageScript) showLocked(t *Target, url string) string {
	t.URL = url
	if pg, ok := s.Pages[url]; ok {
		t.Title, t.Tree = pg[0], pg[1]
	}
	t.navs++
	return "L" + strconv.Itoa(t.navs)
}

// emitNav plays a navigation's events in the order Chrome sends them: the
// load starting and the frame navigating arrive before the reply to
// Page.navigate (so these two are written now, from the handler, ahead of
// the reply), and the load event fires afterwards unless scripted not to.
func (s *PageScript) emitNav(sid string, t *Target, loader string) {
	s.mu.Lock()
	frame, url, noLoad, stopOnly, delay := "F-"+t.ID, t.URL, s.NoLoadEvent, s.StopWithoutLoad, s.LoadDelay
	s.mu.Unlock()
	s.B.Emit(sid, "Page.frameStartedLoading", map[string]any{"frameId": frame})
	s.B.Emit(sid, "Page.frameNavigated", map[string]any{"frame": map[string]any{"id": frame, "loaderId": loader, "url": url}})
	if stopOnly {
		go func() {
			time.Sleep(delay)
			s.B.Emit(sid, "Page.frameStoppedLoading", map[string]any{"frameId": frame})
		}()
		return
	}
	if noLoad {
		return
	}
	go func() {
		time.Sleep(delay)
		s.B.Emit(sid, "Page.loadEventFired", map[string]any{"timestamp": 1})
	}()
}
