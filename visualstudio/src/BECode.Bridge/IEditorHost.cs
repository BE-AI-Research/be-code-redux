using System.Collections.Generic;
using System.Threading;
using System.Threading.Tasks;

namespace BECode.Bridge
{
    /// <summary>
    /// What the user is looking at: active file, cursor line, selection, open
    /// files. Field names mirror the JSON object vscode/src/tools/editor.ts's
    /// <c>context</c> handler returns (file, line, selStart, selEnd,
    /// selection, open). <c>workspaceFolders</c> is NOT carried here:
    /// it is fetched separately, through
    /// <see cref="IEditorHost.GetWorkspaceFoldersAsync"/>, so a tool that only
    /// needs to resolve a path does not also have to read the active editor's
    /// state.
    ///
    /// <see cref="File"/> and each entry of <see cref="Open"/> are ABSOLUTE
    /// paths: the host never relativises anything —
    /// <c>EditorTools.Context</c> applies <see cref="Paths.RelPath"/> before
    /// the reply goes on the wire, exactly where vscode's own <c>context</c>
    /// handler does (against <c>folders()</c>). <see cref="Selection"/> is the
    /// RAW, untruncated selection text: the 2048-character cut
    /// vscode applies is the tool's job now, not the host's.
    ///
    /// What each field is, matching vscode's own
    /// <c>editor.ts</c> "context" handler field for field, so the Visual
    /// Studio host's behaviour is a straight port rather than a guess:
    /// <list type="bullet">
    /// <item><see cref="File"/> is <c>""</c> when there is no active
    /// document, or the active document is not a file on disk (e.g. a diff
    /// view, an output pane, an untitled buffer) — never a partial or
    /// best-effort path. <see cref="Line"/> is the 1-based line the caret
    /// currently sits on (the "active" end of the selection — where the
    /// caret is, which may be either end of a highlighted range) and is 0
    /// only alongside an empty <see cref="File"/>.</item>
    /// <item><see cref="SelStart"/> and <see cref="SelEnd"/> describe the
    /// PRIMARY selection (Visual Studio, like vscode, may have several; only
    /// the primary/active one is reported). When nothing is selected (a bare
    /// caret) both are 0 and <see cref="Selection"/> is <c>""</c> — not
    /// <see cref="Line"/> repeated. When something is selected, both are
    /// 1-based line numbers (<see cref="SelStart"/> &lt;= <see cref="SelEnd"/>
    /// regardless of which direction the user dragged) and
    /// <see cref="Selection"/> is the selected text, RAW/untruncated.</item>
    /// <item><see cref="Open"/> lists file-backed documents only — every
    /// document currently loaded in an editor buffer (not just the visible
    /// tabs) whose content is a real file on disk; skip diff views, output/
    /// tool windows, untitled buffers, and anything else with no path on
    /// disk. Absolute paths.</item>
    /// </list>
    /// </summary>
    public sealed record EditorContext(
        string File,
        int Line,
        int SelStart,
        int SelEnd,
        string Selection,
        IReadOnlyList<string> Open);

    /// <summary>
    /// A source location. <see cref="Path"/> is an ABSOLUTE path;
    /// the tool relativises it for output, exactly where vscode's own
    /// <c>definition</c>/<c>references</c> handlers do. <see cref="Line"/>/
    /// <see cref="Col"/> are 1-based. <see cref="Text"/> is
    /// only populated by <see cref="IEditorHost.ReferencesAsync"/> (the
    /// source line's raw text, matching vscode's <c>references</c>
    /// formatting of <c>path:line: text</c>); <see cref="IEditorHost.DefinitionAsync"/>
    /// leaves it null, since vscode's <c>definition</c> formats only
    /// <c>path:line:col</c>. One record serves both, since they share the
    /// same shape.
    /// </summary>
    public sealed record Location(string Path, int Line, int Col, string? Text = null);

    /// <summary>
    /// One error/warning/info/hint from the language service, in the shape
    /// vscode/src/lib/format.ts's <c>DiagItem</c> takes: <see cref="Severity"/>
    /// is one of "error", "warning", "info", "hint". <see cref="Path"/> is an
    /// ABSOLUTE path; <c>DiagnosticsTools</c> relativises it for
    /// output, exactly where vscode's own <c>diagnostics</c> handler does.
    /// <see cref="Line"/>/<see cref="Col"/> are 1-based.
    /// </summary>
    public sealed record Diagnostic(string Path, int Line, int Col, string Severity, string Source, string Message);

