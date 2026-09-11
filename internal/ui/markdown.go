package ui

import (
	"strings"

	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
)

// RenderMarkdown applies lightweight terminal styling to assistant text:
// chroma-highlighted fenced code blocks, bold headers, and cyan inline
// code. It deliberately avoids a full markdown engine — agent replies are
// mostly prose + code, and partial rendering must never corrupt content.
func RenderMarkdown(text string, enableColor bool) string {
	if !enableColor {
		return text
	}
	var out strings.Builder
	lines := strings.Split(text, "\n")
	inFence := false
	var fenceLang string
	var fenceBuf []string

	flushFence := func() {
		code := strings.Join(fenceBuf, "\n")
		out.WriteString(highlightCode(code, fenceLang))
		if len(fenceBuf) > 0 {
			out.WriteString("\n")
		}
		fenceBuf = nil
	}

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if inFence {
				flushFence()
				inFence = false
			} else {
				inFence = true
				fenceLang = strings.TrimPrefix(trimmed, "```")
			}
			continue
		}
		if inFence {
			fenceBuf = append(fenceBuf, line)
			continue
		}
		out.WriteString(styleProse(line))
		if i < len(lines)-1 {
			out.WriteString("\n")
		}
	}
	if inFence { // unterminated fence
		flushFence()
	}
	return out.String()
}

func styleProse(line string) string {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "#") {
		return "\x1b[1m" + line + "\x1b[0m"
	}
	// inline `code`
	if strings.Count(line, "`") >= 2 {
		var b strings.Builder
		parts := strings.Split(line, "`")
		for i, p := range parts {
			if i%2 == 1 && i < len(parts)-(len(parts)%2) {
				b.WriteString("\x1b[36m" + p + "\x1b[0m")
			} else {
				b.WriteString(p)
			}
		}
		return b.String()
	}
	return line
}

func highlightCode(code, lang string) string {
	lexer := lexers.Get(lang)
	if lexer == nil {
		lexer = lexers.Analyse(code)
	}
	if lexer == nil {
		lexer = lexers.Fallback
	}
	formatter := formatters.Get("terminal256")
	style := styles.Get("monokai")
	it, err := lexer.Tokenise(nil, code)
	if err != nil {
		return code
	}
	var b strings.Builder
	if err := formatter.Format(&b, style, it); err != nil {
		return code
	}
	return b.String()
}
