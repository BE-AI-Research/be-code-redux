# Shared Sessions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One live instance per session code that every entry point joins seamlessly, and a private input line for every attached terminal over one shared rendering.

**Architecture:** The host stops piping raw bytes into Bubble Tea; it runs the program with a nil input, parses each client's bytes with `charmbracelet/x/input` through a per-client pipe, and sends tagged `ClientKeyMsg`/`ClientMouseMsg` values. The served model keeps one textarea per client, leaves the input rows blank in the shared frame, and publishes each client's rendered rows to the host, which splices them into that client's stream after every frame. The launcher, REPL, picker and autosave all consult the live record directory so a live code is joined (attach, or an in-terminal `switch:CODE` handoff), never forked.

**Tech Stack:** Go 1.25, Bubble Tea v1.3.10 (`tea.WithInput(nil)` runs no input reader; the standard renderer writes one `Write` per frame), `github.com/charmbracelet/x/input` v0.3.7 (`NewReader(io.Reader, termType, flags)` + `ReadEvents()`), existing `internal/live` host/client, `internal/tui`.

**Spec:** `docs/superpowers/specs/2026-09-12-shared-sessions-design.md`

## Global Constraints

- Frames: `takeover` removed; new host→client `overlay` (verbatim bytes); `bye` reasons gain `switch:CODE`; `clients` drops `holder`. Hello, input, resize, detach, quit, output, size unchanged. Records, sockets unchanged; `store.Session` gains `HostPID int` (`json:"host_pid,omitempty"`).
- Host API: `InputReader()`, `DetachHolder()` removed; `OnInput(func(client int, b []byte))`, `Detach(client int)`, `Switch(client int, code string)`, `SetOverlay(client int, s string)` added. Callbacks still run on host goroutines and must never be called synchronously from the program's `Update` (hand them to Bubble Tea as `tea.Cmd`s).
- Every attached client's keys are delivered tagged; `--view` clients send nothing; `Ctrl+] d` detaches; `Ctrl+] t` is gone (`Ctrl+]` followed by any other key forwards both bytes).
- Served input area: fixed 3 rows (1 in compact), one textarea per client, per-client history, static cursor (no blink) in served mode. Transcript user line: `you> ` with one client, `<label>> ` for every sender with more than one client attached.
- Palette is owned by the client that typed `/`; other clients' keys are ignored while it is open; it closes if the owner detaches. Approval/picker/plan/menu/context-menu/queue modals are shared, driven by whoever presses keys.
- Queue popup shows only the opener's messages; `Agent.EnqueueFrom(text, client)` records the sender.
- Join-not-fork: `--resume CODE` live → attach; picker/`/resume` in a served TUI → `switch:CODE`; in-process TUI and plain mode print `CODE is live elsewhere; join it with: be-code attach CODE`; `be-code` in a workspace with a live session auto-joins the newest one printing `joining live session CODE (be-code --new starts a fresh one)`; `--new` forces a fresh session. After a switch, a host with no turns and no clients quits.
- Save guard: a hosted or in-process run stamps its pid on every save; if the on-disk stamp names a different live pid, the save is skipped, autosave disabled, and one dimmed warning is shown: `session file is owned by live host <pid>; autosave disabled for this session`.
- Version 0.6.0 in `build.mk` and `CHANGELOG.md`. Commits end with the trailer lines given in the dispatch. Every task ends with `make -f build.mk verify`.

---

### Task 1: Key event conversion and per-client parsing

**Files:**
- Create: `internal/live/keys.go`, `internal/live/keypump.go`
- Test: `internal/live/keys_test.go`, `internal/live/keypump_test.go`
- Modify: `go.mod` (add `github.com/charmbracelet/x/input v0.3.7`)

**Interfaces (produced):**

```go
package live

type ClientKeyMsg struct { Client int; Key tea.KeyMsg }
type ClientMouseMsg struct { Client int; Mouse tea.MouseMsg }

// ConvertEvent maps one x/input event to a Bubble Tea v1 message. ok is false for events with no v1 equivalent.
func ConvertEvent(ev input.Event) (msg tea.Msg, ok bool)

// KeyPump parses each client's raw bytes into tagged messages, one parser per client.
type KeyPump struct { /* unexported */ }
func NewKeyPump(termType string, emit func(tea.Msg)) *KeyPump
func (k *KeyPump) Feed(client int, b []byte)   // never blocks the caller for long: writes into the client's pipe
func (k *KeyPump) Drop(client int)             // closes the client's pipe and stops its goroutine
func (k *KeyPump) Close()
```

- [ ] **Step 1: Write the failing tests**

`internal/live/keys_test.go`:

```go
package live

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/input"
)

func key(t *testing.T, ev input.Event) tea.KeyMsg {
	t.Helper()
	m, ok := ConvertEvent(ev)
	if !ok {
		t.Fatalf("event %v not converted", ev)
	}
	k, isKey := m.(tea.KeyMsg)
	if !isKey {
		t.Fatalf("event %v converted to %T, want KeyMsg", ev, m)
	}
	return k
}

func TestConvertRunesAndModifiers(t *testing.T) {
	k := key(t, input.KeyPressEvent{Code: 'a', Text: "a"})
	if k.Type != tea.KeyRunes || string(k.Runes) != "a" || k.Alt {
		t.Fatalf("plain rune: %+v", k)
	}
	k = key(t, input.KeyPressEvent{Code: 'x', Text: "x", Mod: input.ModAlt})
	if k.Type != tea.KeyRunes || string(k.Runes) != "x" || !k.Alt {
		t.Fatalf("alt rune: %+v", k)
	}
	k = key(t, input.KeyPressEvent{Code: 'c', Mod: input.ModCtrl})
	if k.Type != tea.KeyCtrlC {
		t.Fatalf("ctrl+c: %+v", k)
	}
	k = key(t, input.KeyPressEvent{Code: 'q', Mod: input.ModCtrl})
	if k.Type != tea.KeyCtrlQ {
		t.Fatalf("ctrl+q: %+v", k)
	}
	k = key(t, input.KeyPressEvent{Code: input.KeySpace, Text: " "})
	if k.Type != tea.KeySpace || string(k.Runes) != " " {
		t.Fatalf("space: %+v", k)
	}
	k = key(t, input.KeyPressEvent{Code: ']', Mod: input.ModCtrl})
	if k.Type != tea.KeyCtrlCloseBracket {
		t.Fatalf("ctrl+]: %+v", k)
	}
}

func TestConvertSpecialKeys(t *testing.T) {
	cases := map[rune]tea.KeyType{
		input.KeyEnter: tea.KeyEnter, input.KeyTab: tea.KeyTab, input.KeyBackspace: tea.KeyBackspace,
		input.KeyEscape: tea.KeyEsc, input.KeyUp: tea.KeyUp, input.KeyDown: tea.KeyDown,
		input.KeyLeft: tea.KeyLeft, input.KeyRight: tea.KeyRight, input.KeyHome: tea.KeyHome,
		input.KeyEnd: tea.KeyEnd, input.KeyPgUp: tea.KeyPgUp, input.KeyPgDown: tea.KeyPgDown,
		input.KeyDelete: tea.KeyDelete, input.KeyInsert: tea.KeyInsert, input.KeyF1: tea.KeyF1, input.KeyF12: tea.KeyF12,
	}
	for code, want := range cases {
		if got := key(t, input.KeyPressEvent{Code: code}).Type; got != want {
			t.Errorf("code %d: got %v want %v", code, got, want)
		}
	}
	k := key(t, input.KeyPressEvent{Code: input.KeyTab, Mod: input.ModShift})
	if k.Type != tea.KeyShiftTab {
		t.Fatalf("shift+tab: %+v", k)
	}
	k = key(t, input.KeyPressEvent{Code: input.KeyUp, Mod: input.ModShift})
	if k.Type != tea.KeyShiftUp {
		t.Fatalf("shift+up: %+v", k)
	}
	k = key(t, input.KeyPressEvent{Code: input.KeyLeft, Mod: input.ModCtrl})
	if k.Type != tea.KeyCtrlLeft {
		t.Fatalf("ctrl+left: %+v", k)
	}
	k = key(t, input.KeyPressEvent{Code: input.KeyEnter, Mod: input.ModAlt})
	if k.Type != tea.KeyEnter || !k.Alt {
		t.Fatalf("alt+enter: %+v", k)
	}
}

func TestConvertPasteAndMouse(t *testing.T) {
	m, ok := ConvertEvent(input.PasteEvent("two\nlines"))
	k := m.(tea.KeyMsg)
	if !ok || k.Type != tea.KeyRunes || !k.Paste || string(k.Runes) != "two\nlines" {
		t.Fatalf("paste: %+v %v", k, ok)
	}
	m, ok = ConvertEvent(input.MouseClickEvent{X: 3, Y: 4, Button: ansi.MouseLeft})
	mm := m.(tea.MouseMsg)
	if !ok || mm.X != 3 || mm.Y != 4 || mm.Action != tea.MouseActionPress || mm.Button != tea.MouseButtonLeft {
		t.Fatalf("click: %+v", mm)
	}
	m, _ = ConvertEvent(input.MouseReleaseEvent{X: 3, Y: 4, Button: ansi.MouseLeft})
	if m.(tea.MouseMsg).Action != tea.MouseActionRelease {
		t.Fatal("release")
	}
	m, _ = ConvertEvent(input.MouseMotionEvent{X: 5, Y: 6, Button: ansi.MouseLeft})
	if mm := m.(tea.MouseMsg); mm.Action != tea.MouseActionMotion || mm.Button != tea.MouseButtonLeft {
		t.Fatalf("drag: %+v", mm)
	}
	m, _ = ConvertEvent(input.MouseWheelEvent{X: 1, Y: 1, Button: ansi.MouseWheelUp, Mod: input.ModShift})
	if mm := m.(tea.MouseMsg); mm.Button != tea.MouseButtonWheelUp || mm.Action != tea.MouseActionPress || !mm.Shift {
		t.Fatalf("wheel: %+v", mm)
	}
	if _, ok := ConvertEvent(input.UnknownEvent("x")); ok {
		t.Fatal("unknown events must be dropped")
	}
}
```

