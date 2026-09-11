// Package diff renders compact unified diffs for file-change previews.
// Pure Go, line-based, O(N*M) LCS with a size guard — plenty for reviewing
// agent edits, with zero external dependencies.
package diff

import (
	"fmt"
	"strings"
)

// Line is one diff line.
type Line struct {
	Kind byte // ' ', '+', '-'
	Text string
}

// Hunk is a contiguous run of changes with context.
type Hunk struct {
	AStart, ALines int
	BStart, BLines int
	Lines          []Line
}

// maxLCSCells caps the DP table so pathological inputs fall back to a
// whole-file replace view instead of burning CPU.
const maxLCSCells = 4_000_000

// Unified computes hunks between old and new content with n context lines.
func Unified(oldText, newText string, context int) []Hunk {
	a := splitLines(oldText)
	b := splitLines(newText)
	ops := diffOps(a, b)
	return buildHunks(a, b, ops, context)
}

// Render formats hunks in classic unified style. If color is true, ANSI
// green/red is applied to additions/removals.
func Render(hunks []Hunk, color bool) string {
	var sb strings.Builder
	for _, h := range hunks {
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", h.AStart, h.ALines, h.BStart, h.BLines)
		for _, l := range h.Lines {
			line := string(l.Kind) + l.Text
			if color {
				switch l.Kind {
				case '+':
					line = "\x1b[32m" + line + "\x1b[0m"
				case '-':
					line = "\x1b[31m" + line + "\x1b[0m"
				}
			}
			sb.WriteString(line)
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// Preview builds a bounded, human-oriented preview for approval prompts.
func Preview(path, oldText, newText string, color bool) string {
	if oldText == newText {
		return fmt.Sprintf("%s: no changes", path)
	}
	header := fmt.Sprintf("--- %s (current)\n+++ %s (proposed)\n", path, path)
	if oldText == "" {
		return header + renderNewFile(newText, color)
	}
	hunks := Unified(oldText, newText, 3)
	body := Render(hunks, color)
	const cap = 8 * 1024
	if len(body) > cap {
		body = body[:cap] + "\n... [diff truncated]\n"
	}
	adds, dels := Stats(hunks)
	return header + body + fmt.Sprintf("(+%d -%d lines)", adds, dels)
}

// Stats counts added/removed lines across hunks.
func Stats(hunks []Hunk) (adds, dels int) {
	for _, h := range hunks {
		for _, l := range h.Lines {
			switch l.Kind {
			case '+':
				adds++
			case '-':
				dels++
			}
		}
	}
	return
}

func renderNewFile(newText string, color bool) string {
	lines := splitLines(newText)
	var sb strings.Builder
	shown := lines
	trunc := ""
	if len(shown) > 120 {
		shown = shown[:120]
		trunc = fmt.Sprintf("... [%d more lines]\n", len(lines)-120)
	}
	for _, l := range shown {
		s := "+" + l
		if color {
			s = "\x1b[32m" + s + "\x1b[0m"
		}
		sb.WriteString(s)
		sb.WriteByte('\n')
	}
	sb.WriteString(trunc)
	fmt.Fprintf(&sb, "(new file, %d lines)", len(lines))
	return sb.String()
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

type op struct {
	kind byte // '=', '+', '-'
	ai   int  // index in a for '=', '-'
	bi   int  // index in b for '=', '+'
}

// diffOps produces edit operations via LCS; falls back to full replace for
// oversized inputs.
func diffOps(a, b []string) []op {
	// Trim common prefix/suffix first — typical agent edits touch one spot.
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	am := a[pre : len(a)-suf]
	bm := b[pre : len(b)-suf]

	var mid []op
	if len(am)*len(bm) > maxLCSCells {
		for i := range am {
			mid = append(mid, op{'-', pre + i, 0})
		}
		for j := range bm {
			mid = append(mid, op{'+', 0, pre + j})
		}
	} else {
		mid = lcsOps(am, bm, pre)
	}

	ops := make([]op, 0, pre+len(mid)+suf)
	for i := 0; i < pre; i++ {
		ops = append(ops, op{'=', i, i})
	}
	ops = append(ops, mid...)
	for i := 0; i < suf; i++ {
		ops = append(ops, op{'=', len(a) - suf + i, len(b) - suf + i})
	}
	return ops
}

func lcsOps(a, b []string, offset int) []op {
	n, m := len(a), len(b)
	dp := make([][]int32, n+1)
	for i := range dp {
		dp[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var ops []op
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, op{'=', offset + i, offset + j})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, op{'-', offset + i, 0})
			i++
		default:
			ops = append(ops, op{'+', 0, offset + j})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, op{'-', offset + i, 0})
	}
	for ; j < m; j++ {
		ops = append(ops, op{'+', 0, offset + j})
	}
	return ops
}

func buildHunks(a, b []string, ops []op, context int) []Hunk {
	var hunks []Hunk
	i := 0
	for i < len(ops) {
		if ops[i].kind == '=' {
			i++
			continue
		}
		// Found a change; expand to a hunk with context.
		start := i - context
		if start < 0 {
			start = 0
		}
		// Walk forward, merging changes separated by <= 2*context equal lines.
		end := i
		eq := 0
		for k := i; k < len(ops); k++ {
			if ops[k].kind == '=' {
				eq++
				if eq > 2*context {
					break
				}
			} else {
				eq = 0
				end = k
			}
		}
		stop := end + context + 1
		if stop > len(ops) {
			stop = len(ops)
		}

		h := Hunk{}
		first := true
		for k := start; k < stop; k++ {
			o := ops[k]
			switch o.kind {
			case '=':
				if first {
					h.AStart, h.BStart = o.ai+1, o.bi+1
					first = false
				}
				h.Lines = append(h.Lines, Line{' ', a[o.ai]})
				h.ALines++
				h.BLines++
			case '-':
				if first {
					h.AStart, h.BStart = o.ai+1, bStartFor(ops, k)+1
					first = false
				}
				h.Lines = append(h.Lines, Line{'-', a[o.ai]})
				h.ALines++
			case '+':
				if first {
					h.AStart, h.BStart = aStartFor(ops, k)+1, o.bi+1
					first = false
				}
				h.Lines = append(h.Lines, Line{'+', b[o.bi]})
				h.BLines++
			}
		}
		hunks = append(hunks, h)
		i = stop
	}
	return hunks
}

func bStartFor(ops []op, k int) int {
	for i := k - 1; i >= 0; i-- {
		if ops[i].kind != '-' {
			return opBIndex(ops[i]) + 1
		}
	}
	return 0
}

func aStartFor(ops []op, k int) int {
	for i := k - 1; i >= 0; i-- {
		if ops[i].kind != '+' {
			return ops[i].ai + 1
		}
	}
	return 0
}

func opBIndex(o op) int {
	if o.kind == '-' {
		return -1
	}
	return o.bi
}
