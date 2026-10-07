package agent

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/engine"

	"github.com/brown-enterprises/be-code/internal/packs"
	"github.com/brown-enterprises/be-code/internal/tools"
	"github.com/brown-enterprises/be-code/internal/verify"
)

// packFrame introduces a pack's rules. The wording matters: BECODE.md is
// introduced as facts about the repository that are explicitly not
// instructions, because a session must not execute its own notes. A pack is
// the opposite — the person installed it so that the model would follow it —
// and saying which is which is what keeps the first guarantee intact.
const packFrame = "Operating rules from packs the user installed for this technology. Unlike the project notes above, these are instructions: follow them. Fetch a listed chunk with the pack tool when you need its detail."

// EnablePacks activates packs for this session: their rules go into the
// system prompt, their chunks and templates become reachable through the pack
// tool, and their verify commands run after the project's own.
//
// A pack may be refused here for two reasons, each said once. Core past its
// budget_tokens does not load at all, because the whole point of the cap is
// that a pack cannot quietly eat a small model's window — half a pack's rules
// would be worse than none. And a pack from the project is held back until
// there is a trust gate to approve it: it arrived with somebody's repository,
// it can name commands to run, and a repository is not the person.
//
// Call it once, before a UI starts taking turns. A later change to the active
// set rewrites the system prompt, which costs a full cold read of the prompt
// cache, so it belongs at a request boundary rather than mid-run.
func (a *Agent) EnablePacks(ps []*packs.Pack) {
	// Queued, not noticed: this runs while the agent is being built, before a
	// UI has wired OnNotice, and a.notice would drop the message there. A
	// queued one is delivered as soon as there is somewhere to put it, and
	// under a TUI the transcript is the only place it can be read at all.
	refuse := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		a.QueueNotice(msg)
		if a.Events.OnNotice != nil {
			a.Events.OnNotice(msg)
		}
	}
	var (
		active []tools.PackSource
		blocks []string
		checks []verify.Check
	)
	for _, p := range ps {
		if p.Source == packs.SourceProject && !a.trustProjectPack(p, refuse) {
			continue
		}
		if over, est, budget := packs.CoreOverBudget(p); over {
			refuse("pack %q is not active: its core is about %d tokens against a budget of %d. Shorten core.md or raise core.budget_tokens.", p.Name, est, budget)
			continue
		}
		active = append(active, p)
		blocks = append(blocks, packBlock(p))
		checks = append(checks, p.Checks(a.Tools.Root)...)
	}

	a.packNotes = ""
	if len(blocks) > 0 {
		a.packNotes = packFrame + "\n\n" + strings.Join(blocks, "\n")
	}
	a.packChecks = checks
	// SetPacks registers the pack tool, so the prompt has to be rebuilt after
	// it: the tool list and the embedded-call parser's known names both come
	// from the registry.
	a.Tools.SetPacks(active)
	a.RefreshSystem()
}

// trustProjectPack decides whether a pack that came with this repository may
// run (packs spec §10).
//
// A global pack needs no decision: the person put it in their own dotdir. A
// project pack arrived with somebody's repository, it adds rules to every
// request and it can name commands to run, so it is asked about once —
// naming every one of those commands — and the answer is remembered per
// workspace against the hash of what was approved. Change what the pack
// runs and it asks again; improve its prose and it does not, because a gate
// that interrupts for paragraphs teaches people to approve without reading.
//
// Nobody to ask (headless without -y) is a no. An unapproved project pack is
// disabled and said so, never trusted by default.
func (a *Agent) trustProjectPack(p *packs.Pack, refuse func(string, ...any)) bool {
	hash, err := packs.ContentHash(p.Root)
	if err != nil {
		refuse("pack %q from this project is not active: its contents could not be read to approve (%v)", p.Name, err)
		return false
	}
	path, err := a.packTrustPath()
	if err == nil && packs.TrustedHash(path, p.Name) == hash {
		return true // the same pack, already approved for this workspace
	}
	if a.Tools.Approve == nil {
		refuse("pack %q from this project is not active: it needs approval and there is nobody to ask. Run interactively, or copy the pack to ~/.be-code/packs.", p.Name)
		return false
	}
	if !a.Tools.Approve("pack_trust", packs.TrustDetail(p, a.Tools.Root)) {
		refuse("pack %q from this project is not active.", p.Name)
		return false
	}
	if err != nil {
		// Approved, but with no dotdir to write to it will ask again next
		// session. Better that than refusing what the person just allowed.
		refuse("pack %q is active, but the approval could not be saved (%v), so it will ask again", p.Name, err)
		return true
	}
	if err := packs.Trust(path, p.Name, hash); err != nil {
		refuse("pack %q is active, but the approval could not be saved (%v), so it will ask again", p.Name, err)
	}
	return true
}

// packTrustPath is ~/.be-code/engine/<key>/packs.json: the pack approvals for
// this workspace, beside the working-memory store and the online decisions
// (packs spec §5.5).
func (a *Agent) packTrustPath() (string, error) {
	base, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "engine", engine.Key(a.Tools.Root), "packs.json"), nil
}

// packBlock is one pack's contribution to the system prompt: its rules, then
// the index of what else it can be asked for.
func packBlock(p *packs.Pack) string {
	var b strings.Builder
	b.WriteString(p.Index())
	if p.Core != "" {
		b.WriteString("\n")
		b.WriteString(p.Core)
		b.WriteString("\n")
	}
	return b.String()
}

// verifyProject is what this session verifies: the project the harness
// detects, with the active packs' checks appended in manifest order (packs
// spec §8.1), so the built-in checks still run first and still stop the
// sequence at the first failure.
func (a *Agent) verifyProject() verify.Project {
	proj := verify.Detect(a.Tools.Root)
	if len(a.packChecks) == 0 {
		return proj
	}
	proj.Checks = append(append([]verify.Check(nil), proj.Checks...), a.packChecks...)
	if proj.Kind == "none" || proj.Kind == "" {
		// A pack's checks are real verification even where nothing was
		// detected: an Unreal project is not a project type the harness knows.
		proj.Kind = "pack"
	}
	return proj
}
