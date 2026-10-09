package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/engine"

	"github.com/brown-enterprises/be-code/internal/packs"
	"github.com/brown-enterprises/be-code/internal/playbook"
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
// PackRow is one pack as /packs reports it.
type PackRow struct {
	Name, Version string
	Source        string // "global" or "project"
	Active        bool
	Why           string // why it is or is not active, in a few words
	CoreTokens    int
	CoreBudget    int // 0 when the pack declares none
}

// PackRows is what /packs lists: every pack found on the search path, whether
// it is active, and why.
func (a *Agent) PackRows() []PackRow { return a.packRows }

// PackCommands are the slash commands the active packs offer, keyed by the
// name a person types. A pack ships them as prompt templates; the harness
// already loads exactly that shape from ~/.be-code/commands, so a pack's go
// into the same map.
//
// A person's own command of the same name wins — see the merge at the call
// sites. A pack arrives with somebody's repository or from a dotdir install,
// and neither should be able to redefine a command somebody wrote themselves.
func (a *Agent) PackCommands() map[string]string {
	out := map[string]string{}
	for _, src := range a.Tools.Packs() {
		p, ok := src.(*packs.Pack)
		if !ok {
			continue
		}
		for name, tmpl := range p.Commands() {
			if _, taken := out[name]; taken {
				continue // first active pack wins, in manifest order
			}
			out[name] = tmpl
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// LoadPacks discovers the packs on the search path and activates the ones
// that apply here: detected, or switched on by hand, minus any switched off.
// It is the one entry point — startup and /packs reload both call it — so the
// rules about what is active live in one place.
//
// It rewrites the system prompt, so it belongs between requests. /packs says
// as much: the next request pays a full cold read of the prompt cache.
func (a *Agent) LoadPacks() {
	dir, err := config.Dir()
	if err != nil {
		return
	}
	found, warns := packs.Discover(filepath.Join(dir, "packs"), a.Tools.Root)
	for _, w := range warns {
		a.QueueNotice(w)
		if a.Events.OnNotice != nil {
			a.Events.OnNotice(w)
		}
	}
	statePath, _ := a.packTrustPath()

	var candidates []*packs.Pack
	rows := make([]PackRow, 0, len(found))
	for _, p := range found {
		row := PackRow{Name: p.Name, Version: p.Version, Source: string(p.Source), CoreBudget: p.CoreBudget}
		_, row.CoreTokens, _ = packs.CoreOverBudget(p)
		on, set := false, false
		if statePath != "" {
			on, set = packs.Forced(statePath, p.Name)
		}
		switch {
		case set && !on:
			row.Why = "switched off"
		case set && on:
			row.Why = "switched on"
			candidates = append(candidates, p)
		default:
			// The marker is the project's declaration that it wants this
			// pack (design §7a). It sits above detection and below an
			// explicit /packs on|off, so a pack installed into a project
			// stays wanted even when the project is moved or renamed —
			// ~/.be-code/engine/<key>/ keys on a hash of the absolute
			// workspace path, so a move orphans the per-workspace entry and
			// a pack with no detect block would otherwise go silently
			// inactive while sitting in .becode/packs.
			//
			// Wanting is not trust: an untrusted pack still raises
			// pack_trust below, so a marker can move a pack from inactive to
			// asks-you and never to active.
			if m, ok := packs.ReadMarker(p.Root); ok && m.DeclaresWanted(p.Name) {
				row.Why = "installed in this project"
				candidates = append(candidates, p)
				break
			}
			matched, truncated := p.MatchesDetail(a.Tools.Root)
			switch {
			case matched:
				row.Why = "detected"
				candidates = append(candidates, p)
			case truncated:
				// Not the same as "no": detection stopped before it had seen
				// the whole tree, so saying it does not match would be a
				// statement of fact that may well be false.
				row.Why = "gave up looking on a large workspace; /packs on " + p.Name + " switches it on"
			default:
				row.Why = "does not match this workspace"
			}
		}
		rows = append(rows, row)
	}

	skipped := a.EnablePacks(candidates)
	for i := range rows {
		if rows[i].Why == "switched off" || rows[i].Why == "does not match this workspace" {
			continue
		}
		if reason, ok := skipped[rows[i].Name]; ok {
			rows[i].Why = reason
			continue
		}
		rows[i].Active = true
	}
	a.packRows = rows
}

// StartPacks asks the question LoadPacks could not. A project pack needs
// consent, LoadPacks runs inside buildAgent where Registry.Approve is still
// nil, and a UI assigns its approver only once buildAgent has returned — so
// without this the gate was unreachable in every mode and a pack that came
// with a repository could never be switched on at all.
//
// Every UI calls it once, after wiring Approve, alongside ResolveModel and
// StartSubAgents, which exist for the same reason and are documented as such
// in cmd/root.go. It is a no-op when nothing is pending, so a session with no
// project pack pays nothing and asks nothing. A caller that must go on to
// start a served program wants StartPacksAsync.
func (a *Agent) StartPacks() {
	if !a.packPending {
		return
	}
	if a.Tools.Approve == nil {
		// Headless without -y: there genuinely is nobody, and now we know it
		// rather than assuming it during buildAgent.
		a.packPending = false
		a.noteNoApprover()
		return
	}
	// LoadPacks re-runs discovery and the gate, which is what raises the
	// question. This is still startup — before the first request — so
	// rewriting the system prompt here costs no cache read.
	a.LoadPacks()
}

// StartPacksAsync is StartPacks on a goroutine of its own, for a caller whose
// own goroutine must go on to start a served program. The trust prompt can
// block for as long as a person takes to answer, and a TUI that waited for it
// here would never start the program the question has to be shown in —
// Session.Ask stores it and a view that starts afterwards is shown it.
func (a *Agent) StartPacksAsync() {
	if !a.packPending {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				a.notice("pack activation failed: %v", r)
			}
		}()
		a.StartPacks()
	}()
}

// noteNoApprover says the one true thing about a run that will never have
// somebody to ask: a headless session without -y.
func (a *Agent) noteNoApprover() {
	for _, r := range a.packRows {
		if r.Source == string(packs.SourceProject) && !r.Active {
			msg := fmt.Sprintf("pack %q came with this project and needs approval, and this run has nobody to ask. Run it interactively once to decide, or copy the pack to ~/.be-code/packs.", r.Name)
			a.QueueNotice(msg)
			if a.Events.OnNotice != nil {
				a.Events.OnNotice(msg)
			}
		}
	}
}

// ForcePack switches a pack on or off for this workspace and re-activates,
// or returns it to detection when clear is set. The caller is between
// requests: this rewrites the system prompt.
func (a *Agent) ForcePack(name string, on, clear bool) error {
	path, err := a.packTrustPath()
	if err != nil {
		return err
	}
	known := false
	for _, r := range a.packRows {
		if r.Name == name {
			known = true
		}
	}
	if !known {
		return fmt.Errorf("no pack named %q was found; /packs lists what there is", name)
	}
	if clear {
		err = packs.ClearForced(path, name)
	} else {
		err = packs.SetForced(path, name, on)
	}
	if err != nil {
		return err
	}
	a.LoadPacks()
	return nil
}

// EnablePacks activates exactly the packs given, and reports the ones it
// would not, by name, with the reason. LoadPacks is the usual caller.
func (a *Agent) EnablePacks(ps []*packs.Pack) map[string]string {
	skipped := map[string]string{}
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
		active  []tools.PackSource
		blocks  []string
		checks  []packCheck
		entries []packEntry
	)
	for _, p := range ps {
		if p.Source == packs.SourceProject && !a.trustProjectPack(p, refuse) {
			skipped[p.Name] = "needs approval"
			continue
		}
		if over, est, budget := packs.CoreOverBudget(p); over {
			refuse("pack %q is not active: its core is about %d tokens against a budget of %d. Shorten core.md or raise core.budget_tokens.", p.Name, est, budget)
			skipped[p.Name] = fmt.Sprintf("core is ~%d tokens, over its %d budget", est, budget)
			continue
		}
		active = append(active, p)
		blocks = append(blocks, packBlock(p))
		for _, c := range p.Checks(a.Tools.Root) {
			checks = append(checks, packCheck{check: c, changed: p.Changed(c.Name)})
		}
		if pb := p.Playbook(); len(pb) > 0 {
			entries = append(entries, packEntry{pack: p.Name, entries: pb})
		}
	}

	notes := ""
	if len(blocks) > 0 {
		notes = packFrame + "\n\n" + strings.Join(blocks, "\n")
	}
	// Under packMu, and released before SetPacks/RefreshSystem take their own
	// locks: a prefill goroutine reads these while /packs writes them.
	a.packMu.Lock()
	a.packNotes, a.packChecks, a.packEntries = notes, checks, entries
	a.packMu.Unlock()
	// SetPacks registers the pack tool, so the prompt has to be rebuilt after
	// it: the tool list and the embedded-call parser's known names both come
	// from the registry.
	a.Tools.SetPacks(active)
	a.RefreshSystem()
	return skipped
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
		// Not a refusal: LoadPacks runs inside buildAgent, before any UI has
		// assigned Registry.Approve, so "nobody to ask" is almost always
		// "nobody to ask *yet*". Saying so here told a person running
		// interactively to run interactively, and left the pack off for good.
		// StartPacks asks once the approver exists; only a run that never
		// gets one — headless without -y — stays unapproved, and
		// noteNoApprover below is what tells that person.
		a.packPending = true
		return false
	}
	// Approve, not ApproveCtx: this runs at startup or from /packs, neither
	// of which is inside a turn, so there is no deadline to carry and no
	// fired-turn mark to set. If activation ever becomes reachable from a
	// turn, this has to become ApproveCtx with the turn's context — a
	// question with no deadline would hang a scheduled event.
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

