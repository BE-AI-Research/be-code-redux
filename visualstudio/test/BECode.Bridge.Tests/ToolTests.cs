using System;
using System.Collections.Generic;
using System.IO;
using System.Linq;
using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;
using BECode.Bridge;
using Xunit;

namespace BECode.Bridge.Tests
{
    // Behavioural tests for the eighteen tools, over FakeEditorHost. Text
    // formats are checked against vscode/src/lib/format.ts and
    // vscode/src/tools/*.ts's own formatting (see the per-tool comments for
    // which vscode source each case ports).
    public class ToolTests : IDisposable
    {
        private readonly string _workspace;
        private readonly string[] _folders;

        public ToolTests()
        {
            _workspace = Directory.CreateTempSubdirectory("be-code-tools-").FullName;
            File.WriteAllText(Path.Combine(_workspace, "sample.go"), "package main\n");
            _folders = new[] { _workspace };
        }

        public void Dispose()
        {
            try { Directory.Delete(_workspace, recursive: true); } catch { /* best effort */ }
        }

        private static JsonElement Args(string json)
        {
            using var doc = JsonDocument.Parse(json);
            return doc.RootElement.Clone();
        }

        private EditorContext DefaultContext() => new EditorContext("", 0, 0, 0, "", Array.Empty<string>(), _folders);

        private FakeEditorHost NewHost() => new FakeEditorHost { OnGetContext = ct => Task.FromResult(DefaultContext()) };

        // ---- context (vscode/src/tools/editor.ts's "context" handler) ----

        [Fact]
        public async Task ContextSerialisesTheHostsContextInTheVsCodeShape()
        {
            var host = new FakeEditorHost
            {
                OnGetContext = ct => Task.FromResult(new EditorContext(
                    "a.go", 5, 2, 4, "hello", new[] { "a.go", "b.go" }, new[] { "/ws" })),
            };
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("context", Args("{}"), new object(), CancellationToken.None);

            Assert.False(result.IsError);
            using var doc = JsonDocument.Parse(result.Text);
            var root = doc.RootElement;
            Assert.Equal("a.go", root.GetProperty("file").GetString());
            Assert.Equal(5, root.GetProperty("line").GetInt32());
            Assert.Equal(2, root.GetProperty("selStart").GetInt32());
            Assert.Equal(4, root.GetProperty("selEnd").GetInt32());
            Assert.Equal("hello", root.GetProperty("selection").GetString());
            Assert.Equal(new[] { "a.go", "b.go" }, root.GetProperty("open").EnumerateArray().Select(e => e.GetString()));
            Assert.Equal(new[] { "/ws" }, root.GetProperty("workspaceFolders").EnumerateArray().Select(e => e.GetString()));
        }

        // ---- open ----

        [Fact]
        public async Task OpenReturnsOpenedPathWithLineAndResolvesAnAbsolutePathToTheHost()
        {
            var host = NewHost();
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("open", Args("{\"path\":\"sample.go\",\"line\":3}"), new object(), CancellationToken.None);

            Assert.False(result.IsError);
            Assert.Equal("opened sample.go:3", result.Text);
            var call = Assert.Single(host.OpenCalls);
            Assert.Equal(Path.Combine(_workspace, "sample.go"), call.Path);
            Assert.Equal(3, call.Line);
        }

        [Fact]
        public async Task OpenWithoutLineOmitsTheColonSuffix()
        {
            var host = NewHost();
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("open", Args("{\"path\":\"sample.go\"}"), new object(), CancellationToken.None);

            Assert.Equal("opened sample.go", result.Text);
        }

        [Fact]
        public async Task OpenMissingPathIsErrorNamingTheArgument()
        {
            var registry = new ToolRegistry(NewHost());

            var result = await registry.CallAsync("open", Args("{}"), new object(), CancellationToken.None);

            Assert.True(result.IsError);
            Assert.Contains("path", result.Text, StringComparison.Ordinal);
        }

        [Fact]
        public async Task OpenRejectsAPathOutsideTheWorkspaceWithoutCallingTheHost()
        {
            var host = NewHost();
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("open", Args("{\"path\":\"../outside.txt\"}"), new object(), CancellationToken.None);

            Assert.True(result.IsError);
            Assert.Contains("outside the workspace", result.Text, StringComparison.Ordinal);
            Assert.Empty(host.OpenCalls);
        }

        // ---- definition ----

        [Fact]
        public async Task DefinitionFormatsPathLineColPerLocation()
        {
            var host = NewHost();
            host.OnDefinition = (path, line, col, ct) =>
                Task.FromResult<System.Collections.Generic.IReadOnlyList<Location>?>(new[] { new Location("a.go", 10, 2) });
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("definition", Args("{\"path\":\"sample.go\",\"line\":1,\"col\":1}"), new object(), CancellationToken.None);

            Assert.False(result.IsError);
            Assert.Equal("a.go:10:2", result.Text);
        }

