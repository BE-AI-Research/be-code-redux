package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/packs"
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
			fmt.Printf("no packs found (install one in %s)\n", filepath.Join(dir, "packs", "<name>"))
			return nil
		}
		for _, p := range found {
			mark := " "
			if p.Matches(root) {
				mark = "*"
			}
			_, est, budget := packs.CoreOverBudget(p)
			fmt.Printf("%s %-12s %-8s %-8s core ~%d", mark, p.Name, p.Version, p.Source, est)
			if budget > 0 {
				fmt.Printf("/%d", budget)
			}
			fmt.Printf(" tokens · %d chunks\n", len(p.ChunkNames()))
		}
		fmt.Println("(* matches this workspace · a session's own on/off state is shown by /packs)")
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
