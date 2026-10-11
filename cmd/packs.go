package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/engine"
	"github.com/brown-enterprises/be-code/internal/packs"
	"github.com/brown-enterprises/be-code/internal/packsrc"
	"github.com/brown-enterprises/be-code/internal/ui"
)

var packsCmd = &cobra.Command{
	Use:   "packs",
	Short: "List the installed packs (see also /packs inside a session)",
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := os.Getwd()
		if err != nil {
			return err
		}
		if flagDir != "" {
			root = flagDir
		}
		dir, err := config.Dir()
		if err != nil {
			return err
		}
		found, warns := packs.Discover(filepath.Join(dir, "packs"), root)
		for _, w := range warns {
			fmt.Fprintf(os.Stderr, "warn: %s\n", w)
		}
		if len(found) == 0 {
			fmt.Printf("no packs found (install one with: be-code packs add <name>)\n")
			return nil
		}
		// The same precedence a session uses (§7a), so this listing and
		// /packs cannot disagree about which packs are wanted here: an
		// explicit choice, else the pack's marker, else its detect block.
		abs, aerr := filepath.Abs(root)
		if aerr != nil {
			abs = root
		}
		trustPath := filepath.Join(dir, "engine", engine.Key(abs), "packs.json")
		for _, p := range found {
			mark := " "
			if packs.Wanted(p, root, trustPath) {
				mark = "*"
			}
			_, est, budget := packs.CoreOverBudget(p)
			fmt.Printf("%s %-12s %-8s %-8s core ~%d", mark, p.Name, p.Version, p.Source, est)
			if budget > 0 {
				fmt.Printf("/%d", budget)
			}
			fmt.Printf(" tokens · %d chunks\n", len(p.ChunkNames()))
		}
		fmt.Println("(* wanted here: switched on, installed, or detected · /packs in a session shows the same)")
		return nil
	},
}

// packsValidateCmd is what a pack author runs in CI: everything the loader
// would refuse, and everything it would quietly drop, reported together and
// with a non-zero exit so a broken pack cannot merge.
var packsValidateCmd = &cobra.Command{
	Use:   "validate <dir>",
	Short: "Check a pack directory and report what the loader would refuse or drop",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		p, warns, err := packs.Load(args[0])
		for _, w := range warns {
			fmt.Fprintf(os.Stderr, "warn: %s\n", w)
		}
		if err != nil {
			return err
		}
		fmt.Printf("%s %s (%d chunks)\n", p.Name, p.Version, len(p.ChunkNames()))

		var problems []string
		if over, est, budget := packs.CoreOverBudget(p); over {
			problems = append(problems, fmt.Sprintf("core is ~%d tokens, over its budget of %d", est, budget))
		}
		// A check's program has to be runnable. A pack is installed by
		// copying a folder, and a copy does not always keep the executable
		// bit — which fails at the first build with a confusing error.
		for _, c := range p.Checks(".") {
			if len(c.Argv) == 0 {
				continue
			}
			prog := c.Argv[0]
			if !strings.HasPrefix(prog, p.Root) {
				continue // something on PATH; not ours to check
			}
			st, serr := os.Stat(prog)
			switch {
			case serr != nil:
				problems = append(problems, fmt.Sprintf("check %q: %s is missing", c.Name, prog))
			case st.Mode().Perm()&0o111 == 0:
				problems = append(problems, fmt.Sprintf("check %q: %s is not executable (chmod +x it)", c.Name, prog))
			}
		}
		// mcp_servers is parsed and nothing starts it, so a pack author who
		// declares one would otherwise wait for a server that never arrives.
		if servers := p.DeclaresUnstartedServers(); len(servers) > 0 {
			problems = append(problems, fmt.Sprintf("mcp_servers declares %s, which this version does not start",
				strings.Join(servers, ", ")))
		}
		// A chunk larger than the smallest tool-output cap is truncated on a
		// small window, which is where packs matter most.
		for _, n := range p.ChunkNames() {
			body, cerr := p.Chunk(n)
			if cerr != nil {
				problems = append(problems, fmt.Sprintf("chunk %q: %v", n, cerr))
				continue
			}
			if len(body) > minToolOutputCap {
				problems = append(problems, fmt.Sprintf("chunk %q is %d bytes; on a small context window the cap is %d and it would be truncated mid-chunk",
					n, len(body), minToolOutputCap))
			}
		}
		sort.Strings(problems)
		for _, pr := range problems {
			fmt.Fprintf(os.Stderr, "problem: %s\n", pr)
		}
		if len(problems) > 0 {
			return fmt.Errorf("%d problem(s)", len(problems))
		}
		fmt.Println("ok")
		return nil
	},
}

// minToolOutputCap is the floor the per-call tool output cap can reach
// (agent.applyReserve clamps it to [4 KiB, 24 KiB]). Validation warns against
// the floor rather than the ceiling: a chunk that only fits a large window is
// no use to the small model a pack exists to help.
const minToolOutputCap = 4 * 1024

