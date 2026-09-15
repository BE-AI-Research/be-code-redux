package ui

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"time"

	"github.com/chzyer/readline"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/commands"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/live"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/review"
	"github.com/brown-enterprises/be-code/internal/store"
	"github.com/brown-enterprises/be-code/internal/tools"
	"github.com/brown-enterprises/be-code/internal/verify"
)

// REPL is the plain inline session: readline editing, persistent history,
// slash autocomplete — works over SSH and BE-CLI web terminals.
type REPL struct {
	quitAfter bool // set by /quit typed during a run
	Cfg       *config.Config
	Agent     *agent.Agent
	Provider  provider.Provider
	Custom    map[string]commands.Command
	rl        *readline.Instance

	// lines is fed by the single readline goroutine; it keeps reading while
	// a run is in progress so the user can queue messages or cancel.
	lines chan lineEvent
	mu    sync.Mutex
	ask   chan string // set while prompt() waits for an answer during a run
	busy  bool

	// Review decides where a file change is reviewed (the editor diff, this
	// terminal, or both with the first answer winning). Set by cmd after
	// NewREPL; nil when nothing wired it.
	Review *review.Coordinator
}

type lineEvent struct {
	line string
	err  error
}

// HistoryFile returns the shared input-history path (used by both UIs).
func HistoryFile() string {
	d, err := config.Dir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "history")
}

// NewREPL builds the REPL with readline configured.
func NewREPL(cfg *config.Config, ag *agent.Agent, p provider.Provider) (*REPL, error) {
	custom := commands.Load(ag.Tools.Root)
	items := slashCompleterItems()
	for _, n := range commands.Names(custom) {
		items = append(items, readline.PcItem("/"+n))
	}
	completer := readline.NewPrefixCompleter(items...)
	rl, err := readline.NewEx(&readline.Config{
		Prompt:            cyan("be-code> "),
		HistoryFile:       HistoryFile(),
		HistoryLimit:      2000,
		AutoComplete:      completer,
		InterruptPrompt:   "^C",
		EOFPrompt:         "exit",
		HistorySearchFold: true,
	})
	if err != nil {
		return nil, err
	}
	r := &REPL{Cfg: cfg, Agent: ag, Provider: p, Custom: custom, rl: rl}
	ag.Tools.Approve = r.approve
	return r, nil
}

func slashCompleterItems() []readline.PrefixCompleterInterface {
	items := make([]readline.PrefixCompleterInterface, 0, len(SlashCommands))
	for _, c := range SlashCommands {
		items = append(items, readline.PcItem(c))
	}
	return items
}

// approve renders shell commands and file-write diffs and asks y/N/a.
func (r *REPL) approve(action, detail string) bool {
	switch action {
	case "shell":
		if r.Cfg.AutoApproveShell {
			fmt.Printf("%s %s\n", yell("auto-approved:"), detail)
			return true
		}
		fmt.Printf("%s %s\n", yell("run shell:"), detail)
	case "file_write":
		if !r.Cfg.ApproveFileWrites {
			return true
		}
		fmt.Println(yell("file change:"))
		fmt.Println(ColorizeDiff(detail, useColor))
	default:
		fmt.Printf("%s %s\n", yell(action+":"), detail)
	}
	switch strings.ToLower(r.prompt(yell("approve? [y/N/a(lways)] "))) {
	case "y", "yes":
		return true
	case "a", "always":
		switch action {
		case "shell":
			r.Cfg.AutoApproveShell = true
		case "file_write":
			r.Cfg.ApproveFileWrites = false
		case "consult":
			// Session-wide consent for this one co-worker, recorded on the
			// agent and never in the config file — the same rule the TUI's
			// modal follows. A detail with no parsable name approves this
			// one consultation and nothing more.
			if name := agent.ConsentCoworker(detail); name != "" {
				r.Agent.AllowCoworker(name)
				fmt.Println(dim("co-worker " + name + " allowed for this session"))
			}
		}
		return true
	}
	return false
}

