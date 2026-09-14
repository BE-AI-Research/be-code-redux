# Per-Terminal Rendering Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every terminal attached to a shared session gets its own renderer — its own size, layout, scroll position, input line and theme — while the transcript, agent, queue, roster, approvals and pickers stay one shared thing in the host.

**Architecture:** `internal/tui` splits into `Session` (the core: agent, transcript entries, running state, roster, shared asks, guarded by one mutex, fanning changes out to views through non-blocking mailboxes) and `View` (one Bubble Tea model per terminal, embedding `*Session`, owning size, styles, viewport, textarea and local popups). The served runner starts one program per attached client on `Host.ClientOutput(id)`; the overlay and shared-minimum-size machinery of 0.6.0 is removed.

**Tech Stack:** Go 1.22, Bubble Tea, Bubbles (textarea, viewport, spinner), lipgloss, `charmbracelet/x/input` (unchanged), `internal/live` unix-socket host.

**Spec:** `docs/superpowers/specs/2026-09-13-per-terminal-rendering-design.md`

## Global Constraints

- Target version **0.8.0** (`build.mk` VERSION, CHANGELOG heading, root `CLAUDE.md`).
- **Every task leaves `make -f build.mk verify` green** (vet + build + all tests). No task may be committed with a red tree.
- **Views never write core fields without `Session.mu`.** `View.Update` and `View.View` take `s.mu` for their whole body; every `Session` method that mutates state locks it; agent-goroutine callbacks lock it.
- **Broadcast never blocks.** `Session.broadcast` does a non-blocking send into each view's mailbox; a full mailbox drops the message.
- **Invariant:** no program is running when the host's closing lines are written (`cmd/live.go` after `RunServed` returns).
- `internal/live/client.go`, `chord.go`, `cmd/live.go`'s launcher (`launchServed`, `attachLive`, `decideStart`, `--new`) and `be-code attach` are not changed, except that the client stops handling the removed `FOverlay` frame (Task 8) and `cmd/live.go` stops calling `ClearOverlays` (Task 8).
- Plain mode (`internal/ui/repl.go`) is untouched.
- Config key `client_themes` (`map[string]string`), keyed by `live.LabelKey(label)` = the label with its ` (pid N)` suffix stripped. Lookup on attach: `client_themes[key]` → `theme` → `dark`. `/theme <name>` applies to the calling view only and saves `client_themes[key]`; `/theme default <name>` sets `theme`.
- Exact user-facing strings: `answered by <label>`, `theme "x" is not known; using <name>`, `/theme` with no argument prints `<name> (remembered for "<key>")`, `<name> (config default)` or `<name> (built-in default)`; bye reason for a panicked view is `view error`.
- Commit messages end with:
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01BZcp8PaqA96XDvu2GeDcAb
  ```
- Do not touch the user's real `~/.be-code`; tests use `t.TempDir()` HOME (`tempHome(t)`).

---

## File structure

| File | Responsibility |
|---|---|
| `internal/tui/theme.go` | `Palette` table (unchanged), `styles` struct, `newStyles(name) (styles, bool)`, `ThemeNames`. The ten `st*` globals are gone. |
| `internal/tui/entry.go` | `entry`, `entryKind`, `renderEntry(e entry, st styles, width int, compact, richText bool) string`. |
| `internal/tui/session.go` | `Session`: core state, `NewSession`, agent event wiring, entry append helpers, `broadcast`, views registry (`attachView`/`detachView`), `Submit`/`startTurn`/`finishTurn`, `SetClients`, `Quit`, `Status()`, `userPrefix`. |
| `internal/tui/ask.go` | `ask`, `askKind`, `askAnswer`, `Session.Ask`, `Session.Answer`, `Session.CancelAsk`, `reviewTerminal` (moved from `review.go`). |
| `internal/tui/mailbox.go` | `mailbox`: buffered channel + optional delivery goroutine; `send` never blocks. |
| `internal/tui/view.go` | `View` (was `Model`, `git mv tui.go view.go`): per-terminal state, `NewView`, `Init`/`Update`/`View`, key handlers, slash dispatch, `layout`, header/bottom line. |
| `internal/tui/served.go` | `(*Session).RunServed(ctx, h)`: one program per client, mailboxes, key routing, idle limit, empty-after-switch, quit sequence. `(*Session).RunLocal(ctx)`: the `--no-host` path. |
| `internal/tui/inputs.go` | **deleted** in Task 6; `userPrefix`/`clientLabel`/`clientLabels` move to `session.go`, `newInputArea`/`setInputPrompt`/`inputRows`/`inputWidth`/`inputRow` to `view.go`. |
| `internal/live/host.go` | `ClientOutput(id)`, `OnClientSize`, `Drop(id, reason)`, repaint-after-eviction; later removal of overlays and the shared size. |
| `internal/live/label.go` | `LabelKey`. |
| `internal/config/config.go` | `ClientThemes`. |
| `cmd/live.go`, `cmd/root.go` | Construct `Session`, call `RunServed`/`RunLocal`. |

Task order keeps the tree green at every commit: 1 styles → 2 entries → 3 host additions → 4 Session/View split (single program still) → 5 shared asks → 6 one program per client → 7 themes per device → 8 removals in `live` and `cmd` → 9 served integration test and failure isolation → 10 docs.

---

### Task 1: `styles` value instead of global style variables

**Files:**
- Modify: `internal/tui/theme.go`
- Modify: every file in `internal/tui/` that references `stAccent`, `stBorder`, `stDim`, `stErr`, `stModalTi`, `stOK`, `stStatus`, `stTool`, `stUser`, `stWarn` (155 references outside `theme.go`)
- Modify: `internal/tui/tui.go` (`Model` gains `st styles`; `New` sets it)
- Modify: `internal/tui/palette.go:174-194` (`applyTheme`)
- Test: `internal/tui/theme_test.go`

**Interfaces:**
- Produces: `type styles struct { Accent, Dim, Tool, Err, OK, Warn, User, Status, ModalTi, Border lipgloss.Style; name string }`; `func newStyles(name string) (styles, bool)`; `func (s styles) Name() string`; `Model.st styles`.
- `SetTheme` is deleted. Nothing outside `internal/tui` used it (`grep -rn SetTheme --include=*.go` outside the package must be empty; if `cmd` uses it, replace with nothing — the model applies its own styles).

- [ ] **Step 1: Write the failing test**

Replace `TestUnknownThemeFallsBackToDark` and `TestThemeTableResolvesDistinctAccents` in `internal/tui/theme_test.go` with:

```go
func TestNewStylesResolvesEveryThemeAndRejectsUnknown(t *testing.T) {
	seen := map[string]bool{}
	for _, name := range ThemeNames() {
		st, ok := newStyles(name)
		if !ok {
			t.Fatalf("theme %q not resolved", name)
		}
		if st.Name() != name {
			t.Fatalf("styles for %q report name %q", name, st.Name())
		}
		if name != "mono" {
			key := st.Accent.GetForeground().(lipgloss.Color)
			if seen[string(key)] && name != "monokai" { // monokai shares an accent with no one, but keep the check loose
				t.Logf("accent %s reused by %s", key, name)
			}
			seen[string(key)] = true
		}
	}
	if _, ok := newStyles("no-such-theme"); ok {
		t.Fatal("unknown theme resolved")
	}
	dark, _ := newStyles("dark")
	nord, _ := newStyles("nord")
	if dark.Accent.Render("x") == nord.Accent.Render("x") {
		t.Fatal("two themes render the accent identically; styles are not per value")
	}
}

func TestModelCarriesItsOwnStyles(t *testing.T) {
	m := newTestModel(t)
	if m.st.Name() != "dark" {
		t.Fatalf("default styles %q, want dark", m.st.Name())
	}
	m.applyTheme("nord")
	if m.st.Name() != "nord" {
		t.Fatalf("applyTheme left styles at %q", m.st.Name())
	}
	other := newTestModel(t)
	if other.st.Name() != "dark" {
		t.Fatalf("a second model inherited the first one's theme: %q — styles are still global", other.st.Name())
	}
}
```

Keep `TestThemeCommandAppliesAndPersists` and `TestThemePickerListsAll` as they are (they go through `applyTheme`/`themeItems`).

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/tui/ -run 'TestNewStyles|TestModelCarriesItsOwnStyles' 2>&1 | head`
Expected: build failure `undefined: newStyles` and `m.st undefined`.

- [ ] **Step 3: Replace the globals with a `styles` value**

In `internal/tui/theme.go`, delete the `var (stAccent ... )` block and `SetTheme`; add:

```go
// styles is one theme's set of lipgloss styles. Every View carries its own
// value, so two terminals on one session can render in different themes.
type styles struct {
	Accent, Dim, Tool, Err, OK, Warn, User, Status, ModalTi, Border lipgloss.Style
	name                                                            string
}

// Name is the theme this styles value was built from.
func (s styles) Name() string { return s.name }

// newStyles builds the styles for a theme. ok is false for an unknown name;
// the caller decides the fallback (View uses dark).
func newStyles(name string) (styles, bool) {
	p, ok := themes[name]
	if !ok {
		return styles{}, false
	}
	st := styles{name: p.Name}
	if p.Mono {
		plain := lipgloss.NewStyle()
		st.Accent, st.Dim, st.Tool, st.Err, st.OK, st.Warn = plain, plain, plain, plain, plain, plain
		st.User = lipgloss.NewStyle().Bold(true)
		st.Status = lipgloss.NewStyle().Reverse(true).Padding(0, 1)
		st.ModalTi = lipgloss.NewStyle().Bold(true)
		st.Border = lipgloss.NewStyle().Border(lipgloss.NormalBorder()).Padding(0, 1)
		return st, true
	}
	fg := func(c string) lipgloss.Style { return lipgloss.NewStyle().Foreground(lipgloss.Color(c)) }
	st.Accent, st.Dim, st.Tool = fg(p.Accent), fg(p.Dim), fg(p.Tool)
	st.Err, st.OK, st.Warn = fg(p.Err), fg(p.OK), fg(p.Warn)
	st.User = fg(p.User).Bold(true)
	st.Status = lipgloss.NewStyle().Background(lipgloss.Color(p.StatusBG)).Foreground(lipgloss.Color(p.StatusFG)).Padding(0, 1)
	st.ModalTi = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(p.ModalTitle))
	border := lipgloss.RoundedBorder()
	if p.SquareBorder {
		border = lipgloss.NormalBorder()
	}
	st.Border = lipgloss.NewStyle().Border(border).BorderForeground(lipgloss.Color(p.Border)).Padding(0, 1)
	return st, true
}

// stylesOr is newStyles with the dark fallback for unknown names.
func stylesOr(name string) styles {
	if st, ok := newStyles(name); ok {
		return st
	}
	st, _ := newStyles("dark")
	return st
}
```

In `internal/tui/tui.go`: add `st styles` to `Model` (next to `richText`); in `New`, replace `SetTheme(cfg.Theme)` with `st := stylesOr(cfg.Theme)` and set `st: st` in the literal, and `sp.Style = st.Accent` (build `st` before `sp`).

Mechanical rewrite of every reference (run from `be-code/`):

```bash
cd internal/tui && for f in *.go; do
  [ "$f" = theme.go ] && continue
  sed -i -E 's/\bstAccent\b/m.st.Accent/g; s/\bstBorder\b/m.st.Border/g; s/\bstDim\b/m.st.Dim/g; s/\bstErr\b/m.st.Err/g; s/\bstModalTi\b/m.st.ModalTi/g; s/\bstOK\b/m.st.OK/g; s/\bstStatus\b/m.st.Status/g; s/\bstTool\b/m.st.Tool/g; s/\bstUser\b/m.st.User/g; s/\bstWarn\b/m.st.Warn/g' "$f"
done
```

Then fix the sites where the receiver is not `m` by hand (`go build ./... ` names them): `reviewTerminal` methods use `t.m.st`; functions without a `*Model` receiver that used a style must take a `styles` parameter or move onto `*Model`. In tests, `stAccent.Render("LIVE")` style assertions become `m.st.Accent.Render(...)`.

`applyTheme` in `palette.go`:

```go
func (m *Model) applyTheme(name string) (tea.Model, tea.Cmd) {
	st, ok := newStyles(name)
	if !ok {
		m.appendLine(m.st.Err.Render("unknown theme " + name + "; try /theme to pick one"))
		return m, nil
	}
	m.st = st
	m.spin.Style = st.Accent
	m.cfg.Theme = name
	m.richText = name != "mono"
	if m.cfg.ThemeTerminalColors && m.termWrite != nil {
		m.termWrite(terminalColorSeq(name))
	}
	if err := m.cfg.Save(); err != nil {
		m.appendLine(m.st.Warn.Render("theme set to " + name + " for this session; could not save config: " + err.Error()))
	} else {
		m.appendLine(m.st.OK.Render("theme set to " + name))
	}
	m.refreshTranscript()
	return m, nil
}
```

(The transcript is still pre-rendered text in this task; Task 2 makes it re-render with the new styles.)

- [ ] **Step 4: Build, then run the whole package**

Run: `go build ./... && go vet ./internal/tui/ && go test ./internal/tui/`
Expected: PASS. `grep -rn 'stDim\|stAccent\|SetTheme' --include=*.go . | grep -v 'm.st\.\|t.m.st\.'` prints nothing.

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "tui: styles are a value on the model, not package globals"
```

---

### Task 2: Transcript as entries, rendered per model

**Files:**
- Create: `internal/tui/entry.go`
- Modify: `internal/tui/tui.go` (every `appendLine(...)` call, `flushStreaming`, `refreshTranscript`, `Model` fields)
- Modify: `internal/tui/served.go`, `queue.go`, `palette.go`, `picker.go`, `selection.go`, `review.go` (their `appendLine` calls)
- Test: `internal/tui/entry_test.go` (new)

**Interfaces:**
- Produces: `type entryKind int` with constants `entryPlain, entryUser, entryAssistant, entryTool, entryToolOK, entryToolErr, entryDim, entryOK, entryWarn, entryErr, entryNote, entryError, entryQueued, entryVerdict`; `type entry struct { Kind entryKind; Label, Text string }`; `func renderEntry(e entry, st styles, width int, compact, richText bool) string`; `Model.entries []entry`; `func (m *Model) appendEntry(e entry)`; `func (m *Model) rebuild()`.
- `appendLine(s string)` is **deleted**; every caller becomes `appendEntry`.

- [ ] **Step 1: Write the failing test**

`internal/tui/entry_test.go`:

```go
package tui

import (
	"strings"
	"testing"
)

func TestRenderEntryUsesTheGivenStylesAndWidth(t *testing.T) {
	dark := stylesOr("dark")
	nord := stylesOr("nord")
	e := entry{Kind: entryDim, Text: "attached: phone"}
	if renderEntry(e, dark, 80, false, true) == renderEntry(e, nord, 80, false, true) {
		t.Fatal("dim entry renders identically under two themes")
	}
	user := entry{Kind: entryUser, Label: "you> ", Text: "hello"}
	if got := renderEntry(user, dark, 80, false, true); !strings.Contains(got, "hello") || !strings.Contains(got, "you> ") {
		t.Fatalf("user entry: %q", got)
	}
	tool := entry{Kind: entryTool, Label: "read_file", Text: strings.Repeat("a", 200)}
	full := renderEntry(tool, dark, 120, false, true)
	narrow := renderEntry(tool, dark, 40, true, true)
	if !strings.Contains(full, "● read_file") || len(narrow) >= len(full) {
		t.Fatalf("tool args not truncated for the compact width:\nfull   %q\nnarrow %q", full, narrow)
	}
	md := entry{Kind: entryAssistant, Text: "# Title\n\nsome *text*"}
	if renderEntry(md, dark, 80, false, true) == renderEntry(md, dark, 80, false, false) {
		t.Fatal("assistant entry ignores richText")
	}
	verdict := entry{Kind: entryVerdict, Label: "file_write", Text: "approved"}
	if got := renderEntry(verdict, dark, 80, false, true); !strings.Contains(got, "approved") {
		t.Fatalf("verdict: %q", got)
	}
}