        [Fact]
        public async Task DefinitionWithNoResultsSaysSoWithoutError()
        {
            var host = NewHost();
            host.OnDefinition = (path, line, col, ct) => Task.FromResult<System.Collections.Generic.IReadOnlyList<Location>?>(Array.Empty<Location>());
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("definition", Args("{\"path\":\"sample.go\",\"line\":1,\"col\":1}"), new object(), CancellationToken.None);

            Assert.False(result.IsError);
            Assert.Equal("no definition found (is the language server running and the file saved?)", result.Text);
        }

        [Fact]
        public async Task DefinitionOnAHostThatReturnsNullAnswersNotAvailableForThisFileType()
        {
            var host = NewHost();
            host.OnDefinition = (path, line, col, ct) => Task.FromResult<System.Collections.Generic.IReadOnlyList<Location>?>(null);
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("definition", Args("{\"path\":\"sample.go\",\"line\":1,\"col\":1}"), new object(), CancellationToken.None);

            Assert.True(result.IsError);
            Assert.Equal("not available for this file type", result.Text);
        }

        [Theory]
        [InlineData("{\"line\":1,\"col\":1}", "path")]
        [InlineData("{\"path\":\"sample.go\",\"col\":1}", "line")]
        [InlineData("{\"path\":\"sample.go\",\"line\":1}", "col")]
        public async Task DefinitionMissingRequiredArgumentIsErrorNamingIt(string argsJson, string missing)
        {
            var registry = new ToolRegistry(NewHost());

            var result = await registry.CallAsync("definition", Args(argsJson), new object(), CancellationToken.None);

            Assert.True(result.IsError);
            Assert.Contains(missing, result.Text, StringComparison.Ordinal);
        }

        [Fact]
        public async Task DefinitionRejectsAPathOutsideTheWorkspaceWithoutCallingTheHost()
        {
            var host = NewHost();
            var called = false;
            host.OnDefinition = (path, line, col, ct) => { called = true; return Task.FromResult<System.Collections.Generic.IReadOnlyList<Location>?>(Array.Empty<Location>()); };
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("definition", Args("{\"path\":\"../outside.txt\",\"line\":1,\"col\":1}"), new object(), CancellationToken.None);

            Assert.True(result.IsError);
            Assert.Contains("outside the workspace", result.Text, StringComparison.Ordinal);
            Assert.False(called);
        }

        // ---- references ----

        [Fact]
        public async Task ReferencesFormatsCountAndPathLineText()
        {
            var host = NewHost();
            host.OnReferences = (path, line, col, max, ct) =>
                Task.FromResult<System.Collections.Generic.IReadOnlyList<Location>?>(new[] { new Location("a.go", 3, 1, "  x := 1  ") });
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("references", Args("{\"path\":\"sample.go\",\"line\":1,\"col\":1}"), new object(), CancellationToken.None);

            Assert.False(result.IsError);
            Assert.Equal("1 reference(s)\na.go:3: x := 1", result.Text);
        }

        [Fact]
        public async Task ReferencesDefaultsMaxTo50()
        {
            var host = NewHost();
            int? seenMax = null;
            host.OnReferences = (path, line, col, max, ct) => { seenMax = max; return Task.FromResult<System.Collections.Generic.IReadOnlyList<Location>?>(Array.Empty<Location>()); };
            var registry = new ToolRegistry(host);

            await registry.CallAsync("references", Args("{\"path\":\"sample.go\",\"line\":1,\"col\":1}"), new object(), CancellationToken.None);

            Assert.Equal(50, seenMax);
        }

        [Fact]
        public async Task ReferencesWithNoResultsSaysSoWithoutError()
        {
            var host = NewHost();
            host.OnReferences = (path, line, col, max, ct) => Task.FromResult<System.Collections.Generic.IReadOnlyList<Location>?>(Array.Empty<Location>());
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("references", Args("{\"path\":\"sample.go\",\"line\":1,\"col\":1}"), new object(), CancellationToken.None);

            Assert.False(result.IsError);
            Assert.Equal("no references found", result.Text);
        }

        [Fact]
        public async Task ReferencesOnAHostThatReturnsNullAnswersNotAvailableForThisFileType()
        {
            var host = NewHost();
            host.OnReferences = (path, line, col, max, ct) => Task.FromResult<System.Collections.Generic.IReadOnlyList<Location>?>(null);
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("references", Args("{\"path\":\"sample.go\",\"line\":1,\"col\":1}"), new object(), CancellationToken.None);

            Assert.True(result.IsError);
            Assert.Equal("not available for this file type", result.Text);
        }

