using System.Collections.Generic;
using System.Threading;
using System.Threading.Tasks;

namespace BECode.Bridge
{
    /// <summary>
    /// What the user is looking at: active file, cursor line, selection, open
    /// files, workspace folders. Field names mirror the JSON object
    /// vscode/src/tools/editor.ts's <c>context</c> handler returns
    /// (<c>JSON.stringify({file, line, selStart, selEnd, selection, open,
    /// workspaceFolders})</c>) so <c>EditorTools.Context</c> can serialise this
    /// record with the same shape byte-for-byte.
    /// </summary>
    public sealed record EditorContext(
        string File,
        int Line,
        int SelStart,
        int SelEnd,
        string Selection,
        IReadOnlyList<string> Open,
        IReadOnlyList<string> WorkspaceFolders);

    /// <summary>
    /// A source location. <see cref="Text"/> is only populated by
    /// <see cref="IEditorHost.ReferencesAsync"/> (the source line's text,
    /// matching vscode's <c>references</c> formatting of
    /// <c>path:line: text</c>); <see cref="IEditorHost.DefinitionAsync"/>
    /// leaves it null, since vscode's <c>definition</c> formats only
    /// <c>path:line:col</c>. One record for both, as the brief's interface
    /// signature calls for.
    /// </summary>
    public sealed record Location(string Path, int Line, int Col, string? Text = null);

    /// <summary>
    /// One error/warning/info/hint from the language service, in the shape
    /// vscode/src/lib/format.ts's <c>DiagItem</c> takes: <see cref="Severity"/>
    /// is one of "error", "warning", "info", "hint".
    /// </summary>
    public sealed record Diagnostic(string Path, int Line, int Col, string Severity, string Source, string Message);

    /// <summary>A proposed file change offered for review (vscode/src/tools/review.ts's <c>review_diff</c> arguments).</summary>
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
        Task OpenAsync(string path, int? line, CancellationToken ct);
        Task<IReadOnlyList<Location>?> DefinitionAsync(string path, int line, int col, CancellationToken ct);
        Task<IReadOnlyList<Location>?> ReferencesAsync(string path, int line, int col, int max, CancellationToken ct);
        Task<string?> HoverAsync(string path, int line, int col, CancellationToken ct);
        Task<IReadOnlyList<Diagnostic>> DiagnosticsAsync(string? path, string severity, CancellationToken ct);
        IDebugHost Debug { get; }
        Task<ReviewDecision> ReviewDiffAsync(ReviewRequest request, CancellationToken ct);
        Task<bool> ReviewCancelAsync(string path);
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

    /// <summary>One frame of a call stack. <see cref="Path"/> is null when the frame has no source (matches vscode's <c>f.source?.path ? … : "?"</c> fallback, rendered by the tool).</summary>
    public sealed record StackFrameInfo(string Name, string? Path, int Line, int FrameId);

    /// <summary>
    /// One variable. <see cref="Children"/> is non-null only when the host has
    /// already resolved this variable's nested members (mirrors vscode's
    /// on-demand DAP "variables" request against <c>variablesReference</c>);
    /// <c>DebugTools</c> decides whether to *display* them (only when the
    /// parent scope has ten or fewer variables, capped at twenty children),
    /// exactly as vscode/src/tools/debug.ts's <c>variables()</c> does — see
    /// the deviation note in the task report for why the host, not the tool,
    /// must resolve them eagerly here.
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
        Task<IReadOnlyList<VariableScope>> VariablesAsync(int? frame, string scope, CancellationToken ct);
        Task<EvaluateResult> EvaluateAsync(string expression, int? frame, CancellationToken ct);
        Task<DebugOutputResult> OutputAsync(int since, CancellationToken ct);
        Task StopAsync(CancellationToken ct);
    }
}