func TestTranscriptRebuildsFromEntriesOnThemeChange(t *testing.T) {
	m := newTestModel(t)
	m.appendEntry(entry{Kind: entryOK, Text: "model set to x"})
	before := m.wrapped
	m.applyTheme("nord")
	if m.wrapped == before {
		t.Fatal("theme change did not re-render the transcript")
	}
	if !strings.Contains(m.wrapped, "model set to x") {
		t.Fatalf("entry lost on rebuild: %q", m.wrapped)
	}
	if len(m.entries) < 2 { // the entry plus the "theme set to nord" line
		t.Fatalf("entries = %d", len(m.entries))
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/tui/ -run 'TestRenderEntry|TestTranscriptRebuilds' 2>&1 | head -5`
Expected: build failure `undefined: entry`.

- [ ] **Step 3: Implement `entry.go`**

```go
package tui

import (
	"strings"

	"github.com/brown-enterprises/be-code/internal/ui"
)

// entryKind says how a transcript entry is rendered. Entries hold raw text;
// each View renders them with its own styles and width, which is what lets
// two terminals on one session show the same transcript in different
// themes and at different widths.
type entryKind int

const (
	entryPlain     entryKind = iota // Text as-is (blank lines, pre-formatted blocks)
	entryUser                       // Label = "you> " / "<label>> " / "plan> " in the user style, Text plain
	entryAssistant                  // Text is Markdown source
	entryTool                       // Label = tool name ("● name" in the tool style), Text = args (dim, truncated to the width)
	entryToolOK                     // "  ✓ " then Text dim
	entryToolErr                    // "  ✗ " then Text dim
	entryDim                        // Text dim
	entryOK                         // Text in the OK style
	entryWarn                       // Text in the warn style
	entryErr                        // Text in the error style
	entryNote                       // "note " (warn) then Text plain
	entryError                      // Label (error style, e.g. "error ") then Text plain
	entryQueued                     // Label dim (e.g. "queued> ") then Text plain
	entryVerdict                    // Label (warn, e.g. "file_write") then Text "approved" (OK) or "denied" (Err)
)

type entry struct {
	Kind        entryKind
	Label, Text string
}

// renderEntry renders one entry for a terminal of the given width. Only
// entryTool truncates by width (tool arguments); wrapping of everything
// else is the View's job.
func renderEntry(e entry, st styles, width int, compact, richText bool) string {
	switch e.Kind {
	case entryUser:
		return st.User.Render(e.Label) + e.Text
	case entryAssistant:
		return ui.RenderMarkdown(e.Text, richText)
	case entryTool:
		limit := 140
		if compact {
			limit = width - 12
			if limit < 10 {
				limit = 10
			}
		}
		args := e.Text
		if len(args) > limit {
			args = args[:limit] + "…"
		}
		return st.Tool.Render("● "+e.Label) + " " + st.Dim.Render(args)
	case entryToolOK:
		return st.OK.Render("  ✓ ") + st.Dim.Render(e.Text)
	case entryToolErr:
		return st.Err.Render("  ✗ ") + st.Dim.Render(e.Text)
	case entryDim:
		return st.Dim.Render(e.Text)
	case entryOK:
		return st.OK.Render(e.Text)
	case entryWarn:
		return st.Warn.Render(e.Text)
	case entryErr:
		return st.Err.Render(e.Text)
	case entryNote:
		return st.Warn.Render("note ") + e.Text
	case entryError:
		return st.Err.Render(e.Label) + e.Text
	case entryQueued:
		return st.Dim.Render(e.Label) + e.Text
	case entryVerdict:
		v := st.Err.Render(e.Text)
		if e.Text == "approved" {
			v = st.OK.Render(e.Text)
		}
		return st.Warn.Render(e.Label+" ") + v
	}
	return strings.TrimRight(e.Text, "\n")
}
```

- [ ] **Step 4: Move the model onto entries**

In `internal/tui/tui.go`:

- Replace the field `transcript strings.Builder` with `entries []entry` and `rendered strings.Builder // entries rendered with this model's styles at its width`.
- Delete `appendLine`; add:

```go
// appendEntry records one transcript entry and renders it for this
// terminal.
func (m *Model) appendEntry(e entry) {
	m.entries = append(m.entries, e)
	m.rendered.WriteString(renderEntry(e, m.st, m.width, m.compact(), m.richText))
	m.rendered.WriteString("\n")
	m.refreshTranscript()
}

// rebuild re-renders every entry — after a theme change or a resize, since
// tool-argument truncation and Markdown colouring depend on both.
func (m *Model) rebuild() {
	m.rendered.Reset()
	for _, e := range m.entries {
		m.rendered.WriteString(renderEntry(e, m.st, m.width, m.compact(), m.richText))
		m.rendered.WriteString("\n")
	}
	m.refreshTranscript()
}
```

- `flushStreaming` becomes:

```go
func (m *Model) flushStreaming() {
	if m.streaming.Len() == 0 {
		return
	}
	text := strings.TrimRight(m.streaming.String(), "\n")
	m.lastReply = text
	m.streaming.Reset()
	m.appendEntry(entry{Kind: entryAssistant, Text: text})
}
```

- `refreshTranscript` uses `m.rendered.String() + m.streaming.String()` in place of `m.transcript.String() + m.streaming.String()`.
- In the `tea.WindowSizeMsg` case call `m.rebuild()` instead of `m.refreshTranscript()` (after `m.layout()`).
- In `applyTheme` (palette.go) call `m.rebuild()` instead of `m.refreshTranscript()`.
- Convert every `appendLine` call. The mapping (apply the same rule everywhere, including `served.go`, `queue.go`, `palette.go`, `picker.go`, `selection.go`, `review.go`):

| Old | New |
|---|---|
| `m.appendLine(m.st.Dim.Render(X))` | `m.appendEntry(entry{Kind: entryDim, Text: X})` |
| `m.appendLine(m.st.OK.Render(X))` | `m.appendEntry(entry{Kind: entryOK, Text: X})` |
| `m.appendLine(m.st.Warn.Render(X))` | `m.appendEntry(entry{Kind: entryWarn, Text: X})` |
| `m.appendLine(m.st.Err.Render(X))` | `m.appendEntry(entry{Kind: entryErr, Text: X})` |
| `m.appendLine(m.st.User.Render(P) + T)` | `m.appendEntry(entry{Kind: entryUser, Label: P, Text: T})` |
| `m.appendLine(m.st.Tool.Render("● "+msg.name) + " " + m.st.Dim.Render(args))` (toolStartMsg; drop the local truncation) | `m.appendEntry(entry{Kind: entryTool, Label: msg.name, Text: msg.args})` |
| `m.appendLine(m.st.OK.Render("  ✓ ") + m.st.Dim.Render(first))` | `m.appendEntry(entry{Kind: entryToolOK, Text: first})` |
| `m.appendLine(m.st.Err.Render("  ✗ ") + m.st.Dim.Render(first))` | `m.appendEntry(entry{Kind: entryToolErr, Text: first})` |
| `m.appendLine(m.st.Warn.Render("note ") + string(msg))` | `m.appendEntry(entry{Kind: entryNote, Text: string(msg)})` |
| `m.appendLine(m.st.Err.Render("error ") + msg.err.Error())` and `"init failed: "`, `"plan failed: "` | `m.appendEntry(entry{Kind: entryError, Label: "error ", Text: msg.err.Error()})` etc. |
| `m.appendLine(m.st.Dim.Render("queued> ") + m.userPrefix(from) + text)` | `m.appendEntry(entry{Kind: entryQueued, Label: "queued> ", Text: m.userPrefix(from) + text})` |
| `m.appendLine(m.st.Dim.Render("queued (delivered at the next step)> ") + text)` | `m.appendEntry(entry{Kind: entryQueued, Label: "queued (delivered at the next step)> ", Text: text})` |
| `resolveApproval`: `m.appendLine(m.st.Warn.Render(action+" ") + verdict)` | `m.appendEntry(entry{Kind: entryVerdict, Label: m.approval.action, Text: "approved"})` (or `"denied"`) |
| `m.appendLine("")` | `m.appendEntry(entry{Kind: entryPlain})` |
| the `/help` block, `/handoff` lines, verify `Human()` lines, `/map` | one `entryDim` per line as today |

`grep -n appendLine internal/tui/*.go` must print nothing when done.

- [ ] **Step 5: Run the package tests**

Run: `go test ./internal/tui/`
Expected: PASS. Tests that assert on `m.transcript.String()` must be changed to `m.rendered.String()` (plain-text assertions keep working since styles wrap the same text). `grep -n 'transcript.String()' internal/tui/*_test.go` → replace with `rendered.String()`.

- [ ] **Step 6: Commit**

```bash
git add -A && git commit -m "tui: transcript is a list of entries rendered per model"
```

---

### Task 3: Host additions — per-client output, per-client size, drop, repaint after eviction

**Files:**
- Modify: `internal/live/host.go`
- Create: `internal/live/label.go`
- Test: `internal/live/host_test.go` (append), `internal/live/label_test.go` (new)

**Interfaces:**
- Produces: `func (h *Host) ClientOutput(id int) io.Writer`; `func (h *Host) OnClientSize(f func(id, cols, rows int))`; `func (h *Host) Drop(id int, reason string)`; `func (h *Host) Logf(format string, args ...any)` (exported wrapper over `logf`, for the runner's panic log); `func LabelKey(label string) string`. Nothing is removed yet (`OnSize`, `Size`, `SetOverlay`, `ClearOverlays`, `AnyASCII` stay until Task 8).
- Behaviour: a client's `FResize` still recomputes the shared size (for the old path) **and** calls `onClientSize(id, cols, rows)`. When `enqueue` evicts an `FOutput` frame, it sets `c.lostOutput = true`; `writer`, after draining a batch, if `lostOutput` is set, clears it and calls `onClientSize(c.id, c.cols, c.rows)` so the served program repaints that client in full (Bubble Tea repaints on a `WindowSizeMsg`).

- [ ] **Step 1: Write the failing tests**

Append to `internal/live/host_test.go`. The file already has `startHost(t) (*Host, string)` (token `"tok"`), `dial(t, sock, token, label, cols, rows) *fakeClient` whose fake reads frames into channels `out` (FOutput payloads), `size`, `cl` (rosters) and `bye` (reasons), `idByLabel(t, h, label)` and `within(t, d, f)`. Add two small helpers to the fake: `resize(t, cols, rows)` = `WriteJSON(fc.conn, FResize, Size{Cols: cols, Rows: rows})`, and a `paused chan struct{}` the reader goroutine blocks on when `stopReading()` has been called (`resumeReading()` closes it) — the reader loop does `if p := fc.pauseGate(); p != nil { <-p }` before each `ReadFrame`.

```go
func TestClientOutputReachesOneClientOnly(t *testing.T) {
	h, sock := startHost(t)
	a := dial(t, sock, "tok", "a", 80, 24)
	b := dial(t, sock, "tok", "b", 80, 24)
	within(t, 2*time.Second, func() bool { return len(h.Clients()) == 2 })
	<-a.cl // roster frames from the attaches
	io.WriteString(h.ClientOutput(idByLabel(t, h, "a")), "only-a")
	select {
	case p := <-a.out:
		if !strings.Contains(string(p), "only-a") {
			t.Fatalf("a got %q", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a never received its private output")
	}
	select {
	case p := <-b.out:
		t.Fatalf("b received a's private output: %q", p)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestResizeNotifiesPerClient(t *testing.T) {
	h, sock := startHost(t)
	sizes := make(chan string, 16)
	h.OnClientSize(func(id, cols, rows int) { sizes <- fmt.Sprintf("%d:%dx%d", id, cols, rows) })
	a := dial(t, sock, "tok", "a", 80, 24)
	within(t, 2*time.Second, func() bool { return len(h.Clients()) == 1 })
	id := idByLabel(t, h, "a")
	a.resize(t, 40, 15)
	deadline := time.After(2 * time.Second)
	for {
		select {
		case got := <-sizes:
			if got == fmt.Sprintf("%d:40x15", id) {
				return
			}
		case <-deadline:
			t.Fatal("OnClientSize never reported a's new size")
		}
	}
}

func TestDropSaysGoodbyeWithTheReason(t *testing.T) {
	h, sock := startHost(t)
	a := dial(t, sock, "tok", "a", 80, 24)
	within(t, 2*time.Second, func() bool { return len(h.Clients()) == 1 })
	h.Drop(idByLabel(t, h, "a"), "view error")
	select {
	case reason := <-a.bye:
		if reason != "view error" {
			t.Fatalf("bye reason %q", reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no bye")
	}
	within(t, 2*time.Second, func() bool { return len(h.Clients()) == 0 })
}

func TestEvictedOutputTriggersARepaintRequest(t *testing.T) {
	h, sock := startHost(t)
	repaints := make(chan int, 64)
	h.OnClientSize(func(id, _, _ int) { repaints <- id })
	a := dial(t, sock, "tok", "a", 80, 24)
	within(t, 2*time.Second, func() bool { return len(h.Clients()) == 1 })
	for len(repaints) > 0 { // the attach itself reports the size once
		<-repaints
	}
	a.stopReading()
	w := h.ClientOutput(idByLabel(t, h, "a"))
	big := strings.Repeat("x", 256*1024)
	for i := 0; i < maxQueuedOutput*4; i++ {
		io.WriteString(w, big)
	}
	a.resumeReading()
	select {
	case <-repaints:
	case <-time.After(5 * time.Second):
		t.Fatal("no repaint request after output frames were evicted")
	}
}
```

`internal/live/label_test.go`:

```go
package live

import "testing"

func TestLabelKeyStripsThePID(t *testing.T) {
	for in, want := range map[string]string{
		"ssh from 10.0.0.5 (pid 2)": "ssh from 10.0.0.5",
		"vscode (pid 1)":            "vscode",
		"local (pid 12345)":         "local",
		"phone":                     "phone",
		"odd (pid x)":               "odd (pid x)",
		"  spaced (pid 3)  ":        "spaced",
	} {
		if got := LabelKey(in); got != want {
			t.Errorf("LabelKey(%q) = %q, want %q", in, got, want)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/live/ -run 'TestClientOutput|TestResizeNotifies|TestDropSays|TestEvicted|TestLabelKey' 2>&1 | head -5`
Expected: build failure (`h.ClientOutput undefined`, `undefined: LabelKey`).

- [ ] **Step 3: Implement**

`internal/live/label.go`:

```go
package live

import (
	"regexp"
	"strings"
)

var pidSuffix = regexp.MustCompile(`\s*\(pid \d+\)\s*$`)

// LabelKey is a client label without its " (pid N)" suffix: the part that
// identifies the device rather than the process, and so the key a
// per-device setting (config client_themes) is stored under.
func LabelKey(label string) string {
	return strings.TrimSpace(pidSuffix.ReplaceAllString(label, ""))
}
```

`internal/live/host.go`:

- `client` gains `lostOutput bool // an FOutput frame was evicted; guarded by qmu`.
- `Host` gains `onClientSize func(id, cols, rows int)`.
- Add:

```go
// OnClientSize registers the callback for one client's own size: on attach,
// on every resize frame it sends, and after output to it was evicted (a
// repaint request; see enqueue).
func (h *Host) OnClientSize(f func(id, cols, rows int)) {
	h.mu.Lock()
	h.onClientSize = f
	h.mu.Unlock()
}

// ClientOutput is a writer that reaches one client only. Bytes are queued
// on that client's own queue exactly like fan-out frames, so a stalled
// terminal never blocks the writer.
func (h *Host) ClientOutput(id int) io.Writer { return clientWriter{h, id} }

type clientWriter struct {
	h  *Host
	id int
}

func (w clientWriter) Write(p []byte) (int, error) {
	cp := append([]byte(nil), p...)
	w.h.mu.Lock()
	c := w.h.byIDLocked(w.id)
	w.h.mu.Unlock()
	if c == nil {
		return 0, io.ErrClosedPipe
	}
	c.enqueue(FOutput, cp)
	return len(p), nil
}

// Logf writes one line to the host log (the served runner uses it for a
// view that panicked).
func (h *Host) Logf(format string, args ...any) { h.logf(format, args...) }

// Drop disconnects one client with reason as its bye.
func (h *Host) Drop(id int, reason string) {
	if c := h.byID(id); c != nil {
		h.detach(c, reason)
	}
}
```

- In `enqueue`, inside the eviction branch (`if n >= maxQueuedOutput {...}`), after removing the oldest frame add `if t == FOutput { c.lostOutput = true }`.
- In `writer`, after the inner `for _, f := range c.drain() {...}` loop completes for a batch:

```go
		c.qmu.Lock()
		lost := c.lostOutput
		c.lostOutput = false
		c.qmu.Unlock()
		if lost {
			h.mu.Lock()
			f, cols, rows := h.onClientSize, c.cols, c.rows
			h.mu.Unlock()
			if f != nil {
				f(c.id, cols, rows)
			}
		}
```

- In `handle`'s `FResize` case, after `h.recompute()`: read `f := h.onClientSize` under `h.mu` and call `f(c.id, s.Cols, s.Rows)` outside it.
- In `recomputeAttach`, when `attached != nil`, also call `onClientSize(attached.id, attached.cols, attached.rows)` (read under `h.mu` with the others) so a new client's program can be sized before any resize.

- [ ] **Step 4: Run the live package tests**

Run: `go test ./internal/live/ -count=3`
Expected: PASS, three times.

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "live: per-client output writer, per-client size callback, Drop, LabelKey, repaint after eviction"
```

---

### Task 4: Split `Model` into `Session` and `View` (single program still)

**Files:**
- Create: `internal/tui/session.go`, `internal/tui/mailbox.go`
- Rename: `internal/tui/tui.go` → `internal/tui/view.go` (`git mv`), `Model` → `View`, receiver `m` stays `m` (fewer diffs; Task 6 does not rename it either)
- Modify: `internal/tui/served.go`, `review.go`, `picker.go`, `palette.go`, `queue.go`, `selection.go`, `compact.go`, `inputs.go`, `wheel.go`
- Modify: `cmd/root.go:511-516`, `cmd/live.go:102-134`
- Test: `internal/tui/session_test.go` (new); existing tests updated for the new constructors

**Interfaces:**
- Produces:

```go
type Session struct {
	mu sync.Mutex // guards every field below except viewsMu/views and the test seams

	cfg  *config.Config
	ag   *agent.Agent
	prov provider.Provider
	rootCtx  context.Context
	cancelFn context.CancelFunc

	entries   []entry
	streaming strings.Builder
	lastReply string
	lastTool  string

	running    bool
	statusNote string
	usage      usageMsg
	toast      string
	toastUntil time.Time
	now        func() time.Time

	review   *review.Coordinator
	custom   map[string]commands.Command
	histFile *inputHistory

	clients       []live.ClientInfo
	served        bool
	host          *live.Host
	switchPending bool
	idleSince     time.Time
	liveCodes     func() map[string]bool
	liveRecords   func() []live.Record
	loadSession   func(id string) (*store.Session, error)
	switchClient  func(id int, code string)
	detachClient  func(id int)
	dropKeyClient func(id int)

	ideAnnounced, initChecked, initHinted bool

	ask      *ask // the open shared ask (Task 5); nil when none
	askGen   int
	quitting bool
	onQuit   func() // set by the served runner (Task 6)

	viewsMu sync.Mutex
	views   map[int]*View // each View carries its mailbox (v.mb)

	startTurnHook func(string) // test seam
	sendHook      func(tea.Msg) // test seam consulted by broadcast
}

func NewSession(cfg *config.Config, ag *agent.Agent, prov provider.Provider) *Session
func (s *Session) NewView(id int, label string) *View   // styles from cfg.Theme for now (Task 7 adds the per-device lookup)
func (s *Session) attachView(v *View)                  // registers v.mb under v.id
func (s *Session) detachView(id int)
func (s *Session) viewByID(id int) *View
func (s *Session) broadcast(msg tea.Msg)               // non-blocking to every view
func (s *Session) send(msg tea.Msg)                     // = broadcast (kept as the name the goroutines use)
func (s *Session) appendEntry(e entry)                  // locks, appends, broadcasts entryMsg{e}
func (s *Session) Entries() []entry                     // snapshot under the lock
func (s *Session) SetReview(c *review.Coordinator)
func (s *Session) ReviewTerminal() review.Terminal
func (s *Session) userPrefix(from int) string
func (s *Session) clientLabel(from int) string

type View struct {
	*Session
	id    int
	label string
	st    styles
	richText bool
	program *tea.Program
	vp viewport.Model; modalVP viewport.Model; spin spinner.Model
	inputs map[int]*textarea.Model // still per client id in this task; Task 6 makes it one textarea
	mode mode; prevMode mode; picker *picker; pending *planReadyMsg; approval *approvalMsg
	approvalGen, cancelledGen int; askGen atomic.Int64
	width, height int; ready bool
	rendered strings.Builder; wrapped string; sel *selection; wheelFrame int
	quitHint map[int]bool
	queueCursor, queueOwner, paletteOwner, menuOwner, pickerOwner int
	clipboardWrite func(string) error; clipboardRead func() (string, error); termWrite func(string)
	ascii bool
	sendHook func(tea.Msg) // test seam
}
```

- Messages: `entryMsg{e entry}` (new). `deltaMsg`, `toolStartMsg`, `toolEndMsg`, `noticeMsg`, `transientMsg`, `statusMsg`, `usageMsg`, `thinkingMsg`, `turnDoneMsg`, `initDoneMsg`, `planReadyMsg`, `clientsMsg`, `idleTickMsg`, `hostQuitMsg`, `pickerItemsMsg`, `approvalMsg`, `approvalCancelMsg` keep their meaning in this task (they are still delivered to the one view); Tasks 5 and 6 rework the shared ones.
- `mailbox`:

```go
type mailbox struct {
	ch   chan tea.Msg
	done chan struct{}
}
func newMailbox() *mailbox                       // ch buffered 256
func (mb *mailbox) send(msg tea.Msg)              // select { case ch <- msg: default: } — never blocks
func (mb *mailbox) run(deliver func(tea.Msg))     // for msg := range ch { deliver(msg) }; close(done) on exit
func (mb *mailbox) close()                        // close(ch)
func (mb *mailbox) drainInto(v *View)             // test helper: for { select { case m := <-ch: v.Update(m); default: return } }
```

- Locking rule established in this task: `View.Update` and `View.View` lock `m.Session.mu` for their whole body (`m.mu.Lock(); defer m.mu.Unlock()`), and **`Session.appendEntry`/`broadcast` are called from Update while the lock is already held**, so `Session` methods come in two flavours: exported/agent-side methods lock (`appendEntry`), and `...Locked` variants assume the caller holds `mu` (`appendEntryLocked`). Update calls the `Locked` variants. `broadcast` takes only `viewsMu`, never `mu`, so it is safe from both.

- [ ] **Step 1: Write the failing test**

`internal/tui/session_test.go`:

```go
package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/live"
)

// Two views of one session see the same entries, each rendered at its own
// size and with its own styles; a resize of one leaves the other's buffer
// untouched.
func TestTwoViewsShareEntriesButRenderSeparately(t *testing.T) {
	s := newTestSession(t)
	a := s.NewView(1, "desk (pid 1)")
	b := s.NewView(2, "tablet (pid 2)")
	a.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	b.Update(tea.WindowSizeMsg{Width: 40, Height: 15})
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "desk (pid 1)", UTF8: true}, {ID: 2, Label: "tablet (pid 2)", UTF8: true}})
	s.appendEntry(entry{Kind: entryTool, Label: "read_file", Text: strings.Repeat("a", 200)})
	flush(a, b)
	if len(a.entries) != len(b.entries) || len(a.entries) < 1 {
		t.Fatalf("entries diverged: %d vs %d", len(a.entries), len(b.entries))
	}
	if !strings.Contains(a.rendered.String(), "read_file") || !strings.Contains(b.rendered.String(), "read_file") {
		t.Fatal("entry not rendered on both views")
	}
	if len(b.rendered.String()) >= len(a.rendered.String()) {
		t.Fatal("the narrow view did not truncate tool args")
	}
	if !b.compact() || a.compact() {
		t.Fatalf("layouts: a compact=%v b compact=%v", a.compact(), b.compact())
	}
	before := a.rendered.String()
	b.Update(tea.WindowSizeMsg{Width: 60, Height: 15})
	if a.rendered.String() != before {
		t.Fatal("resizing view b changed view a's rendered buffer")
	}
	// Every view got the roster; the bottom line shows both labels.
	if !strings.Contains(a.View(), "⧉ 2") {
		t.Fatalf("view a bottom line: %q", a.View())
	}
}