    /// <summary>
    /// A proposed file change offered for review (vscode/src/tools/review.ts's
    /// <c>review_diff</c> arguments). <see cref="Proposed"/> may legally be an
    /// empty string (an emptied file); only
    /// <see cref="Path"/> is required to be non-empty.
    /// </summary>
    public sealed record ReviewRequest(string Path, string? Original, string Proposed, string? Summary, bool Shared);

    /// <summary>
    /// The user's (or a prior "accept all this session") answer to a review.
    /// Serialised by <c>ReviewTools</c> to exactly
    /// <c>{"decision":"accept"|"reject"|"accept_all"|"cancelled"}</c>, matching
    /// the VS Code extension's <c>review_diff</c> reply.
    /// </summary>
    public enum ReviewDecision { Accept, Reject, AcceptAll, Cancelled }

    /// <summary>
    /// What the eighteen tools need from an editor. Implemented once here as
    /// the seam every tool is written against (<c>ToolRegistry</c>); the real
    /// Visual Studio implementation (over DTE / the text manager / the
    /// Error List / EnvDTE.Debugger / IVsDifferenceService) cannot be compiled
    /// on this machine, so nothing in this project may depend on anything
    /// beyond this interface, and nothing in this project has ever exercised
    /// a real implementation of it. Four conventions hold for EVERY member
    /// below, stated once here rather than repeated verbatim on each one:
    ///
    /// <list type="number">
    /// <item><b>Everything decidable without Visual Studio lives in the
    /// tools, not here.</b> Every method
    /// returns raw, absolute, unfiltered, untruncated data and does
    /// UI-thread work only; filtering, truncation, path relativisation and
    /// text formatting are the tools' job. A null return from
    /// <see cref="DefinitionAsync"/>, <see cref="ReferencesAsync"/> or
    /// <see cref="HoverAsync"/> means "no language service for this file
    /// type" (Visual Studio's design deviates here from vscode, which
    /// always has *a* provider result, possibly empty — this project adds
    /// the "not available for this file type" case the design spec's
    /// section 4 calls for, distinguished from a provider that ran and
    /// found nothing, which is an empty list / empty string instead).</item>
    ///
    /// <item><b>Lines and columns are 1-BASED everywhere across the seam, in
    /// BOTH directions.</b> Every argument that names a line or
    /// column (<see cref="OpenAsync"/>, <see cref="DefinitionAsync"/>,
    /// <see cref="ReferencesAsync"/>, <see cref="HoverAsync"/>,
    /// <see cref="IDebugHost.SetBreakpointAsync"/>) and every result field
    /// that reports one (<see cref="Location"/>'s Line/Col,
    /// <see cref="Diagnostic"/>'s Line/Col, <see cref="StackFrameInfo"/>'s
    /// Line, <see cref="EditorContext"/>'s Line/SelStart/SelEnd) is 1-based.
    /// The TOOLS clamp an incoming line/col to a minimum of 1 before this
    /// interface ever sees it (vscode's own editor.ts clamps with
    /// <c>Math.max(0, line-1)</c> one layer further in, converting to its
    /// own 0-based <c>Position</c> — the C# seam stops at "never less than
    /// 1", the conversion below is the host's own problem to solve). What
    /// this means concretely for a Visual Studio host: DTE's own
    /// <c>TextSelection</c>/<c>TextPoint</c> (<c>ActivePoint.Line</c>,
    /// <c>ActivePoint.LineCharOffset</c>, …) are ALREADY 1-based — no
    /// conversion needed going through DTE. Roslyn's <c>LinePosition</c>
    /// and the editor's own <c>ITextSnapshotLine</c> (and
    /// <c>SnapshotPoint</c>) are 0-based — the host must add 1 converting a
    /// Roslyn/text-buffer position OUT to this interface, and subtract 1
    /// converting an incoming 1-based argument IN before calling
    /// Roslyn/text-buffer APIs with it.</item>
    ///
    /// <item><b>Threading.</b> Every member may be called from a
    /// thread-pool thread, and calls are CONCURRENT: <c>BridgeServer</c> runs
    /// <c>tools/call</c> concurrently per connection, and every connection to
    /// this bridge shares the ONE <see cref="IEditorHost"/> instance. The
    /// host implementation must marshal to Visual Studio's UI thread itself
    /// (e.g. via <c>JoinableTaskFactory.SwitchToMainThreadAsync</c>) for
    /// anything that needs it — DTE, the text buffer, the debugger — and
    /// must be safe under concurrent calls, including two calls touching the
    /// same file or the same debug session at once. Nothing about this
    /// interface's shape enforces serialisation; the host provides
    /// whatever internal locking its own state (e.g. <see cref="IDebugHost"/>'s
    /// single active session) needs.</item>
    ///
    /// <item><b>Cancellation.</b> Every member observes
    /// <paramref name="ct"/>-equivalent parameters and MAY throw
    /// <see cref="System.OperationCanceledException"/> — but only when its
    /// own <c>ct</c> is the reason. A review that must be abandoned for any
    /// OTHER reason (Visual Studio shutting down, the document closed
    /// underneath it, the debug session torn down externally) is not a
    /// cancellation of the caller's request and must not be reported as one
    /// via a token that was never cancelled; <see cref="ReviewDiffAsync"/>
    /// specifically returns <see cref="ReviewDecision.Cancelled"/> for that
    /// case (see its own doc comment). That said, <c>ReviewTools.ReviewDiff</c>
    /// is tolerant of a host that gets this distinction wrong: ANY
    /// <see cref="System.OperationCanceledException"/> it throws, whatever
    /// the reason, is treated as <see cref="ReviewDecision.Cancelled"/>
    /// rather than being allowed to escape uncaught.</item>
    /// </list>
    /// </summary>
    public interface IEditorHost
    {
        Task<EditorContext> GetContextAsync(CancellationToken ct);