// SetReview hands the REPL the review coordinator built in cmd, so /review
// can report and change where file changes are reviewed.
func (r *REPL) SetReview(c *review.Coordinator) { r.Review = c }

// ReviewTerminal is this UI as the coordinator's terminal-side reviewer.
func (r *REPL) ReviewTerminal() review.Terminal { return replTerminal{r} }

type replTerminal struct{ r *REPL }

// Ask prints the diff and asks the same y/N/a question as approve, but on a
// cancellable wait: the editor may answer the same change first, and the
// coordinator then withdraws this question.
func (t replTerminal) Ask(ctx context.Context, preview string) bool {
	fmt.Println(yell("file change:"))
	fmt.Println(ColorizeDiff(preview, useColor))
	switch strings.ToLower(t.r.promptCtx(ctx, yell("approve? [y/N/a(lways)] "))) {
	case "y", "yes":
		return true
	case "a", "always":
		// Both switches, as in the TUI: cfg stops the terminal prompt and
		// the registry flag stops the editor diff (see Registry.ApproveWrites).
		t.r.Cfg.ApproveFileWrites = false
		t.r.Agent.Tools.ApproveWrites = false
		return true
	}
	return false
}

// Withdraw says why the question on screen no longer needs an answer. The
// wait itself ends with the coordinator's cancellation.
func (t replTerminal) Withdraw(note string) {
	if note != "" {
		fmt.Printf("\n%s\n", dim(note))
	}
}

// Run drives the interactive loop until /quit or EOF.
func (r *REPL) Run(ctx context.Context) error {
	defer r.rl.Close()
	fmt.Printf("BE-Code — offline agentic coding %s\n", dim("(plain mode)"))
	fmt.Printf("%s\n", dim(fmt.Sprintf("provider=%s model=%s workspace=%s",
		r.Provider.Name(), r.Agent.Model, r.Agent.Tools.Root)))
	if status, err := r.Provider.Ping(ctx); err != nil {
		fmt.Printf("%s backend unreachable: %v\n", red("warn>"), err)
	} else {
		fmt.Printf("%s %s %s\n", grn("ok>"), r.Provider.Name(), dim(status))
	}
	fmt.Printf("%s\n\n", dim("/help for commands · Tab completes · ↑ history · type while it works to queue a message"))
	if NeedsInitHint(r.Agent.Tools.Root) {
		fmt.Printf("%s\n", dim(InitHint))
	}

	r.lines = make(chan lineEvent)
	go func() {
		for {
			line, err := r.rl.Readline()
			r.lines <- lineEvent{line, err}
			if err != nil && err != readline.ErrInterrupt {
				return
			}
		}
	}()
	for {
		ev := <-r.lines
		if ev.err == readline.ErrInterrupt {
			continue // ^C at prompt clears the line
		}
		if ev.err == io.EOF {
			fmt.Println()
			return nil
		}
		if ev.err != nil {
			return ev.err
		}
		input := strings.TrimSpace(ev.line)
		if input == "" {
			continue
		}
		if strings.HasPrefix(input, "/") {
			if quit := r.command(ctx, input); quit {
				return nil
			}
			continue
		}
		r.turn(ctx, input)
		if r.quitAfter {
			return nil
		}
	}
}

// prompt asks a one-off question through readline with a temporary prompt.
func (r *REPL) prompt(q string) string { return r.promptCtx(context.Background(), q) }

// promptCtx is prompt on a cancellable wait: a shared file-change review can
// be withdrawn (the editor answered it) while the question is on screen, and
// then the answer is no longer wanted. An empty string is the result either
// way, which reads as "no".
func (r *REPL) promptCtx(ctx context.Context, q string) string {
	r.setPrompt(q)
	defer r.setPrompt(cyan("be-code> "))
	r.mu.Lock()
	busy := r.busy
	ch := make(chan string, 1)
	if busy {
		r.ask = ch
	}
	r.mu.Unlock()
	if !busy {
		select {
		case ev := <-r.lines:
			if ev.err != nil {
				return ""
			}
			return strings.TrimSpace(ev.line)
		case <-ctx.Done():
			return ""
		}
	}
	defer func() { r.mu.Lock(); r.ask = nil; r.mu.Unlock() }()
	select {
	case line := <-ch:
		return strings.TrimSpace(line)
	case <-ctx.Done():
		r.abandonAsk(ch)
		return ""
	}
}

