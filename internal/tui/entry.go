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
	// The co-working model's two lines: the question the primary (or the
	// person) put to it, and the answer it gave. Both carry the co-worker's
	// name as the Label and are rendered in the theme's Cowork colour, so a
	// second voice in the transcript is never mistaken for the primary's.
	entryCoworkAsk // Label = co-worker name ("<name>? "), Text = the question
	entryCowork    // Label = co-worker name ("<name>> "), Text = the Markdown answer
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
	case entryCoworkAsk:
		return st.Cowork.Render(e.Label+"? ") + e.Text
	case entryCowork:
		return st.Cowork.Render(e.Label+"> ") + ui.RenderMarkdown(e.Text, richText)
	case entryVerdict:
		v := st.Err.Render(e.Text)
		if e.Text == "approved" {
			v = st.OK.Render(e.Text)
		}
		return st.Warn.Render(e.Label+" ") + v
	}
	return strings.TrimRight(e.Text, "\n")
}