        /// <summary>
        /// The solution/folder roots tools confine every path argument to,
        /// and that <c>context</c> reports as
        /// <c>workspaceFolders</c> (unmodified, absolute — <c>context</c> is
        /// the one place these are NOT relativised, since relativising a
        /// folder against itself is meaningless). Split out from
        /// <see cref="GetContextAsync"/> so a tool that only needs to
        /// resolve a path argument (every path-taking tool but
        /// <c>context</c> itself) does not also have to read the active
        /// editor's state.
        /// </summary>
        Task<IReadOnlyList<string>> GetWorkspaceFoldersAsync(CancellationToken ct);

        /// <summary>
        /// Brings <paramref name="path"/> (absolute, already confined) to
        /// the front of the editor, scrolled to <paramref name="line"/>
        /// (1-based) when given. Do this without
        /// taking keyboard focus away from wherever the user is currently
        /// typing, if Visual Studio allows it (vscode's own analogue is
        /// <c>{ preserveFocus: true }</c> on <c>showTextDocument</c>) — the
        /// point is to let the model show the user something without
        /// interrupting them. When <paramref name="path"/> does not exist,
        /// throw <see cref="System.IO.FileNotFoundException"/>;
        /// <c>EditorTools.Open</c> catches it and answers
        /// <c>isError:true, "file not found: &lt;path&gt;"</c> rather than
        /// letting the exception escape.
        /// </summary>
        Task OpenAsync(string path, int? line, CancellationToken ct);
        Task<IReadOnlyList<Location>?> DefinitionAsync(string path, int line, int col, CancellationToken ct);

        /// <summary>
        /// Every reference, unfiltered — no <c>max</c> parameter
        /// here: the tool reports the TOTAL count and prints only the first
        /// <c>max</c> (default 50), exactly as vscode does
        /// (<c>res.length</c> read before <c>.slice()</c>).
        /// </summary>
        Task<IReadOnlyList<Location>?> ReferencesAsync(string path, int line, int col, CancellationToken ct);