        // ---- hover ----

        [Fact]
        public async Task HoverReturnsTheHostsTextTrimmed()
        {
            var host = NewHost();
            host.OnHover = (path, line, col, ct) => Task.FromResult<string?>("  func Foo() int  ");
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("hover", Args("{\"path\":\"sample.go\",\"line\":1,\"col\":1}"), new object(), CancellationToken.None);

            Assert.False(result.IsError);
            Assert.Equal("func Foo() int", result.Text);
        }

        [Fact]
        public async Task HoverWithEmptyTextSaysSoWithoutError()
        {
            var host = NewHost();
            host.OnHover = (path, line, col, ct) => Task.FromResult<string?>("");
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("hover", Args("{\"path\":\"sample.go\",\"line\":1,\"col\":1}"), new object(), CancellationToken.None);

            Assert.False(result.IsError);
            Assert.Equal("no hover information", result.Text);
        }

        [Fact]
        public async Task HoverOnAHostThatReturnsNullAnswersNotAvailableForThisFileType()
        {
            var host = NewHost();
            host.OnHover = (path, line, col, ct) => Task.FromResult<string?>(null);
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("hover", Args("{\"path\":\"sample.go\",\"line\":1,\"col\":1}"), new object(), CancellationToken.None);

            Assert.True(result.IsError);
            Assert.Equal("not available for this file type", result.Text);
        }

        // ---- diagnostics (vscode/src/lib/format.ts's formatDiagnostics/severitiesFor) ----

        [Fact]
        public async Task DiagnosticsDefaultsSeverityToAll()
        {
            var host = NewHost();
            string? seenSeverity = "not set";
            host.OnDiagnostics = (path, severity, ct) => { seenSeverity = severity; return Task.FromResult<System.Collections.Generic.IReadOnlyList<Diagnostic>>(Array.Empty<Diagnostic>()); };
            var registry = new ToolRegistry(host);

            await registry.CallAsync("diagnostics", Args("{}"), new object(), CancellationToken.None);

            Assert.Equal("all", seenSeverity);
        }

        [Fact]
        public async Task DiagnosticsPassesAnExplicitSeverityThrough()
        {
            var host = NewHost();
            string? seenSeverity = null;
            host.OnDiagnostics = (path, severity, ct) => { seenSeverity = severity; return Task.FromResult<System.Collections.Generic.IReadOnlyList<Diagnostic>>(Array.Empty<Diagnostic>()); };
            var registry = new ToolRegistry(host);

            await registry.CallAsync("diagnostics", Args("{\"severity\":\"error\"}"), new object(), CancellationToken.None);

            Assert.Equal("error", seenSeverity);
        }

        [Fact]
        public async Task DiagnosticsSaysSoWhenClean()
        {
            var registry = new ToolRegistry(NewHost());

            var result = await registry.CallAsync("diagnostics", Args("{}"), new object(), CancellationToken.None);

            Assert.Equal("no diagnostics", result.Text);
        }

        [Fact]
        public async Task DiagnosticsGroupsByFileWithACountSummaryFirst()
        {
            // Ported from vscode/test/format.test.ts's "groups by file with a count summary first".
            var host = NewHost();
            host.OnDiagnostics = (path, severity, ct) => Task.FromResult<System.Collections.Generic.IReadOnlyList<Diagnostic>>(new[]
            {
                new Diagnostic("b.go", 3, 1, "error", "go", "undefined: x"),
                new Diagnostic("a.py", 10, 5, "warning", "Pylance", "unused"),
                new Diagnostic("b.go", 1, 1, "error", "go", "missing import"),
            });
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("diagnostics", Args("{}"), new object(), CancellationToken.None);

            var firstLine = result.Text.Split('\n')[0];
            Assert.Equal("2 errors, 1 warning in 2 files", firstLine);
            Assert.Contains("b.go:1:1 error go: missing import", result.Text, StringComparison.Ordinal);
            Assert.True(result.Text.IndexOf("a.py", StringComparison.Ordinal) < result.Text.IndexOf("b.go", StringComparison.Ordinal));
        }

        // ---- review_diff / review_cancel (vscode/src/tools/review.ts, vscode/test/review.test.ts) ----

        private static async Task WaitForAsync(Func<bool> condition, TimeSpan timeout)
        {
            var deadline = DateTime.UtcNow + timeout;
            while (!condition())
            {
                if (DateTime.UtcNow >= deadline)
                {
                    throw new TimeoutException($"condition not met within {timeout}");
                }

                await Task.Delay(10);
            }
        }