`internal/live/keypump_test.go`:

```go
package live

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestKeyPumpTagsClientsAndParsesSequences(t *testing.T) {
	got := make(chan tea.Msg, 16)
	kp := NewKeyPump("xterm-256color", func(m tea.Msg) { got <- m })
	defer kp.Close()
	kp.Feed(1, []byte("a"))
	kp.Feed(2, []byte("\x1b[A")) // up arrow
	kp.Feed(1, []byte("\x1b[<0;4;5M")) // SGR left press at col 4,row 5
	seen := map[int][]tea.Msg{}
	for i := 0; i < 3; i++ {
		select {
		case m := <-got:
			switch v := m.(type) {
			case ClientKeyMsg:
				seen[v.Client] = append(seen[v.Client], v.Key)
			case ClientMouseMsg:
				seen[v.Client] = append(seen[v.Client], v.Mouse)
			default:
				t.Fatalf("untagged message %T", m)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("pump delivered fewer than 3 messages")
		}
	}
	if len(seen[1]) != 2 || len(seen[2]) != 1 {
		t.Fatalf("tagging: %+v", seen)
	}
	if k := seen[1][0].(tea.KeyMsg); k.Type != tea.KeyRunes || string(k.Runes) != "a" {
		t.Fatalf("client 1 key: %+v", k)
	}
	if k := seen[2][0].(tea.KeyMsg); k.Type != tea.KeyUp {
		t.Fatalf("client 2 key: %+v", k)
	}
	if mm := seen[1][1].(tea.MouseMsg); mm.X != 3 || mm.Y != 4 || mm.Action != tea.MouseActionPress {
		t.Fatalf("client 1 mouse: %+v", mm)
	}
	kp.Drop(2)
	kp.Feed(2, []byte("z")) // dropped client: silently ignored
	select {
	case m := <-got:
		t.Fatalf("message after Drop: %+v", m)
	case <-time.After(200 * time.Millisecond):
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go get github.com/charmbracelet/x/input@v0.3.7 && go mod tidy`, then `go test ./internal/live -run 'TestConvert|TestKeyPump'`; expected undefined `ConvertEvent`, `NewKeyPump`.

- [ ] **Step 3: Implement**

`internal/live/keys.go`:

```go
package live

import (
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/input"
)

// ClientKeyMsg is a key press from one attached terminal.
type ClientKeyMsg struct {
	Client int
	Key    tea.KeyMsg
}

// ClientMouseMsg is a mouse event from one attached terminal.
type ClientMouseMsg struct {
	Client int
	Mouse  tea.MouseMsg
}

var specialKeys = map[rune]tea.KeyType{
	input.KeyEnter: tea.KeyEnter, input.KeyTab: tea.KeyTab, input.KeyBackspace: tea.KeyBackspace,
	input.KeyEscape: tea.KeyEsc, input.KeyUp: tea.KeyUp, input.KeyDown: tea.KeyDown,
	input.KeyLeft: tea.KeyLeft, input.KeyRight: tea.KeyRight, input.KeyHome: tea.KeyHome,
	input.KeyEnd: tea.KeyEnd, input.KeyPgUp: tea.KeyPgUp, input.KeyPgDown: tea.KeyPgDown,
	input.KeyDelete: tea.KeyDelete, input.KeyInsert: tea.KeyInsert,
	input.KeyF1: tea.KeyF1, input.KeyF2: tea.KeyF2, input.KeyF3: tea.KeyF3, input.KeyF4: tea.KeyF4,
	input.KeyF5: tea.KeyF5, input.KeyF6: tea.KeyF6, input.KeyF7: tea.KeyF7, input.KeyF8: tea.KeyF8,
	input.KeyF9: tea.KeyF9, input.KeyF10: tea.KeyF10, input.KeyF11: tea.KeyF11, input.KeyF12: tea.KeyF12,
	input.KeyF13: tea.KeyF13, input.KeyF14: tea.KeyF14, input.KeyF15: tea.KeyF15, input.KeyF16: tea.KeyF16,
	input.KeyF17: tea.KeyF17, input.KeyF18: tea.KeyF18, input.KeyF19: tea.KeyF19, input.KeyF20: tea.KeyF20,
}

// modified arrows/keys that Bubble Tea v1 names individually.
type modKey struct {
	code rune
	mod  input.KeyMod
}

var modifiedKeys = map[modKey]tea.KeyType{
	{input.KeyTab, input.ModShift}: tea.KeyShiftTab,
	{input.KeyUp, input.ModShift}: tea.KeyShiftUp, {input.KeyDown, input.ModShift}: tea.KeyShiftDown,
	{input.KeyLeft, input.ModShift}: tea.KeyShiftLeft, {input.KeyRight, input.ModShift}: tea.KeyShiftRight,
	{input.KeyHome, input.ModShift}: tea.KeyShiftHome, {input.KeyEnd, input.ModShift}: tea.KeyShiftEnd,
	{input.KeyUp, input.ModCtrl}: tea.KeyCtrlUp, {input.KeyDown, input.ModCtrl}: tea.KeyCtrlDown,
	{input.KeyLeft, input.ModCtrl}: tea.KeyCtrlLeft, {input.KeyRight, input.ModCtrl}: tea.KeyCtrlRight,
	{input.KeyHome, input.ModCtrl}: tea.KeyCtrlHome, {input.KeyEnd, input.ModCtrl}: tea.KeyCtrlEnd,
	{input.KeyPgUp, input.ModCtrl}: tea.KeyCtrlPgUp, {input.KeyPgDown, input.ModCtrl}: tea.KeyCtrlPgDown,
	{input.KeyUp, input.ModCtrl | input.ModShift}: tea.KeyCtrlShiftUp, {input.KeyDown, input.ModCtrl | input.ModShift}: tea.KeyCtrlShiftDown,
	{input.KeyLeft, input.ModCtrl | input.ModShift}: tea.KeyCtrlShiftLeft, {input.KeyRight, input.ModCtrl | input.ModShift}: tea.KeyCtrlShiftRight,
	{input.KeyHome, input.ModCtrl | input.ModShift}: tea.KeyCtrlShiftHome, {input.KeyEnd, input.ModCtrl | input.ModShift}: tea.KeyCtrlShiftEnd,
}

var ctrlPunct = map[rune]tea.KeyType{
	'@': tea.KeyCtrlAt, ' ': tea.KeyCtrlAt, '[': tea.KeyEsc, '\\': tea.KeyCtrlBackslash,
	']': tea.KeyCtrlCloseBracket, '^': tea.KeyCtrlCaret, '_': tea.KeyCtrlUnderscore, '?': tea.KeyCtrlQuestionMark,
}

func convertKey(k input.Key) (tea.KeyMsg, bool) {
	alt := k.Mod&input.ModAlt != 0
	mod := k.Mod &^ (input.ModAlt | input.ModCapsLock | input.ModNumLock)
	if t, ok := modifiedKeys[modKey{k.Code, mod}]; ok {
		return tea.KeyMsg{Type: t, Alt: alt}, true
	}
	if mod&input.ModCtrl != 0 {
		c := unicode.ToLower(k.Code)
		if c >= 'a' && c <= 'z' {
			return tea.KeyMsg{Type: tea.KeyType(c - 'a' + 1), Alt: alt}, true // KeyCtrlA == 1
		}
		if t, ok := ctrlPunct[c]; ok {
			return tea.KeyMsg{Type: t, Alt: alt}, true
		}
	}
	if t, ok := specialKeys[k.Code]; ok {
		return tea.KeyMsg{Type: t, Alt: alt}, true
	}
	if k.Code == input.KeySpace {
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}, Alt: alt}, true
	}
	if k.Text != "" {
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k.Text), Alt: alt}, true
	}
	if k.Code > 0 && k.Code < input.KeyExtended && unicode.IsPrint(k.Code) {
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{k.Code}, Alt: alt}, true
	}
	return tea.KeyMsg{}, false
}

var mouseButtons = map[ansi.MouseButton]tea.MouseButton{
	ansi.MouseNone: tea.MouseButtonNone, ansi.MouseLeft: tea.MouseButtonLeft, ansi.MouseMiddle: tea.MouseButtonMiddle,
	ansi.MouseRight: tea.MouseButtonRight, ansi.MouseWheelUp: tea.MouseButtonWheelUp, ansi.MouseWheelDown: tea.MouseButtonWheelDown,
	ansi.MouseWheelLeft: tea.MouseButtonWheelLeft, ansi.MouseWheelRight: tea.MouseButtonWheelRight,
	ansi.MouseBackward: tea.MouseButtonBackward, ansi.MouseForward: tea.MouseButtonForward,
}

func convertMouse(m input.Mouse, action tea.MouseAction) tea.MouseMsg {
	return tea.MouseMsg{X: m.X, Y: m.Y, Action: action, Button: mouseButtons[m.Button],
		Shift: m.Mod&input.ModShift != 0, Alt: m.Mod&input.ModAlt != 0, Ctrl: m.Mod&input.ModCtrl != 0}
}

// ConvertEvent maps an x/input event onto the Bubble Tea v1 message the
// program would have received from its own input reader.
func ConvertEvent(ev input.Event) (tea.Msg, bool) {
	switch e := ev.(type) {
	case input.KeyPressEvent:
		return convertKey(input.Key(e))
	case input.PasteEvent:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(string(e)), Paste: true}, true
	case input.MouseClickEvent:
		return convertMouse(input.Mouse(e), tea.MouseActionPress), true
	case input.MouseReleaseEvent:
		return convertMouse(input.Mouse(e), tea.MouseActionRelease), true
	case input.MouseMotionEvent:
		return convertMouse(input.Mouse(e), tea.MouseActionMotion), true
	case input.MouseWheelEvent:
		return convertMouse(input.Mouse(e), tea.MouseActionPress), true
	}
	return nil, false
}
```