// setPrompt changes readline's prompt (nil in tests, which never read a line).
func (r *REPL) setPrompt(q string) {
	if r.rl == nil {
		return
	}
	r.rl.SetPrompt(q)
	r.rl.Refresh()
}

// abandonAsk retires a question nobody is waiting for any more — a shared
// file-change review the editor answered first. It stops taking typed lines
// before the caller returns (not just in the deferred cleanup, which would
// leave a window for runBusy to forward a second line into a channel nobody
// reads) and hands back a line already forwarded into it: the user typed it,
// so it belongs to the agent as an ordinary queued message.
func (r *REPL) abandonAsk(ch chan string) {
	r.mu.Lock()
	r.ask = nil
	r.mu.Unlock()
	select {
	case line := <-ch:
		go func(l string) { r.lines <- lineEvent{line: l} }(line)
	default:
	}
}

// turn runs one agent request. Input typed during the run is queued for
// the model; Ctrl-C cancels; anything still queued when the run ends starts
// another turn.
func (r *REPL) turn(ctx context.Context, input string) {
	for {
		var (
			answer string
			rep    *agent.ReviewedReport
			err    error
		)
		r.runBusy(ctx, func(ctx context.Context) { answer, rep, err = r.Agent.RunFull(ctx, input) })
		r.printOutcome(answer, rep, err)
		left := r.Agent.DrainInbox()
		if len(left) == 0 || err != nil {
			return
		}
		input = strings.Join(left, "\n")
		fmt.Printf("%s %s\n", cyan("you>"), input)
	}
}

// queueCommand implements /queue, /queue edit N, /queue drop N — usable
// while a run is in progress so the user can change their mind about
// queued messages. edit pulls the message out of the queue into the input
// line (prefilled via readline) so it is paused until re-sent.
func (r *REPL) queueCommand(input string) {
	fields := strings.Fields(input)
	items := r.Agent.Peek()
	if len(fields) == 1 {
		if len(items) == 0 {
			fmt.Println(dim("no queued messages"))
			return
		}
		for i, it := range items {
			fmt.Printf("  %d  %s\n", i+1, strings.SplitN(it, "\n", 2)[0])
		}
		fmt.Println(dim("  /queue edit N · /queue drop N"))
		return
	}
	if len(fields) < 3 {
		fmt.Println(dim("usage: /queue | /queue edit N | /queue drop N"))
		return
	}
	var n int
	if _, err := fmt.Sscan(fields[2], &n); err != nil || n < 1 {
		fmt.Println(dim("usage: /queue edit N | /queue drop N (N from /queue)"))
		return
	}
	switch fields[1] {
	case "edit":
		text, ok := r.Agent.Remove(n - 1)
		if !ok {
			fmt.Println(dim("that message was already delivered"))
			return
		}
		fmt.Println(dim("editing queued message (paused): Enter re-queues it"))
		_, _ = r.rl.WriteStdin([]byte(text))
	case "drop":
		text, ok := r.Agent.Remove(n - 1)
		if !ok {
			fmt.Println(dim("that message was already delivered"))
			return
		}
		fmt.Println(dim("dropped: " + strings.SplitN(text, "\n", 2)[0]))
	default:
		fmt.Println(dim("usage: /queue | /queue edit N | /queue drop N"))
	}
}

