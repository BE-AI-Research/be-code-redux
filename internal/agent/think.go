package agent

import (
	"regexp"
	"strings"
)

var thinkBlockRe = regexp.MustCompile(`(?s)<(think|thought|thinking)>.*?</(think|thought|thinking)>\s*`)

// StripThink removes complete reasoning blocks from final text, plus an
// unterminated trailing block (model ran out of tokens mid-thought).
func StripThink(s string) string {
	s = thinkBlockRe.ReplaceAllString(s, "")
	for _, tag := range []string{"<think>", "<thought>", "<thinking>"} {
		if i := strings.Index(s, tag); i >= 0 && !strings.Contains(s[i:], "</") {
			s = s[:i]
		}
	}
	return strings.TrimSpace(s)
}

// ThinkFilter incrementally suppresses reasoning blocks in a streamed
// delta sequence, so the user never watches 2k tokens of <think> scroll by.
// It buffers only when a partial tag straddles a chunk boundary.
type ThinkFilter struct {
	inThink bool
	pending string // partial-tag holdback
}

var openTags = []string{"<think>", "<thought>", "<thinking>"}
var closeTags = []string{"</think>", "</thought>", "</thinking>"}

// Feed processes one delta and returns the user-visible portion.
func (f *ThinkFilter) Feed(delta string) string {
	s := f.pending + delta
	f.pending = ""
	var out strings.Builder
	for s != "" {
		if f.inThink {
			idx, tag := findFirst(s, closeTags)
			if idx < 0 {
				if hb := holdback(s, closeTags); hb > 0 {
					f.pending = s[len(s)-hb:]
				}
				return out.String() // rest is thought; drop it
			}
			s = s[idx+len(tag):]
			f.inThink = false
			continue
		}
		idx, tag := findFirst(s, openTags)
		if idx < 0 {
			hb := holdback(s, openTags)
			out.WriteString(s[:len(s)-hb])
			f.pending = s[len(s)-hb:]
			return out.String()
		}
		out.WriteString(s[:idx])
		s = s[idx+len(tag):]
		f.inThink = true
	}
	return out.String()
}

// Flush releases any held-back partial tag at stream end.
func (f *ThinkFilter) Flush() string {
	if f.inThink {
		f.pending = ""
		return ""
	}
	p := f.pending
	f.pending = ""
	return p
}

func findFirst(s string, tags []string) (int, string) {
	best := -1
	bestTag := ""
	for _, t := range tags {
		if i := strings.Index(s, t); i >= 0 && (best < 0 || i < best) {
			best, bestTag = i, t
		}
	}
	return best, bestTag
}

// holdback returns how many trailing bytes could be the start of any tag.
func holdback(s string, tags []string) int {
	max := 0
	for _, t := range tags {
		for n := len(t) - 1; n > 0; n-- {
			if n <= len(s) && strings.HasSuffix(s, t[:n]) {
				if n > max {
					max = n
				}
				break
			}
		}
	}
	return max
}