Check the exact names `ansi.MouseLeft`, `ansi.MouseMiddle`, `ansi.MouseRight`, `ansi.MouseWheelUp/Down/Left/Right`, `ansi.MouseBackward/Forward` in `x/ansi@v0.11.6/mouse.go` (they are aliases of `MouseButton1…`); if a name differs, use the numbered constant with a comment. `tea.KeyCtrlA` is `1` in v1 (`keyETX`-style numbering) — assert it in the test with `tea.KeyType('c'-'a'+1) == tea.KeyCtrlC` if in doubt.

`internal/live/keypump.go`:

```go
package live

import (
	"io"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/input"
)

// KeyPump turns each client's raw terminal bytes into tagged Bubble Tea
// messages. One x/input reader per client keeps escape sequences from two
// terminals from interleaving.
type KeyPump struct {
	term string
	emit func(tea.Msg)
	mu      sync.Mutex
	pipes   map[int]*io.PipeWriter
	dropped map[int]bool
	wg      sync.WaitGroup
}

func NewKeyPump(termType string, emit func(tea.Msg)) *KeyPump {
	return &KeyPump{term: termType, emit: emit, pipes: map[int]*io.PipeWriter{}, dropped: map[int]bool{}}
}

func (k *KeyPump) Feed(client int, b []byte) {
	k.mu.Lock()
	if k.dropped[client] {
		k.mu.Unlock()
		return
	}
	w, ok := k.pipes[client]
	if !ok {
		var r *io.PipeReader
		r, w = io.Pipe()
		k.pipes[client] = w
		k.wg.Add(1)
		go k.run(client, r)
	}
	k.mu.Unlock()
	w.Write(b) // blocks only until the client's own parser goroutine reads it
}

func (k *KeyPump) run(client int, r *io.PipeReader) {
	defer k.wg.Done()
	rd, err := input.NewReader(r, k.term, 0)
	if err != nil {
		r.Close()
		return
	}
	for {
		evs, err := rd.ReadEvents()
		if err != nil {
			return
		}
		for _, ev := range evs {
			m, ok := ConvertEvent(ev)
			if !ok {
				continue
			}
			switch v := m.(type) {
			case tea.KeyMsg:
				k.emit(ClientKeyMsg{Client: client, Key: v})
			case tea.MouseMsg:
				k.emit(ClientMouseMsg{Client: client, Mouse: v})
			}
		}
	}
}

func (k *KeyPump) Drop(client int) {
	k.mu.Lock()
	w, ok := k.pipes[client]
	delete(k.pipes, client)
	k.dropped[client] = true
	k.mu.Unlock()
	if ok {
		w.Close()
	}
}

func (k *KeyPump) Close() {
	k.mu.Lock()
	for id, w := range k.pipes {
		w.Close()
		delete(k.pipes, id)
	}
	k.mu.Unlock()
	k.wg.Wait()
}
```

Client ids are never reused by the host (`nextID` only grows), so `dropped` never needs pruning.

- [ ] **Step 4: Run** `go test ./internal/live -race -run 'TestConvert|TestKeyPump' -v`; expected PASS.
- [ ] **Step 5: Verify and commit** — `make -f build.mk verify`; commit `live: key event conversion and per-client key pump`.

---

### Task 2: Host — tagged input, overlays, switch, no holder

**Files:**
- Modify: `internal/live/frame.go` (`FTakeover` → `FOverlay`; `ClientInfo` drops `Holder`), `internal/live/host.go`
- Test: `internal/live/host_test.go` (update holder tests; add overlay/switch/OnInput tests)

**Interfaces:**
- Consumes: nothing new.
- Produces: `func (h *Host) OnInput(f func(client int, b []byte))`, `func (h *Host) Detach(client int)`, `func (h *Host) Switch(client int, code string)`, `func (h *Host) SetOverlay(client int, s string)`, `const FOverlay FrameType`, `const ReasonSwitchPrefix = "switch:"`. Removed: `FTakeover`, `pumpInput`, `inCh/inR/inW/stopInput`, `holder` election. **Kept as compatibility shims until Task 4 (so `internal/tui` and `cmd` keep compiling at every commit):** `InputReader() io.Reader` (returns a pipe nothing writes to), `DetachHolder()` (no-op), `ClientInfo.Holder` (always false). Task 4 deletes all three together with the TUI's uses.

- [ ] **Step 1: Write the failing tests** (replace the holder-based assertions in `host_test.go`; keep the helpers `startHost`, `dial`, `within`, `fakeClient` — add an `overlay chan []byte` to `fakeClient` filled on `FOverlay`):

```go
func TestHostDeliversTaggedInputFromEveryClient(t *testing.T) {
	h, sock := startHost(t)
	type in struct{ id int; b string }
	got := make(chan in, 8)
	h.OnInput(func(id int, b []byte) { got <- in{id, string(b)} })
	a := dial(t, sock, "tok", "a", 100, 40)
	within(t, time.Second, func() bool { return len(h.Clients()) == 1 })
	b := dial(t, sock, "tok", "b", 80, 24)
	within(t, time.Second, func() bool { return len(h.Clients()) == 2 })
	WriteFrame(a.conn, FInput, []byte("A"))
	WriteFrame(b.conn, FInput, []byte("B"))
	seen := map[int]string{}
	for i := 0; i < 2; i++ {
		select {
		case x := <-got:
			seen[x.id] += x.b
		case <-time.After(time.Second):
			t.Fatal("input not delivered")
		}
	}
	cl := h.Clients()
	if seen[cl[0].ID] != "A" || seen[cl[1].ID] != "B" {
		t.Fatalf("tagging: %+v (clients %+v)", seen, cl)
	}
}

func TestHostOverlayGoesToOneClientAndFollowsEveryFrame(t *testing.T) {
	h, sock := startHost(t)
	a := dial(t, sock, "tok", "a", 100, 40)
	b := dial(t, sock, "tok", "b", 100, 40)
	within(t, time.Second, func() bool { return len(h.Clients()) == 2 })
	idA := h.Clients()[0].ID
	h.SetOverlay(idA, "OVERLAY-A")
	select {
	case p := <-a.overlay:
		if string(p) != "OVERLAY-A" {
			t.Fatalf("overlay payload %q", p)
		}
	case <-time.After(time.Second):
		t.Fatal("a did not get its overlay")
	}
	select {
	case p := <-b.overlay:
		t.Fatalf("b received a's overlay: %q", p)
	case <-time.After(200 * time.Millisecond):
	}
	h.Output().Write([]byte("FRAME"))
	<-a.out
	select { // a's overlay is re-sent right after the shared frame
	case p := <-a.overlay:
		if string(p) != "OVERLAY-A" {
			t.Fatalf("re-sent overlay %q", p)
		}
	case <-time.After(time.Second):
		t.Fatal("overlay not re-sent after a frame")
	}
	<-b.out
	select {
	case <-b.overlay:
		t.Fatal("b has no overlay yet must not receive one")
	case <-time.After(200 * time.Millisecond):
	}
	h.SetOverlay(idA, "OVERLAY-A") // unchanged: no write
	select {
	case <-a.overlay:
		t.Fatal("unchanged overlay was re-sent")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestHostSwitchAndDetachByID(t *testing.T) {
	h, sock := startHost(t)
	a := dial(t, sock, "tok", "a", 100, 40)
	b := dial(t, sock, "tok", "b", 100, 40)
	within(t, time.Second, func() bool { return len(h.Clients()) == 2 })
	cl := h.Clients()
	h.Switch(cl[0].ID, "ZZZ999")
	select {
	case r := <-a.bye:
		if r != ReasonSwitchPrefix+"ZZZ999" {
			t.Fatalf("switch reason %q", r)
		}
	case <-time.After(time.Second):
		t.Fatal("no switch bye")
	}
	within(t, time.Second, func() bool { return len(h.Clients()) == 1 })
	h.Detach(cl[1].ID)
	select {
	case r := <-b.bye:
		if r != ReasonDetached {
			t.Fatalf("detach reason %q", r)
		}
	case <-time.After(time.Second):
		t.Fatal("no detach bye")
	}
	h.Detach(12345) // unknown id: no-op
}
```

