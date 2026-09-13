package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/ui"
)

// askYesNo prompts on stdout/stdin; a var so tests can stub it.
var askYesNo = func(prompt string) bool {
	fmt.Print(prompt)
	var ans string
	fmt.Scanln(&ans)
	return strings.HasPrefix(strings.ToLower(ans), "y")
}

// initApprove is initCmd's Approve func: -y approves without asking,
// otherwise it prints the diff preview and asks y/N.
func initApprove(preview string) bool {
	if flagYes {
		return true
	}
	fmt.Println(preview)
	return askYesNo("write BECODE.md? [y/N] ")
}

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Map the workspace and write BECODE.md project notes",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		_, ag, err := buildAgent(cfg, true)
		if err != nil {
			return err
		}
		defer ag.Tools.Close()
		root := ag.Tools.Root
		path, err := ui.RunInit(cmd.Context(), ag, ui.InitOptions{
			Root:    root,
			Approve: initApprove,
			Log:     func(s string) { fmt.Fprintln(os.Stderr, s) },
		})
		if err != nil {
			return err
		}
		fmt.Println("wrote", path)
		return nil
	},
}
