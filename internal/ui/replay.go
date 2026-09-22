package ui

import (
	"regexp"
	"strings"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/provider"
)

// A resumed session used to start with a blank terminal: the model got its
// history back, the person did not. Replay turns the saved messages into
// the lines the live transcript would have shown, in both UIs' own styles.

type ReplayKind int

const (
	ReplayUser      ReplayKind = iota // Text: what the person typed
	ReplayAssistant                   // Text: the reply (Markdown)
	ReplayTool                        // Label: tool name, Text: arguments
	ReplayToolOK                      // Text: first line of the result
	ReplayToolErr                     // Text: first line of the error
	ReplaySummary                     // Text: a compaction summary
)

type ReplayLine struct {
	Kind        ReplayKind
	Label, Text string
}

// ReplayDivider is the line that separates the replayed transcript from
// what happens next.
const ReplayDivider = "— resumed here —"

var compatResult = regexp.MustCompile(`(?s)<tool_result name="([^"]*)" status="([^"]*)">\n?(.*?)\n?</tool_result>`)

// Replay converts saved messages to transcript lines. turns > 0 keeps only
// the last turns user requests (and everything after them).
func Replay(msgs []provider.Message, turns int) []ReplayLine {
	if turns > 0 {
		seen := 0
		start := 0
		for i := len(msgs) - 1; i >= 0; i-- {
			m := msgs[i]
			if m.Role == provider.RoleUser && !isCompatResult(m.Content) && !strings.HasPrefix(m.Content, agent.SummaryPrefix) {
				seen++
				if seen == turns {
					start = i
					break
				}
			}
		}
		msgs = msgs[start:]
	}
	var out []ReplayLine
	for _, m := range msgs {
		// The harness state attached to a message is for the model; a replay
		// shows what the person and the tools said.
		m.Content = agent.StripHarnessState(m.Content)
		switch m.Role {
		case provider.RoleUser:
			switch {
			case strings.HasPrefix(m.Content, agent.SummaryPrefix):
				out = append(out, ReplayLine{Kind: ReplaySummary, Text: strings.TrimSpace(strings.TrimPrefix(m.Content, agent.SummaryPrefix))})
			case isCompatResult(m.Content):
				for _, r := range compatResult.FindAllStringSubmatch(m.Content, -1) {
					out = append(out, resultLine(r[1], r[2] == "error", r[3]))
				}
			default:
				if t := strings.TrimSpace(m.Content); t != "" {
					out = append(out, ReplayLine{Kind: ReplayUser, Text: t})
				}
			}
		case provider.RoleAssistant:
			if t := strings.TrimSpace(m.Content); t != "" {
				out = append(out, ReplayLine{Kind: ReplayAssistant, Text: t})
			}
			for _, tc := range m.ToolCalls {
				out = append(out, ReplayLine{Kind: ReplayTool, Label: tc.Name, Text: tc.Arguments})
			}
		case provider.RoleTool:
			out = append(out, resultLine(m.Name, looksLikeError(m.Content), m.Content))
		}
	}
	return out
}

func isCompatResult(s string) bool { return strings.HasPrefix(s, "<tool_result") }

// looksLikeError guesses at a native tool result's status, which the saved
// message does not carry: the tools' own error texts start this way.
func looksLikeError(content string) bool {
	first := strings.ToLower(strings.SplitN(strings.TrimSpace(content), "\n", 2)[0])
	return strings.HasPrefix(first, "error") || strings.Contains(first, "no such file") || strings.HasPrefix(first, "bad ") || strings.HasPrefix(first, "denied")
}

func resultLine(name string, isErr bool, content string) ReplayLine {
	first := strings.SplitN(strings.TrimSpace(content), "\n", 2)[0]
	if len(first) > 100 {
		first = first[:100] + "…"
	}
	kind := ReplayToolOK
	if isErr {
		kind = ReplayToolErr
	}
	return ReplayLine{Kind: kind, Label: name, Text: first}
}
