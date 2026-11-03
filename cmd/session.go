package cmd

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/brown-enterprises/be-code/internal/live"
	"github.com/brown-enterprises/be-code/internal/procattr"
	"github.com/brown-enterprises/be-code/internal/store"
)

var sessionCmd = &cobra.Command{
	Use:   "session",
	Short: "A saved session's application-API token",
}

// refuseIfLive refuses when any of codes is the code of a live host. It runs
// before the store is read: a live session may have no file yet.
func refuseIfLive(codes ...string) error {
	dir, err := live.Dir()
	if err != nil {
		return nil
	}
	recs, _ := live.List(dir)
	for _, r := range recs {
		for _, c := range codes {
			if c != "" && r.Code == c {
				return fmt.Errorf("%s is live; join it (be-code attach %s) and use /session token there", r.Code, r.Code)
			}
		}
	}
	return nil
}

// loadForToken loads a session file that no running process owns. The live
// registry is asked first, by code and by the code of a session id, and the
// loaded session's own code is checked again once it is read; then its
// host_pid, which names a non-hosted process the registry never sees.
func loadForToken(arg string) (*store.Session, error) {
	if err := refuseIfLive(arg, store.CodeFor(arg)); err != nil {
		return nil, err
	}
	s, err := store.Load(arg)
	if err != nil {
		return nil, err
	}
	if err := refuseIfLive(s.ResumeCode()); err != nil {
		return nil, err
	}
	// A plain-REPL or --no-host session is in no live registry, yet it may
	// be open and serving the API; its next autosave would write its own
	// token back over this one. Its save stamp names it.
	if pid := s.HostPID; pid != 0 && pid != os.Getpid() && procattr.Alive(pid) {
		return nil, fmt.Errorf("%s is open in a running session (pid %d); join it and use /session token there", s.ResumeCode(), pid)
	}
	return s, nil
}

// secretAllowed refuses to print a secret where a person is not looking: a
// pipe is fine only when the caller said so with --stdout. It runs before
// anything is minted or saved, so a refusal changes nothing.
func secretAllowed(cmd *cobra.Command) error {
	force, _ := cmd.Flags().GetBool("stdout")
	if !force && !stdoutIsTTY() {
		return errors.New("stdout is not a terminal; pass --stdout to print the token anyway")
	}
	return nil
}

// printSecret prints a token only where secretAllowed permits it.
func printSecret(cmd *cobra.Command, secret string) error {
	if err := secretAllowed(cmd); err != nil {
		return err
	}
	fmt.Println(secret)
	return nil
}

var sessionTokenCmd = &cobra.Command{
	Use:   "token <code>",
	Short: "Print the session's API token, minting one if it has none",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := secretAllowed(cmd); err != nil {
			return err
		}
		s, err := loadForToken(args[0])
		if err != nil {
			return err
		}
		had := s.Token != ""
		tok := s.AccessToken()
		if !had {
			if err := s.Save(); err != nil {
				return err
			}
		}
		return printSecret(cmd, tok)
	},
}

var sessionRotateCmd = &cobra.Command{
	Use:   "rotate <code>",
	Short: "Replace the session's API token; --grace keeps the old one working for a while",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := secretAllowed(cmd); err != nil {
			return err
		}
		s, err := loadForToken(args[0])
		if err != nil {
			return err
		}
		var grace time.Duration
		if graceS, _ := cmd.Flags().GetString("grace"); graceS != "" {
			if grace, err = time.ParseDuration(graceS); err != nil {
				return fmt.Errorf("--grace: %w", err)
			}
		}
		if err := s.Rotate(grace); err != nil {
			return err
		}
		if err := s.Save(); err != nil {
			return err
		}
		return printSecret(cmd, s.Token)
	},
}

func init() {
	sessionTokenCmd.Flags().Bool("stdout", false, "print the token even when stdout is not a terminal")
	sessionRotateCmd.Flags().Bool("stdout", false, "print the token even when stdout is not a terminal")
	sessionRotateCmd.Flags().String("grace", "", "how long the old token keeps working, e.g. 10m (default: it stops at once; at most 24h)")
	sessionCmd.AddCommand(sessionTokenCmd, sessionRotateCmd)
}
