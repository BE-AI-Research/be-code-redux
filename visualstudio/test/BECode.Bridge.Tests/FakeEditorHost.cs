using System;
using System.Collections.Generic;
using System.Threading;
using System.Threading.Tasks;
using BECode.Bridge;

namespace BECode.Bridge.Tests
{
    /// <summary>
    /// A configurable <see cref="IEditorHost"/> double for behavioural tests
    /// in <c>ToolTests.cs</c>. Every member has a settable delegate/field with
    /// a reasonable default (defaults mirror an empty/idle editor), so a test
    /// only wires up the one or two things its scenario needs.
    /// </summary>
    internal sealed class FakeEditorHost : IEditorHost
    {
        public Func<CancellationToken, Task<EditorContext>> OnGetContext { get; set; } =
            ct => Task.FromResult(new EditorContext("", 0, 0, 0, "", Array.Empty<string>()));

        // Ruling S2: workspace folders are fetched separately from context.
        // Default: empty (no solution/folder open) — tests that need a real
        // workspace set this explicitly (see ToolTests.NewHost()).
        public Func<CancellationToken, Task<IReadOnlyList<string>>> OnGetWorkspaceFolders { get; set; } =
            ct => Task.FromResult<IReadOnlyList<string>>(Array.Empty<string>());

        public List<(string Path, int? Line)> OpenCalls { get; } = new List<(string, int?)>();
        public Func<string, int?, CancellationToken, Task>? OnOpen { get; set; }

        public Func<string, int, int, CancellationToken, Task<IReadOnlyList<Location>?>>? OnDefinition { get; set; }
        public Func<string, int, int, CancellationToken, Task<IReadOnlyList<Location>?>>? OnReferences { get; set; }
        public Func<string, int, int, CancellationToken, Task<string?>>? OnHover { get; set; }

        public Func<string?, CancellationToken, Task<IReadOnlyList<Diagnostic>>> OnDiagnostics { get; set; } =
            (path, ct) => Task.FromResult<IReadOnlyList<Diagnostic>>(Array.Empty<Diagnostic>());

        public FakeDebugHost DebugHost { get; } = new FakeDebugHost();

        public Func<ReviewRequest, CancellationToken, Task<ReviewDecision>>? OnReviewDiff { get; set; }

        public Task<EditorContext> GetContextAsync(CancellationToken ct) => OnGetContext(ct);

        public Task<IReadOnlyList<string>> GetWorkspaceFoldersAsync(CancellationToken ct) => OnGetWorkspaceFolders(ct);

        public async Task OpenAsync(string path, int? line, CancellationToken ct)
        {
            OpenCalls.Add((path, line));
            if (OnOpen != null)
            {
                await OnOpen(path, line, ct).ConfigureAwait(false);
            }
        }

        public Task<IReadOnlyList<Location>?> DefinitionAsync(string path, int line, int col, CancellationToken ct) =>
            OnDefinition != null
                ? OnDefinition(path, line, col, ct)
                : Task.FromResult<IReadOnlyList<Location>?>(Array.Empty<Location>());

        public Task<IReadOnlyList<Location>?> ReferencesAsync(string path, int line, int col, CancellationToken ct) =>
            OnReferences != null
                ? OnReferences(path, line, col, ct)
                : Task.FromResult<IReadOnlyList<Location>?>(Array.Empty<Location>());

        public Task<string?> HoverAsync(string path, int line, int col, CancellationToken ct) =>
            OnHover != null ? OnHover(path, line, col, ct) : Task.FromResult<string?>("");

        public Task<IReadOnlyList<Diagnostic>> DiagnosticsAsync(string? path, CancellationToken ct) =>
            OnDiagnostics(path, ct);

        public IDebugHost Debug => DebugHost;

        public Task<ReviewDecision> ReviewDiffAsync(ReviewRequest request, CancellationToken ct) =>
            OnReviewDiff != null ? OnReviewDiff(request, ct) : Task.FromResult(ReviewDecision.Reject);
    }