// Broadcast must never block: a view whose mailbox is full drops messages
// instead of stalling the core.
func TestBroadcastNeverBlocksOnAFullMailbox(t *testing.T) {
	s := newTestSession(t)
	v := s.NewView(1, "x")
	_ = v
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10000; i++ {
			s.broadcast(entryMsg{entry{Kind: entryDim, Text: "x"}})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("broadcast blocked on a full mailbox")
	}
}
```

Add to the test helpers (`scroll_test.go` or a new `helpers_test.go`):

```go
func newTestSession(t *testing.T, prep ...func(*agent.Agent)) *Session {
	t.Helper()
	cfg := config.Default()
	cfg.RepoMap = false
	reg, err := tools.NewRegistry(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ag := agent.New(cfg, nullProvider{}, "m", reg, "")
	for _, f := range prep {
		f(ag)
	}
	s := NewSession(cfg, ag, nullProvider{})
	s.rootCtx = context.Background()
	return s
}

// newTestModel keeps its name: one view of a fresh session at 80x24, id 0.
func newTestModel(t *testing.T, prep ...func(*agent.Agent)) *View {
	t.Helper()
	s := newTestSession(t, prep...)
	v := s.NewView(0, "local")
	v.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return v
}

// flush delivers every queued broadcast to the given views, in order.
func flush(views ...*View) {
	for _, v := range views {
		if mb := v.mailboxForTest(); mb != nil {
			mb.drainInto(v)
		}
	}
}
```

`NewView` registers a mailbox for the view (`s.attachView(id, mb)`) and `View.mailboxForTest()` returns it. In production the runner starts the mailbox's `run` goroutine; in tests nobody does, so `flush` drains it by hand.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/tui/ -run 'TestTwoViewsShare|TestBroadcastNever' 2>&1 | head -5`
Expected: build failure `undefined: newTestSession` / `NewSession`.

- [ ] **Step 3: Split the struct**

1. `git mv internal/tui/tui.go internal/tui/view.go`; `sed -i 's/\bModel\b/View/g' internal/tui/*.go` (then fix the two places where `View` was already a method name: `func (m *View) View() string` is fine; `tea.Model` return types must stay `tea.Model` — re-run `sed -i 's/tea\.View\b/tea.Model/g' internal/tui/*.go`).
2. Create `session.go` with the `Session` struct above. Move the listed fields out of `View` into `Session`; `View` embeds `*Session` as its first field. Field access through promotion means most method bodies compile unchanged.
3. `NewSession` takes what `New` did: build the agent event wiring on the **session** (`ag.Tools.Approve`, `ag.Events`, `ag.Tools.OnStatus`) — their bodies call `s.send(...)` (= `broadcast`). `approveFromAgent` moves to `Session` unchanged in behaviour (it broadcasts an `approvalMsg` with a reply channel; in this task the one view answers it; Task 5 replaces it).
4. `NewView(id, label)`:

```go
func (s *Session) NewView(id int, label string) *View {
	st := stylesOr(s.cfg.Theme)
	sp := spinner.New()
	sp.Spinner = spinner.MiniDot
	sp.Style = st.Accent
	v := &View{Session: s, id: id, label: label, st: st, richText: s.cfg.Theme != "mono", spin: sp,
		clipboardWrite: writeClipboard, clipboardRead: readClipboard, termWrite: writeTerminal}
	v.inputFor(0)
	v.mb = newMailbox()
	s.attachView(v)
	s.mu.Lock()
	v.usage = s.usage
	s.mu.Unlock()
	return v
}
```

(`usage` stays on the session; the view reads `m.usage` through promotion — remove any per-view copy if it causes confusion.)

5. `View.Update`:

```go
func (m *View) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m.mu.Lock()
	defer m.mu.Unlock()
	wasVisible := m.overlayVisible()
	model, cmd := m.update(msg)
	nowVisible := m.overlayVisible()
	switch t := msg.(type) {
	case tea.KeyMsg:
		m.publishAfterKey(wasVisible, nowVisible, 0)
	case live.ClientKeyMsg:
		m.publishAfterKey(wasVisible, nowVisible, t.Client)
	default:
		m.publishVisibilityChange(wasVisible, nowVisible)
	}
	return model, cmd
}
```

and `View.View` starts with `m.mu.Lock(); defer m.mu.Unlock()`.

6. `entryMsg`: add `case entryMsg: m.renderEntryLocal(msg.e)` to `update`, where `renderEntryLocal` appends to `m.rendered` and refreshes (no append to `m.entries` — the session already did). `Session.appendEntry(e)` = lock, `appendEntryLocked(e)`, unlock; `appendEntryLocked` = `s.entries = append(...)`, `s.broadcast(entryMsg{e})`. **Inside `update` (lock held) every former `m.appendEntry(e)` becomes `m.appendEntryLocked(e)`** — the view's own rendering then happens when its mailbox delivers the `entryMsg`. For the single-view path in this task that means Update must drain its own mailbox before returning, or tests see nothing rendered: add at the end of `Update`, still under the lock, `m.mb.drainInto(m)` guarded against recursion (`drainInto` calls `m.update`, not `m.Update`). Keep this self-drain in Task 6 too: a view renders its own broadcasts synchronously, and the mailbox goroutine only matters for broadcasts from *other* goroutines.
7. `send`/`broadcast` on `Session`; delete `View.send`. `sendHook` stays on `View` only for `review_integration_test.go` until Task 5 removes it — simplest: `Session.broadcast` also calls `s.sendHook` if set; move the seam to `Session`.
8. `View.mb *mailbox` and `func (m *View) mailboxForTest() *mailbox { return m.mb }`.
9. `SetClients(infos)` on `Session`: what `updateClients` did (roster diff, `attached:`/`detached:` entries, `switchPending`, `dropInput`/`dropKeyClient`, popup-owner cleanup) — for this task keep it as a method on `View` reached from `clientsMsg` (single view), but move the roster field to the session. Task 6 finishes the move.
10. `cmd/root.go`:

```go
	s := tui.NewSession(cfg, ag, p)
	coord := review.New(mode, editor, s.ReviewTerminal(), nil)
	s.SetReview(coord)
	ag.Tools.ReviewWrite = coord.Decide
	ag.Tools.ReviewInvolvesEditor = func() bool { return coord.Resolve() != review.ModeTUI && editor != nil }
	return s.RunLocal(ctx)
```

`RunLocal` (in `served.go`, for now) = the old `Model.Run` on `s.NewView(0, "local")`. `cmd/live.go`: `s := tui.NewSession(cfg, ag, p)` … `s.ReviewTerminal()`, `s.SetReview(coord)`, `err = s.RunServed(context.Background(), h)` where `RunServed` in this task still builds **one** view (`s.NewView(0, "shared")`) and runs the old single-program body.

- [ ] **Step 4: Build and run everything**

Run: `go build ./... && go test ./internal/tui/ ./cmd/... && make -f build.mk verify 2>&1 | tail -3`
Expected: PASS. Existing tests compile against `*View` via `newTestModel`; `twoClients(t)` still returns one view with `served=true` and a two-client roster (Task 6 rewrites it).

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "tui: split Model into Session (shared core) and View (per-terminal), one program still"
```

---

### Task 5: Shared asks — approvals, plan and shared pickers answered once

**Files:**
- Create: `internal/tui/ask.go`
- Modify: `internal/tui/view.go` (delete `approvalMsg`, `approvalCancelMsg`, `modeApproval`, `modePlan`; add `modeAsk`; `handleApprovalKey`/`handlePlanKey`/`resolveApproval` → `handleAskKey`; `viewApproval`/plan view → `viewAsk`), `review.go` (delete `reviewTerminal`; keep `reviewCommand`), `picker.go` (model/provider/session pickers become shared asks), `palette.go` (theme picker stays local `modePicker`; menu entries call the new openers), `session.go` (`approveFromAgent` → `Ask`)
- Test: `internal/tui/ask_test.go` (new), rewrite `review_test.go` and `review_integration_test.go`

**Interfaces:**
- Produces:

```go
type askKind int
const (askApproval askKind = iota; askPlan; askPicker)

type ask struct {
	Gen    int
	Kind   askKind
	Action string     // askApproval: "shell" | "file_write"
	Title  string     // askPicker title / plan title
	Detail string     // approval diff preview / plan text
	Items  []pickItem // askPicker
	onPick func(v *View, id string, from int) tea.Cmd // askPicker, run by the answering view under s.mu
	reply  chan askAnswer                              // askApproval, askPlan
	req    string                                       // askPlan: the request text (for ExecutePlan)
}

type askAnswer struct {
	OK   bool
	Note string // "shell auto-approve enabled for this session" etc.
	From int
}

type askMsg struct{ a *ask }              // broadcast when an ask opens
type askResolvedMsg struct{ gen int; by string } // broadcast when it is answered or withdrawn

func (s *Session) Ask(ctx context.Context, a *ask) askAnswer   // blocks; returns {OK:false} on ctx.Done
func (s *Session) Answer(gen int, ans askAnswer, from int) (tea.Cmd, bool) // called under s.mu by a view; false = stale
func (s *Session) CancelAsk(gen int, by string)                 // withdraw (editor answered); idempotent
func (s *Session) current() *ask                                // under s.mu
```

- `View`: `mode == modeAsk` while `s.current() != nil && m.askShown == s.current().Gen`; `m.askShown int`; `m.modalVP` holds the detail; `m.picker` holds the view-local cursor/filter over `ask.Items` for `askPicker`.
- `approveFromAgent(action, detail) bool` → `ans := s.Ask(ctx, &ask{Kind: askApproval, Action: action, Detail: detail}); return ans.OK` (with the `AutoApproveShell`/`ApproveFileWrites` short-cuts kept).
- `reviewTerminal.Ask(ctx, preview)` → `s.Ask(ctx, &ask{Kind: askApproval, Action: "file_write", Detail: preview}).OK`; `Withdraw(note)` → `s.CancelAsk(s.lastGen(), note)` where the note is `answered in VS Code` as today (the coordinator passes it).
- Plan: `/plan` goroutine → `plan, err := ag.Plan(...)`; on success `ans := s.Ask(ctx, &ask{Kind: askPlan, Title: "Implementation plan — approve to execute", Detail: plan, req: req})`; if `ans.OK` → `ag.ExecutePlan` then `finishTurn`, else entry `plan discarded` and `finishTurn(nil, nil)` without the verify lines. `planReadyMsg` and `modePlan` are deleted.
- Pickers: `openModelPicker` etc. become `func (m *View) askModelPicker() tea.Cmd` that returns a Cmd loading the items on a goroutine and then calling `s.Ask(ctx, &ask{Kind: askPicker, Title: "Select model", Items: items, onPick: ...})`; `onPick` for the model picker: `func(v *View, id string, _ int) tea.Cmd { v.ag.SetModel(id); v.appendEntryLocked(entry{Kind: entryOK, Text: "model set to " + id}); return nil }`; for the session picker: `func(v *View, id string, from int) tea.Cmd { _, cmd := v.resumeFrom(id, from); return cmd }`. The load error path appends an `entryErr` and asks nothing.
- Answer semantics: `Answer` (under `s.mu`, called from the answering view's Update): if `gen != s.ask.Gen` return `(nil,false)`. Otherwise for `askPicker` run `onPick` (it may return a Cmd — the switch), for the others send on `reply` (buffered 1). Then `s.ask = nil`, broadcast `askResolvedMsg{gen, by: s.clientLabel(from)}`. The answering view closes its own modal directly; the others close on `askResolvedMsg` and append **to their own rendered buffer only** the dimmed line `answered by <label>` (not an entry: `m.renderLocalNote("answered by " + msg.by)`).
- Transcript verdict lines (`file_write approved`) are session entries appended by `Answer` for approvals, so every terminal sees them.

- [ ] **Step 1: Write the failing tests**

`internal/tui/ask_test.go`:

```go
package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/live"
)