        [Theory]
        [InlineData("{\"proposed\":\"new\"}", "path")]
        [InlineData("{\"path\":\"a.txt\"}", "proposed")]
        public async Task ReviewDiffMissingRequiredArgumentIsErrorNamingIt(string argsJson, string missing)
        {
            var registry = new ToolRegistry(NewHost());

            var result = await registry.CallAsync("review_diff", Args(argsJson), new object(), CancellationToken.None);

            Assert.True(result.IsError);
            Assert.Contains(missing, result.Text, StringComparison.Ordinal);
        }

        [Fact]
        public async Task ReviewCancelMissingPathIsErrorNamingIt()
        {
            var registry = new ToolRegistry(NewHost());

            var result = await registry.CallAsync("review_cancel", Args("{}"), new object(), CancellationToken.None);

            Assert.True(result.IsError);
            Assert.Contains("path", result.Text, StringComparison.Ordinal);
        }

        [Theory]
        [InlineData(ReviewDecision.Accept, "{\"decision\":\"accept\"}")]
        [InlineData(ReviewDecision.Reject, "{\"decision\":\"reject\"}")]
        [InlineData(ReviewDecision.AcceptAll, "{\"decision\":\"accept_all\"}")]
        [InlineData(ReviewDecision.Cancelled, "{\"decision\":\"cancelled\"}")]
        public async Task ReviewDiffSerialisesEachDecisionExactly(ReviewDecision decision, string expected)
        {
            var host = new FakeEditorHost { OnReviewDiff = (req, ct) => Task.FromResult(decision) };
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("review_diff", Args("{\"path\":\"a.txt\",\"proposed\":\"new\"}"), new object(), CancellationToken.None);

            Assert.False(result.IsError);
            Assert.Equal(expected, result.Text);
        }

        [Fact]
        public async Task AcceptAllIsRememberedForTheConnectionAndSkipsAskingTheHostAgain()
        {
            var callCount = 0;
            var host = new FakeEditorHost { OnReviewDiff = (req, ct) => { callCount++; return Task.FromResult(ReviewDecision.AcceptAll); } };
            var registry = new ToolRegistry(host);
            var connA = new object();
            var connB = new object();

            var first = await registry.CallAsync("review_diff", Args("{\"path\":\"a.txt\",\"proposed\":\"new\"}"), connA, CancellationToken.None);
            Assert.Equal("{\"decision\":\"accept_all\"}", first.Text);
            Assert.Equal(1, callCount);

            // Same connection, a different path: accept-all is per connection, not per path.
            var second = await registry.CallAsync("review_diff", Args("{\"path\":\"b.txt\",\"proposed\":\"new\"}"), connA, CancellationToken.None);
            Assert.Equal("{\"decision\":\"accept\"}", second.Text);
            Assert.Equal(1, callCount); // host not asked again

            // A different connection still asks.
            var third = await registry.CallAsync("review_diff", Args("{\"path\":\"a.txt\",\"proposed\":\"new\"}"), connB, CancellationToken.None);
            Assert.Equal("{\"decision\":\"accept_all\"}", third.Text);
            Assert.Equal(2, callCount);
        }

        [Fact]
        public async Task ConnectionClosedDropsAcceptAllState()
        {
            var host = new FakeEditorHost { OnReviewDiff = (req, ct) => Task.FromResult(ReviewDecision.AcceptAll) };
            var registry = new ToolRegistry(host);
            var conn = new object();

            await registry.CallAsync("review_diff", Args("{\"path\":\"a.txt\",\"proposed\":\"new\"}"), conn, CancellationToken.None);
            registry.ConnectionClosed(conn);

            host.OnReviewDiff = (req, ct) => Task.FromResult(ReviewDecision.Reject);
            var result = await registry.CallAsync("review_diff", Args("{\"path\":\"a.txt\",\"proposed\":\"new\"}"), conn, CancellationToken.None);

            Assert.Equal("{\"decision\":\"reject\"}", result.Text);
        }

        [Fact]
        public async Task ReviewCancelForAnUnknownPathReturnsCancelledFalse()
        {
            var registry = new ToolRegistry(NewHost());

            var result = await registry.CallAsync("review_cancel", Args("{\"path\":\"nope.txt\"}"), new object(), CancellationToken.None);

            Assert.Equal("{\"cancelled\":false}", result.Text);
        }