        /// <summary>
        /// Type/signature information for the symbol at <paramref name="path"/>:<paramref name="line"/>:<paramref name="col"/>,
        /// as PLAIN TEXT — no markup (no Markdown, no HTML). When
        /// the language service offers more than one part (e.g. a type
        /// signature plus documentation), join them with a BLANK LINE
        /// between (<c>"\n\n"</c>), matching vscode's own
        /// <c>res.flatMap(...).join("\n\n")</c>. Null means "no language
        /// service for this file type" (<c>EditorTools</c> answers "not
        /// available for this file type"); <c>""</c> means the provider ran
        /// and found nothing at that position (<c>EditorTools</c> answers
        /// "no hover information"); the tool also trims whatever is
        /// returned, so leading/trailing whitespace either way is harmless.
        /// </summary>
        Task<string?> HoverAsync(string path, int line, int col, CancellationToken ct);

        /// <summary>
        /// Every diagnostic for <paramref name="path"/> (absolute), or for
        /// the whole solution when <paramref name="path"/> is null — no
        /// severity filtering here: <c>DiagnosticsTools</c>
        /// filters with the ported <c>severitiesFor</c>. A
        /// non-null <paramref name="path"/> is matched EXACTLY, as an
        /// absolute path (case-insensitively on Windows, matching
        /// <see cref="Paths"/>'s own comparison rule) — never a substring
        /// match, and never matched against a display name (a project item
        /// caption, a solution-relative label, …); compare the actual
        /// on-disk paths.
        /// </summary>
        Task<IReadOnlyList<Diagnostic>> DiagnosticsAsync(string? path, CancellationToken ct);
        IDebugHost Debug { get; }

        /// <summary>
        /// Shows <paramref name="request"/> as a difference-viewer diff and
        /// returns the user's decision. When <paramref name="ct"/> is
        /// cancelled — a <c>review_cancel</c> for this specific review, or
        /// the connection going away — the host MUST close the difference
        /// viewer and complete, either by returning
        /// <see cref="ReviewDecision.Cancelled"/> or by throwing
        /// <see cref="System.OperationCanceledException"/>; <c>ReviewTools</c>
        /// treats both the same way, as "cancelled". There is
        /// exactly one cancellation mechanism: this token. A review abandoned for a reason that has NOTHING to do with
        /// this token (Visual Studio shutting down, the document closed
        /// underneath it) should still be reported as
        /// <see cref="ReviewDecision.Cancelled"/> — <c>ReviewTools</c> treats
        /// any <see cref="System.OperationCanceledException"/> from this
        /// method as "cancelled" regardless of whether <paramref name="ct"/>
        /// itself was ever cancelled, so throwing it is always safe even
        /// when the token is not the actual cause.
        /// </summary>
        Task<ReviewDecision> ReviewDiffAsync(ReviewRequest request, CancellationToken ct);
    }

    /// <summary>How a debug run last stopped, waiting, exited or ended — mirrors vscode/src/lib/stopwaiter.ts's <c>StopResult</c>.</summary>
    public enum StopKind { Stopped, Timeout, Exited, Terminated }

    /// <summary>
    /// The outcome of a debug_start/debug_continue/debug_step wait. The tool
    /// (not the host) turns this into text, calling
    /// <see cref="IDebugHost.StackAsync"/> itself for a top-of-stack summary
    /// when <see cref="Kind"/> is <see cref="StopKind.Stopped"/> — the same
    /// two-step vscode's <c>DebugManager.describe()</c> takes. See
    /// <see cref="IDebugHost.StartAsync"/>'s doc comment for
    /// exactly which real-world outcome maps to which <see cref="StopKind"/>.
    /// </summary>
    public sealed record StopResult(StopKind Kind, string? Reason = null, int? ExitCode = null);

    /// <summary>
    /// One startup project or launch profile, as Visual Studio understands
    /// debug configurations — the design spec's stand-in for vscode's
    /// <c>launch.json</c> entries (<c>debug_configs</c> "lists the solution's
    /// startup projects and launch profiles rather than launch.json entries").
    /// <see cref="Kind"/> is free display text, printed VERBATIM
    /// by <c>DebugTools.Configs</c> as <c>"{Name} ({Kind})"</c> — no parsing,
    /// no validation. The two values expected in practice are
    /// <c>"startup project"</c> (a project set as the solution's startup
    /// project, or one of several in a multi-project startup) and
    /// <c>"launch profile"</c> (an entry from that project's
    /// launchSettings.json/debug profile list); anything more specific
    /// Visual Studio's own UI uses for these is fine too — it is display
    /// text only, never matched against by any tool.
    /// </summary>
    public sealed record DebugConfigInfo(string Name, string Kind);

