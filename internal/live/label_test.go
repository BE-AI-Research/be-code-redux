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