        [Fact]
        public async Task ReviewCancelResolvesAPendingReviewDiffAsCancelled()
        {
            var hostCalled = new TaskCompletionSource<bool>();
            var host = new FakeEditorHost
            {
                OnReviewDiff = async (req, ct) =>
                {
                    var tcs = new TaskCompletionSource<bool>();
                    using (ct.Register(() => tcs.TrySetResult(true)))
                    {
                        hostCalled.SetResult(true);
                        await tcs.Task;
                    }

                    return ReviewDecision.Cancelled;
                },
            };
            var registry = new ToolRegistry(host);
            var conn = new object();

            var diffTask = registry.CallAsync("review_diff", Args("{\"path\":\"a.txt\",\"proposed\":\"new\"}"), conn, CancellationToken.None);
            await hostCalled.Task;

            var cancelResult = await registry.CallAsync("review_cancel", Args("{\"path\":\"a.txt\"}"), conn, CancellationToken.None);
            Assert.Equal("{\"cancelled\":true}", cancelResult.Text);

            var diffResult = await diffTask;
            Assert.Equal("{\"decision\":\"cancelled\"}", diffResult.Text);
        }

        [Fact]
        public async Task ReviewCancelAfterTheDecisionAlreadyResolvedReturnsCancelledFalse()
        {
            var host = new FakeEditorHost { OnReviewDiff = (req, ct) => Task.FromResult(ReviewDecision.Accept) };
            var registry = new ToolRegistry(host);
            var conn = new object();

            var diffResult = await registry.CallAsync("review_diff", Args("{\"path\":\"a.txt\",\"proposed\":\"new\"}"), conn, CancellationToken.None);
            Assert.Equal("{\"decision\":\"accept\"}", diffResult.Text);

            var cancelResult = await registry.CallAsync("review_cancel", Args("{\"path\":\"a.txt\"}"), conn, CancellationToken.None);
            Assert.Equal("{\"cancelled\":false}", cancelResult.Text);
        }

        [Fact]
        public async Task TwoConnectionsReviewingTheSamePathAreIsolatedFromEachOthersCancel()
        {
            var resolvers = new List<TaskCompletionSource<ReviewDecision>>();
            var host = new FakeEditorHost
            {
                OnReviewDiff = (req, ct) =>
                {
                    var tcs = new TaskCompletionSource<ReviewDecision>();
                    lock (resolvers)
                    {
                        resolvers.Add(tcs);
                    }

                    ct.Register(() => tcs.TrySetResult(ReviewDecision.Cancelled));
                    return tcs.Task;
                },
            };
            var registry = new ToolRegistry(host);
            var connA = new object();
            var connB = new object();

            var diffA = registry.CallAsync("review_diff", Args("{\"path\":\"shared.txt\",\"proposed\":\"new\"}"), connA, CancellationToken.None);
            await WaitForAsync(() => resolvers.Count >= 1, TimeSpan.FromSeconds(5));
            var diffB = registry.CallAsync("review_diff", Args("{\"path\":\"shared.txt\",\"proposed\":\"new\"}"), connB, CancellationToken.None);
            await WaitForAsync(() => resolvers.Count >= 2, TimeSpan.FromSeconds(5));

            var cancelA = await registry.CallAsync("review_cancel", Args("{\"path\":\"shared.txt\"}"), connA, CancellationToken.None);
            Assert.Equal("{\"cancelled\":true}", cancelA.Text);
            Assert.Equal("{\"decision\":\"cancelled\"}", (await diffA).Text);

            var cancelAAgain = await registry.CallAsync("review_cancel", Args("{\"path\":\"shared.txt\"}"), connA, CancellationToken.None);
            Assert.Equal("{\"cancelled\":false}", cancelAAgain.Text);

            // B was never touched by any of the above.
            resolvers[1].SetResult(ReviewDecision.Accept);
            Assert.Equal("{\"decision\":\"accept\"}", (await diffB).Text);
        }

        [Fact]
        public void BothReviewToolsAreOmittedFromList()
        {
            var registry = new ToolRegistry(NewHost());

            var names = registry.List().Select(t => t.Name).ToList();

            Assert.DoesNotContain("review_diff", names);
            Assert.DoesNotContain("review_cancel", names);
        }

        // ---- debug_* (vscode/src/tools/debug.ts's DebugManager) ----

        [Fact]
        public async Task DebugConfigsFormatsNameAndKindPerEntry()
        {
            var host = NewHost();
            host.DebugHost.OnConfigs = ct => Task.FromResult<IReadOnlyList<DebugConfigInfo>>(new[]
            {
                new DebugConfigInfo("WebApp", "Startup Project"),
                new DebugConfigInfo("Attach to IIS", "Launch Profile"),
            });
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("debug_configs", Args("{}"), new object(), CancellationToken.None);

            Assert.Equal("WebApp (Startup Project)\nAttach to IIS (Launch Profile)", result.Text);
        }