Delete `TestHostFansOutAndElectsHolder`'s holder assertions and the `FTakeover` usage; replace its input section with the tagged test above; keep the size assertions. Update `TestHostDetachHolderPassesToMostRecentRemaining` to `TestHostDetachByIDKeepsOthers` (detach one of three by id; the other two remain; size recomputed).

- [ ] **Step 2: Run to verify failure** — `go test ./internal/live -run TestHost`; expected undefined `OnInput`, `SetOverlay`, `Switch`, `FOverlay`, `ReasonSwitchPrefix`.

- [ ] **Step 3: Implement**

`frame.go`: rename `FTakeover` to `FOverlay` in the const block (keep the position so numbering is unchanged; comment `// host→client: one client's private input rows`); leave `Holder bool` on `ClientInfo` for now (Task 4 removes it).

`host.go` changes:
- Remove fields `holder`, `inCh`, `stopInput`; remove `pumpInput`; keep `inR`/`inW` only so `InputReader()` still returns a reader (nothing writes to it; `Close` still closes it); turn `DetachHolder()` into a no-op with a `// Deprecated: removed in Task 4` comment; add `onInput func(int, []byte)` and `func (h *Host) OnInput(f func(int, []byte)) { h.mu.Lock(); h.onInput = f; h.mu.Unlock() }`.
- `client` gains `overlay string` (guarded by `h.mu`).
- In `handle`: `case FInput:` → `h.mu.Lock(); f := h.onInput; h.mu.Unlock(); if f != nil { f(c.id, append([]byte(nil), payload...)) }`. Remove `case FTakeover`. Registration no longer sets a holder.
- `enqueue` treats `FOverlay` like `FOutput` for eviction (both are screen bytes; the latest wins).
- `detach`: drop the holder hand-off block. Keep everything else.
- Add:

```go
// ReasonSwitchPrefix prefixes a bye that tells the client to reattach to
// another live session in the same terminal.
const ReasonSwitchPrefix = "switch:"

func (h *Host) byID(id int) *client {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.clients {
		if c.id == id {
			return c
		}
	}
	return nil
}

// Detach drops one client with the ordinary detached reason.
func (h *Host) Detach(id int) {
	if c := h.byID(id); c != nil {
		h.detach(c, ReasonDetached)
	}
}

// Switch hands one client over to another live session.
func (h *Host) Switch(id int, code string) {
	if c := h.byID(id); c != nil {
		h.detach(c, ReasonSwitchPrefix+code)
	}
}

// SetOverlay records a client's private input rows and sends them. The
// same rows are appended to every shared frame that client receives, so a
// full repaint never erases them. An unchanged overlay is not re-sent.
func (h *Host) SetOverlay(id int, s string) {
	h.mu.Lock()
	var c *client
	for _, x := range h.clients {
		if x.id == id {
			c = x
		}
	}
	if c == nil || c.overlay == s {
		h.mu.Unlock()
		return
	}
	c.overlay = s
	h.mu.Unlock()
	c.enqueue(FOverlay, []byte(s))
}
```

- `fanout.Write`: after `c.enqueue(FOutput, cp)`, read `c.overlay` under `h.mu` (take the snapshot of overlays together with the client slice) and, if non-empty, `c.enqueue(FOverlay, []byte(overlay))`.
- `Close`: nothing holder-related remains; keep the bounded detach loop.
- `infosLocked`: never sets `Holder` (field kept until Task 4).
- `ReasonDetached` currently lives in `client.go`; it is referenced from `host.go` already, fine.