func twoViews(t *testing.T) (*Session, *View, *View) {
	t.Helper()
	tempHome(t)
	s := newTestSession(t)
	s.served = true
	a := s.NewView(1, "desk (pid 1)")
	b := s.NewView(2, "tablet (pid 2)")
	a.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	b.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "desk (pid 1)", UTF8: true}, {ID: 2, Label: "tablet (pid 2)", UTF8: true}})
	flush(a, b)
	return s, a, b
}

func TestSharedApprovalFirstAnswerWinsAndTheOtherViewIsToldWho(t *testing.T) {
	s, a, b := twoViews(t)
	decided := make(chan bool, 1)
	go func() { decided <- s.approveFromAgent("file_write", "--- a\n+++ b\n") }()
	waitFor(t, func() bool { flush(a, b); return a.mode == modeAsk && b.mode == modeAsk })
	if !strings.Contains(a.View(), "approval required") || !strings.Contains(b.View(), "approval required") {
		t.Fatal("both views must show the modal")
	}
	b.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	select {
	case ok := <-decided:
		if !ok {
			t.Fatal("y must approve")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the agent goroutine was never answered")
	}
	flush(a, b)
	if a.mode == modeAsk || b.mode == modeAsk {
		t.Fatalf("modals still open: a=%v b=%v", a.mode, b.mode)
	}
	if !strings.Contains(a.wrapped, "answered by tablet (pid 2)") {
		t.Fatalf("view a was not told who answered:\n%s", a.wrapped)
	}
	if strings.Contains(b.wrapped, "answered by") {
		t.Fatal("the answering view must not be told it answered")
	}
	if !strings.Contains(a.wrapped, "file_write") || !strings.Contains(b.wrapped, "approved") {
		t.Fatal("the verdict line is a shared entry and must be on both")
	}
}

func TestStaleAnswerIsIgnored(t *testing.T) {
	s, a, b := twoViews(t)
	go s.approveFromAgent("shell", "ls")
	waitFor(t, func() bool { flush(a, b); return a.mode == modeAsk && b.mode == modeAsk })
	gen := s.current().Gen
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if _, ok := s.Answer(gen, askAnswer{OK: true}, 2); ok {
		t.Fatal("an answer for a resolved generation must be ignored")
	}
}

func TestEditorWithdrawalClosesEveryView(t *testing.T) {
	s, a, b := twoViews(t)
	term := s.ReviewTerminal()
	res := make(chan bool, 1)
	go func() { res <- term.Ask(context.Background(), "diff") }()
	waitFor(t, func() bool { flush(a, b); return a.mode == modeAsk })
	term.Withdraw("answered in VS Code")
	flush(a, b)
	if a.mode == modeAsk || b.mode == modeAsk {
		t.Fatal("withdrawal left a modal open")
	}
	if !strings.Contains(a.wrapped, "answered in VS Code") || !strings.Contains(b.wrapped, "answered in VS Code") {
		t.Fatal("withdrawal note missing")
	}
	select {
	case ok := <-res:
		if ok {
			t.Fatal("a withdrawn ask must return false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ask did not return after Withdraw")
	}
}

func TestSharedPickerCursorIsLocalButTheAnswerIsShared(t *testing.T) {
	s, a, b := twoViews(t)
	picked := make(chan string, 1)
	go s.Ask(context.Background(), &ask{Kind: askPicker, Title: "Select model", Items: []pickItem{{id: "one", label: "one"}, {id: "two", label: "two"}},
		onPick: func(v *View, id string, from int) tea.Cmd { picked <- id; return nil }})
	waitFor(t, func() bool { flush(a, b); return a.mode == modeAsk && b.mode == modeAsk })
	b.Update(tea.KeyMsg{Type: tea.KeyDown})
	if a.picker.cursor != 0 || b.picker.cursor != 1 {
		t.Fatalf("cursors: a=%d b=%d", a.picker.cursor, b.picker.cursor)
	}
	b.Update(tea.KeyMsg{Type: tea.KeyEnter})
	select {
	case id := <-picked:
		if id != "two" {
			t.Fatalf("picked %q", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("onPick never ran")
	}
	flush(a, b)
	if a.mode == modeAsk {
		t.Fatal("view a still shows the picker")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in 2s")
}
```

Rewrite `review_integration_test.go` so it uses `twoViews`, `s.ReviewTerminal()` and `coord.Decide` on a goroutine, `flush(a, b)`, the `y` key on `b`, and asserts `answered by tablet (pid 2)` is absent (the editor loses, so its cancel arrives: assert `editor.cancels` receives the path as today). Delete `review_test.go`'s `TestApprovalCancel*`, `TestStaleApprovalCancel*`, `TestReviewTerminalNumbersEachAsk`; keep `TestReviewCommand` and `TestApprovalStillAnswerable` adapted to `modeAsk`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/tui/ -run 'TestSharedApproval|TestStaleAnswer|TestEditorWithdrawal|TestSharedPicker' 2>&1 | head -5`
Expected: build failure `undefined: ask`.

- [ ] **Step 3: Implement `ask.go`**

```go
package tui

import (
	"context"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/review"
)

type askKind int

const (
	askApproval askKind = iota
	askPlan
	askPicker
)

// ask is one question the session puts to every attached terminal at once.
// The first answer wins; the rest are told who answered.
type ask struct {
	Gen    int
	Kind   askKind
	Action string
	Title  string
	Detail string
	Items  []pickItem
	onPick func(v *View, id string, from int) tea.Cmd
	reply  chan askAnswer
	req    string
}

type askAnswer struct {
	OK   bool
	Note string
	From int
}

type askMsg struct{ a *ask }

type askResolvedMsg struct {
	gen int
	by  string
}

// Ask raises a shared question and blocks until a terminal answers, the
// ask is withdrawn, or ctx ends. Never call it while holding s.mu.
func (s *Session) Ask(ctx context.Context, a *ask) askAnswer {
	a.reply = make(chan askAnswer, 1)
	s.mu.Lock()
	s.askGen++
	a.Gen = s.askGen
	s.ask = a
	s.broadcast(askMsg{a})
	s.mu.Unlock()
	select {
	case ans := <-a.reply:
		return ans
	case <-ctx.Done():
		s.CancelAsk(a.Gen, "")
		return askAnswer{}
	}
}

// current is the open ask, if any. Caller holds s.mu.
func (s *Session) current() *ask { return s.ask }

// lastGen is the generation of the newest ask raised. Caller need not hold
// s.mu.
func (s *Session) lastGen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.askGen
}

// Answer resolves the open ask if gen is current. Caller holds s.mu (it is
// a view's Update). For approvals the verdict line becomes a shared entry.
func (s *Session) Answer(gen int, ans askAnswer, from int) (tea.Cmd, bool) {
	a := s.ask
	if a == nil || a.Gen != gen {
		return nil, false
	}
	ans.From = from
	var cmd tea.Cmd
	switch a.Kind {
	case askApproval:
		verdict := "denied"
		if ans.OK {
			verdict = "approved"
		}
		s.appendEntryLocked(entry{Kind: entryVerdict, Label: a.Action, Text: verdict})
		if ans.Note != "" {
			s.appendEntryLocked(entry{Kind: entryDim, Text: ans.Note})
		}
		a.reply <- ans
	case askPlan:
		if ans.OK {
			s.appendEntryLocked(entry{Kind: entryOK, Text: "plan approved — executing"})
		} else {
			s.appendEntryLocked(entry{Kind: entryWarn, Text: "plan discarded"})
		}
		a.reply <- ans
	case askPicker:
		if ans.OK && a.onPick != nil {
			cmd = a.onPick(s.viewByID(from), ans.Note, from) // Note carries the picked id
		}
		a.reply <- ans
	}
	s.ask = nil
	s.broadcast(askResolvedMsg{gen: gen, by: s.clientLabel(from)})
	return cmd, true
}

// CancelAsk withdraws the open ask (the editor answered, or the asker's ctx
// ended). by is the note every view shows; "" shows nothing.
func (s *Session) CancelAsk(gen int, by string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.ask
	if a == nil || a.Gen != gen {
		return
	}
	s.ask = nil
	select {
	case a.reply <- askAnswer{}:
	default:
	}
	s.broadcast(askResolvedMsg{gen: gen, by: by})
}

// SetReview hands the session the review coordinator built in cmd.
func (s *Session) SetReview(c *review.Coordinator) { s.review = c }

// ReviewTerminal is this session as the coordinator's terminal-side
// reviewer: the shared ask, which any attached terminal can answer.
func (s *Session) ReviewTerminal() review.Terminal { return reviewTerminal{s} }

type reviewTerminal struct{ s *Session }

func (t reviewTerminal) Ask(ctx context.Context, preview string) bool {
	return t.s.Ask(ctx, &ask{Kind: askApproval, Action: "file_write", Detail: preview}).OK
}

func (t reviewTerminal) Withdraw(note string) { t.s.CancelAsk(t.s.lastGen(), note) }

func (s *Session) approveFromAgent(action, detail string) bool {
	if action == "shell" && s.cfg.AutoApproveShell {
		return true
	}
	if action == "file_write" && !s.cfg.ApproveFileWrites {
		return true
	}
	ctx := s.rootCtx
	if ctx == nil {
		ctx = context.Background()
	}
	return s.Ask(ctx, &ask{Kind: askApproval, Action: action, Detail: detail}).OK
}

var _ = fmt.Sprintf
```

`s.viewByID(from)` is Task 4's registry lookup. `askResolvedMsg` with `by == ""` shows no note.

View side (`view.go`):

- `case askMsg:` → `m.showAsk(msg.a)`: `m.askShown = a.Gen; m.mode = modeAsk; m.modalVP = viewport.New(m.width-6, m.modalHeight())`; for `askApproval` `m.modalVP.SetContent(ui.ColorizeDiff(a.Detail, true))`; for `askPlan` `SetContent(a.Detail)`; for `askPicker` `m.picker = &picker{title: a.Title, items: a.Items}`.
- `case askResolvedMsg:` → if `m.mode == modeAsk && m.askShown == msg.gen` { `m.mode = m.idleMode(); m.picker = nil; m.focusInputs()`; if `msg.by != ""` and `msg.by != m.label` → `m.renderLocalNote("answered by " + msg.by)` (for a withdrawal `by` is the note itself, e.g. `answered in VS Code`, so: if `msg.by` starts with `answered` show it verbatim, else prefix `answered by `) }.
- `handleAskKey(k)`: for approval `y`/`n`/`esc`/`a` → `cmd, ok := m.Answer(m.askShown, askAnswer{OK: ..., Note: ...}, m.id)`; the `a` key keeps its side effects (`cfg.AutoApproveShell = true` or `cfg.ApproveFileWrites = false; ag.Tools.ApproveWrites = false`) before answering; `if ok { m.mode = m.idleMode(); m.picker = nil }`. For plan: `y`/`n`/`esc`. For picker: reuse `handlePickerKey`'s cursor/filter handling on `m.picker`, and on Enter `m.Answer(m.askShown, askAnswer{OK: true, Note: items[cursor].id}, m.id)`; Esc → `askAnswer{OK: false}`. Every other key scrolls `m.modalVP`.
- `viewAsk()`: the three old renderings (`viewApproval`, plan body, `viewPicker`) selected by `s.current().Kind` (read under the lock the caller holds).
- `renderLocalNote(text)`: `m.rendered.WriteString(m.st.Dim.Render(text) + "\n"); m.refreshTranscript()`.
- `idleMode()` returns `modeBusy` when `m.running`, else `modeInput` (unchanged).
- `/plan`: replace the `planReadyMsg` flow with the goroutine described in Interfaces; `finishTurn` is Task 4's `turnDoneMsg` handler moved onto `Session` as `func (s *Session) finishTurn(rep *agent.ReviewedReport, err error)` (locks, appends the verdict/verify entries, `running=false`, `statusNote=""`, broadcasts `runStateMsg{}`; leftover queue → `startTurnLocked`). If `finishTurn` is not yet on `Session` after Task 4, do that move here: the turn goroutine calls `s.finishTurn(rep, err)` directly instead of sending `turnDoneMsg`; views react to `runStateMsg{running bool, note string}` (set `m.mode` busy/input, placeholder, `focusInputs`, close the queue popup).

- [ ] **Step 4: Run the package**

Run: `go test ./internal/tui/ -count=3 && go build ./...`
Expected: PASS three times (the integration test used to flake; it now flushes deterministically).

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "tui: shared asks — approvals, plan and shared pickers answered once for the session"
```

---

### Task 6: One program per client; per-view input; the runner

**Files:**
- Modify: `internal/tui/served.go` (rewrite), `view.go`, `session.go`
- Delete: `internal/tui/inputs.go` (its surviving helpers move as listed in the file table)
- Modify: `cmd/live.go:134-150`
- Test: `internal/tui/served_test.go` (rewrite), `shared_test.go` (rewrite), `queue_test.go`, `busy_test.go`, `layout_test.go` (adapt)

**Interfaces:**
- `View` now has **one** textarea `input textarea.Model` and `quitHint bool`; `inputFor`, `dropInput`, `inputs`, `queueOwner`, `paletteOwner`, `menuOwner`, `pickerOwner`, `handleGuestKey`, `overlayFor`, `inputTop`, `blankInputRows`, `publishOverlay`, `publishAllOverlays`, `clearAllOverlays`, `publishAfterKey`, `publishVisibilityChange`, `overlayVisible`, `setOverlay`, `live.ClientKeyMsg`/`ClientMouseMsg` cases in `update` are deleted. Keys arrive as `tea.KeyMsg` and `from` is always `m.id`. `handleKey(k)` / `handleMouse(msg)` lose their `from` parameter; slash commands pass `m.id` where a client id is needed.
- `Session.SetClients(infos []live.ClientInfo)`: locks; diffs against `s.clients`; appends `attached: <label>` / `detached: <label>` entries; clears `switchPending` when anyone is present; calls `dropKeyClient(id)` and `histFile.drop(id)` for departed ids; stores the roster; broadcasts `clientsMsg(infos)`. Views on `clientsMsg` only re-render the bottom line (nothing else).
- `Session.Quit()`: locks; cancels a running turn; sets `s.quitting = true`; broadcasts `quitMsg{}`; every view's `update` returns `tea.Quit` on `quitMsg`. In-process, `RunLocal` returns when its one program exits. Served, the runner waits for all programs.
- `(*Session).RunServed(ctx context.Context, h *live.Host) error`:

```go
func (s *Session) RunServed(ctx context.Context, h *live.Host) error {
	defer pinColorProfile()()
	s.mu.Lock()
	s.rootCtx, s.host, s.served, s.idleSince = ctx, h, true, time.Now()
	s.detachClient, s.switchClient = h.Detach, h.Switch
	s.mu.Unlock()

	r := &runner{s: s, h: h, programs: map[int]*program{}, early: map[int][]tea.Msg{}, quit: make(chan struct{})}
	pump := live.NewKeyPump("xterm-256color", r.route)
	defer pump.Close()
	s.mu.Lock()
	s.dropKeyClient = pump.Drop
	s.mu.Unlock()

	h.OnClientSize(r.onClientSize)
	h.OnClients(r.onClients)
	h.OnQuit(func() { s.Quit() })
	h.OnInput(pump.Feed)
	r.onClients(h.Clients()) // clients that attached before this call

	if s.cfg.LiveIdleLimit > 0 {
		go r.idleLoop()
	}
	<-r.quit           // closed by the last program exiting after Quit, or by idle/empty rules calling s.Quit
	r.stopAll()        // quits any program still running and waits for it
	s.histFile.save()
	return nil
}
```

with

```go
type program struct {
	v    *View
	p    *tea.Program
	mb   *mailbox
	done chan struct{}
}

type runner struct {
	s        *Session
	h        *live.Host
	mu       sync.Mutex
	programs map[int]*program
	early    map[int][]tea.Msg // keys that arrived before the program started (cap 256)
	quit     chan struct{}
	quitOnce sync.Once
}

// onClients is the single source of attach and detach.
func (r *runner) onClients(infos []live.ClientInfo) {
	r.s.SetClients(infos)
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[int]bool{}
	for _, c := range infos {
		seen[c.ID] = true
		if _, ok := r.programs[c.ID]; !ok {
			r.startLocked(c)
		}
	}
	for id, pr := range r.programs {
		if !seen[id] {
			go r.stop(pr, id)
			delete(r.programs, id)
		}
	}
	if len(infos) == 0 {
		if r.s.emptyAfterSwitch() { // switchPending && !running && session has no messages
			r.s.Quit()
		}
	}
}

func (r *runner) startLocked(c live.ClientInfo) {
	v := r.s.NewView(c.ID, c.Label) // Task 7 adds the theme lookup inside NewView
	v.ascii = !c.UTF8
	w := r.h.ClientOutput(c.ID)
	v.termWrite = func(s string) { io.WriteString(w, s) }
	v.clipboardWrite = func(s string) error { io.WriteString(w, osc52(s)); return writeClipboardTools(s) }
	p := tea.NewProgram(v, tea.WithInput(nil), tea.WithOutput(w), tea.WithAltScreen(),
		tea.WithMouseCellMotion(), tea.WithoutSignalHandler(), tea.WithoutCatchPanics())
	v.program = p
	pr := &program{v: v, p: p, mb: v.mb, done: make(chan struct{})}
	r.programs[c.ID] = pr
	go pr.mb.run(func(msg tea.Msg) { p.Send(msg) })
	if r.s.cfg.ThemeTerminalColors {
		v.termWrite(terminalColorSeq(v.st.Name()))
	}
	go func() {
		defer close(pr.done)
		defer func() {
			if rec := recover(); rec != nil {
				r.h.Logf("view %d panicked: %v\n%s", c.ID, rec, debug.Stack())
				r.h.Drop(c.ID, "view error")
			}
		}()
		_, _ = p.Run()
		r.programExited()
	}()
	pr.mb.send(tea.WindowSizeMsg{Width: c.Cols, Height: c.Rows})
	for _, k := range r.early[c.ID] {
		pr.mb.send(k)
	}
	delete(r.early, c.ID)
}

func (r *runner) stop(pr *program, id int) {
	pr.p.Quit()
	select {
	case <-pr.done:
	case <-time.After(2 * time.Second):
		pr.p.Kill()
		<-pr.done
	}
	pr.mb.close()
	if r.s.cfg.ThemeTerminalColors {
		io.WriteString(r.h.ClientOutput(id), terminalColorReset())
	}
	r.s.detachView(id)
}

// route delivers a client's key or mouse event to its program.
func (r *runner) route(msg tea.Msg) {
	var id int
	var out tea.Msg
	switch t := msg.(type) {
	case live.ClientKeyMsg:
		id, out = t.Client, t.Key
	case live.ClientMouseMsg:
		id, out = t.Client, t.Mouse
	default:
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if pr, ok := r.programs[id]; ok {
		pr.mb.send(out)
		return
	}
	if len(r.early[id]) < 256 {
		r.early[id] = append(r.early[id], out)
	}
}

func (r *runner) onClientSize(id, cols, rows int) {
	r.mu.Lock()
	pr, ok := r.programs[id]
	r.mu.Unlock()
	if ok {
		pr.mb.send(tea.WindowSizeMsg{Width: cols, Height: rows})
	}
}

// programExited: when the session is quitting and no program is left, let
// RunServed return.
func (r *runner) programExited() {
	if !r.s.isQuitting() {
		return
	}
	r.mu.Lock()
	running := 0
	for _, pr := range r.programs {
		select {
		case <-pr.done:
		default:
			running++
		}
	}
	r.mu.Unlock()
	if running == 0 {
		r.quitOnce.Do(func() { close(r.quit) })
	}
}

func (r *runner) stopAll() {
	r.mu.Lock()
	prs := r.programs
	r.programs = map[int]*program{}
	r.mu.Unlock()
	for id, pr := range prs {
		r.stop(pr, id)
	}
}

func (r *runner) idleLoop() {
	t := time.NewTicker(idleTickInterval)
	defer t.Stop()
	for {
		select {
		case <-r.quit:
			return
		case now := <-t.C:
			if r.s.idleExpired(now) { // no clients, not running, past LiveIdleLimit minutes since idleSince
				r.s.Quit()
				return
			}
		}
	}
}
```

`s.Quit()` with **no** programs registered (idle quit with nobody attached) must still close `r.quit`: `Quit` broadcasts, then the runner's `programExited` check is also invoked by `Quit` through a hook `s.onQuit func()` the runner sets to `r.programExited`. `emptyAfterSwitch`, `isQuitting`, `idleExpired` are small locked getters on `Session`.

- `RunLocal(ctx)`: `v := s.NewView(0, "local")`; terminal colours as the old `Run`; `p := tea.NewProgram(v, tea.WithAltScreen(), tea.WithMouseCellMotion())`; `go v.mb.run(func(m tea.Msg){ p.Send(m) })`; `p.Run()`; `v.mb.close()`; `s.histFile.save()`.
- `cmd/live.go` after `RunServed`: delete the `h.ClearOverlays()` call and its comment (Task 8 removes the method; here just stop calling it — leave the method until Task 8 so the tree stays green if the order is swapped).
- Bottom line, header, wheel: unchanged in content; `⧉ N` and labels read `m.clients` (session roster) under the lock.
- `userPrefix(from)`/`clientLabel(from)`/`clientLabels(room)` move to `session.go` (they read the roster).
- `/detach` → `detach, id := m.detachClient, m.id; return m, func() tea.Msg { detach(id); return nil }`. `/clients` unchanged. `/quit` → `m.QuitLocked()` (the locked variant, since Update holds `mu`) and `return m, nil` — the view exits when its own `quitMsg` arrives. Double Ctrl+C the same.
- The transient toast, spinner, wheel tick and toast tick stay per view (each view runs its own `tea.Tick`s; the toast text is on the session, set by `transientMsg`, and each view schedules its own expiry tick).
- Streaming: views keep a local `streaming` string appended on `deltaMsg`; `flushStreaming` moves to `Session` (`flushLocked`: appends the assistant entry — which broadcasts `entryMsg` — and broadcasts `streamEndMsg{}` **before** the entry so views clear their local streaming text first). Views on `deltaMsg` append to their local copy and refresh; on `streamEndMsg` reset it. A view attaching mid-stream seeds its local copy from `s.streaming` in `NewView`.
- Agent events: `OnDelta` → `s.onDelta(t)`: lock, `s.streaming.WriteString(t)`, broadcast `deltaMsg(t)`, unlock. `OnToolStart` → lock, `flushLocked()`, `appendEntryLocked(entryTool)`, `s.statusNote = "running "+n`, broadcast `statusMsg`, unlock. `OnToolEnd` similarly. `OnNotice` → lock, flush, append `entryNote` (or `entryDim` for `[editor:` notes), unlock. `OnTransient` → lock, `s.toast, s.toastUntil = msg, now+toastFor`, broadcast `transientMsg(msg)`, unlock. `OnReasoning` → `statusMsg` throttled as today. `OnStatus` → `statusMsg`.
- `Submit(text, from)` (was `startTurnFrom`): lock, `appendEntryLocked(entryUser{Label: userPrefix(from), Text})`, `startTurnLocked(text)`, unlock. `startTurnLocked`: `running=true; statusNote="thinking"; broadcast runStateMsg{true,"thinking"}`; cancellable ctx; goroutine `RunFull` → `s.finishTurn`.
- Views on `runStateMsg{running, note}`: `m.mode = modeBusy` or `modeInput` (unless a local popup or ask is open — then `prevMode`), input placeholder text as today, `m.input.Focus()`.

- [ ] **Step 1: Write the failing tests**

Rewrite `internal/tui/shared_test.go` (delete every overlay/guest/owner test) to:

```go
func TestEachViewTypesIntoItsOwnInput(t *testing.T) {
	_, a, b := twoViews(t)
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hello")})
	b.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("world")})
	if a.input.Value() != "hello" || b.input.Value() != "world" {
		t.Fatalf("inputs: a=%q b=%q", a.input.Value(), b.input.Value())
	}
	if !strings.Contains(a.View(), "hello") || strings.Contains(a.View(), "world") {
		t.Fatal("a view must render its own draft and nobody else's")
	}
}

func TestEnterSubmitsWithTheSendersLabel(t *testing.T) {
	s, a, b := twoViews(t)
	var started []string
	s.startTurnHook = func(text string) { started = append(started, text) }
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("from desk")})
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	flush(a, b)
	if len(started) != 1 || started[0] != "from desk" {
		t.Fatalf("started: %v", started)
	}
	if !strings.Contains(b.wrapped, "desk (pid 1)> from desk") {
		t.Fatalf("the other view did not see the labelled line:\n%s", b.wrapped)
	}
	if !b.running || b.mode != modeBusy {
		t.Fatalf("b running=%v mode=%v", b.running, b.mode)
	}
}

func TestPaletteIsLocalToTheViewThatOpenedIt(t *testing.T) {
	_, a, b := twoViews(t)
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	if a.mode != modePalette || b.mode == modePalette {
		t.Fatalf("modes: a=%v b=%v", a.mode, b.mode)
	}
	b.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("typing")})
	if b.input.Value() != "typing" || a.mode != modePalette {
		t.Fatal("b's keys must reach b's input and leave a's palette alone")
	}
}

func TestScrollIsPerView(t *testing.T) {
	s, a, b := twoViews(t)
	for i := 0; i < 200; i++ {
		s.appendEntry(entry{Kind: entryDim, Text: fmt.Sprintf("line %d", i)})
	}
	flush(a, b)
	a.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	if a.vp.AtBottom() || !b.vp.AtBottom() {
		t.Fatalf("a atBottom=%v b atBottom=%v", a.vp.AtBottom(), b.vp.AtBottom())
	}
}

func TestQuitEndsEveryView(t *testing.T) {
	_, a, b := twoViews(t)
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/quit")})
	_, cmdA := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	_ = cmdA
	flush(a, b)
	// each view returned tea.Quit on its quitMsg: drainInto records the last cmd
	if !a.quitSeen || !b.quitSeen {
		t.Fatalf("quit reached a=%v b=%v", a.quitSeen, b.quitSeen)
	}
}

func TestDetachedViewStopsReceiving(t *testing.T) {
	s, a, b := twoViews(t)
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "desk (pid 1)", UTF8: true}})
	s.detachView(2)
	s.appendEntry(entry{Kind: entryDim, Text: "after"})
	flush(a, b)
	if !strings.Contains(a.wrapped, "after") || strings.Contains(b.wrapped, "after") {
		t.Fatal("a detached view must not receive broadcasts")
	}
	if !strings.Contains(a.wrapped, "detached: tablet (pid 2)") {
		t.Fatalf("no detached line:\n%s", a.wrapped)
	}
}
```

(`quitSeen` is a test-only bool the view sets when `update` handles `quitMsg`; `mailbox.drainInto` calls `v.update` and records nothing else.)

`served_test.go`: keep `TestClientsNotServedMessage`, `TestClientsServedEmptyMessage`, `TestPinColorProfileForcesColourAndRestores`; delete the overlay tests and `TestDetachDoesNotBlockTheUpdateLoop`'s overlay parts; rewrite `TestIdleLimitQuitsWhenUnattachedAndIdle` against `s.idleExpired(now)`; add:

```go
func TestRunnerStartsOneProgramPerClientAndStopsOnDetach(t *testing.T) {
	s := newTestSession(t)
	s.served = true
	r := &runner{s: s, programs: map[int]*program{}, early: map[int][]tea.Msg{}, quit: make(chan struct{}), noPrograms: true}
	r.onClients([]live.ClientInfo{{ID: 1, Label: "a", Cols: 80, Rows: 24, UTF8: true}, {ID: 2, Label: "b", Cols: 40, Rows: 15, UTF8: true}})
	if len(r.programs) != 2 {
		t.Fatalf("programs: %d", len(r.programs))
	}
	r.route(live.ClientKeyMsg{Client: 2, Key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}})
	r.route(live.ClientKeyMsg{Client: 3, Key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("early")}})
	flush(r.programs[1].v, r.programs[2].v)
	if r.programs[2].v.input.Value() != "x" || r.programs[1].v.input.Value() != "" {
		t.Fatal("key routed to the wrong view")
	}
	if len(r.early[3]) != 1 {
		t.Fatal("a key for a client with no program yet must be buffered")
	}
	r.onClients([]live.ClientInfo{{ID: 1, Label: "a", Cols: 80, Rows: 24, UTF8: true}})
	if _, ok := r.programs[2]; ok {
		t.Fatal("detached client's program not removed")
	}
}
```

`noPrograms` is a test-only runner flag that makes `startLocked` skip `tea.NewProgram`/`Run` and the mailbox goroutine (the view and mailbox are created; `flush` drives them).

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/tui/ -run 'TestEachViewTypes|TestRunnerStarts|TestScrollIsPerView' 2>&1 | head -5`
Expected: build failure (`a.input undefined`, `undefined: runner`).