        [Fact]
        public async Task DebugConfigsWithNoneSaysSoWithoutError()
        {
            var registry = new ToolRegistry(NewHost());

            var result = await registry.CallAsync("debug_configs", Args("{}"), new object(), CancellationToken.None);

            Assert.False(result.IsError);
            Assert.Equal("no startup projects or launch profiles found", result.Text);
        }

        [Theory]
        [InlineData("{\"program\":\"./main.go\",\"type\":\"go\"}")]
        [InlineData("{\"program\":\"./main.go\"}")]
        [InlineData("{\"type\":\"python\"}")]
        public async Task DebugStartWithTheVsCodeOnlyProgramTypeShapeIsRefused(string argsJson)
        {
            var host = NewHost();
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("debug_start", Args(argsJson), new object(), CancellationToken.None);

            Assert.True(result.IsError);
            Assert.Contains("Visual Studio debugs the startup project", result.Text, StringComparison.Ordinal);
            Assert.Empty(host.DebugHost.StartCalls);
        }

        [Fact]
        public async Task DebugStartWithConfigCallsHostAndDescribesAStop()
        {
            var host = NewHost();
            host.DebugHost.OnStart = (config, ct) => Task.FromResult(new StopResult(StopKind.Stopped, "breakpoint"));
            host.DebugHost.OnStack = (depth, ct) => Task.FromResult<IReadOnlyList<StackFrameInfo>>(new[]
            {
                new StackFrameInfo("main.main", "main.go", 12, 1),
            });
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("debug_start", Args("{\"config\":\"WebApp\"}"), new object(), CancellationToken.None);

            Assert.False(result.IsError);
            Assert.Equal("WebApp", Assert.Single(host.DebugHost.StartCalls));
            Assert.Equal("stopped (breakpoint)\n#0 main.main main.go:12  [frame 1]", result.Text);
        }

        [Fact]
        public async Task DebugStartWithNoConfigStartsTheDefaultStartupProject()
        {
            var host = NewHost();
            host.DebugHost.OnStart = (config, ct) => Task.FromResult(new StopResult(StopKind.Timeout));
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("debug_start", Args("{}"), new object(), CancellationToken.None);

            Assert.False(result.IsError);
            Assert.Null(Assert.Single(host.DebugHost.StartCalls));
            Assert.Equal("still running after 60s (no breakpoint hit); use debug_output or debug_stop", result.Text);
        }

        [Fact]
        public async Task DebugStartExitedFormatsTheExitCode()
        {
            var host = NewHost();
            host.DebugHost.OnStart = (config, ct) => Task.FromResult(new StopResult(StopKind.Exited, ExitCode: 2));
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("debug_start", Args("{}"), new object(), CancellationToken.None);

            Assert.Equal("program exited with code 2", result.Text);
        }

        [Fact]
        public async Task DebugStartTerminatedFormatsAFixedMessage()
        {
            var host = NewHost();
            host.DebugHost.OnStart = (config, ct) => Task.FromResult(new StopResult(StopKind.Terminated));
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("debug_start", Args("{}"), new object(), CancellationToken.None);

            Assert.Equal("debug session terminated", result.Text);
        }

        [Fact]
        public async Task DebugStartFallsBackWhenTheStackCannotBeReadAfterAStop()
        {
            var host = NewHost();
            host.DebugHost.OnStart = (config, ct) => Task.FromResult(new StopResult(StopKind.Stopped, "step"));
            host.DebugHost.OnStack = (depth, ct) => throw new InvalidOperationException("session gone");
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("debug_start", Args("{}"), new object(), CancellationToken.None);

            Assert.Equal("stopped (step)\n(session ended before the stack could be read)", result.Text);
        }

        [Theory]
        [InlineData("{\"line\":1}", "path")]
        [InlineData("{\"path\":\"sample.go\"}", "line")]
        public async Task DebugBreakpointMissingRequiredArgumentIsErrorNamingIt(string argsJson, string missing)
        {
            var registry = new ToolRegistry(NewHost());

            var result = await registry.CallAsync("debug_breakpoint", Args(argsJson), new object(), CancellationToken.None);

            Assert.True(result.IsError);
            Assert.Contains(missing, result.Text, StringComparison.Ordinal);
        }

        [Fact]
        public async Task DebugBreakpointFormatsTheBreakpointsInTheFile()
        {
            var host = NewHost();
            host.DebugHost.OnSetBreakpoint = (path, line, action, condition, ct) =>
                Task.FromResult<IReadOnlyList<BreakpointInfo>>(new[] { new BreakpointInfo(5, "x > 1"), new BreakpointInfo(9, null) });
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("debug_breakpoint", Args("{\"path\":\"sample.go\",\"line\":5,\"condition\":\"x > 1\"}"), new object(), CancellationToken.None);

            Assert.Equal("sample.go:5 if x > 1\nsample.go:9", result.Text);
        }