    /// <summary>A breakpoint in one file, as it stands after debug_breakpoint's add/remove. <see cref="Line"/> is 1-based.</summary>
    public sealed record BreakpointInfo(int Line, string? Condition);

    /// <summary>
    /// <c>debug_breakpoint</c>'s <c>action</c> argument
    /// becomes this enum at the seam — exactly the two values
    /// vscode/tools.manifest.json's <c>debug_breakpoint.action</c> schema
    /// enumerates. The tool parses the incoming string and refuses an
    /// unrecognised value itself; the host never sees a raw string.
    /// </summary>
    public enum BreakpointAction { Add, Remove }

    /// <summary>
    /// <c>debug_step</c>'s <c>step</c> argument becomes this
    /// enum at the seam — exactly the three values
    /// vscode/tools.manifest.json's <c>debug_step.step</c> schema
    /// enumerates. The tool parses the incoming string and refuses an
    /// unrecognised value itself; the host never sees a raw string.
    /// </summary>
    public enum DebugStepKind { Over, Into, Out }

    /// <summary>
    /// One frame of a call stack. <see cref="Path"/> is an ABSOLUTE path
    /// (<c>DebugTools</c> relativises it for output, exactly
    /// where vscode's own <c>debug_stack</c> handler does), or null when the
    /// frame has no source (matches vscode's <c>f.source?.path ? … : "?"</c>
    /// fallback, rendered by the tool). <see cref="Line"/> is 1-based.
    /// </summary>
    public sealed record StackFrameInfo(string Name, string? Path, int Line, int FrameId);

    /// <summary>
    /// One variable. <see cref="Children"/> is non-null only when the host
    /// has already resolved this variable's nested members (mirrors
    /// vscode's on-demand DAP "variables" request against
    /// <c>variablesReference</c>); <c>DebugTools</c> decides whether to
    /// *display* them (only when the parent scope has ten or fewer
    /// variables, capped at twenty children), exactly as
    /// vscode/src/tools/debug.ts's <c>variables()</c> does — see
    /// <see cref="IDebugHost.VariablesAsync"/>'s doc comment for
    /// the bound on how deep the host itself may go resolving them.
    /// </summary>
    public sealed record VariableInfo(string Name, string Value, string? Type, IReadOnlyList<VariableInfo>? Children = null);

    /// <summary>
    /// One DAP-equivalent scope ("Locals", "Arguments", …) and its
    /// variables. <see cref="Name"/> is what
    /// <c>DebugTools.Variables</c> filters on — a name containing "local"
    /// (case-insensitively) is treated as the locals scope, one containing
    /// "arg" as the arguments scope; the host SHOULD name them "Locals" and
    /// "Arguments" (matching Visual Studio's own Locals/Autos/Watch window
    /// naming) so that filtering behaves as expected.
    /// </summary>
    public sealed record VariableScope(string Name, IReadOnlyList<VariableInfo> Variables);

    /// <summary>The result of evaluating an expression in a frame.</summary>
    public sealed record EvaluateResult(string Result, string? Type);

    /// <summary>Debug console output since a cursor, and the new cursor to pass next time (mirrors vscode/src/lib/stopwaiter.ts's <c>RingLog.since</c>).</summary>
    public sealed record DebugOutputResult(IReadOnlyList<string> Lines, int Cursor);

    /// <summary>
    /// One method per debug_* tool (ten), mirroring
    /// vscode/src/tools/debug.ts's <c>DebugManager</c>. Frame defaulting
    /// ("use the top frame when none is given", vscode's <c>topFrameId()</c>)
    /// is the host's job here, since <see cref="VariablesAsync"/> and
    /// <see cref="EvaluateAsync"/> take a nullable frame rather than the tool
    /// resolving it first. Only one debug session is ever active through
    /// this interface at a time — <see cref="StartAsync"/> replaces
    /// whatever was running, exactly as vscode's own <c>DebugManager.start()</c>
    /// stops a prior session first.
    /// </summary>
    public interface IDebugHost
    {
        Task<IReadOnlyList<DebugConfigInfo>> ConfigsAsync(CancellationToken ct);

