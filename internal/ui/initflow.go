package ui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/diff"
	"github.com/brown-enterprises/be-code/internal/discover"
	"github.com/brown-enterprises/be-code/internal/verify"
)

// InitHint nudges a user working in a project with no notes file toward
// /init (see NeedsInitHint).
const InitHint = "no BECODE.md; /init maps this project"

// InitOptions drive one init run for any UI.
type InitOptions struct {
	Root    string
	Approve func(preview string) bool // asked once with the diff preview; nil = approved
	Log     func(line string)         // progress lines (dimmed in the TUI)
}

// RunInit scans the workspace, asks the model for BECODE.md prose, validates
// it, and — once the caller approves the diff — writes BECODE.md (backing up
// any previous copy to BECODE.md.bak) and refreshes the agent's system
// prompt with the new notes. Returns the path written.
func RunInit(ctx context.Context, ag *agent.Agent, opt InitOptions) (string, error) {
	log := opt.Log
	if log == nil {
		log = func(string) {}
	}
	log("mapping the workspace…")
	facts, err := discover.Scan(opt.Root)
	if err != nil {
		return "", err
	}
	log(fmt.Sprintf("measured %d files (%s); asking the model for the overview…", facts.Files, facts.Kind))
	doc, fallback, err := ag.InitProject(ctx, facts)
	if err != nil {
		return "", err
	}
	if fallback {
		log("the model's overview was rejected twice; writing the fact sheet instead")
	}
	path := filepath.Join(opt.Root, "BECODE.md")
	old, _ := os.ReadFile(path)
	if opt.Approve != nil && !opt.Approve(diff.Preview("BECODE.md", string(old), doc, false)) {
		return "", errors.New("BECODE.md write rejected")
	}
	if len(old) > 0 {
		if err := os.WriteFile(path+".bak", old, 0o644); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		return "", err
	}
	ag.SetProjectNotes(doc)
	if len(old) > 0 {
		log("wrote BECODE.md (previous copy in BECODE.md.bak)")
	} else {
		log("wrote BECODE.md")
	}
	return path, nil
}

// NeedsInitHint reports whether root looks like a project (verify.Detect
// recognizes it) but has no notes file (BECODE.md or CLAUDE.md) yet.
func NeedsInitHint(root string) bool {
	if verify.Detect(root).Kind == "none" {
		return false
	}
	for _, n := range []string{"BECODE.md", "becode.md", "CLAUDE.md"} {
		if _, err := os.Stat(filepath.Join(root, n)); err == nil {
			return false
		}
	}
	return true
}