        [Fact]
        public async Task DebugBreakpointWithNoneLeftSaysSoWithoutError()
        {
            var host = NewHost();
            host.DebugHost.OnSetBreakpoint = (path, line, action, condition, ct) => Task.FromResult<IReadOnlyList<BreakpointInfo>>(Array.Empty<BreakpointInfo>());
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("debug_breakpoint", Args("{\"path\":\"sample.go\",\"line\":5,\"action\":\"remove\"}"), new object(), CancellationToken.None);

            Assert.False(result.IsError);
            Assert.Equal("no breakpoints in sample.go", result.Text);
        }

        [Fact]
        public async Task DebugBreakpointRejectsAPathOutsideTheWorkspaceWithoutCallingTheHost()
        {
            var host = NewHost();
            var called = false;
            host.DebugHost.OnSetBreakpoint = (path, line, action, condition, ct) => { called = true; return Task.FromResult<IReadOnlyList<BreakpointInfo>>(Array.Empty<BreakpointInfo>()); };
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("debug_breakpoint", Args("{\"path\":\"../outside.txt\",\"line\":1}"), new object(), CancellationToken.None);

            Assert.True(result.IsError);
            Assert.Contains("outside the workspace", result.Text, StringComparison.Ordinal);
            Assert.False(called);
        }

        [Fact]
        public async Task DebugContinueDescribesTheNextStop()
        {
            var host = NewHost();
            host.DebugHost.OnContinue = ct => Task.FromResult(new StopResult(StopKind.Terminated));
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("debug_continue", Args("{}"), new object(), CancellationToken.None);

            Assert.Equal("debug session terminated", result.Text);
        }

        [Fact]
        public async Task DebugStepMissingRequiredStepArgumentIsErrorNamingIt()
        {
            var registry = new ToolRegistry(NewHost());

            var result = await registry.CallAsync("debug_step", Args("{}"), new object(), CancellationToken.None);

            Assert.True(result.IsError);
            Assert.Contains("step", result.Text, StringComparison.Ordinal);
        }

        [Theory]
        [InlineData("over")]
        [InlineData("into")]
        [InlineData("out")]
        public async Task DebugStepPassesTheStepValueThroughToTheHost(string step)
        {
            var host = NewHost();
            string? seen = null;
            host.DebugHost.OnStep = (s, ct) => { seen = s; return Task.FromResult(new StopResult(StopKind.Terminated)); };
            var registry = new ToolRegistry(host);

            await registry.CallAsync("debug_step", Args($"{{\"step\":\"{step}\"}}"), new object(), CancellationToken.None);

            Assert.Equal(step, seen);
        }

        [Fact]
        public async Task DebugStackFormatsFramesAndDefaultsDepthTo10()
        {
            var host = NewHost();
            int? seenDepth = null;
            host.DebugHost.OnStack = (depth, ct) => { seenDepth = depth; return Task.FromResult<IReadOnlyList<StackFrameInfo>>(new[]
            {
                new StackFrameInfo("main.main", "main.go", 12, 1),
                new StackFrameInfo("main.helper", null, 4, 2),
            }); };
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("debug_stack", Args("{}"), new object(), CancellationToken.None);

            Assert.Equal(10, seenDepth);
            Assert.Equal("#0 main.main main.go:12  [frame 1]\n#1 main.helper ?:4  [frame 2]", result.Text);
        }

        [Fact]
        public async Task DebugStackWithNoFramesSaysSo()
        {
            var registry = new ToolRegistry(NewHost());

            var result = await registry.CallAsync("debug_stack", Args("{}"), new object(), CancellationToken.None);

            Assert.Equal("no frames", result.Text);
        }

        [Fact]
        public async Task DebugVariablesDefaultsScopeToLocalsAndFiltersByName()
        {
            var host = NewHost();
            string? seenScope = null;
            host.DebugHost.OnVariables = (frame, scope, ct) =>
            {
                seenScope = scope;
                return Task.FromResult<IReadOnlyList<VariableScope>>(new[]
                {
                    new VariableScope("Locals", new[] { new VariableInfo("x", "1", "int") }),
                    new VariableScope("Arguments", new[] { new VariableInfo("a", "2", "int") }),
                });
            };
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("debug_variables", Args("{}"), new object(), CancellationToken.None);

            Assert.Equal("locals", seenScope);
            Assert.Equal("Locals:\n  x = 1 (int)", result.Text);
        }

