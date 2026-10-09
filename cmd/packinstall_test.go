package cmd

import (
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/config"
)

// captureStderr is this package's existing helper (cmd/engine_test.go:64).
// The stderr message is the only evidence of *which* branch refused, and the
// two branches mean different things: one says "-y never approves this", the
// other promises "use -y to auto-approve".

// -y means "run the commands I chose", not "fetch code from the internet and
// put its rules in my system prompt".
func TestHeadlessNeverApprovesPackInstall(t *testing.T) {
	cfg := config.Default()
	cfg.AutoApproveShell = true
	cfg.AutoApproveBrowser = true

	var ok bool
	out := captureStderr(t, func() {
		ok = headlessApprover(cfg)("pack_install", "install assist from https://example/r")
	})
	if ok {
		t.Fatal("-y approved pack_install")
	}
	// It must be refused *because it is classified*, not because stdin
	// happened not to be a terminal. Those differ in what they tell the
	// person, and the fallback's advice would be a false promise: -y is
	// already set here and still did not approve it.
	if !strings.Contains(out, "refused pack_install") {
		t.Errorf("pack_install was refused by the wrong branch; -y must never approve it.\ngot: %s", out)
	}
	if strings.Contains(out, "use -y") {
		t.Errorf("the refusal promises that -y would work, which it does not.\ngot: %s", out)
	}
}

// With no -y at all and no terminal, the hint must not promise -y either:
// pack_install always asks a person.
func TestHeadlessNonInteractivePackInstallDoesNotPromiseYes(t *testing.T) {
	cfg := config.Default()
	stub := func() bool { return false }
	saved := stdinIsTTY
	stdinIsTTY = stub
	defer func() { stdinIsTTY = saved }()

	var ok bool
	out := captureStderr(t, func() {
		ok = headlessApprover(cfg)("pack_install", "install assist from https://example/r")
	})
	if ok {
		t.Fatal("a non-interactive run approved pack_install")
	}
	if strings.Contains(out, "use -y") {
		t.Errorf("pack_install always asks a person, so the hint must not promise -y.\ngot: %s", out)
	}
	if !strings.Contains(out, "always asks a person") {
		t.Errorf("the message should say this action always asks.\ngot: %s", out)
	}
}

// Spec §8 and §14: -y never answers pack_install. packsApprover is the only
// approver the install path actually reaches — headlessApprover is wired for
// `run` and `init`, neither of which installs — so this is the assertion that
// matters. Before this test, -y approved it here, which made an unattended
// install reach the trust file: the model could run
// `be-code packs add x --source <url> -y` through shell under
// AutoApproveShell and end up with its own standing instructions replaced.
func TestPacksApproverNeverAcceptsDashY(t *testing.T) {
	savedYes, savedTTY := flagYes, stdinIsTTY
	flagYes = true
	stdinIsTTY = func() bool { return false }
	defer func() { flagYes, stdinIsTTY = savedYes, savedTTY }()

	var buf strings.Builder
	if packsApprover(&buf)("pack_install", "install assist from https://example/r") {
		t.Fatal("-y approved pack_install")
	}
	if strings.Contains(buf.String(), "approved by -y") {
		t.Errorf("the output claims -y approved it: %s", buf.String())
	}
	if strings.Contains(buf.String(), "use -y") {
		t.Errorf("the refusal must not promise that -y would work: %s", buf.String())
	}
}

// M3 re-graded: packOptions used filepath.Abs, where the rest of cmd/ uses
// mustAbs. `-C '~/proj'` (tilde quoted, as a script or a config file would
// have it) then installed into <cwd>/~/proj and keyed the trust file on that
// path — silently the wrong workspace, with no error.
func TestPackOptionsExpandsTheWorkspaceLikeEveryOtherCommand(t *testing.T) {
	saved := flagDir
	flagDir = "~/be-code-pack-test"
	defer func() { flagDir = saved }()

	var buf strings.Builder
	o, err := packOptions(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(o.Workspace, "~") {
		t.Errorf("workspace still holds a literal tilde: %q", o.Workspace)
	}
}