// runBusy runs fn on its own goroutine while this goroutine keeps servicing
// typed lines: plain text is queued for the agent, answers go to a waiting
// prompt(), Ctrl-C cancels fn and discards the queue, and EOF is deferred
// until fn returns so piped input still gets its run.
func (r *REPL) runBusy(ctx context.Context, fn func(ctx context.Context)) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Restore rather than clear: /consult is busy-safe and runs its own
	// runBusy, so this nests inside the primary's. Clearing the flag on the
	// inner one's way out would tell prompt() to read r.lines directly
	// while the outer loop is still reading it, and an approval would race
	// the run for the next typed line.
	r.mu.Lock()
	wasBusy := r.busy
	r.busy = true
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.busy = wasBusy; r.mu.Unlock() }()
	done := make(chan struct{})
	go func() { fn(runCtx); close(done) }()
	var deferred *lineEvent
	for {
		select {
		case <-done:
			if deferred != nil {
				go func(ev lineEvent) { r.lines <- ev }(*deferred)
			}
			return
		case ev := <-r.lines:
			switch {
			case ev.err == readline.ErrInterrupt:
				fmt.Printf("\n%s\n", yell("interrupted"))
				if n := len(r.Agent.DrainInbox()); n > 0 {
					fmt.Println(dim(fmt.Sprintf("discarded %d queued message(s)", n)))
				}
				cancel()
			case ev.err != nil:
				deferred = &ev // EOF or read error: hand back after the run
			default:
				line := strings.TrimSpace(ev.line)
				if line == "" {
					continue
				}
				r.mu.Lock()
				ask := r.ask
				r.mu.Unlock()
				if ask != nil {
					// Never block: the question may have been withdrawn
					// between that read and this send (see abandonAsk), and
					// a wedged forward would stop servicing input — Ctrl-C
					// included — for the rest of the run. An unanswerable
					// line falls through to the ordinary queue path below.
					select {
					case ask <- line:
						continue
					default:
					}
				}
				if strings.HasPrefix(line, "/queue") {
					r.queueCommand(line)
					continue
				}
				if strings.HasPrefix(line, "/") {
					if BusySafeCommand(line) {
						if r.command(ctx, line) {
							r.quitAfter = true // leaving mid-turn: stop the run, then exit
							cancel()
						}
						continue
					}
					fmt.Println(dim("commands wait until the agent is done (Ctrl-C cancels); plain text is queued; /queue edits the queue"))
					continue
				}
				r.Agent.Enqueue(line)
				fmt.Printf("%s %s\n", dim("queued (delivered at the next step)>"), line)
			}
		}
	}
}

