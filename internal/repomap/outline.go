package repomap

import (
	"path/filepath"
	"strings"
)

// Outline extracts the symbol names Build would list for one file. name is
// used for its extension only; an extension with no extractor yields nil.
func Outline(name string, data []byte) []string {
	exts, ok := extractors[strings.ToLower(filepath.Ext(name))]
	if !ok || len(data) > maxFileSize {
		return nil
	}
	src := string(data)
	var syms []string
	seen := map[string]bool{}
	for _, ex := range exts {
		for _, match := range ex.re.FindAllStringSubmatch(src, -1) {
			n := match[len(match)-1]
			if n == "" || seen[n] || n == "init" || n == "main" && len(syms) > 0 {
				continue
			}
			seen[n] = true
			syms = append(syms, n)
			if len(syms) >= maxSymbolsFile {
				return syms
			}
		}
	}
	return syms
}