- [ ] **Step 3: Implement** the runner and the view changes exactly as in Interfaces; delete `inputs.go` after moving `newInputArea`, `setInputPrompt`, `inputRows` (no longer served-dependent: `1` when `m.compact()`, else `3`), `inputWidth`, `wheelWidthNow`, `inputRow` (renders `m.input.View()` always) into `view.go`, and `clientLabel`/`userPrefix`/`clientLabels` into `session.go`. `newInputArea` keeps the cursor static only when `m.served` (a blinking cursor is a frame every 500 ms per client over the socket).

- [ ] **Step 4: Run everything**

Run: `make -f build.mk verify 2>&1 | tail -3 && go test ./internal/tui/ -count=5`
Expected: PASS. Then a manual smoke in two terminals (`be-code` in one, `be-code attach <code>` in another at a different size): each shows its own prompt, sizes differ, both see the same transcript, `/quit` from one ends both. Record the observation in the commit message.

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "tui: one program per attached terminal; per-view input, size and scroll; overlays gone"
```

---

### Task 7: Themes per device

**Files:**
- Modify: `internal/config/config.go` (`ClientThemes map[string]string \`json:"client_themes"\``, default `map[string]string{}`), `internal/tui/session.go` (`NewView` theme lookup), `internal/tui/palette.go` (`applyTheme`, `themeItems`, `/theme` parsing, `openThemePicker` stays a local `modePicker`), `internal/tui/view.go` (`/theme` dispatch), `README.md` config reference
- Test: `internal/tui/theme_test.go` (extend), `internal/config/config_test.go` (round-trip of `client_themes`)