// VerifyProjectNow is what an explicitly requested verification covers: the
// detected project plus every active pack's checks, unfiltered. A person who
// typed /verify asked for all of it, and the changed-file list is not the
// question they asked.
//
// It exists because /verify in both UIs and be-code verify called
// verify.Detect directly, so an Unreal workspace with the ue5 pack active was
// told "no verification checks apply (unrecognized project type)" while the
// automatic repair loop was running a full UnrealBuildTool build on the same
// files. Those call sites also passed no gate, so a denied check ran and
// shell_after_web never applied — VerifyNow routes through runChecks, which
// is where both of those live.
func (a *Agent) VerifyProjectNow() verify.Project { return a.verifyProject() }

// VerifyNow runs VerifyProjectNow through the same gate the automatic loop
// uses. A nil report means the verification was declined, not that it passed.
func (a *Agent) VerifyNow(ctx context.Context) *verify.Report {
	return a.runChecks(ctx, a.verifyProject())
}

// packCheck is one pack's verify command plus the when.changed globs it
// declared. The globs are kept beside the check rather than inside
// verify.Check because internal/verify must not import internal/packs — the
// dependency runs the other way — and because the decision needs the changed
// file list, which only the agent has.
type packCheck struct {
	check   verify.Check
	changed []string // when.changed globs; empty means unconditional
}

