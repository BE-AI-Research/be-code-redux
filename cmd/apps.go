package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/brown-enterprises/be-code/internal/api"
)

func appsRegistry() (*api.Registry, error) {
	dir, err := api.Dir()
	if err != nil {
		return nil, err
	}
	return api.NewRegistry(filepath.Join(dir, "apps")), nil
}

var appsCmd = &cobra.Command{
	Use:   "apps",
	Short: "Register the programs that may call this machine's application API",
	Long: `Each app has a key, printed once at add time, and a profile saying what it may ask for.
The API is served by an interactive session with api.enabled on (or /api on) over
~/.be-code/api/api.sock. See the README's "Application API" section.`,
}

var appsAddCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Register an app and print its key (once)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		reg, err := appsRegistry()
		if err != nil {
			return err
		}
		p, err := profileFromFlags(cmd)
		if err != nil {
			return err
		}
		key, err := reg.Add(args[0], p)
		if err != nil {
			return err
		}
		// The key exists only here: print it even if reading the file back fails.
		shown := p
		if a, err := reg.Get(args[0]); err == nil {
			shown = a.Profile
		}
		fmt.Printf("registered app %s\n%s\nkey (shown once; store it with the app):\n  %s\n", args[0], describeProfile(shown), key)
		return nil
	},
}

func profileFromFlags(cmd *cobra.Command) (api.Profile, error) {
	var p api.Profile
	if file, _ := cmd.Flags().GetString("profile"); file != "" {
		raw, err := os.ReadFile(file)
		if err != nil {
			return p, err
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return p, fmt.Errorf("%s: %w", file, err)
		}
		return p, nil
	}
	split := func(s string) []string {
		if strings.TrimSpace(s) == "" {
			return nil
		}
		var out []string
		for _, x := range strings.Split(s, ",") {
			if x = strings.TrimSpace(x); x != "" {
				out = append(out, x)
			}
		}
		return out
	}
	tiers, _ := cmd.Flags().GetString("tiers")
	models, _ := cmd.Flags().GetString("models")
	packs, _ := cmd.Flags().GetString("packs")
	p.Tiers, p.Models, p.Packs = split(tiers), split(models), split(packs)
	p.Sessions, _ = cmd.Flags().GetString("sessions")
	p.Budget.PerHour, _ = cmd.Flags().GetInt("per-hour")
	p.Budget.Tokens, _ = cmd.Flags().GetInt("max-tokens")
	return p, nil
}

func describeProfile(p api.Profile) string {
	tiers := strings.Join(p.Tiers, ", ")
	if tiers == "" {
		tiers = "(none: health only)"
	}
	models := strings.Join(p.Models, ", ")
	if models == "" {
		models = "default"
	}
	packs := strings.Join(p.Packs, ", ")
	if packs == "" {
		packs = "(none)"
	}
	return fmt.Sprintf("  tiers: %s\n  models: %s\n  packs: %s\n  sessions: %s\n  budget: %d tokens per reply, %d per hour",
		tiers, models, packs, p.Sessions, p.Budget.Tokens, p.Budget.PerHour)
}

var appsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List registered apps",
	RunE: func(cmd *cobra.Command, args []string) error {
		reg, err := appsRegistry()
		if err != nil {
			return err
		}
		apps, err := reg.List()
		if err != nil {
			return err
		}
		if len(apps) == 0 {
			fmt.Println("no apps registered; `be-code apps add <name>` registers one")
			return nil
		}
		fmt.Printf("%-20s %-20s %-8s %-10s %s\n", "NAME", "TIERS", "SESSIONS", "CREATED", "STATE")
		for _, a := range apps {
			state := "active"
			if a.Revoked != nil {
				state = "revoked " + a.Revoked.Format("2006-01-02")
			}
			fmt.Printf("%-20s %-20s %-8s %-10s %s\n", a.Name, strings.Join(a.Profile.Tiers, ","), a.Profile.Sessions, a.Created.Format("2006-01-02"), state)
		}
		return nil
	},
}

var appsShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Show an app's profile (never its key or hash)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		reg, err := appsRegistry()
		if err != nil {
			return err
		}
		a, err := reg.Get(args[0])
		if err != nil {
			return err
		}
		fmt.Printf("%s (created %s)\n%s\n", a.Name, a.Created.Format(time.RFC3339), describeProfile(a.Profile))
		if a.Revoked != nil {
			fmt.Printf("  revoked %s\n", a.Revoked.Format(time.RFC3339))
		}
		return nil
	},
}

var appsRotateCmd = &cobra.Command{
	Use:   "rotate <name>",
	Short: "Mint a new key; the old one stops at once",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		reg, err := appsRegistry()
		if err != nil {
			return err
		}
		key, err := reg.Rotate(args[0])
		if err != nil {
			return err
		}
		fmt.Printf("new key for %s (shown once):\n  %s\n", args[0], key)
		return nil
	},
}

var appsRevokeCmd = &cobra.Command{
	Use:   "revoke <name>",
	Short: "Stop an app's key without deleting its file",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		reg, err := appsRegistry()
		if err != nil {
			return err
		}
		if err := reg.Revoke(args[0]); err != nil {
			return err
		}
		fmt.Printf("revoked %s; `be-code apps rotate %s` issues a new key, `apps remove` deletes it\n", args[0], args[0])
		return nil
	},
}

var appsRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Delete an app",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		reg, err := appsRegistry()
		if err != nil {
			return err
		}
		if err := reg.Remove(args[0]); err != nil {
			return err
		}
		fmt.Printf("removed %s\n", args[0])
		return nil
	},
}

func init() {
	f := appsAddCmd.Flags()
	f.String("profile", "", "read the profile from a JSON file instead of the flags")
	f.String("tiers", "query", "comma-separated: query, completions")
	f.String("models", "", "comma-separated model names the app may ask for (default is always allowed)")
	f.String("packs", "", "comma-separated packs whose rules a query may include")
	f.String("sessions", "none", "none or named")
	f.Int("per-hour", api.DefaultPerHour, "requests allowed per sliding hour (0 = unbounded)")
	f.Int("max-tokens", api.DefaultTokens, "most tokens one reply may be asked for (0 = unbounded)")
	appsCmd.AddCommand(appsAddCmd, appsListCmd, appsShowCmd, appsRotateCmd, appsRevokeCmd, appsRemoveCmd)
}
