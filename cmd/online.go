package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/ui"
)

var onlineCmd = &cobra.Command{
	Use:   "online",
	Short: "Add or revoke an online provider's stored credential",
	Long: `be-code online add [name] adds an online provider (a preset or a custom
OpenAI-compatible endpoint) with a pasted key or, for OpenRouter, a browser sign-in,
storing the key in ~/.be-code/credentials.json (mode 0600; an environment variable
named by api_key_env always wins). be-code online revoke <name> deletes the stored
credential. Inside a session the same flow is /online add.`,
}

var onlineAddCmd = &cobra.Command{
	Use:   "add [name]",
	Short: "Add an online provider (asks on the terminal; never a --key flag)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// A real isatty check: /dev/null is a character device, so the
		// ModeCharDevice test stdinIsTTY uses would let a redirected stdin
		// through to a prompt nobody can answer.
		if !term.IsTerminal(os.Stdin.Fd()) {
			return errors.New("be-code online add needs a terminal (it asks for the key; there is no --key flag)")
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		name := ""
		if len(args) == 1 {
			name = args[0]
		}
		p := newStdioPrompter(os.Stdin, cmd.OutOrStdout())
		_, _, err = ui.OnlineAdd(cmd.Context(), cfg, p, name)
		return err
	},
}

var onlineRevokeCmd = &cobra.Command{
	Use:   "revoke <name>",
	Short: "Delete the stored credential for a provider",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), ui.RevokeCredential(cfg, args[0]))
		return nil
	},
}

// stdioPrompter asks on the process's own stdio for the standalone
// command, which runs without any readline. Lines are read one per
// request on a goroutine so that Ask honours its context: OnlineAdd
// withdraws its "paste the redirect URL" question when the browser
// sign-in finishes, and the read left parked then answers the next Ask
// instead of being lost or blocking the flow.
type stdioPrompter struct {
	in      *os.File
	out     io.Writer
	req     chan struct{}
	lines   chan lineResult
	pending bool
}

type lineResult struct {
	line string
	ok   bool
}

func newStdioPrompter(in *os.File, out io.Writer) *stdioPrompter {
	p := &stdioPrompter{in: in, out: out, req: make(chan struct{}), lines: make(chan lineResult)}
	r := bufio.NewReader(in)
	go func() {
		for range p.req {
			line, err := r.ReadString('\n')
			p.lines <- lineResult{strings.TrimRight(line, "\r\n"), err == nil || line != ""}
		}
	}()
	return p
}

func (p *stdioPrompter) Ask(ctx context.Context, q string) (string, bool) {
	fmt.Fprint(p.out, q)
	if !p.pending {
		p.req <- struct{}{}
		p.pending = true
	}
	select {
	case r := <-p.lines:
		p.pending = false
		return r.line, r.ok
	case <-ctx.Done():
		fmt.Fprintln(p.out)
		return "", false
	}
}

func (p *stdioPrompter) AskSecret(ctx context.Context, q string) (string, bool) {
	if p.pending {
		// A withdrawn question's read is still parked on stdin and would
		// take the key with echo on; finish it first.
		fmt.Fprint(p.out, "press Enter to continue ")
		select {
		case <-p.lines:
			p.pending = false
		case <-ctx.Done():
			return "", false
		}
	}
	fmt.Fprintln(p.out, "(the key is not echoed on a terminal)")
	fmt.Fprint(p.out, q)
	b, err := term.ReadPassword(p.in.Fd())
	fmt.Fprintln(p.out)
	if err != nil {
		return "", false
	}
	return string(b), true
}

func (p *stdioPrompter) Say(line string) { fmt.Fprintln(p.out, line) }

func init() {
	onlineCmd.AddCommand(onlineAddCmd, onlineRevokeCmd)
}
