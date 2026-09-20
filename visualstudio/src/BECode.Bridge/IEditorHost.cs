using System.Collections.Generic;
using System.Threading;
using System.Threading.Tasks;

namespace BECode.Bridge
{
    /// <summary>
    /// What the user is looking at: active file, cursor line, selection, open
    /// files. Field names mirror the JSON object vscode/src/tools/editor.ts's
    /// <c>context</c> handler returns (file, line, selStart, selEnd,
    /// selection, open). <c>workspaceFolders</c> is NOT carried here (Ruling
    /// S2): it is fetched separately, through
    /// <see cref="IEditorHost.GetWorkspaceFoldersAsync"/>, so a tool that only
    /// needs to resolve a path does not also have to read the active editor's
    /// state.
    ///
    /// <see cref="File"/> and each entry of <see cref="Open"/> are ABSOLUTE
    /// paths (Ruling S3): the host never relativises anything —
    /// <c>EditorTools.Context</c> applies <see cref="Paths.RelPath"/> before
    /// the reply goes on the wire, exactly where vscode's own <c>context</c>
    /// handler does (against <c>folders()</c>). <see cref="Selection"/> is the
    /// RAW, untruncated selection text (Ruling S6): the 2048-character cut
    /// vscode applies is the tool's job now, not the host's.
    /// </summary>
    public sealed record EditorContext(
        string File,
        int Line,
        int SelStart,
        int SelEnd,
        string Selection,
        IReadOnlyList<string> Open);

    /// <summary>
    /// A source location. <see cref="Path"/> is an ABSOLUTE path (Ruling S3);
    /// the tool relativises it for output, exactly where vscode's own
    /// <c>definition</c>/<c>references</c> handlers do. <see cref="Text"/> is
    /// only populated by <see cref="IEditorHost.ReferencesAsync"/> (the
    /// source line's raw text, matching vscode's <c>references</c>
    /// formatting of <c>path:line: text</c>); <see cref="IEditorHost.DefinitionAsync"/>
    /// leaves it null, since vscode's <c>definition</c> formats only
    /// <c>path:line:col</c>. One record for both, as the brief's interface
    /// signature calls for.
    /// </summary>
    public sealed record Location(string Path, int Line, int Col, string? Text = null);

    /// <summary>
    /// One error/warning/info/hint from the language service, in the shape
    /// vscode/src/lib/format.ts's <c>DiagItem</c> takes: <see cref="Severity"/>
    /// is one of "error", "warning", "info", "hint". <see cref="Path"/> is an
    /// ABSOLUTE path (Ruling S3); <c>DiagnosticsTools</c> relativises it for
    /// output, exactly where vscode's own <c>diagnostics</c> handler does.
    /// </summary>
    public sealed record Diagnostic(string Path, int Line, int Col, string Severity, string Source, string Message);

    /// <summary>
    /// A proposed file change offered for review (vscode/src/tools/review.ts's
    /// <c>review_diff</c> arguments). <see cref="Proposed"/> may legally be an
    /// empty string (an emptied file, fix round 1 F4); only
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
    /// Visual Studio implementation (Task 6, over DTE / the text manager / the
    /// Error List / EnvDTE.Debugger / IVsDifferenceService) cannot be compiled
    /// on this machine, so nothing in this project may depend on anything
    /// beyond this interface.
    ///
    /// The dividing line (Ruling R-8, review round 1): everything decidable
    /// without Visual Studio lives in the tools, not here. Every method below
    /// returns raw, absolute, unfiltered, untruncated data and does
    /// UI-thread work only; filtering, truncation, path relativisation and
    /// text formatting are the tools' job.
    ///
    /// A null return from <see cref="DefinitionAsync"/>, <see cref="ReferencesAsync"/>
    /// or <see cref="HoverAsync"/> means "no language service for this file
    /// type" (Visual Studio's design deviates here: vscode always has *a*
    /// provider result, possibly empty, so those three vscode tools only ever
    /// say "no definition/references found" or "no hover information" — this
    /// project adds the "not available for this file type" case the design
    /// spec's section 4 calls for, distinguished from a provider that ran and
    /// found nothing, which is an empty list / empty string instead).
    /// </summary>
    public interface IEditorHost
    {
        Task<EditorContext> GetContextAsync(CancellationToken ct);

        /// <summary>
        /// The solution/folder roots tools confine every path argument to
        /// (Ruling S2), and that <c>context</c> reports as
        /// <c>workspaceFolders</c> (unmodified, absolute — <c>context</c> is
        /// the one place these are NOT relativised, since relativising a
        /// folder against itself is meaningless). Split out from
        /// <see cref="GetContextAsync"/> so a tool that only needs to
        /// resolve a path argument (every path-taking tool but
        /// <c>context</c> itself) does not also have to read the active
        /// editor's state.
        /// </summary>
        Task<IReadOnlyList<string>> GetWorkspaceFoldersAsync(CancellationToken ct);