// verifyProject is what this session verifies, for a request whose changed
// files are not known. It runs every pack check, which is the safe answer:
// see verifyProjectFor.
func (a *Agent) verifyProject() verify.Project {
	return a.verifyProjectFor(nil)
}

// verifyProjectFor is what this session verifies given the files this request
// changed: the project the harness detects, with the active packs' checks
// appended in manifest order (packs spec §8.1), so the built-in checks still
// run first and still stop the sequence at the first failure.
//
// A pack check that declares when.changed is skipped when nothing it watches
// changed. Both shipped packs declare it precisely because their checks are
// builds: without this, editing a README ran a full UnrealBuildTool build on
// a 30-minute timeout, or all three of the ui-web node checks, on every turn
// that touched any file at all.
//
// `changed` being nil means "not known" rather than "nothing changed", and
// then every check runs. Skipping verification because the harness could not
// work out what moved would be a silent loss of the whole safety net, which
// is worse than a slow build; the project's own detected checks are never
// filtered either way, so a pack can never suppress them.
func (a *Agent) verifyProjectFor(changed []string) verify.Project {
	proj := verify.Detect(a.Tools.Root)
	a.packMu.RLock()
	packChecks := a.packChecks
	a.packMu.RUnlock()
	if len(packChecks) == 0 {
		return proj
	}
	checks := append([]verify.Check(nil), proj.Checks...)
	added := 0
	for _, pc := range packChecks {
		if len(changed) > 0 && len(pc.changed) > 0 && !packs.MatchesAnyGlob(pc.changed, changed) {
			continue
		}
		checks = append(checks, pc.check)
		added++
	}
	proj.Checks = checks
	if added > 0 && (proj.Kind == "none" || proj.Kind == "") {
		// A pack's checks are real verification even where nothing was
		// detected: an Unreal project is not a project type the harness knows.
		proj.Kind = "pack"
	}
	return proj
}

