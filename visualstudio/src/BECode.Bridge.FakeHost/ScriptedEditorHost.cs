using System;
using System.Collections.Generic;
using System.IO;
using System.Threading;
using System.Threading.Tasks;

namespace BECode.Bridge.FakeHost
{
    /// <summary>
    /// A plain, deterministic <see cref="IEditorHost"/> for the fake host
    /// console app: every member answers a fixed, canned value (never a
    /// delegate a caller reconfigures — that shape is
    /// <c>BECode.Bridge.Tests.FakeEditorHost</c>, used by the C# unit
    /// tests). This is the thing Task 4's Go contract test drives the real
    /// <c>internal/ide</c> client against, so its answers exist to prove the
    /// wire agrees end to end, not to model a real Visual Studio session.
    ///
    /// <see cref="ReviewDiffAsync"/> is the one member with scripted
    /// per-path behaviour, keyed by the incoming <see cref="ReviewRequest.Path"/>'s
    /// file name (case-insensitively): a path ending "accept.txt" answers
    /// <see cref="ReviewDecision.Accept"/> at once, "reject.txt" answers
    /// <see cref="ReviewDecision.Reject"/> at once, and "pending.txt" blocks
    /// until <c>ct</c> is cancelled and then answers
    /// <see cref="ReviewDecision.Cancelled"/> — the fixture the contract
    /// test's concurrent review_diff/review_cancel regression case needs.
    /// Anything else answers Accept.
    /// </summary>
    internal sealed class ScriptedEditorHost : IEditorHost
    {
        private readonly string _workspace;
        private readonly string _activeFile;

        public ScriptedEditorHost(string workspace)
        {
            _workspace = workspace;
            _activeFile = Path.Combine(_workspace, "sample.go");
            Debug = new ScriptedDebugHost(_workspace);
        }

        public IDebugHost Debug { get; }

        public Task<EditorContext> GetContextAsync(CancellationToken ct)
        {
            return Task.FromResult(new EditorContext(
                _activeFile,
                7,
                2,
                4,
                "hello",
                new[] { _activeFile }));
        }

        public Task<IReadOnlyList<string>> GetWorkspaceFoldersAsync(CancellationToken ct)
        {
            return Task.FromResult<IReadOnlyList<string>>(new[] { _workspace });
        }

        public Task OpenAsync(string path, int? line, CancellationToken ct)
        {
            // Ruling from IEditorHost.OpenAsync's own doc comment: a path
            // that does not exist throws FileNotFoundException, which
            // EditorTools.Open turns into an ordinary isError result.
            if (!File.Exists(path))
            {
                throw new FileNotFoundException(path);
            }

            return Task.CompletedTask;
        }

        public Task<IReadOnlyList<Location>?> DefinitionAsync(string path, int line, int col, CancellationToken ct)
        {
            return Task.FromResult<IReadOnlyList<Location>?>(new[]
            {
                new Location(_activeFile, 3, 5),
            });
        }

        public Task<IReadOnlyList<Location>?> ReferencesAsync(string path, int line, int col, CancellationToken ct)
        {
            return Task.FromResult<IReadOnlyList<Location>?>(new[]
            {
                new Location(_activeFile, 3, 5, "var x = scripted"),
                new Location(_activeFile, 9, 1, "return x"),
            });
        }

        public Task<string?> HoverAsync(string path, int line, int col, CancellationToken ct)
        {
            return Task.FromResult<string?>("func Scripted() int");
        }

        public Task<IReadOnlyList<Diagnostic>> DiagnosticsAsync(string? path, CancellationToken ct)
        {
            return Task.FromResult<IReadOnlyList<Diagnostic>>(new[]
            {
                new Diagnostic(_activeFile, 1, 1, "error", "fakehost", "scripted diagnostic"),
            });
        }

        public async Task<ReviewDecision> ReviewDiffAsync(ReviewRequest request, CancellationToken ct)
        {
            var name = Path.GetFileName(request.Path);

            if (string.Equals(name, "accept.txt", StringComparison.OrdinalIgnoreCase))
            {
                return ReviewDecision.Accept;
            }

            if (string.Equals(name, "reject.txt", StringComparison.OrdinalIgnoreCase))
            {
                return ReviewDecision.Reject;
            }

            if (string.Equals(name, "pending.txt", StringComparison.OrdinalIgnoreCase))
            {
                // Blocks until review_cancel (or the connection going away)
                // cancels ct — the fixture the contract test's concurrency
                // regression case exercises. IEditorHost.ReviewDiffAsync's
                // own doc comment (Ruling S1/D4) allows either answering
                // Cancelled or throwing OperationCanceledException; this
                // host does the former, explicitly.
                var tcs = new TaskCompletionSource<bool>();
                using (ct.Register(() => tcs.TrySetResult(true)))
                {
                    await tcs.Task.ConfigureAwait(false);
                }

                return ReviewDecision.Cancelled;
            }

            return ReviewDecision.Accept;
        }
    }

    /// <summary>Deterministic <see cref="IDebugHost"/> for the fake host, one canned answer per debug_* tool.</summary>
    internal sealed class ScriptedDebugHost : IDebugHost
    {
        private readonly string _workspace;
        private readonly string _mainFile;

        public ScriptedDebugHost(string workspace)
        {
            _workspace = workspace;
            _mainFile = Path.Combine(_workspace, "sample.go");
        }

        public Task<IReadOnlyList<DebugConfigInfo>> ConfigsAsync(CancellationToken ct)
        {
            return Task.FromResult<IReadOnlyList<DebugConfigInfo>>(new[]
            {
                new DebugConfigInfo("FakeApp", "startup project"),
            });
        }

        public Task<StopResult> StartAsync(string? config, CancellationToken ct)
        {
            return Task.FromResult(new StopResult(StopKind.Stopped, "breakpoint"));
        }

        public Task<IReadOnlyList<BreakpointInfo>> SetBreakpointAsync(string path, int line, BreakpointAction action, string? condition, CancellationToken ct)
        {
            return Task.FromResult<IReadOnlyList<BreakpointInfo>>(new[]
            {
                new BreakpointInfo(line, condition),
            });
        }

        public Task<StopResult> ContinueAsync(CancellationToken ct)
        {
            return Task.FromResult(new StopResult(StopKind.Stopped, "step"));
        }

        public Task<StopResult> StepAsync(DebugStepKind step, CancellationToken ct)
        {
            return Task.FromResult(new StopResult(StopKind.Stopped, "step"));
        }

        public Task<IReadOnlyList<StackFrameInfo>> StackAsync(int depth, CancellationToken ct)
        {
            return Task.FromResult<IReadOnlyList<StackFrameInfo>>(new[]
            {
                new StackFrameInfo("main.main", _mainFile, 10, 1),
            });
        }

        public Task<IReadOnlyList<VariableScope>> VariablesAsync(int? frame, CancellationToken ct)
        {
            var vars = new[] { new VariableInfo("x", "42", "int") };
            return Task.FromResult<IReadOnlyList<VariableScope>>(new[]
            {
                new VariableScope("Locals", vars),
            });
        }

        public Task<EvaluateResult> EvaluateAsync(string expression, int? frame, CancellationToken ct)
        {
            return Task.FromResult(new EvaluateResult("42", "int"));
        }

        public Task<DebugOutputResult> OutputAsync(int since, CancellationToken ct)
        {
            return Task.FromResult(new DebugOutputResult(new[] { "scripted output line" }, since + 1));
        }

        public Task StopAsync(CancellationToken ct)
        {
            return Task.CompletedTask;
        }
    }
}