        [Fact]
        public async Task DebugVariablesScopeAllShowsEveryScope()
        {
            var host = NewHost();
            host.DebugHost.OnVariables = (frame, scope, ct) => Task.FromResult<IReadOnlyList<VariableScope>>(new[]
            {
                new VariableScope("Locals", new[] { new VariableInfo("x", "1", "int") }),
                new VariableScope("Arguments", new[] { new VariableInfo("a", "2", "int") }),
            });
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("debug_variables", Args("{\"scope\":\"all\"}"), new object(), CancellationToken.None);

            Assert.Equal("Locals:\n  x = 1 (int)\nArguments:\n  a = 2 (int)", result.Text);
        }

        [Fact]
        public async Task DebugVariablesExpandsChildrenOnlyWhenTheScopeHasTenOrFewerVariables()
        {
            var childVar = new VariableInfo("Field", "val", null);
            var withChildren = new VariableInfo("y", "&Foo{...}", "*main.Foo", new[] { childVar });

            // Small scope (<=10 vars): children of a variable that has them are shown.
            var hostSmall = NewHost();
            hostSmall.DebugHost.OnVariables = (frame, scope, ct) => Task.FromResult<IReadOnlyList<VariableScope>>(new[]
            {
                new VariableScope("Locals", new[] { new VariableInfo("x", "1", "int"), withChildren }),
            });
            var registrySmall = new ToolRegistry(hostSmall);
            var smallResult = await registrySmall.CallAsync("debug_variables", Args("{\"scope\":\"all\"}"), new object(), CancellationToken.None);
            Assert.Equal("Locals:\n  x = 1 (int)\n  y = &Foo{...} (*main.Foo)\n    Field = val", smallResult.Text);

            // Large scope (>10 vars): children are never shown, even for a variable that has them.
            var many = Enumerable.Range(0, 11).Select(i => new VariableInfo($"v{i}", i.ToString(), "int")).ToList();
            many[10] = withChildren;
            var hostLarge = NewHost();
            hostLarge.DebugHost.OnVariables = (frame, scope, ct) => Task.FromResult<IReadOnlyList<VariableScope>>(new[]
            {
                new VariableScope("Locals", many),
            });
            var registryLarge = new ToolRegistry(hostLarge);
            var largeResult = await registryLarge.CallAsync("debug_variables", Args("{\"scope\":\"all\"}"), new object(), CancellationToken.None);
            Assert.DoesNotContain("Field = val", largeResult.Text);
        }

        [Fact]
        public async Task DebugVariablesWithNoScopesSaysSoWithoutError()
        {
            var registry = new ToolRegistry(NewHost());

            var result = await registry.CallAsync("debug_variables", Args("{}"), new object(), CancellationToken.None);

            Assert.False(result.IsError);
            Assert.Equal("no variables in scope", result.Text);
        }

        [Fact]
        public async Task DebugEvaluateMissingExpressionIsErrorNamingIt()
        {
            var registry = new ToolRegistry(NewHost());

            var result = await registry.CallAsync("debug_evaluate", Args("{}"), new object(), CancellationToken.None);

            Assert.True(result.IsError);
            Assert.Contains("expression", result.Text, StringComparison.Ordinal);
        }

        [Fact]
        public async Task DebugEvaluateFormatsResultWithType()
        {
            var host = NewHost();
            host.DebugHost.OnEvaluate = (expression, frame, ct) => Task.FromResult(new EvaluateResult("42", "int"));
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("debug_evaluate", Args("{\"expression\":\"1+41\"}"), new object(), CancellationToken.None);

            Assert.Equal("42 (int)", result.Text);
        }

        [Fact]
        public async Task DebugOutputFormatsLinesAndCursorDefaultingSinceToZero()
        {
            var host = NewHost();
            int? seenSince = null;
            host.DebugHost.OnOutput = (since, ct) => { seenSince = since; return Task.FromResult(new DebugOutputResult(new[] { "line1", "line2" }, 7)); };
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("debug_output", Args("{}"), new object(), CancellationToken.None);

            Assert.Equal(0, seenSince);
            Assert.Equal("line1\nline2\n[cursor 7]", result.Text);
        }

        [Fact]
        public async Task DebugOutputWithNoNewLinesSaysSo()
        {
            var host = NewHost();
            host.DebugHost.OnOutput = (since, ct) => Task.FromResult(new DebugOutputResult(Array.Empty<string>(), 3));
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("debug_output", Args("{\"since\":3}"), new object(), CancellationToken.None);

            Assert.Equal("(no new output)\n[cursor 3]", result.Text);
        }

        [Fact]
        public async Task DebugStopAlwaysReturnsStoppedAndCallsTheHost()
        {
            var host = NewHost();
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("debug_stop", Args("{}"), new object(), CancellationToken.None);

            Assert.False(result.IsError);
            Assert.Equal("stopped", result.Text);
            Assert.True(host.DebugHost.StopCalled);
        }
    }
}