        Task OpenAsync(string path, int? line, CancellationToken ct);
        Task<IReadOnlyList<Location>?> DefinitionAsync(string path, int line, int col, CancellationToken ct);

        /// <summary>
        /// Every reference, unfiltered (Ruling S5) — no <c>max</c> parameter
        /// here: the tool reports the TOTAL count and prints only the first
        /// <c>max</c> (default 50), exactly as vscode does
        /// (<c>res.length</c> read before <c>.slice()</c>).
        /// </summary>
        Task<IReadOnlyList<Location>?> ReferencesAsync(string path, int line, int col, CancellationToken ct);
        Task<string?> HoverAsync(string path, int line, int col, CancellationToken ct);

        /// <summary>
        /// Every diagnostic for <paramref name="path"/> (absolute), or for
        /// the whole solution when <paramref name="path"/> is null — no
        /// severity filtering here (Ruling S4): <c>DiagnosticsTools</c>
        /// filters with the ported <c>severitiesFor</c>.
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
        /// treats both the same way, as "cancelled" (Ruling S1). There is
        /// exactly one cancellation mechanism: this token. (An earlier draft
        /// of this interface also had <c>ReviewCancelAsync(string path)</c>;
        /// it was removed in fix round 1 — nothing ever called it.)
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
    /// two-step vscode's <c>DebugManager.describe()</c> takes.
    /// </summary>
    public sealed record StopResult(StopKind Kind, string? Reason = null, int? ExitCode = null);

    /// <summary>
    /// One startup project or launch profile, as Visual Studio understands
    /// debug configurations — the design spec's stand-in for vscode's
    /// <c>launch.json</c> entries (<c>debug_configs</c> "lists the solution's
    /// startup projects and launch profiles rather than launch.json entries").
    /// </summary>
    public sealed record DebugConfigInfo(string Name, string Kind);

    /// <summary>A breakpoint in one file, as it stands after debug_breakpoint's add/remove.</summary>
    public sealed record BreakpointInfo(int Line, string? Condition);

    /// <summary>
    /// One frame of a call stack. <see cref="Path"/> is an ABSOLUTE path
    /// (Ruling S3; <c>DebugTools</c> relativises it for output, exactly
    /// where vscode's own <c>debug_stack</c> handler does), or null when the
    /// frame has no source (matches vscode's <c>f.source?.path ? … : "?"</c>
    /// fallback, rendered by the tool).
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
    /// <see cref="IDebugHost.VariablesAsync"/>'s doc comment (Ruling S7) for
    /// the bound on how deep the host itself may go resolving them.
    /// </summary>
    public sealed record VariableInfo(string Name, string Value, string? Type, IReadOnlyList<VariableInfo>? Children = null);

    /// <summary>One DAP scope ("Locals", "Arguments", …) and its variables.</summary>
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
    /// resolving it first.
    /// </summary>
    public interface IDebugHost
    {
        Task<IReadOnlyList<DebugConfigInfo>> ConfigsAsync(CancellationToken ct);
        Task<StopResult> StartAsync(string? config, CancellationToken ct);
        Task<IReadOnlyList<BreakpointInfo>> SetBreakpointAsync(string path, int line, string action, string? condition, CancellationToken ct);
        Task<StopResult> ContinueAsync(CancellationToken ct);
        Task<StopResult> StepAsync(string step, CancellationToken ct);
        Task<IReadOnlyList<StackFrameInfo>> StackAsync(int depth, CancellationToken ct);

        /// <summary>
        /// Every scope's every variable, for <paramref name="frame"/> (the
        /// top frame when null) — unfiltered by <paramref name="scope"/>;
        /// <c>DebugTools</c> filters to locals/args/all by scope name, the
        /// same string matching vscode's <c>variables()</c> does. Children
        /// (<see cref="VariableInfo.Children"/>) are resolved by the host at
        /// most ONE level deep, and only for a scope with ten or fewer
        /// variables in total (Ruling S7) — never walk the object graph
        /// beyond that. The tool then prints at most 60 variables per scope
        /// and 20 children per variable; it never asks for more than the
        /// host already resolved.
        /// </summary>
        Task<IReadOnlyList<VariableScope>> VariablesAsync(int? frame, string scope, CancellationToken ct);
        Task<EvaluateResult> EvaluateAsync(string expression, int? frame, CancellationToken ct);
        Task<DebugOutputResult> OutputAsync(int since, CancellationToken ct);
        Task StopAsync(CancellationToken ct);
    }
}
