package tools

import "testing"

// A verification command written as an argument vector still has to honour
// shell_deny. There is no shell, so there are no segments to split: the globs
// are matched against the command as a person reads it and against the
// program it runs, so a rule written either way holds.
func TestCheckDeniedMatchesDisplayAndProgram(t *testing.T) {
	cases := []struct {
		name    string
		deny    []string
		display string
		argv    []string
		want    bool
	}{
		{"program matched by its own path", []string{"/usr/bin/curl"},
			"/usr/bin/curl http://example.com", []string{"/usr/bin/curl", "http://example.com"}, true},
		{"program matched by a glob", []string{"*curl*"},
			"/usr/bin/curl http://example.com", []string{"/usr/bin/curl", "http://example.com"}, true},
		// The rule is the person's intent, and it holds even though no shell
		// will ever read this argument as syntax.
		{"an argument matched through the display form", []string{"*rm -rf*"},
			`ue-build --project "/p; rm -rf ~"`, []string{"ue-build", "--project", "/p; rm -rf ~"}, true},
		{"nothing matches", []string{"*curl*", "sudo *"},
			"ue-build --project /p", []string{"ue-build", "--project", "/p"}, false},
		{"no deny rules", nil, "anything at all", []string{"anything", "at", "all"}, false},
		// A shell-string check has segments, so a denied command cannot hide
		// behind an allowed prefix.
		{"shell string, denied segment", []string{"rm -rf*"},
			"go build ./... && rm -rf /", nil, true},
		{"shell string, allowed", []string{"rm -rf*"}, "go build ./...", nil, false},
	}
	for _, tc := range cases {
		r := &Registry{ShellDeny: tc.deny}
		if got := r.CheckDenied(tc.display, tc.argv); got != tc.want {
			t.Errorf("%s: CheckDenied(%q, %v) = %v, want %v", tc.name, tc.display, tc.argv, got, tc.want)
		}
	}
}