// command handles slash commands; returns true to quit.
func (r *REPL) command(ctx context.Context, input string) bool {
	fields := strings.Fields(input)
	switch fields[0] {
	case "/quit", "/exit", "/q":
		return true
	case "/help":
		fmt.Print(`commands:
  /models /model /provider    backend & model switching (profiles auto-apply)
  /sessions /resume [code]    saved-session management
  /handoff                    show the briefing carried over from a resumed session
  /plan <task>                read-only planning phase, then approve & execute
  /undo                       roll back the last turn's file changes
  /commit                     model-written git commit of current changes
  /init                       generate BECODE.md project memory
  /verify /tools /map         checks, tool list, repo map
  /compact /stats /config     context compaction, usage stats, settings
  /clear /quit
`)
		if len(r.Custom) > 0 {
			fmt.Printf("custom commands: /%s\n", strings.Join(commands.Names(r.Custom), " /"))
		}
		fmt.Println("@path in a message pins that file into context. Tab completes; ↑ history.")
	case "/models":
		models, err := r.Provider.ListModels(ctx)
		if err != nil {
			fmt.Printf("%s %v\n", red("error>"), err)
			break
		}
		for _, m := range models {
			extra := ""
			if m.SizeBytes > 0 {
				extra = fmt.Sprintf("  %s %.1fGB %s", m.Family, float64(m.SizeBytes)/1e9, m.Quantization)
			}
			fmt.Printf("  %s%s\n", m.ID, dim(extra))
		}
	case "/model":
		if len(fields) < 2 {
			fmt.Printf("current model: %s (profile %s)\n", r.Agent.Model, r.Agent.Profile.Family)
			break
		}
		r.Agent.SetModel(fields[1])
		fmt.Printf("model set to %s (profile %s)\n", fields[1], r.Agent.Profile.Family)
	case "/provider":
		if len(fields) < 2 {
			fmt.Printf("current provider: %s (configured: see /config)\n", r.Provider.Name())
			break
		}
		p, err := provider.FromConfig(r.Cfg, fields[1])
		if err != nil {
			fmt.Printf("%s %v\n", red("error>"), err)
			break
		}
		r.Provider = p
		r.Agent.Provider = p
		r.Agent.SetModel(provider.ResolveModel(r.Cfg, fields[1], ""))
		fmt.Printf("provider set to %s (model %s)\n", p.Name(), r.Agent.Model)
	case "/sessions":
		printSessions()
	case "/resume":
		id := "last"
		if len(fields) > 1 {
			id = fields[1]
		}
		// Join, never fork: a session with a host running somewhere is not
		// loaded a second time here — two programs on one session file are
		// blind to each other's turns. Plain mode has no host of its own to
		// switch through, so it says how to join instead. The registry is
		// asked before the store: a host's session exists before its first
		// autosave, so a live code may have no file yet.
		if joinLiveHint(strings.ToUpper(id)) {
			break
		}
		s, err := store.Load(id)
		if err != nil {
			fmt.Printf("%s %v\n", red("error>"), err)
			break
		}
		if joinLiveHint(s.ResumeCode()) {
			break
		}
		r.Agent.Resume(s)
		fmt.Printf("resumed %s — %s (%d messages)\n", s.ResumeCode(), s.Title, len(s.Messages))
		if s.Handoff != "" {
			fmt.Printf("%s\n", dim("handoff briefing loaded into the system prompt; /handoff shows it"))
		}
	case "/theme":
		if len(fields) < 2 {
			fmt.Println("themes: dark, light, mono, dracula, nord, gruvbox, monokai, one-dark, solarized-dark, solarized-light, tokyo-night, catppuccin, github-light")
			fmt.Println(dim("usage: /theme <name>  (plain mode uses the terminal's own colours; the TUI applies the palette)"))
			break
		}
		r.Cfg.Theme = strings.ToLower(fields[1])
		if err := r.Cfg.Save(); err != nil {
			fmt.Printf("%s %v\n", red("error>"), err)
			break
		}
		fmt.Printf("theme set to %s (applies to the TUI on next start)\n", r.Cfg.Theme)
	case "/queue":
		r.queueCommand(input)
	case "/handoff":
		if h := r.Agent.Handoff(); h != "" {
			fmt.Println(h)
		} else {
			fmt.Println(dim("no handoff briefing in this session"))
		}
	case "/verify":
		proj := verify.Detect(r.Agent.Tools.Root)
		rep := verify.RunChecks(ctx, r.Agent.Tools.Root, proj)
		fmt.Println(rep.Human())
	case "/tools":
		for _, n := range r.Agent.Tools.Names() {
			fmt.Printf("  %s\n", n)
		}
	case "/review":
		if r.Review == nil {
			fmt.Println(dim("review: not available in this session"))
			break
		}
		if len(fields) > 1 {
			if err := r.Review.SetMode(review.Mode(strings.ToLower(fields[1]))); err != nil {
				fmt.Printf("%s %v\n", red("error>"), err)
				break
			}
		}
		fmt.Printf("review: %s (resolves to %s)\n", r.Review.Mode(), r.Review.Resolve())
	case "/coworkers":
		cws := r.Agent.Coworkers()
		if len(cws) == 0 {
			fmt.Println(dim(`no co-working models configured (see README "Co-working models")`))
			break
		}
		for _, cw := range cws {
			line := fmt.Sprintf("  %s  %s/%s  %s", cw.Name, cw.Provider, cw.Model, cw.Skills)
			if cw.Online {
				line += " (online)"
			}
			if n := r.Agent.ConsultCount(cw.Name); n > 0 {
				line += dim(fmt.Sprintf(" · consulted %d", n))
			}
			fmt.Println(line)
		}
	case "/consult":
		who, q := "", strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(input), fields[0]))
		// The first word is a co-worker name only when it names one that
		// exists; otherwise it is the first word of the question, so
		// "/consult why is this failing?" still reaches the default one.
		if len(fields) > 1 {
			for _, cw := range r.Agent.Coworkers() {
				if cw.Name == fields[1] {
					who, q = cw.Name, strings.TrimSpace(strings.TrimPrefix(q, fields[1]))
					break
				}
			}
		}
		if q == "" {
			fmt.Println(dim("usage: /consult [name] <question>"))
			break
		}
		r.runBusy(ctx, func(ctx context.Context) {
			// No Recent: RecentContext reads fields only the agent's own
			// goroutine may touch, and this one is the UI's. A person
			// asking directly says what they mean anyway.
			res, err := r.Agent.Consult(ctx, agent.ConsultRequest{Who: who, Question: q, Origin: "user:local"})
			// A consultation that got as far as running reports itself
			// through OnConsultStart/OnConsultEnd; only the refusals before
			// it started (unknown name, declined, none configured) never
			// reach an event.
			if err != nil && !res.Started {
				fmt.Printf("%s %v\n", red("error>"), err)
			}
		})
	case "/task":
		if r.Agent.Engine == nil {
			fmt.Println(dim("working memory is off (engine.enabled)"))
			break
		}
		if len(fields) > 1 && fields[1] == "clear" {
			r.Agent.Engine.ClearSession()
			fmt.Println(dim("working memory cleared for this session"))
			break
		}
		fmt.Println(r.Agent.Engine.LedgerText())
	case "/notes":
		if r.Agent.Engine == nil {
			fmt.Println(dim("working memory is off (engine.enabled)"))
			break
		}
		sub := ""
		if len(fields) > 1 {
			sub = fields[1]
		}
		switch sub {
		case "add":
			text := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(input, "/notes"), " add"))
			if text == "" {
				fmt.Println("usage: /notes add <text>")
				break
			}
			r.Agent.Engine.AddNoteLine(text)
			fmt.Println(dim("noted"))
		case "drop":
			n, _ := strconv.Atoi(strings.Join(fields[2:], ""))
			if err := r.Agent.Engine.DropNote(n); err != nil {
				fmt.Printf("%s %v\n", red("error>"), err)
			}
		case "clear":
			r.Agent.Engine.ClearNotes()
			fmt.Println(dim("notes cleared"))
		default:
			notes := strings.TrimRight(r.Agent.Engine.Notes(), "\n")
			if notes == "" {
				fmt.Println(dim("no notes"))
				break
			}
			for i, l := range strings.Split(notes, "\n") {
				fmt.Printf("%d. %s\n", i+1, l)
			}
		}
	case "/config":
		p, _ := config.Path()
		fmt.Printf("config: %s\n  provider=%s model=%s ui=%s context_tokens=%d max_turns=%d max_repairs=%d\n  compat_tool_calls=%s approve_file_writes=%v auto_approve_shell=%v\n",
			p, r.Cfg.DefaultProvider, r.Cfg.Model, r.Cfg.UI, r.Cfg.ContextTokens, r.Cfg.MaxTurns,
			r.Cfg.MaxRepairs, r.Cfg.CompatToolCalls, r.Cfg.ApproveFileWrites, r.Cfg.AutoApproveShell)
	case "/clear":
		r.Agent.History.Messages = nil
		r.Agent.SetSession(store.NewSession(r.Provider.Name(), r.Agent.Model, r.Agent.Tools.Root))
		fmt.Println("history cleared; new session started")
	case "/undo":
		restored, err := r.Agent.Undo()
		if err != nil {
			fmt.Printf("%s %v\n", red("error>"), err)
			break
		}
		fmt.Printf("restored: %s (%d undo levels left)\n", strings.Join(restored, ", "), r.Agent.Checkpoints.Depth())
	case "/commit":
		line, err := r.Agent.GenerateCommit(ctx)
		if err != nil {
			fmt.Printf("%s %v\n", red("error>"), err)
			break
		}
		fmt.Printf("%s %s\n", grn("committed:"), line)
	case "/init":
		path, err := RunInit(ctx, r.Agent, InitOptions{
			Root:    r.Agent.Tools.Root,
			Approve: func(p string) bool { return r.approve("file_write", p) },
			Log:     func(s string) { fmt.Println(dim(s)) },
		})
		if err != nil {
			fmt.Printf("%s %v\n", red("error>"), err)
			break
		}
		fmt.Printf("%s %s\n", grn("wrote"), path)
	case "/compact":
		if err := r.Agent.Compact(ctx); err != nil {
			fmt.Printf("%s %v\n", red("error>"), err)
			break
		}
		fmt.Printf("compacted; context now ~%d tokens\n", r.Agent.History.Tokens())
	case "/stats":
		s := r.Agent.Usage()
		fmt.Printf("requests=%d tool_calls=%d prompt_tokens=%d completion_tokens=%d elapsed=%s ctx=%d/%d\n",
			s.Requests, s.ToolCalls, s.PromptTokens, s.CompletionTokens,
			s.Elapsed.Round(100*time.Millisecond), r.Agent.History.Tokens(), r.Agent.History.Budget)
	case "/map":
		m := r.Agent.RepoMap()
		if m == "" {
			fmt.Println("no repo map (unrecognized files or disabled)")
			break
		}
		fmt.Println(m)
	case "/plan":
		req := strings.TrimSpace(strings.TrimPrefix(input, "/plan"))
		if req == "" {
			fmt.Println("usage: /plan <task description>")
			break
		}
		fmt.Println(dim("planning (read-only)... Ctrl-C cancels"))
		var plan string
		var perr error
		r.runBusy(ctx, func(ctx context.Context) { plan, perr = r.Agent.Plan(ctx, req) })
		if perr != nil {
			fmt.Printf("%s %v\n", red("error>"), perr)
			break
		}
		fmt.Println("\n" + plan + "\n")
		if strings.ToLower(r.prompt(yell("execute this plan? [y/N] "))) == "y" {
			r.runBusy(ctx, func(ctx context.Context) {
				answer, rep, err := r.Agent.ExecutePlan(ctx, req, plan)
				r.printOutcome(answer, rep, err)
			})
		} else {
			fmt.Println("plan discarded")
		}
	case "/clients", "/detach":
		fmt.Println("only available in the full-screen TUI")
	default:
		if c, ok := r.Custom[strings.TrimPrefix(fields[0], "/")]; ok {
			args := strings.TrimSpace(strings.TrimPrefix(input, fields[0]))
			r.turn(ctx, c.Expand(args))
			break
		}
		fmt.Printf("unknown command %s (/help)\n", fields[0])
	}
	return false
}