- [ ] **Step 4: Run** `go test ./internal/live -race`; expected PASS (fix any test still referencing `Holder`/`FTakeover`/`InputReader`, including `client_test.go`'s takeover test — remove `TestAttachTakeoverOrdering` or rewrite it as "Ctrl+] t forwards both bytes" in Task 3).
- [ ] **Step 5: Verify and commit** — `make -f build.mk verify` (the shims keep `tui`/`cmd` compiling; the TUI's holder tests must still pass because `Holder` is simply never true — adjust `internal/tui/served_test.go` expectations that assert holder text only if they fail, and say so in the report); commit `live: tagged input, per-client overlays, switch; holder election removed`.

---

### Task 3: Client — overlay frames, switch reason, chord change

**Files:**
- Modify: `internal/live/client.go`, `internal/live/chord.go`, `cmd/live.go` (`attachLive` switch loop)
- Test: `internal/live/client_test.go`, `internal/live/chord_test.go`, `cmd/live_test.go`

**Interfaces:**
- Consumes: `FOverlay`, `ReasonSwitchPrefix` (Task 2).
- Produces: `func SwitchTarget(reason string) (code string, ok bool)`; `attachLive` loops on switch.

- [ ] **Step 1: Write the failing tests**

`chord_test.go` — replace the takeover case:

```go
	c.Feed([]byte{0x1d}, now)
	fwd, act = c.Feed([]byte("t"), now)
	if string(fwd) != "\x1dt" || act != ActionNone {
		t.Fatalf("Ctrl+] t is no longer a chord; both bytes forward: %q %v", fwd, act)
	}
```

`client_test.go` additions:

```go
func TestAttachWritesOverlayFramesVerbatim(t *testing.T) {
	h, sock := startHost(t)
	rec := &Record{Code: "T", PID: os.Getpid(), Socket: sock, Token: "tok"}
	stdinR, _ := io.Pipe()
	var stdout syncBuffer
	go Attach(context.Background(), rec, AttachOptions{Label: "t", Stdin: stdinR, Stdout: &stdout,
		Raw: func() (func(), error) { return func() {}, nil }, Size: func() (int, int) { return 80, 24 }, UTF8: true})
	within(t, time.Second, func() bool { return len(h.Clients()) == 1 })
	h.SetOverlay(h.Clients()[0].ID, "\x1b[22;1Hhello")
	within(t, time.Second, func() bool { return strings.Contains(stdout.String(), "\x1b[22;1Hhello") })
}

func TestSwitchTarget(t *testing.T) {
	if code, ok := SwitchTarget("switch:ABC123"); !ok || code != "ABC123" {
		t.Fatalf("%q %v", code, ok)
	}
	if _, ok := SwitchTarget("detached"); ok {
		t.Fatal("plain reason is not a switch")
	}
}

func TestAttachReturnsSwitchReason(t *testing.T) {
	h, sock := startHost(t)
	rec := &Record{Code: "T", PID: os.Getpid(), Socket: sock, Token: "tok"}
	stdinR, _ := io.Pipe()
	done := make(chan string, 1)
	go func() {
		r, _ := Attach(context.Background(), rec, AttachOptions{Label: "t", Stdin: stdinR, Stdout: io.Discard,
			Raw: func() (func(), error) { return func() {}, nil }, Size: func() (int, int) { return 80, 24 }, UTF8: true})
		done <- r
	}()
	within(t, time.Second, func() bool { return len(h.Clients()) == 1 })
	h.Switch(h.Clients()[0].ID, "NEW001")
	select {
	case r := <-done:
		if r != "switch:NEW001" {
			t.Fatalf("reason %q", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Attach did not return on switch")
	}
}
```

`cmd/live_test.go` addition (unit-level, no sockets): extract the reason-handling of `attachLive` into `func nextAttach(dir, reason string) (*live.Record, string)`; test that `"switch:ABC"` with a live record for `ABC` returns that record and no message; with no record returns nil and `"ABC ended before you could join it"`; `""`/`ReasonDetached` return nil and the "detached from … still running" message; `ReasonEnded` returns nil and `"<code>: session ended"` is printed by the caller as today.

- [ ] **Step 2: Run to verify failure** — expected undefined `SwitchTarget`, failing chord case.

- [ ] **Step 3: Implement**

`chord.go`: remove `ActionTakeover` and the `'t'` case (keep the `Action` type with `ActionNone`/`ActionDetach`); `'d'` and `chordKey` still detach; any other key appends `chordKey` then the key.

`client.go`:
- Reader loop: `case FOverlay: opt.Stdout.Write(p)` (identical to `FOutput`).
- Remove the `ActionTakeover` branch in the stdin goroutine.
- Add:

```go
// SwitchTarget extracts the session code from a "switch:CODE" bye reason.
func SwitchTarget(reason string) (string, bool) {
	if strings.HasPrefix(reason, ReasonSwitchPrefix) && len(reason) > len(ReasonSwitchPrefix) {
		return reason[len(ReasonSwitchPrefix):], true
	}
	return "", false
}
```

- On a switch reason the client must not print anything and must leave the alt screen for the next attach to re-enter cleanly: treat it like a detach for screen handling (clear + exit modes), which is what the non-ended path already does.

`cmd/live.go` `attachLive`:

```go
func attachLive(ctx context.Context, rec *live.Record, view bool) error {
	dir, err := live.Dir()
	if err != nil {
		return err
	}
	for {
		opt := live.DefaultAttachOptions()
		opt.View = view
		reason, err := live.Attach(ctx, rec, opt)
		if err != nil {
			return err
		}
		next, msg := nextAttach(dir, rec.Code, reason)
		if msg != "" {
			fmt.Println(msg)
		}
		if next == nil {
			return nil
		}
		rec = next
	}
}

// nextAttach interprets a bye reason: a switch names the record to attach
// next; everything else ends the attach with a line for the user.
func nextAttach(dir, code, reason string) (*live.Record, string) {
	if target, ok := live.SwitchTarget(reason); ok {
		if rec := findLive(dir, target); rec != nil {
			return rec, ""
		}
		return nil, fmt.Sprintf("%s ended before you could join it", target)
	}
	if reason == "" || reason == live.ReasonDetached {
		return nil, fmt.Sprintf("detached from %s (still running); be-code attach %s to return", code, code)
	}
	return nil, fmt.Sprintf("%s: %s", code, reason)
}
```

- [ ] **Step 4: Run** `go test ./internal/live -race && go test ./cmd/...`; expected PASS.
- [ ] **Step 5: Verify and commit** — `make -f build.mk verify`; commit `live: overlay frames, switch reason, takeover chord removed`.

---

### Task 4: TUI — per-client textareas and tagged routing

**Files:**
- Modify: `internal/tui/tui.go`, `internal/tui/queue.go`, `internal/tui/palette.go`, `internal/tui/served.go`, `internal/agent/inbox.go`, `internal/ui/repl.go` (only if `Peek` changes), `internal/live/host.go` + `frame.go` (delete the Task 2 shims: `InputReader`, `DetachHolder`, `ClientInfo.Holder`, `inR/inW`)
- Create: `internal/tui/inputs.go`
- Test: `internal/tui/shared_test.go`, `internal/agent/inbox_test.go` (extend)

**Interfaces:**
- Consumes: `live.ClientKeyMsg`, `live.ClientMouseMsg` (Task 1), `live.ClientInfo` without `Holder` (Task 2).
- Produces: `func (m *Model) inputFor(client int) *textarea.Model`, `func (m *Model) dropInput(client int)`, `func (m *Model) clientLabel(client int) string`, `Model.paletteOwner int`, `func (a *Agent) EnqueueFrom(text string, from int)`, `type InboxItem struct{ Text string; From int }`, `func (a *Agent) Items() []InboxItem`; `handleKey(k tea.KeyMsg, from int)`, `handleMouse(msg tea.MouseMsg, from int)`; `Model.inputRows() int`.

- [ ] **Step 1: Write the failing tests**

`internal/agent/inbox_test.go` (add):

```go
func TestEnqueueFromRecordsSender(t *testing.T) {
	a := &Agent{}
	a.Enqueue("plain")
	a.EnqueueFrom("from two", 2)
	items := a.Items()
	if len(items) != 2 || items[0].From != 0 || items[1].From != 2 || items[1].Text != "from two" {
		t.Fatalf("%+v", items)
	}
	if got := a.Peek(); len(got) != 2 || got[1] != "from two" {
		t.Fatalf("Peek must still return texts in order: %v", got)
	}
}
```

`internal/tui/shared_test.go`:

```go
package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/live"
)

func runes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func twoClients(t *testing.T) *Model {
	t.Helper()
	m := newTestModel(t)
	m.served = true
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.Update(clientsMsg{{ID: 1, Label: "desk (pid 1)", UTF8: true}, {ID: 2, Label: "tablet (pid 2)", UTF8: true}})
	return m
}

func TestEachClientTypesIntoItsOwnInput(t *testing.T) {
	m := twoClients(t)
	m.Update(live.ClientKeyMsg{Client: 1, Key: runes("hello")})
	m.Update(live.ClientKeyMsg{Client: 2, Key: runes("world")})
	m.Update(live.ClientKeyMsg{Client: 1, Key: runes("!")})
	if got := m.inputFor(1).Value(); got != "hello!" {
		t.Fatalf("client 1 input %q", got)
	}
	if got := m.inputFor(2).Value(); got != "world" {
		t.Fatalf("client 2 input %q", got)
	}
	if !strings.Contains(m.View(), m.blankInputRows()) {
		t.Fatalf("served view must leave %d blank input rows:\n%s", m.inputRows(), m.View())
	}
	if strings.Contains(m.View(), "hello!") || strings.Contains(m.View(), "world") {
		t.Fatal("shared frame must not render any client's input text")
	}
}

func TestEnterQueuesOnlyTheSendersTextWithLabel(t *testing.T) {
	m := twoClients(t)
	m.mode = modeBusy
	m.running = true
	m.Update(live.ClientKeyMsg{Client: 2, Key: runes("do it later")})
	m.Update(live.ClientKeyMsg{Client: 1, Key: runes("mine")})
	m.Update(live.ClientKeyMsg{Client: 2, Key: tea.KeyMsg{Type: tea.KeyEnter}})
	items := m.ag.Items()
	if len(items) != 1 || items[0].Text != "do it later" || items[0].From != 2 {
		t.Fatalf("queue: %+v", items)
	}
	if got := m.inputFor(1).Value(); got != "mine" {
		t.Fatalf("client 1's draft was disturbed: %q", got)
	}
	if !strings.Contains(m.transcript.String(), "tablet (pid 2)> ") {
		t.Fatalf("queued line lacks the sender label:\n%s", m.transcript.String())
	}
}

func TestTranscriptPrefixUsesLabelOnlyWithSeveralClients(t *testing.T) {
	m := newTestModel(t)
	if got := m.userPrefix(0); got != "you> " {
		t.Fatalf("single client prefix %q", got)
	}
	m = twoClients(t)
	if got := m.userPrefix(1); got != "desk (pid 1)> " {
		t.Fatalf("multi client prefix %q", got)
	}
}

func TestQueuePopupShowsOnlyTheOpenersMessages(t *testing.T) {
	m := twoClients(t)
	m.mode = modeBusy
	m.running = true
	m.ag.EnqueueFrom("from one", 1)
	m.ag.EnqueueFrom("from two", 2)
	m.Update(live.ClientKeyMsg{Client: 2, Key: tea.KeyMsg{Type: tea.KeyUp}})
	if m.mode != modeQueue {
		t.Fatal("Up on an empty input opens the queue popup")
	}
	v := m.View()
	if !strings.Contains(v, "from two") || strings.Contains(v, "from one") {
		t.Fatalf("popup must list only the opener's messages:\n%s", v)
	}
	m.Update(live.ClientKeyMsg{Client: 2, Key: tea.KeyMsg{Type: tea.KeyEnter}}) // edit
	if got := m.inputFor(2).Value(); got != "from two" {
		t.Fatalf("edit pulled %q into client 2's input", got)
	}
	if items := m.ag.Items(); len(items) != 1 || items[0].From != 1 {
		t.Fatalf("client 1's message must remain queued: %+v", items)
	}
}

func TestPaletteIsOwnedByTheClientThatOpenedIt(t *testing.T) {
	m := twoClients(t)
	m.Update(live.ClientKeyMsg{Client: 2, Key: runes("/")})
	if m.mode != modePalette || m.paletteOwner != 2 {
		t.Fatalf("palette owner %d mode %v", m.paletteOwner, m.mode)
	}
	m.Update(live.ClientKeyMsg{Client: 1, Key: tea.KeyMsg{Type: tea.KeyEsc}})
	if m.mode != modePalette {
		t.Fatal("another client's Esc must not close the owner's palette")
	}
	m.Update(clientsMsg{{ID: 1, Label: "desk (pid 1)", UTF8: true}}) // owner detached
	if m.mode == modePalette {
		t.Fatal("palette must close when its owner detaches")
	}
	if m.inputs[2] != nil {
		t.Fatal("detached client's textarea must be dropped")
	}
}

func TestInProcessModelStillUsesClientZero(t *testing.T) {
	m := newTestModel(t)
	m.Update(runes("abc"))
	if got := m.inputFor(0).Value(); got != "abc" {
		t.Fatalf("plain KeyMsg goes to client 0: %q", got)
	}
	if !strings.Contains(m.View(), "abc") {
		t.Fatal("in-process view renders the single textarea")
	}
}

func TestDetachCommandDetachesTheTypingClient(t *testing.T) {
	m := twoClients(t)
	var detached []int
	m.detachClient = func(id int) { detached = append(detached, id) }
	m.Update(live.ClientKeyMsg{Client: 2, Key: runes("/detach")})
	_, cmd := m.Update(live.ClientKeyMsg{Client: 2, Key: tea.KeyMsg{Type: tea.KeyEnter}})
	if cmd != nil {
		cmd()
	}
	if len(detached) != 1 || detached[0] != 2 {
		t.Fatalf("detached %v", detached)
	}
}

func TestBottomLineListsClientLabels(t *testing.T) {
	m := twoClients(t)
	v := m.View()
	if !strings.Contains(v, "⧉ 2") || !strings.Contains(v, "desk (pid 1), tablet (pid 2)") {
		t.Fatalf("bottom line:\n%s", v)
	}
	if strings.Contains(v, "input:") || strings.Contains(v, "Ctrl+] t") {
		t.Fatal("holder text must be gone")
	}
}
```

- [ ] **Step 2: Run to verify failure** — expected compile errors (`inputFor`, `EnqueueFrom`, `Items`, `paletteOwner`, `detachClient`, `userPrefix`, `inputRows`).

- [ ] **Step 3: Implement**

`internal/agent/inbox.go`:

```go
// InboxItem is a queued message and the client that queued it (0 = the
// local terminal).
type InboxItem struct {
	Text string
	From int
}
```

Change the inbox slice to `[]InboxItem`; `Enqueue(text)` = `EnqueueFrom(text, 0)`; add `EnqueueFrom(text string, from int)`; `Items() []InboxItem` (copy); `Peek() []string` maps texts; `DrainInbox` returns texts; `Remove(i)` returns the text. Keep every existing signature.

`internal/tui/inputs.go`:

```go
package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textarea"
)

// newInputArea builds one textarea with the prompt, height and key
// bindings every input line shares (moved here from New).
func (m *Model) newInputArea() *textarea.Model {
	ta := textarea.New()
	// Move the textarea configuration block from New() (tui.go, the lines from
	// `ta := textarea.New()` through `ta.Focus()`: placeholder, SetPromptFunc,
	// SetHeight(3), CharLimit, ShowLineNumbers=false, KeyMap changes) here
	// verbatim, so New() and per-client creation share one definition.
	if m.served {
		ta.Cursor.SetMode(cursor.CursorStatic) // blinking would push an overlay every 500 ms per client
	}
	ta.SetWidth(m.inputWidth())
	t := &ta
	return t
}

// inputFor returns the textarea of one client, creating it on first use.
// The in-process TUI is client 0.
func (m *Model) inputFor(client int) *textarea.Model {
	if m.inputs == nil {
		m.inputs = map[int]*textarea.Model{}
	}
	ta, ok := m.inputs[client]
	if !ok {
		ta = m.newInputArea()
		m.inputs[client] = ta
	}
	return ta
}

func (m *Model) dropInput(client int) { delete(m.inputs, client) }

// inputRows is the height of the input area: the textarea's own height in
// process, and the fixed served height (3, or 1 in compact).
func (m *Model) inputRows() int {
	if m.served && m.compact() {
		return 1
	}
	return 3
}

// clientLabel names a client for transcript prefixes and the bottom line.
func (m *Model) clientLabel(client int) string {
	for _, c := range m.clients {
		if c.ID == client {
			return c.Label
		}
	}
	return "you"
}

// userPrefix is the transcript prefix for a user line: "you> " with a single
// terminal, "<label>> " for every sender once several are attached.
func (m *Model) userPrefix(client int) string {
	if len(m.clients) > 1 {
		return m.clientLabel(client) + "> "
	}
	return "you> "
}

// blankInputRows is what the shared frame shows where each client's private
// input rows will be spliced in by the host.
func (m *Model) blankInputRows() string {
	row := strings.Repeat(" ", m.inputWidth())
	rows := make([]string, m.inputRows())
	for i := range rows {
		rows[i] = row
	}
	return strings.Join(rows, "\n")
}

func (m *Model) inputWidth() int { return m.width - m.wheelWidthNow() - 2 }
```

(`m.wheelWidthNow()` is whatever `layout()` uses today for the wheel column width in full vs compact — name it accordingly if a helper already exists.)

`tui.go`:
- Replace field `input textarea.Model` with `inputs map[int]*textarea.Model`; `New()` calls `m.inputFor(0)` after sizing. Every `m.input.` site becomes `in := m.inputFor(from); in.…` inside handlers that have `from`, and `m.inputFor(0)` in in-process-only paths (there should be none left that lack a client; the queue edit uses `m.queueOwner`, the palette uses `m.paletteOwner`).
- `Update`: `case tea.KeyMsg: return m.handleKey(msg, 0)`; `case live.ClientKeyMsg: return m.handleKey(msg.Key, msg.Client)`; `case tea.MouseMsg: return m.handleMouse(msg, 0)`; `case live.ClientMouseMsg: return m.handleMouse(msg.Mouse, msg.Client)`. The generic textarea update for non-key messages (`if m.mode == modeInput { m.input, cmd = m.input.Update(msg) }`) becomes a loop over `m.inputs`.
- `handleKey(k tea.KeyMsg, from int)`: modal handlers keep their signatures except `handlePaletteKey(k, from)` (ignore when `from != m.paletteOwner`), `handleQueueKey(k, from)` (edit pulls into `m.inputFor(m.queueOwner)`), `handleBusyKey(k, from)`, and the `modeInput` block uses `in := m.inputFor(from)`. Enter: `m.startTurnFrom(text, from)` which appends `m.userPrefix(from)+text` (rename the existing `startTurn`'s prefix line); busy Enter: `m.ag.EnqueueFrom(text, from)` and the queued line uses `m.userPrefix(from)` before `queued (delivered at the next step)> `: render as `stDim.Render("queued> ") + m.userPrefix(from) + text` when several clients, else as today. `/` opens the palette with `m.paletteOwner = from`. Up on empty input opens the queue with `m.queueOwner = from`.
- `slashCommand(text string, from int)`: `/detach` → `if m.detachClient == nil { … not served … }; id := from; return m, func() tea.Msg { m.detachClient(id); return nil }` (replaces `detachHolder`). `/clients` prints labels only.
- `View()`: in served mode the input row is `lipgloss.JoinHorizontal(lipgloss.Top, m.blankInputRows(), " "+m.wheelView())`; in process it stays `m.inputFor(0).View()`.
- `layout()`: `SetWidth(m.inputWidth())` and `SetHeight(m.inputRows())` on every textarea in `m.inputs`.
- `bottomLine`: clients block becomes `stAccent.Render(fmt.Sprintf(" ⧉ %d", n)) + stDim.Render(" · "+labels)` where `labels` joins client labels with `", "` and is truncated with `…` to fit `m.width`; compact keeps `⧉ N` only (Task 5 of the previous plan already handles the compact branch; adapt the label part).

`queue.go`: `openQueue(from int)` sets `m.queueOwner = from`, filters `m.ag.Items()` by `From == from` keeping the full indices in `m.queueIndex []int` so `Remove(m.queueIndex[cursor])` still addresses the right message; `queueBox` renders the filtered list.

`palette.go`: `openPalette(initial string, from int)`; `onPick` uses `m.inputFor(m.paletteOwner)`.

`served.go`: `updateClients` also drops textareas of departed clients (`m.dropInput(id)`), closes the palette if `m.paletteOwner` departed (`m.mode = modeInput; m.picker = nil`), and closes the queue popup if `m.queueOwner` departed (`m.ag.Hold(false)`). Replace `detachHolder func()` with `detachClient func(id int)`; `RunServed` sets `m.detachClient = h.Detach` (Task 5 finishes `RunServed`). Keep `RunServed` compiling by temporarily using `tea.WithInput(nil)` and wiring the key pump:

```go
	pump := live.NewKeyPump("xterm-256color", func(msg tea.Msg) { p.Send(msg) })
	defer pump.Close()
	h.OnInput(pump.Feed)
```

(`p.Send` from the pump goroutines is fine: they are not the update goroutine.) Drop `h.InputReader()`.

- [ ] **Step 4: Run** `go test ./internal/tui ./internal/agent ./internal/ui`; expected PASS, including every pre-existing TUI test (they send plain `tea.KeyMsg`, which routes to client 0).
- [ ] **Step 5: Verify and commit** — `make -f build.mk verify` must be green again here; commit `tui: per-client input lines, tagged key routing, per-sender queue and palette`.

---

### Task 5: Served rendering — overlays on the wire and the served startup

**Files:**
- Modify: `internal/tui/served.go`, `internal/tui/tui.go` (`Update` tail), `internal/tui/compact.go` (input rows in compact)
- Test: `internal/tui/served_test.go`

**Interfaces:**
- Consumes: `Host.SetOverlay`, `Host.OnInput`, `live.KeyPump` (Tasks 1–2), `inputFor`/`inputRows`/`blankInputRows` (Task 4).
- Produces: `func (m *Model) overlayFor(client int) string`; `func (m *Model) publishOverlay(client int)`; `func (m *Model) publishAllOverlays()`.

- [ ] **Step 1: Write the failing tests** (append to `served_test.go`):

```go
func TestOverlayPositionsEachRowAndParksTheCursor(t *testing.T) {
	m := twoClients(t) // 100x30, served, 3 input rows
	m.Update(live.ClientKeyMsg{Client: 2, Key: runes("typed")})
	ov := m.overlayFor(2)
	first := m.height - m.inputRows() // 1-based row of the first input line
	for r := 0; r < m.inputRows(); r++ {
		if !strings.Contains(ov, fmt.Sprintf("\x1b[%d;1H", first+r)) {
			t.Fatalf("overlay lacks a move to row %d:\n%q", first+r, ov)
		}
	}
	if !strings.Contains(ov, "typed") {
		t.Fatal("overlay lacks the client's text")
	}
	if !strings.HasSuffix(ov, fmt.Sprintf("\x1b[%d;%dH", m.height, m.width)) {
		t.Fatalf("overlay must park the cursor at the bottom-right:\n%q", ov)
	}
	if strings.Contains(ov, "\x1b[K") {
		t.Fatal("overlay must pad rows, not clear to end of line (the wheel lives to the right)")
	}
}

func TestOverlayIsPublishedAfterAKeyAndForEveryoneOnResize(t *testing.T) {
	m := twoClients(t)
	var pub []int
	m.setOverlay = func(id int, s string) { pub = append(pub, id) }
	m.Update(live.ClientKeyMsg{Client: 1, Key: runes("a")})
	if len(pub) != 1 || pub[0] != 1 {
		t.Fatalf("after a key: %v", pub)
	}
	pub = nil
	m.Update(tea.WindowSizeMsg{Width: 90, Height: 28})
	sort.Ints(pub)
	if len(pub) != 2 || pub[0] != 1 || pub[1] != 2 {
		t.Fatalf("after a resize: %v", pub)
	}
}
```

- [ ] **Step 2: Run to verify failure** — undefined `overlayFor`, `setOverlay`.

- [ ] **Step 3: Implement** (`served.go`):

```go
// overlayFor renders one client's private input rows as absolute-positioned
// terminal output. Rows are padded to the input width rather than cleared,
// because the shared wheel column sits to their right.
func (m *Model) overlayFor(client int) string {
	ta := m.inputFor(client)
	lines := strings.Split(ta.View(), "\n")
	var b strings.Builder
	first := m.height - m.inputRows()
	w := m.inputWidth()
	for r := 0; r < m.inputRows(); r++ {
		line := ""
		if r < len(lines) {
			line = lines[r]
		}
		fmt.Fprintf(&b, "\x1b[%d;1H%s", first+r, padToWidth(line, w))
	}
	fmt.Fprintf(&b, "\x1b[%d;%dH", m.height, m.width)
	return b.String()
}
```

`padToWidth` pads with spaces using `lipgloss.Width` (styled text) and truncates with `ansi.Truncate` if wider. Model fields: `setOverlay func(id int, s string)` (nil in process). `publishOverlay(id)`: `if m.served && m.setOverlay != nil { m.setOverlay(id, m.overlayFor(id)) }`. `publishAllOverlays()` loops `m.inputs`. Calls: at the end of `handleKey` for `from` (all modes — a key may have changed the textarea, e.g. queue edit into the owner's input), after `WindowSizeMsg` handling and `clientsMsg` (all), after a textarea blink/other non-key update (all — only when `m.served`; static cursors mean this is rare). `RunServed` sets `m.setOverlay = h.SetOverlay`, builds the program with `tea.WithInput(nil)`, `pump := live.NewKeyPump(...)`, `h.OnInput(pump.Feed)`, and drops the pump entry on detach inside `updateClients` via a `m.dropKeyClient func(int)` set to `pump.Drop`. `compact.go`: the compact layout's input area is one row (`inputRows()` already returns 1 when served and compact; ensure `layout()` applies `SetHeight(1)` so the textarea scrolls in one line).

- [ ] **Step 4: Run** `go test ./internal/tui -race`; expected PASS.
- [ ] **Step 5: Verify and commit** — `make -f build.mk verify`; commit `tui: per-client overlays over the shared frame; tagged input in served mode`.

---

### Task 6: Join, never fork — launcher, REPL, picker, save guard

**Files:**
- Modify: `cmd/live.go` (`launchServed`, `--new`), `cmd/root.go` (flag), `internal/ui/repl.go` (`/resume`), `internal/tui/picker.go`, `internal/tui/tui.go` (`resumeSession`), `internal/tui/served.go` (empty-host quit), `internal/store/sessions.go` (`HostPID`), `internal/agent/loop.go` (`autosave` guard), `internal/live/record.go` (`LiveCode(dir, code) *Record` helper if `findLive` should move into `live`)
- Test: `cmd/live_test.go`, `internal/tui/picker_test.go` (new), `internal/agent/save_guard_test.go` (new), `internal/store/sessions_test.go`

**Interfaces:**
- Consumes: `Host.Switch` (Task 2), `nextAttach` (Task 3).
- Produces: `func LiveCode(dir, code string) *Record` in `internal/live` (moves `findLive`'s logic: `List` then match); `var flagNew bool`; `func (m *Model) switchTo(code string) (tea.Model, tea.Cmd)`; `store.Session.HostPID`; `func (a *Agent) SaveGuard() (blocked bool, owner int)`.

- [ ] **Step 1: Write the failing tests**

`internal/store/sessions_test.go` (add): a saved session round-trips `HostPID`.

`internal/agent/save_guard_test.go`:

```go
func TestAutosaveSkipsWhenAnotherLiveHostOwnsTheFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := newTestAgent(t) // the package's existing helper that builds an Agent with a fresh store.Session
	// A real other process stands in for the other host: its pid is alive.
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skip("no sleep binary")
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	other := *a.Session
	other.HostPID = cmd.Process.Pid
	if err := other.Save(); err != nil {
		t.Fatal(err)
	}
	a.autosave("hello")
	if !a.saveDisabled {
		t.Fatal("autosave must be disabled when a live host owns the file")
	}
	got, _ := store.Load(a.Session.ID)
	if got.HostPID != cmd.Process.Pid {
		t.Fatal("the other host's file was overwritten")
	}
	// a dead stamp is ignored
	cmd.Process.Kill(); cmd.Wait()
	a.saveDisabled = false
	a.autosave("hello")
	got, _ = store.Load(a.Session.ID)
	if got.HostPID != os.Getpid() {
		t.Fatal("dead stamp must be taken over")
	}
}
```

(Adapt the agent construction to the package's existing test helpers; the assertions are what matter.)

`cmd/live_test.go` (add): `hostArgs` includes `--new=true` when the flag is changed; `nextAttach` cases from Task 3; a `decideStart(dir, workspace, flagResume, flagNew) (rec *live.Record, msg string)` helper tested against a temp live dir: live record for the workspace + no flags → that record and `joining live session CODE (be-code --new starts a fresh one)`; `--new` → nil; `--resume X` with `X` live → `X`'s record and `joining live session X`; `--resume X` not live → nil.

`internal/tui/picker_test.go`:

```go
func TestSessionPickerMarksLiveRowsAndSwitches(t *testing.T) {
	m := twoClients(t)
	var switched []string
	m.switchClient = func(id int, code string) { switched = append(switched, fmt.Sprint(id, ":", code)) }
	m.liveCodes = func() map[string]bool { return map[string]bool{"ABC123": true} }
	items := m.sessionItems([]store.SessionMeta{{ID: "1", Code: "ABC123", Title: "live one"}, {ID: "2", Code: "DEF456", Title: "saved"}})
	if !strings.Contains(items[0].label, "LIVE") || strings.Contains(items[1].label, "LIVE") {
		t.Fatalf("live marking: %+v", items)
	}
	_, cmd := m.resumeFrom("1", 2) // client 2 picked the live row
	if cmd != nil { cmd() }
	if len(switched) != 1 || switched[0] != "2:ABC123" {
		t.Fatalf("switch: %v", switched)
	}
}

func TestInProcessResumeOfALiveCodePrintsInsteadOfLoading(t *testing.T) {
	m := newTestModel(t)
	m.liveCodes = func() map[string]bool { return map[string]bool{"ABC123": true} }
	before := m.ag.Session
	m.resumeFrom("1", 0) // "1" has code ABC123 per the stubbed store.Load below
	if m.ag.Session != before || !strings.Contains(m.transcript.String(), "ABC123 is live elsewhere; join it with: be-code attach ABC123") {
		t.Fatalf("in-process resume of a live code:\n%s", m.transcript.String())
	}
}
```

(Give `resumeFrom` a `loadSession func(id string) (*store.Session, error)` hook on the model, defaulting to `store.Load`, so the test can stub it.)

- [ ] **Step 2: Run to verify failure** — undefined symbols.

- [ ] **Step 3: Implement**

`store`: add `HostPID int \`json:"host_pid,omitempty"\`` to `Session`.

`agent/loop.go` `autosave`: before `Save()`:

```go
	if a.saveDisabled {
		return
	}
	if on, err := store.Load(a.Session.ID); err == nil && on.HostPID != 0 && on.HostPID != os.Getpid() && live.Alive(on.HostPID) {
		a.saveDisabled = true
		a.notice("session file is owned by live host %d; autosave disabled for this session", on.HostPID)
		return
	}
	a.Session.HostPID = os.Getpid()
```

(`store.Load` accepts an ID; if it also accepts codes, keep using the ID. `a.notice` already routes to the UI's dimmed transcript line via `Events`.)

`live/record.go`: `func LiveCode(dir, code string) *Record` (List, match Code). `cmd/live.go`'s `findLive` becomes a thin wrapper or is replaced.

`cmd/root.go`: `--new` persistent bool `flagNew` ("start a fresh session even when this workspace has a live one"); `hostArgs` forwards it like any changed flag (it is harmless in the host).

`cmd/live.go` `launchServed`: replace the offer prompt with:

```go
	if rec, msg := decideStart(dir, workspace, flagResume, flagNew); rec != nil {
		fmt.Println(msg)
		return attachLive(ctx, rec, false)
	}
```

```go
// decideStart picks a live session to join, or nil to start a fresh host.
func decideStart(dir, workspace, resume string, fresh bool) (*live.Record, string) {
	if resume != "" {
		s, err := store.Load(resume)
		if err != nil {
			return nil, ""
		}
		if rec := live.LiveCode(dir, s.ResumeCode()); rec != nil {
			return rec, "joining live session " + rec.Code
		}
		return nil, ""
	}
	if fresh {
		return nil, ""
	}
	if rec := newestLiveIn(dir, workspace); rec != nil {
		return rec, fmt.Sprintf("joining live session %s (be-code --new starts a fresh one)", rec.Code)
	}
	return nil, ""
}
```

Remove `offerAttach` and its `fmt.Scanln` prompt (and their tests). The "already live" error path after `decideStart` is unreachable for `--resume`; keep it as a guard.

`internal/ui/repl.go` `/resume`: before `store.Load`, `if dir, err := live.Dir(); err == nil { if rec := live.LiveCode(dir, codeOf(id)); rec != nil { print "<code> is live elsewhere; join it with: be-code attach <code>"; break } }` where `codeOf` loads the session to get its code (one `store.Load`, reuse it).

`internal/tui`: model hooks `liveCodes func() map[string]bool` (default: `live.Dir` + `live.List` → set of codes), `loadSession func(string) (*store.Session, error)` (default `store.Load`), `switchClient func(id int, code string)` (served: `h.Switch`). `sessionItems(metas)` marks live codes with a `LIVE` tag in the label. `resumeFrom(id string, from int)`:

```go
func (m *Model) resumeFrom(id string, from int) (tea.Model, tea.Cmd) {
	s, err := m.loadSession(id)
	if err != nil {
		m.appendLine(stErr.Render(err.Error()))
		return m, nil
	}
	code := s.ResumeCode()
	if m.liveCodes()[code] && !(m.ag.Session != nil && m.ag.Session.ResumeCode() == code) {
		if m.served && m.switchClient != nil {
			m.switchPending = true
			sw, c := m.switchClient, from
			return m, func() tea.Msg { sw(c, code); return nil }
		}
		m.appendLine(stDim.Render(fmt.Sprintf("%s is live elsewhere; join it with: be-code attach %s", code, code)))
		return m, nil
	}
	m.ag.Resume(s)
	… (existing resumed/handoff lines)
}
```

The picker's `onPick` passes the picking client (the picker handler now receives `from`; store it in `m.pickerOwner` when the picker opens, like the palette). `resumeSession(id)` becomes `resumeFrom(id, 0)` for the `/resume` slash command path, which also receives `from` from `slashCommand`.

Empty-host quit: in `updateClients`, `if m.switchPending && len(m.clients) == 0 && m.ag.Session != nil && len(m.ag.Session.Messages) == 0 { return tea.Quit }` (return the cmd from `updateClients`; adjust its signature). `switchPending` is cleared when a client attaches.

- [ ] **Step 4: Run** `go test ./cmd/... ./internal/tui ./internal/agent ./internal/store ./internal/ui`; expected PASS.
- [ ] **Step 5: Verify and commit** — `make -f build.mk verify`; commit `join live sessions everywhere: --resume attaches, picker switches, --new, save guard`.

---

### Task 7: Docs, version, checklist, pty walk

**Files:**
- Modify: `README.md`, `CHANGELOG.md`, `build.mk`, `/home/sbrown/Documents/Development/BE-CodeRedux/CLAUDE.md` (outside the repo), `docs/live-checklist.md`
- Create: `docs/superpowers/pty_walk.py` is NOT committed; the walk lives in the SDD workspace or scratch; document the scenes in the checklist only.

- [ ] **Step 1: Docs**
  - README "Live sessions and handoff" → "Shared sessions": every terminal has its own input line; `<label>> ` prefixes; joining is automatic (`be-code` in a live workspace, `--resume CODE`, `attach CODE`, the session manager picker); `--new`; `Ctrl+] d`; shared modals; the save guard message; remove `Ctrl+] t` and the holder text; config keys unchanged.
  - CHANGELOG `## v0.6.0 — 2026-09-12 — shared sessions`: per-terminal input, tagged input, overlays, join-not-fork (`--resume` attaches, picker switch, auto-join with `--new`), save guard, takeover chord removed, protocol note (`overlay` frame, `switch:` reason, `holder` field gone).
  - `build.mk` VERSION 0.6.0.
  - Root CLAUDE.md "Live sessions": tagged input via `KeyPump`/`ConvertEvent`, per-client textareas and `overlayFor`, `SetOverlay` splice, join-not-fork paths (`decideStart`, `resumeFrom`, `switch:` bye handled by `attachLive`), save guard, and the rule that `/detach` and switches are `tea.Cmd`s.
  - `docs/live-checklist.md`: replace the takeover step with "type in both terminals at once: each sees only its own draft; both messages appear in the transcript with labels"; add "from a third terminal: `be-code --resume CODE` joins the live session, no prompt"; add "in a fresh `be-code --new` session, `/menu → Resume a saved session` → the LIVE row: this terminal switches into the live session and the empty one exits".

- [ ] **Step 2: Verification** — `make -f build.mk verify`, `sh test/e2e/run_e2e.sh`, `GOOS=windows GOARCH=amd64 go build ./...`, `GOOS=darwin GOARCH=arm64 go build ./...`; then a pty walk (throwaway HOME with the real config copied in, the built binary) covering: two terminals typing at once (assert each pty shows its own draft only and both transcript lines with labels), `--resume CODE` from a third pty joining (assert the "joining live session" line and a full frame), a `--new` session switching via the picker (assert the switching pty shows the live session's transcript and the empty host's record disappears), and `/quit` ending it for everyone with the resume line at column 0. Leave no hosts and an untouched real `~/.be-code`.

- [ ] **Step 3: Commit** — `docs: shared sessions (0.6.0)`.