// playbookNote is what a pack adds to a repair turn: the cause of this
// failure and what to do about it, from the matching playbook entry (packs
// spec §11.2).
//
// It also carries the escalation the playbook exists for. A weak model's
// instinct, told the same compiler error twice, is to make the same edit
// again — so a signature seen before does not get the fix repeated:
//
//	first time   the entry's fix
//	seen again   the entry's next step, because the fix did not work
//	after that   the entry named as already tried, which is the state the
//	             co-worker consultation below already handles
//
// The count is per request, reset where verification starts, so a failure
// returning in a later request is new again — the file may have changed under
// it. Returns "" when no pack is active, nothing parsed the output, or no
// entry matches.
func (a *Agent) playbookNote(rep *verify.Report) string {
	a.packMu.RLock()
	packEntries := a.packEntries
	a.packMu.RUnlock()
	if rep == nil || len(packEntries) == 0 {
		return ""
	}
	_, diag := rep.FirstFailure()
	if diag == nil || diag.First == nil {
		return ""
	}
	d := diag.First
	var hit *playbook.Entry
	var packName string
	for _, pe := range packEntries {
		if e := playbook.Match(pe.entries, d.Kind, d.Raw, d.Message); e != nil {
			hit, packName = e, pe.pack
			break
		}
	}
	if hit == nil {
		return ""
	}
	if a.verifySigs == nil {
		a.verifySigs = map[string]int{}
	}
	a.verifySigs[diag.Signature]++
	seen := a.verifySigs[diag.Signature]

	var b strings.Builder
	fmt.Fprintf(&b, "playbook %s › %s\n", packName, hit.Title)
	if hit.Cause != "" {
		fmt.Fprintf(&b, "cause: %s\n", hit.Cause)
	}
	switch {
	case seen == 1:
		if hit.Fix != "" {
			fmt.Fprintf(&b, "fix:\n%s\n", hit.Fix)
		}
	case seen == 2:
		b.WriteString("This is the same failure as before, so the previous change did not fix it. Do not repeat it.\n")
		if hit.Next != "" {
			fmt.Fprintf(&b, "next:\n%s\n", hit.Next)
		}
	default:
		// No plea not to repeat: a prose instruction of that kind has no
		// measurable effect on whether a model repeats a failed action
		// (arXiv 2608.23651). What changes behaviour is a different context,
		// which is what the auto:verify consultation after this provides. So
		// this rung states the situation and stops offering advice that has
		// already not worked.
		fmt.Fprintf(&b, "This failure has not moved in %d attempts, and this entry's fix and next step have both been tried.\n", seen)
	}
	return b.String()
}

// packEntry pairs a pack's playbook with its name, so a repair turn can say
// which pack the advice came from.
type packEntry struct {
	pack    string
	entries []playbook.Entry
}