func (r *REPL) printOutcome(_ string, rep *agent.ReviewedReport, err error) {
	fmt.Println()
	if err != nil {
		fmt.Printf("%s %v\n\n", red("error>"), err)
		return
	}
	if rep != nil && rep.Verify != nil {
		mark := grn("verified")
		if !rep.Verify.Passed() {
			mark = red("verification failed after repairs")
		}
		fmt.Printf("%s\n%s\n", rep.Verify.Human(), mark)
	}
	if rep != nil && rep.Reviewed {
		if rep.ReviewIssues == "" {
			fmt.Println(grn("reviewer: approved"))
		} else {
			fmt.Println(yell("reviewer raised issues (repair attempted)"))
		}
	}
	fmt.Println()
}

// joinLiveHint prints how to join code if a host advertises it, and reports
// whether it did.
func joinLiveHint(code string) bool {
	dir, err := live.Dir()
	if err != nil {
		return false
	}
	rec := live.LiveCode(dir, code)
	if rec == nil {
		return false
	}
	fmt.Printf("%s\n", dim(fmt.Sprintf("%s is live elsewhere; join it with: be-code attach %s", rec.Code, rec.Code)))
	return true
}

// printSessions lists the saved sessions, marking the ones a host is
// running, plus the live sessions that have no file yet (a host's session
// exists before its first autosave) — the same rows `be-code sessions` shows.
func printSessions() {
	metas, err := store.List()
	if err != nil {
		fmt.Printf("%s %v\n", red("error>"), err)
		return
	}
	liveByCode := map[string]live.Record{}
	if dir, derr := live.Dir(); derr == nil {
		recs, _ := live.List(dir)
		for _, r := range recs {
			liveByCode[r.Code] = r
		}
	}
	if len(metas) == 0 && len(liveByCode) == 0 {
		fmt.Println("no saved sessions")
		return
	}
	for _, m := range metas {
		mark := ""
		if _, ok := liveByCode[m.Code]; ok {
			mark = "LIVE  "
			delete(liveByCode, m.Code)
		}
		fmt.Printf("  %s  %s%s  %s\n", m.Code, mark, m.Title,
			dim(fmt.Sprintf("(%s, %d turns, %s)", m.ID, m.Turns, m.UpdatedAt.Format("Jan 2 15:04"))))
	}
	codes := make([]string, 0, len(liveByCode))
	for code := range liveByCode {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	for _, code := range codes {
		r := liveByCode[code]
		fmt.Printf("  %s  LIVE  (no saved turns yet)  %s\n", r.Code,
			dim(fmt.Sprintf("(%s, started %s)", r.Workspace, r.StartedAt.Format("Jan 2 15:04"))))
	}
	fmt.Println(dim("  /resume <code>   (a LIVE one: be-code attach <code>)"))
}

// Events wires agent callbacks to terminal output (plain mode).
func Events() agent.Events {
	// Thinking models can reason silently for minutes; show "thinking" plus
	// a dot per ~400 chars so the terminal is never dead, and break the line
	// before any real output follows.
	thinking, last := 0, 0
	endThinking := func() {
		if thinking > 0 {
			fmt.Println()
			thinking, last = 0, 0
		}
	}
	return agent.Events{
		OnDelta: func(t string) { endThinking(); fmt.Print(t) },
		OnToolStart: func(name, args string) {
			endThinking()
			a := args
			if len(a) > 160 {
				a = a[:160] + "..."
			}
			fmt.Printf("\n%s %s %s\n", cyan("tool>"), name, dim(a))
		},
		OnToolEnd: func(name string, res tools.Result) {
			if res.IsError {
				first := strings.SplitN(res.Content, "\n", 2)[0]
				fmt.Printf("%s %s: %s\n", red("err>"), name, dim(first))
			}
		},
		OnNotice: func(msg string) { endThinking(); fmt.Printf("%s %s\n", yell("note>"), msg) },
		// A consultation is announced before the wait it explains, and
		// closed by the co-worker's answer plus what it cost. Progress
		// (files read so far) is a live status line, which plain mode has
		// nowhere to put, so it is left to the TUI.
		OnConsultStart: func(name, question, origin string) {
			endThinking()
			fmt.Printf("%s %s\n", cyan(name+"?"), question)
		},
		OnConsultEnd: func(res agent.ConsultResult, err error) {
			endThinking()
			if err != nil {
				fmt.Printf("%s %s: %v\n", yell("note>"), res.Coworker, err)
				return
			}
			fmt.Printf("%s %s\n", cyan(res.Coworker+">"), res.Answer)
			if res.Partial {
				fmt.Println(dim("(partial)"))
			}
			fmt.Println(dim(fmt.Sprintf("%s read %d files in %s", res.Coworker, res.Read, res.Elapsed.Round(time.Second))))
		},
		OnReasoning: func(t string) {
			if thinking == 0 {
				fmt.Print(dim("thinking"))
			}
			thinking += len(t)
			if thinking-last >= 400 {
				last = thinking
				fmt.Print(dim("."))
			}
		},
	}
}