        /// <summary>
        /// Starts <paramref name="config"/> (a name <see cref="ConfigsAsync"/>
        /// listed — a startup project or launch profile) or, when null, the
        /// solution's current startup project as-is, replacing any prior
        /// session. This method — and <see cref="ContinueAsync"/>
        /// and <see cref="StepAsync"/>, which share this exact contract —
        /// starts or resumes execution and then WAITS for the next stop
        /// (breakpoint hit, step complete, an unhandled exception pausing
        /// execution, or the process exiting), for AT MOST 60 SECONDS,
        /// before returning. <c>DebugTools</c> prints
        /// <c>"still running after 60s"</c> from the timeout case; it does
        /// not itself enforce any timeout — the host owns the full 60
        /// seconds. Exactly which <see cref="StopKind"/> each real-world
        /// outcome maps to:
        /// <list type="bullet">
        /// <item><see cref="StopKind.Stopped"/> — a breakpoint was hit, a
        /// step finished, or an unhandled exception paused execution, all
        /// within the 60s window. Set <see cref="StopResult.Reason"/> to a
        /// short word describing why (e.g. <c>"breakpoint"</c>,
        /// <c>"step"</c>, <c>"exception"</c>) — free text, printed verbatim
        /// by the tool as <c>"stopped ({reason})"</c>; there is no fixed
        /// vocabulary to match.</item>
        /// <item><see cref="StopKind.Timeout"/> — none of the above happened
        /// within 60 seconds and the debuggee is still running.</item>
        /// <item><see cref="StopKind.Exited"/> — the debuggee process ran to
        /// completion (normally or with a non-zero exit code) within the
        /// window; set <see cref="StopResult.ExitCode"/> when known.</item>
        /// <item><see cref="StopKind.Terminated"/> — the debug session ended
        /// some other way within the window: the debugger detached, Visual
        /// Studio tore the session down, or anything else that is not a
        /// normal process exit.</item>
        /// </list>
        /// </summary>
        Task<StopResult> StartAsync(string? config, CancellationToken ct);
        Task<IReadOnlyList<BreakpointInfo>> SetBreakpointAsync(string path, int line, BreakpointAction action, string? condition, CancellationToken ct);

        /// <summary>Resumes and waits for the next stop — see <see cref="StartAsync"/>'s doc comment for the full wait/timeout contract and the <see cref="StopKind"/> mapping, which applies here unchanged.</summary>
        Task<StopResult> ContinueAsync(CancellationToken ct);

        /// <summary>Steps (<paramref name="step"/>: over/into/out) and waits for the next stop — see <see cref="StartAsync"/>'s doc comment for the full wait/timeout contract and the <see cref="StopKind"/> mapping, which applies here unchanged.</summary>
        Task<StopResult> StepAsync(DebugStepKind step, CancellationToken ct);
        Task<IReadOnlyList<StackFrameInfo>> StackAsync(int depth, CancellationToken ct);

        /// <summary>
        /// Every scope's every variable, for <paramref name="frame"/> (the
        /// top frame when null) — <c>scope</c> is NOT a parameter here:
        /// the host returns everything it has, and
        /// <c>DebugTools</c> filters to locals/args/all by scope name (see
        /// <see cref="VariableScope"/>'s doc comment for the exact naming
        /// rule). Children (<see cref="VariableInfo.Children"/>) are
        /// resolved by the host at most ONE level deep, and only for a
        /// scope with ten or fewer variables in total — never
        /// walk the object graph beyond that. The tool then prints at most
        /// 60 variables per scope and 20 children per variable; it never
        /// asks for more than the host already resolved.
        /// </summary>
        Task<IReadOnlyList<VariableScope>> VariablesAsync(int? frame, CancellationToken ct);
        Task<EvaluateResult> EvaluateAsync(string expression, int? frame, CancellationToken ct);
        Task<DebugOutputResult> OutputAsync(int since, CancellationToken ct);
        Task StopAsync(CancellationToken ct);
    }
}
