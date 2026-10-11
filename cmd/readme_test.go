package cmd

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// A closing code fence with prose on the same line is not a closing fence:
// CommonMark treats the line as content and the block runs on to the next
// bare fence. One such line in the work-ledger section put the rest of that
// section and the whole config reference inside a code block on GitHub.
func TestReadmeFencesCloseOnTheirOwnLine(t *testing.T) {
	b, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	// An opening fence may carry an info string ("```bash"); nothing may
	// follow it, and a closing fence carries nothing at all.
	bad := regexp.MustCompile("^```\\S*\\s+\\S")
	for i, line := range strings.Split(string(b), "\n") {
		if bad.MatchString(line) {
			t.Errorf("README.md:%d: text after a code fence: %q", i+1, line)
		}
	}
}
