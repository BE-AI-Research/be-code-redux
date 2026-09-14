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