**Interfaces:**
- `func (s *Session) themeFor(label string) (name, origin string)`: `client_themes[live.LabelKey(label)]` → `(name, "remembered for \"<key>\"")`; else `cfg.Theme` → `(cfg.Theme, "config default")`; else `("dark", "built-in default")`. An unknown name at a step falls through to the next; the view records `m.themeWarn = "theme \"x\" is not known; using <name>"` and prints it as a local dim note on its first `WindowSizeMsg`.
- `View.theme, View.themeOrigin string`.
- `/theme` → local note `<name> (<origin>)`; `/theme <name>` → `applyTheme(name, false)`; `/theme default <name>` → `applyTheme(name, true)`.
- `applyTheme(name string, asDefault bool)`: unknown → local `entryErr`-styled note `unknown theme <name>; try /theme to pick one`; else `m.st = st; m.spin.Style = st.Accent; m.richText = name != "mono"; m.theme = name; m.rebuild()`; terminal colours to `m.termWrite`; then under the lock `if asDefault { cfg.Theme = name; origin = "config default" } else { cfg.ClientThemes[live.LabelKey(m.label)] = name; origin = "remembered for ..." }`; `cfg.Save()` with the same warning text as today on failure; local note `theme set to <name>` (or `theme default set to <name>`). Theme changes are **local notes**, never shared entries.
- `themeItems()` marks the calling view's `m.theme` as `(current)` and appends `pickItem{id: "", label: "default: " + cfg.Theme, desc: "what a new device gets; /theme default <name> changes it"}` whose pick does nothing.

- [ ] **Step 1: Write the failing tests**

Append to `theme_test.go`:

```go
func TestThemeIsPerViewAndRememberedByDevice(t *testing.T) {
	s, a, b := twoViews(t)
	a.slashCommand("/theme nord")
	if a.st.Name() != "nord" || b.st.Name() != "dark" {
		t.Fatalf("themes: a=%s b=%s", a.st.Name(), b.st.Name())
	}
	if got := s.cfg.ClientThemes["desk"]; got != "nord" {
		t.Fatalf("client_themes[desk] = %q", got)
	}
	if s.cfg.Theme != "dark" {
		t.Fatalf("config theme changed to %q by a per-device pick", s.cfg.Theme)
	}
	c := s.NewView(3, "desk (pid 99)")
	if c.st.Name() != "nord" {
		t.Fatalf("a new view from the same device got %q", c.st.Name())
	}
	if name, origin := s.themeFor("desk (pid 99)"); name != "nord" || origin != `remembered for "desk"` {
		t.Fatalf("themeFor = %q, %q", name, origin)
	}
	b.slashCommand("/theme default gruvbox")
	if s.cfg.Theme != "gruvbox" || b.st.Name() != "gruvbox" || a.st.Name() != "nord" {
		t.Fatalf("default: cfg=%s a=%s b=%s", s.cfg.Theme, a.st.Name(), b.st.Name())
	}
	d := s.NewView(4, "new device (pid 5)")
	if d.st.Name() != "gruvbox" {
		t.Fatalf("a new device did not get the config default: %s", d.st.Name())
	}
	b.slashCommand("/theme")
	if !strings.Contains(b.wrapped, "gruvbox (config default)") {
		t.Fatalf("/theme report:\n%s", b.wrapped)
	}
}

func TestUnknownRememberedThemeFallsThroughWithAWarning(t *testing.T) {
	s := newTestSession(t)
	s.cfg.ClientThemes["phone"] = "no-such"
	v := s.NewView(7, "phone (pid 1)")
	v.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if v.st.Name() != "dark" {
		t.Fatalf("fallback theme %s", v.st.Name())
	}
	if !strings.Contains(v.wrapped, `theme "no-such" is not known; using dark`) {
		t.Fatalf("no warning:\n%s", v.wrapped)
	}
}
```

