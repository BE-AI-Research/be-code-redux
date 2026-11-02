package cmd

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/brown-enterprises/be-code/internal/live"
	"github.com/brown-enterprises/be-code/internal/store"
)

var sessionCmd = &cobra.Command{
	Use:   "session",
	Short: "A saved session's application-API token",
}

// loadForToken loads a session file that no running host owns. A live
// session's file belongs to its host: join it and use /session token.
func loadForToken(code string) (*store.Session, error) {
	s, err := store.Load(code)
	if err != nil {
		return nil, err
	}
	if dir, derr := live.Dir(); derr == nil {
		recs, _ := live.List(dir)
		for _, r := range recs {
			if r.Code == s.ResumeCode() {
				return nil, fmt.Errorf("%s is live; join it (be-code attach %s) and use /session token there", r.Code, r.Code)
			}
		}
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