    /// <summary>Configurable <see cref="IDebugHost"/> double, mirrored one delegate per debug_* tool.</summary>
    internal sealed class FakeDebugHost : IDebugHost
    {
        public Func<CancellationToken, Task<IReadOnlyList<DebugConfigInfo>>> OnConfigs { get; set; } =
            ct => Task.FromResult<IReadOnlyList<DebugConfigInfo>>(Array.Empty<DebugConfigInfo>());

        public Func<string?, CancellationToken, Task<StopResult>>? OnStart { get; set; }
        public List<string?> StartCalls { get; } = new List<string?>();

        public Func<string, int, string, string?, CancellationToken, Task<IReadOnlyList<BreakpointInfo>>>? OnSetBreakpoint { get; set; }

        public Func<CancellationToken, Task<StopResult>>? OnContinue { get; set; }
        public Func<string, CancellationToken, Task<StopResult>>? OnStep { get; set; }

        public Func<int, CancellationToken, Task<IReadOnlyList<StackFrameInfo>>> OnStack { get; set; } =
            (depth, ct) => Task.FromResult<IReadOnlyList<StackFrameInfo>>(Array.Empty<StackFrameInfo>());

        public Func<int?, string, CancellationToken, Task<IReadOnlyList<VariableScope>>> OnVariables { get; set; } =
            (frame, scope, ct) => Task.FromResult<IReadOnlyList<VariableScope>>(Array.Empty<VariableScope>());

        public Func<string, int?, CancellationToken, Task<EvaluateResult>>? OnEvaluate { get; set; }

        public Func<int, CancellationToken, Task<DebugOutputResult>> OnOutput { get; set; } =
            (since, ct) => Task.FromResult(new DebugOutputResult(Array.Empty<string>(), since));

        public bool StopCalled { get; private set; }
        public Func<CancellationToken, Task>? OnStop { get; set; }

        public Task<IReadOnlyList<DebugConfigInfo>> ConfigsAsync(CancellationToken ct) => OnConfigs(ct);

        public Task<StopResult> StartAsync(string? config, CancellationToken ct)
        {
            StartCalls.Add(config);
            return OnStart != null ? OnStart(config, ct) : Task.FromResult(new StopResult(StopKind.Terminated));
        }

        public Task<IReadOnlyList<BreakpointInfo>> SetBreakpointAsync(string path, int line, string action, string? condition, CancellationToken ct) =>
            OnSetBreakpoint != null
                ? OnSetBreakpoint(path, line, action, condition, ct)
                : Task.FromResult<IReadOnlyList<BreakpointInfo>>(Array.Empty<BreakpointInfo>());

        public Task<StopResult> ContinueAsync(CancellationToken ct) =>
            OnContinue != null ? OnContinue(ct) : Task.FromResult(new StopResult(StopKind.Terminated));

        public Task<StopResult> StepAsync(string step, CancellationToken ct) =>
            OnStep != null ? OnStep(step, ct) : Task.FromResult(new StopResult(StopKind.Terminated));

        public Task<IReadOnlyList<StackFrameInfo>> StackAsync(int depth, CancellationToken ct) => OnStack(depth, ct);

        public Task<IReadOnlyList<VariableScope>> VariablesAsync(int? frame, string scope, CancellationToken ct) => OnVariables(frame, scope, ct);

        public Task<EvaluateResult> EvaluateAsync(string expression, int? frame, CancellationToken ct) =>
            OnEvaluate != null ? OnEvaluate(expression, frame, ct) : Task.FromResult(new EvaluateResult("", null));

        public Task<DebugOutputResult> OutputAsync(int since, CancellationToken ct) => OnOutput(since, ct);

        public async Task StopAsync(CancellationToken ct)
        {
            StopCalled = true;
            if (OnStop != null)
            {
                await OnStop(ct).ConfigureAwait(false);
            }
        }
    }
}