`slashCommand(text)` is the view's dispatcher (it no longer takes `from`).

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/tui/ -run 'TestThemeIsPerView|TestUnknownRemembered' 2>&1 | head -5`
Expected: FAIL (`ClientThemes` undefined, or themes shared).

- [ ] **Step 3: Implement** as in Interfaces. README config reference: add the row

```
- `client_themes` ({}): theme per device, keyed by the attached terminal's label without its pid (`"ssh from 10.0.0.5": "nord"`). Written by `/theme <name>` in a shared session; `/theme default <name>` sets `theme` instead.
```

next to the `theme` row, and in the shared-sessions section: "Each terminal renders at its own size and in its own theme; `/theme` changes only the terminal that ran it and is remembered for that device."

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/tui/ ./internal/config/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "tui: themes per terminal, remembered per device in client_themes; /theme default"
```

---

### Task 8: Remove the shared-size and overlay machinery from `live` and `cmd`

**Files:**
- Modify: `internal/live/host.go` (delete `OnSize`, `onSize`, `Size`, `AnyASCII`, `SetOverlay`, `ClearOverlays`, `client.overlay`, the overlay branch in `fanout.Write`, the shared `h.cols/h.rows` and their recompute; `recomputeAttach` keeps the roster broadcast (`FClients`) and the per-client `onClientSize` calls; the `FSize` frame is no longer sent on resize — only to a freshly attached client is it not needed either, so delete every `FSize` enqueue), `internal/live/frame.go` (keep the `FOverlay`/`FSize` constants so numbering is stable; comment them `// unused since 0.8.0`), `internal/live/client.go` (the `case FOutput, FOverlay:` branch becomes `case FOutput:`; `case FSize:` stays harmless), `cmd/live.go` (no `ClearOverlays`)
- Test: `internal/live/host_test.go` (delete size-minimum/overlay tests; add `TestNoSharedSizeFramesOnResize`)

- [ ] **Step 1: Write the failing test**

```go
func TestResizeSendsNoSizeFrameToOtherClients(t *testing.T) {
	h, sock := startHost(t)
	a := dial(t, sock, "tok", "a", 80, 24)
	b := dial(t, sock, "tok", "b", 120, 40)
	within(t, 2*time.Second, func() bool { return len(h.Clients()) == 2 })
	for len(b.cl) > 0 {
		<-b.cl
	}
	a.resize(t, 40, 15)
	select {
	case <-b.size:
		t.Fatal("b received a shared-size frame after a's resize")
	case <-time.After(300 * time.Millisecond):
	}
	select {
	case cl := <-b.cl:
		for _, c := range cl {
			if c.Label == "a" && (c.Cols != 40 || c.Rows != 15) {
				t.Fatalf("roster carries a's old size: %+v", c)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("b did not receive the roster update carrying a's new size")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/live/ -run TestResizeSendsNoSizeFrame`
Expected: FAIL (`b received a shared-size frame`).

- [ ] **Step 3: Delete** the listed code. `grep -rn 'SetOverlay\|ClearOverlays\|AnyASCII\|OnSize\b\|h.Size()' --include=*.go .` must print nothing outside comments.

- [ ] **Step 4: Verify**

Run: `make -f build.mk verify 2>&1 | tail -3`
Expected: all packages ok.

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "live: drop the shared minimum size and overlay frames; roster carries sizes"
```

---

### Task 9: Served integration test and failure isolation

**Files:**
- Create: `internal/tui/served_integration_test.go`
- Modify: `internal/tui/served.go` (only if the test finds a defect)

- [ ] **Step 1: Write the test**

Use the real `live.Host`: `live.NewHost("tok", io.Discard)`, `h.Listen(sock)` in a `t.TempDir()` **short enough for AF_UNIX** (use `os.MkdirTemp("", "bl")` as `startHost` does, never the long test tempdir), `go h.Serve()`. Copy `fakeClient`/`dial` from `internal/live/host_test.go` (about 40 lines) into `internal/tui/livefake_test.go` as `dialFake`, adding `collect(t, needle, timeout) string` (concatenates `out` payloads until one contains `needle`), `bytesSoFar() int`, `resize`, `detach(t)` (writes `FDetach`) and `readBye(t) string`; `newTestHostFor(t)` returns `(h, sock, "tok")`. `live`'s API stays unchanged.

```go
func TestServedSessionRendersEachClientAtItsOwnSize(t *testing.T) {
	tempHome(t)
	s := newTestSession(t)
	h, sock, token := newTestHostFor(t)
	done := make(chan error, 1)
	go func() { done <- s.RunServed(context.Background(), h) }()
	small := dialFake(t, sock, token, "phone (pid 1)", 40, 15)
	large := dialFake(t, sock, token, "desk (pid 2)", 120, 40)
	waitFor(t, func() bool { return len(h.Clients()) == 2 })
	s.appendEntry(entry{Kind: entryDim, Text: "shared line"})
	smallOut := small.collect(t, "shared line", 3*time.Second)
	largeOut := large.collect(t, "shared line", 3*time.Second)
	if !strings.Contains(smallOut, "> ") || strings.Contains(smallOut, "(>): ") {
		t.Fatalf("small client not in compact layout:\n%s", smallOut)
	}
	if !strings.Contains(largeOut, "(>): ") || !strings.Contains(largeOut, "BE-Code Redux") {
		t.Fatalf("large client not in full layout with header:\n%s", largeOut)
	}
	before := large.bytesSoFar()
	small.resize(t, 60, 20)
	time.Sleep(300 * time.Millisecond)
	if large.bytesSoFar() != before {
		t.Fatal("resizing the small client produced output for the large one")
	}
	small.detach(t)
	waitFor(t, func() bool { return len(h.Clients()) == 1 })
	s.Quit()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunServed did not return after Quit")
	}
}

func TestPanickingViewIsDroppedAndTheSessionContinues(t *testing.T) {
	tempHome(t)
	s := newTestSession(t)
	h, sock, token := newTestHostFor(t)
	go s.RunServed(context.Background(), h)
	a := dialFake(t, sock, token, "a (pid 1)", 80, 24)
	b := dialFake(t, sock, token, "b (pid 2)", 80, 24)
	waitFor(t, func() bool { return len(h.Clients()) == 2 })
	s.viewByID(1).panicOnNextUpdate = true // test-only hook in View.update
	s.appendEntry(entry{Kind: entryDim, Text: "boom"})
	if reason := a.readBye(t); reason != "view error" {
		t.Fatalf("bye reason %q", reason)
	}
	b.collect(t, "boom", 3*time.Second)
	waitFor(t, func() bool { return len(h.Clients()) == 1 })
	s.Quit()
}
```

- [ ] **Step 2: Run it**

Run: `go test ./internal/tui/ -run 'TestServedSession|TestPanickingView' -count=3 -race`
Expected: PASS three times with the race detector clean. Fix any defect in the runner it exposes (do not weaken the test).

- [ ] **Step 3: Run the whole verify with the race detector on the two packages**

Run: `go test -race ./internal/tui/ ./internal/live/ && make -f build.mk verify 2>&1 | tail -2`
Expected: clean.

- [ ] **Step 4: Commit**

```bash
git add -A && git commit -m "tui: served integration test — per-client layout, isolated resize, panic isolation"
```

---

### Task 10: Docs, checklist, changelog, version

**Files:**
- Modify: `README.md` (shared sessions section: per-terminal rendering, `/theme` behaviour, `client_themes` already added in Task 7), `CHANGELOG.md` (v0.8.0 entry), `build.mk` (`VERSION := 0.8.0`), `docs/live-checklist.md` (items 3, 6, 9 rewritten; new theme item), `/home/sbrown/Documents/Development/BE-CodeRedux/CLAUDE.md` (the "Shared (live) sessions" section rewritten for Session/View; the `(currently vX)` line; the "Per-client input over a shared frame" and "Two invariants" paragraphs replaced)

- [ ] **Step 1: CHANGELOG entry**

```markdown
## v0.8.0 — every terminal renders itself

- **Per-terminal rendering in shared sessions.** The host now runs one
  renderer per attached terminal: each has its own size and layout (a phone
  gets compact while the desktop keeps the full layout and header), its own
  scroll position, selection, input line and theme. The transcript, agent,
  message queue, roster, approvals and pickers are one shared session. The
  0.6.0 overlay mechanism and the "smallest terminal wins" shared size are
  gone. (`internal/tui`: `Session` + `View`; `internal/live`:
  `ClientOutput`, `OnClientSize`, `Drop`.)
- **Shared prompts, answered once.** Approvals, the plan prompt and the
  model/provider/session pickers appear on every terminal; the first answer
  wins and the others close with `answered by <label>`. The shared review
  prompt uses the same path.
- **Themes per device.** `/theme <name>` recolours only the terminal that ran
  it and is remembered in `client_themes` under that device's label;
  `/theme default <name>` sets the config default for new devices; `/theme`
  alone reports the theme in use and where it came from.
- A terminal whose renderer fails is disconnected with `view error`; the
  session and the other terminals continue.
```

- [ ] **Step 2: Checklist items** (replace 3, 6, 9; add 14):

```
3. Type in both terminals at once, without pressing Enter: each terminal shows only its
   own draft on its own input line. Press Enter in each: both messages appear in the
   shared transcript, each prefixed with the label of the terminal that sent it.
6. Resize the smallest terminal: only that terminal relayouts; the others do not move.
   Drag-select transcript text in one terminal: the highlight appears there only.
9. From a phone SSH app: attach; expect the compact layout on the phone (no header,
   short bottom line, `>` prompt) while the desktop keeps its full layout and header.
14. On the phone: `/theme nord`. Expect the phone recoloured, the desktop unchanged, and
    `theme set to nord` on the phone only. Detach and reattach the phone: still nord.
    `/theme` on the desktop prints `dark (config default)`; `/theme default gruvbox`
    there changes what a new device gets and the desktop itself, not the phone.
```

- [ ] **Step 3: Root CLAUDE.md** — rewrite the "Shared (live) sessions" section's rendering paragraphs to describe: `Session` (core, `mu`, entries, broadcast, mailboxes), `View` (per client program on `Host.ClientOutput`), the runner in `served.go` (`onClients` is the single source of attach/detach, `route`, `stop`, the quit invariant "no program is running when the closing lines are written"), shared asks (`ask.go`: first answer wins, `askResolvedMsg`), themes per device (`client_themes`, `LabelKey`), and the removal list. Update the version line to 0.8.0.

- [ ] **Step 4: Verify and commit**

Run: `make -f build.mk verify 2>&1 | tail -2 && grep -n 'VERSION' build.mk`
Expected: ok; `VERSION := 0.8.0`.

```bash
git add -A && git commit -m "docs: per-terminal rendering (0.8.0) — README, changelog, checklist, CLAUDE.md"
```

---

## Self-review notes

- Spec §1 core/views → Tasks 4, 6. §2 host/runner → Tasks 3, 6, 8. §3 shared asks and local popups → Task 5 (popups stay local by construction in Task 6). §4 themes → Tasks 1, 7. §5 rendering per view → Tasks 2, 6 (streaming per view). §6 lifecycle, failure, testing, removals → Tasks 6, 8, 9; docs → Task 10.
- Spec deviation recorded: `ui.RenderMarkdown` keeps its signature (it never wrapped; the view wraps) — spec amended 2026-09-13.
- Names used across tasks: `styles`/`newStyles`/`stylesOr` (T1) used by T2, T4, T7; `entry`/`renderEntry`/`appendEntry`/`appendEntryLocked`/`rebuild` (T2) used by T4–T7; `ClientOutput`/`OnClientSize`/`Drop`/`LabelKey` (T3) used by T6, T7, T8; `Session`/`View`/`NewSession`/`NewView`/`mailbox`/`flush`/`newTestSession` (T4) used by T5–T9; `ask`/`Ask`/`Answer`/`CancelAsk`/`askMsg`/`askResolvedMsg`/`modeAsk`/`twoViews`/`waitFor` (T5) used by T6, T7, T9; `runner`/`program`/`RunServed`/`RunLocal`/`SetClients`/`Quit`/`runStateMsg`/`quitMsg`/`streamEndMsg` (T6) used by T8, T9; `themeFor`/`applyTheme(name, asDefault)` (T7).