var packsInfoCmd = &cobra.Command{
	Use:   "info <name>",
	Short: "Show one pack's rules, chunks and verify commands",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		root, err := os.Getwd()
		if err != nil {
			return err
		}
		if flagDir != "" {
			root = flagDir
		}
		dir, err := config.Dir()
		if err != nil {
			return err
		}
		found, _ := packs.Discover(filepath.Join(dir, "packs"), root)
		for _, p := range found {
			if p.Name != args[0] {
				continue
			}
			fmt.Printf("%s %s (%s)\n%s\n\n%s\n", p.Name, p.Version, p.Source, p.Root, p.Index())
			for _, c := range p.Checks(root) {
				fmt.Printf("verify %s (%s): %s\n", c.Name, c.Parser, c.Display())
			}
			if p.Core != "" {
				fmt.Printf("\ncore (~%d tokens):\n%s\n", len(p.Core)/3, p.Core)
			}
			return nil
		}
		return fmt.Errorf("no pack named %q; be-code packs lists what there is", args[0])
	},
}

var flagPackSource string

// packsApprover is the person's seam for an install from the CLI, and the only
// approver the install path reaches: headlessApprover is wired for `run` and
// `init`, neither of which installs.
//
// **-y does not answer it** (spec §8, §14), like the updater's `update`. An
// install reaches the network and then writes rules into the model's own
// system prompt *and records trust for them*, which is the one thing writing
// a pack directory by hand cannot do. Honouring -y here made an unattended
// install reach the trust file — including one the model runs itself through
// shell under AutoApproveShell, which is exactly the capability §8 withholds:
// a model that can install a pack can rewrite its own instructions.
func packsApprover(out io.Writer) func(string, string) bool {
	return func(action, detail string) bool {
		fmt.Fprintf(out, "%s\n", detail)
		if !stdinIsTTY() {
			fmt.Fprintln(out, "declined: installing a pack always asks a person, and -y does not answer it")
			return false
		}
		fmt.Fprint(out, "install it? [y/N] ")
		r := bufio.NewReader(os.Stdin)
		line, _ := r.ReadString('\n')
		return strings.EqualFold(strings.TrimSpace(line), "y")
	}
}

// packOptions is the workspace, the trust file and the approver, which every
// one of the three subcommands needs.
func packOptions(out io.Writer) (packsrc.Options, error) {
	// mustAbs, not filepath.Abs: every other command in this package uses it,
	// and it expands a leading ~ — which a quoted -C '~/proj' from a script
	// or a config file otherwise turns into a literal directory named "~",
	// silently installing into the wrong workspace and keying the trust file
	// on that path.
	root := mustAbs(flagDir)
	base, err := config.Dir()
	if err != nil {
		return packsrc.Options{}, err
	}
	return packsrc.Options{
		Workspace: root,
		TrustPath: filepath.Join(base, "engine", engine.Key(root), "packs.json"),
		Approve:   packsApprover(out),
	}, nil
}

var packsAddCmd = &cobra.Command{
	Use:   "add [name[@ref]]",
	Short: "Install a skill pack into this project",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		out := cmd.OutOrStdout()
		name, ref := "", ""
		if len(args) == 1 {
			name, ref, _ = strings.Cut(args[0], "@")
		}
		spec := flagPackSource
		if spec == "" {
			if cfg, err := config.Load(); err == nil && cfg.Packs.Source != "" {
				spec = cfg.Packs.Source
			}
		}
		s, err := packsrc.Resolve(spec, ref)
		if err != nil {
			return err
		}
		o, err := packOptions(out)
		if err != nil {
			return err
		}
		o.Force, _ = cmd.Flags().GetBool("force")
		if name == "" {
			avail, lerr := packsrc.List(cmd.Context(), s)
			if lerr != nil {
				return lerr
			}
			var names []string
			for _, a := range avail {
				fmt.Fprintf(out, "  %-12s %-8s %s\n", a.Name, a.Version, a.Description)
				names = append(names, a.Name)
			}
			return fmt.Errorf("name the pack to install: %s", strings.Join(names, ", "))
		}
		res, err := packsrc.Install(cmd.Context(), s, name, o)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "installed %s %s into %s\n", res.Pack, res.Version, res.Dir)
		if res.Commit != "" {
			fmt.Fprintf(out, "  from %s at %s\n", s.Spec, short(res.Commit))
		}
		fmt.Fprintf(out, "  commit it so the project carries its own pack setup\n")
		return nil
	},
}

var packsUpdateCmd = &cobra.Command{
	Use:   "update <name>",
	Short: "Re-fetch an installed pack at the ref it was installed from",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		out := cmd.OutOrStdout()
		o, err := packOptions(out)
		if err != nil {
			return err
		}
		res, err := packsrc.Update(cmd.Context(), args[0], o)
		if err != nil {
			return err
		}
		switch {
		case res.Previous == res.Commit:
			fmt.Fprintf(out, "%s is already at %s\n", res.Pack, short(res.Commit))
		default:
			fmt.Fprintf(out, "updated %s %s: %s → %s\n", res.Pack, res.Version, short(res.Previous), short(res.Commit))
		}
		return nil
	},
}

var packsRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Delete a pack from this project and forget its approval",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		out := cmd.OutOrStdout()
		o, err := packOptions(out)
		if err != nil {
			return err
		}
		if err := packsrc.Remove(args[0], o); err != nil {
			return err
		}
		fmt.Fprintf(out, "removed %s from %s%s\n", args[0], packsrc.PacksDir(o.Workspace),
			ui.RemoveHint(cmd.Context(), o.Workspace, args[0]))
		return nil
	},
}

func short(commit string) string {
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}
