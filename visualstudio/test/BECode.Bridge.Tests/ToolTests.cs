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

        private EditorContext DefaultContext() => new EditorContext("", 0, 0, 0, "", Array.Empty<string>());

        private FakeEditorHost NewHost() => new FakeEditorHost
        {
            OnGetContext = ct => Task.FromResult(DefaultContext()),
            OnGetWorkspaceFolders = ct => Task.FromResult<IReadOnlyList<string>>(_folders),
        };

        // ---- context (vscode/src/tools/editor.ts's "context" handler) ----

        [Fact]
        public async Task ContextSerialisesTheHostsContextInTheVsCodeShape()
        {
            // Ruling S3: file/open cross the seam absolute; the tool
            // relativises them against workspaceFolders (Ruling S2, fetched
            // separately from GetContextAsync) before they go on the wire.
            var aGo = Path.Combine(_workspace, "a.go");
            var bGo = Path.Combine(_workspace, "b.go");
            var host = new FakeEditorHost
            {
                OnGetContext = ct => Task.FromResult(new EditorContext(aGo, 5, 2, 4, "hello", new[] { aGo, bGo })),
                OnGetWorkspaceFolders = ct => Task.FromResult<IReadOnlyList<string>>(_folders),
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
            Assert.Equal(new[] { _workspace }, root.GetProperty("workspaceFolders").EnumerateArray().Select(e => e.GetString()));
        }

        [Fact]
        public async Task ContextLeavesAnEmptyFileEmptyRatherThanTryingToRelativiseIt()
        {
            var host = new FakeEditorHost
            {
                OnGetContext = ct => Task.FromResult(new EditorContext("", 0, 0, 0, "", Array.Empty<string>())),
                OnGetWorkspaceFolders = ct => Task.FromResult<IReadOnlyList<string>>(_folders),
            };
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("context", Args("{}"), new object(), CancellationToken.None);

            using var doc = JsonDocument.Parse(result.Text);
            Assert.Equal("", doc.RootElement.GetProperty("file").GetString());
        }

        [Fact]
        public async Task ContextTruncatesTheSelectionTo2048Characters()
        {
            // Ruling S6: the host returns the raw, untruncated selection;
            // the 2048-character cut is the tool's job now.
            var raw = new string('x', 3000);
            var host = new FakeEditorHost
            {
                OnGetContext = ct => Task.FromResult(new EditorContext("", 1, 0, 0, raw, Array.Empty<string>())),
                OnGetWorkspaceFolders = ct => Task.FromResult<IReadOnlyList<string>>(_folders),
            };
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("context", Args("{}"), new object(), CancellationToken.None);

            using var doc = JsonDocument.Parse(result.Text);
            var selection = doc.RootElement.GetProperty("selection").GetString();
            Assert.Equal(2048, selection!.Length);
            Assert.Equal(raw.Substring(0, 2048), selection);
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

        [Fact]
        public async Task OpenRefusesWhenNoWorkspaceFolderIsOpen()
        {
            // Ruling S9: an empty workspace-folder list (devenv.exe with no
            // solution/folder open) is refused with a clear message, rather
            // than confining to the process's arbitrary current directory.
            var host = new FakeEditorHost { OnGetWorkspaceFolders = ct => Task.FromResult<IReadOnlyList<string>>(Array.Empty<string>()) };
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("open", Args("{\"path\":\"sample.go\"}"), new object(), CancellationToken.None);

            Assert.True(result.IsError);
            Assert.Equal("no solution or folder is open", result.Text);
            Assert.Empty(host.OpenCalls);
        }

        // ---- definition ----

        [Fact]
        public async Task DefinitionFormatsPathLineColPerLocationRelativisingTheAbsolutePathTheHostReturns()
        {
            // Ruling S3: Location.Path crosses the seam absolute; the tool
            // relativises it for output, exactly where vscode's own
            // definition handler does.
            var host = NewHost();
            host.OnDefinition = (path, line, col, ct) =>
                Task.FromResult<IReadOnlyList<Location>?>(new[] { new Location(Path.Combine(_workspace, "a.go"), 10, 2) });
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
        public async Task ReferencesFormatsCountAndPathLineTextRelativisingTheAbsolutePathTheHostReturns()
        {
            var host = NewHost();
            host.OnReferences = (path, line, col, ct) =>
                Task.FromResult<IReadOnlyList<Location>?>(new[] { new Location(Path.Combine(_workspace, "a.go"), 3, 1, "  x := 1  ") });
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("references", Args("{\"path\":\"sample.go\",\"line\":1,\"col\":1}"), new object(), CancellationToken.None);

            Assert.False(result.IsError);
            Assert.Equal("1 reference(s)\na.go:3: x := 1", result.Text);
        }

        [Fact]
        public async Task ReferencesReportsTheTotalCountAndPrintsOnlyTheFirstMax()
        {
            // Ruling S5: the host is no longer given a `max` — it returns
            // every reference; the tool reports the TOTAL count and prints
            // only the first `max` (default 50), matching vscode's own
            // res.length read before slicing.
            var host = NewHost();
            var all = Enumerable.Range(0, 200)
                .Select(i => new Location(Path.Combine(_workspace, "a.go"), i + 1, 1, $"line{i}"))
                .ToArray();
            host.OnReferences = (path, line, col, ct) => Task.FromResult<IReadOnlyList<Location>?>(all);
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("references", Args("{\"path\":\"sample.go\",\"line\":1,\"col\":1}"), new object(), CancellationToken.None);

            Assert.False(result.IsError);
            Assert.StartsWith("200 reference(s)", result.Text);
            var lines = result.Text.Split('\n');
            Assert.Equal(51, lines.Length); // header + 50 shown
            Assert.Contains("line0", lines[1]);
            Assert.Contains("line49", lines[50]);
        }

        [Fact]
        public async Task ReferencesWithNoResultsSaysSoWithoutError()
        {
            var host = NewHost();
            host.OnReferences = (path, line, col, ct) => Task.FromResult<IReadOnlyList<Location>?>(Array.Empty<Location>());
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("references", Args("{\"path\":\"sample.go\",\"line\":1,\"col\":1}"), new object(), CancellationToken.None);

            Assert.False(result.IsError);
            Assert.Equal("no references found", result.Text);
        }

        [Fact]
        public async Task ReferencesOnAHostThatReturnsNullAnswersNotAvailableForThisFileType()
        {
            var host = NewHost();
            host.OnReferences = (path, line, col, ct) => Task.FromResult<IReadOnlyList<Location>?>(null);
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
        public async Task DiagnosticsDefaultsSeverityToErrorsAndWarnings()
        {
            // F3 (Ruling R-6 — the task brief's "defaults to all" was wrong):
            // the default is errors and warnings, as the manifest's own
            // description says ("severity: error, warning or all (default:
            // errors and warnings)") and vscode/src/lib/format.ts's
            // severitiesFor implements. The host no longer takes a severity
            // parameter at all (Ruling S4): it returns everything, and the
            // tool filters.
            var host = NewHost();
            host.OnDiagnostics = (path, ct) => Task.FromResult<IReadOnlyList<Diagnostic>>(new[]
            {
                new Diagnostic("a.go", 1, 1, "error", "go", "e1"),
                new Diagnostic("a.go", 2, 1, "warning", "go", "w1"),
                new Diagnostic("a.go", 3, 1, "info", "go", "i1"),
                new Diagnostic("a.go", 4, 1, "hint", "go", "h1"),
            });
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("diagnostics", Args("{}"), new object(), CancellationToken.None);

            Assert.Contains("e1", result.Text);
            Assert.Contains("w1", result.Text);
            Assert.DoesNotContain("i1", result.Text);
            Assert.DoesNotContain("h1", result.Text);
            Assert.StartsWith("1 error, 1 warning in 1 file", result.Text);
        }

        [Fact]
        public async Task DiagnosticsWithSeverityErrorFiltersOutEverythingElse()
        {
            var host = NewHost();
            host.OnDiagnostics = (path, ct) => Task.FromResult<IReadOnlyList<Diagnostic>>(new[]
            {
                new Diagnostic("a.go", 1, 1, "error", "go", "e1"),
                new Diagnostic("a.go", 2, 1, "warning", "go", "w1"),
            });
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("diagnostics", Args("{\"severity\":\"error\"}"), new object(), CancellationToken.None);

            Assert.Contains("e1", result.Text);
            Assert.DoesNotContain("w1", result.Text);
        }

        [Fact]
        public async Task DiagnosticsWithSeverityAllIncludesInfoAndHint()
        {
            var host = NewHost();
            host.OnDiagnostics = (path, ct) => Task.FromResult<IReadOnlyList<Diagnostic>>(new[]
            {
                new Diagnostic("a.go", 3, 1, "info", "go", "i1"),
                new Diagnostic("a.go", 4, 1, "hint", "go", "h1"),
            });
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("diagnostics", Args("{\"severity\":\"all\"}"), new object(), CancellationToken.None);

            Assert.Contains("i1", result.Text);
            Assert.Contains("h1", result.Text);
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
            // Ported from vscode/test/format.test.ts's "groups by file with a
            // count summary first". Diagnostic.Path is absolute (Ruling S3);
            // relativised names still sort/group the same way.
            var host = NewHost();
            host.OnDiagnostics = (path, ct) => Task.FromResult<IReadOnlyList<Diagnostic>>(new[]
            {
                new Diagnostic(Path.Combine(_workspace, "b.go"), 3, 1, "error", "go", "undefined: x"),
                new Diagnostic(Path.Combine(_workspace, "a.py"), 10, 5, "warning", "Pylance", "unused"),
                new Diagnostic(Path.Combine(_workspace, "b.go"), 1, 1, "error", "go", "missing import"),
            });
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("diagnostics", Args("{}"), new object(), CancellationToken.None);

            var firstLine = result.Text.Split('\n')[0];
            Assert.Equal("2 errors, 1 warning in 2 files", firstLine);
            Assert.Contains("b.go:1:1 error go: missing import", result.Text, StringComparison.Ordinal);
            Assert.True(result.Text.IndexOf("a.py", StringComparison.Ordinal) < result.Text.IndexOf("b.go", StringComparison.Ordinal));
        }

        [Fact]
        public async Task DiagnosticsRelativisesAPathInASubdirectoryForDisplay()
        {
            var host = NewHost();
            host.OnDiagnostics = (path, ct) => Task.FromResult<IReadOnlyList<Diagnostic>>(new[]
            {
                new Diagnostic(Path.Combine(_workspace, "internal", "pkg", "a.go"), 1, 1, "error", "go", "e1"),
            });
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("diagnostics", Args("{}"), new object(), CancellationToken.None);

            Assert.Contains("internal/pkg/a.go:1:1 error go: e1", result.Text, StringComparison.Ordinal);
        }

        [Fact]
        public async Task DiagnosticsResolvesThePathFilterArgumentToAnAbsolutePathBeforeCallingTheHost()
        {
            // Ruling S3/S4: IEditorHost.DiagnosticsAsync's path is
            // "absolute or null" — an incoming relative filter argument must
            // be resolved and confined like every other path argument.
            var host = NewHost();
            string? seenPath = "not set";
            host.OnDiagnostics = (path, ct) => { seenPath = path; return Task.FromResult<IReadOnlyList<Diagnostic>>(Array.Empty<Diagnostic>()); };
            var registry = new ToolRegistry(host);

            await registry.CallAsync("diagnostics", Args("{\"path\":\"sample.go\"}"), new object(), CancellationToken.None);

            Assert.Equal(Path.Combine(_workspace, "sample.go"), seenPath);
        }

        [Fact]
        public async Task DiagnosticsRejectsAPathFilterOutsideTheWorkspaceWithoutCallingTheHost()
        {
            var host = NewHost();
            var called = false;
            host.OnDiagnostics = (path, ct) => { called = true; return Task.FromResult<IReadOnlyList<Diagnostic>>(Array.Empty<Diagnostic>()); };
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("diagnostics", Args("{\"path\":\"../outside.txt\"}"), new object(), CancellationToken.None);

            Assert.True(result.IsError);
            Assert.Contains("outside the workspace", result.Text, StringComparison.Ordinal);
            Assert.False(called);
        }

        [Fact]
        public async Task DiagnosticsRefusesAPathFilterWhenNoWorkspaceFolderIsOpen()
        {
            var host = new FakeEditorHost { OnGetWorkspaceFolders = ct => Task.FromResult<IReadOnlyList<string>>(Array.Empty<string>()) };
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("diagnostics", Args("{\"path\":\"sample.go\"}"), new object(), CancellationToken.None);

            Assert.True(result.IsError);
            Assert.Equal("no solution or folder is open", result.Text);
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
        public async Task ConnectionClosedCancelsAPendingReviewDiffsToken()
        {
            // F1: ConnectionClosed used to drop _pending[connection] without
            // cancelling those CancellationTokenSources — the host was never
            // told, and the pending review_diff call could hang forever.
            var hostCalled = new TaskCompletionSource<bool>();
            var hostObservedCancellation = new TaskCompletionSource<bool>();
            var host = new FakeEditorHost
            {
                OnReviewDiff = async (req, ct) =>
                {
                    var tcs = new TaskCompletionSource<bool>();
                    using (ct.Register(() => { hostObservedCancellation.TrySetResult(true); tcs.TrySetResult(true); }))
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

            registry.ConnectionClosed(conn);

            // The host must have observed cancellation, and the pending
            // review_diff task must complete (its result is discarded by the
            // caller — the connection is gone — but it must not hang).
            await hostObservedCancellation.Task.WaitAsync(TimeSpan.FromSeconds(5));
            var diffResult = await diffTask.WaitAsync(TimeSpan.FromSeconds(5));
            Assert.Equal("{\"decision\":\"cancelled\"}", diffResult.Text);
        }

        [Fact]
        public async Task ConnectionClosedDoesNotLetAStillRunningReviewDiffResurrectAcceptAllForTheDroppedConnection()
        {
            // F1 (coordinator's note): a still-running review_diff for a
            // connection ConnectionClosed already tore down must not
            // re-create per-connection state — here, a misbehaving host that
            // ignores cancellation and answers AcceptAll anyway must not get
            // to write _acceptAll for a connection that is already gone.
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

                    // Misbehaving: answers AcceptAll despite the token firing.
                    return ReviewDecision.AcceptAll;
                },
            };
            var registry = new ToolRegistry(host);
            var conn = new object();

            var diffTask = registry.CallAsync("review_diff", Args("{\"path\":\"a.txt\",\"proposed\":\"new\"}"), conn, CancellationToken.None);
            await hostCalled.Task;

            registry.ConnectionClosed(conn);
            await diffTask.WaitAsync(TimeSpan.FromSeconds(5));

            // A fresh review_diff on the SAME connection object (its
            // per-connection state should have been dropped, not silently
            // repopulated by the call above) must still ask the host, not
            // short-circuit to "accept" via a resurrected accept-all.
            var askedAgain = false;
            host.OnReviewDiff = (req, ct) => { askedAgain = true; return Task.FromResult(ReviewDecision.Reject); };
            var second = await registry.CallAsync("review_diff", Args("{\"path\":\"b.txt\",\"proposed\":\"new\"}"), conn, CancellationToken.None);

            Assert.True(askedAgain);
            Assert.Equal("{\"decision\":\"reject\"}", second.Text);
        }

        [Theory]
        [InlineData("{\"path\":\"a.txt\"}")]
        [InlineData("{\"path\":\"a.txt\",\"proposed\":123}")]
        public async Task ReviewDiffProposedMissingOrWrongTypeIsErrorNamingIt(string argsJson)
        {
            var registry = new ToolRegistry(NewHost());

            var result = await registry.CallAsync("review_diff", Args(argsJson), new object(), CancellationToken.None);

            Assert.True(result.IsError);
            Assert.Contains("proposed", result.Text, StringComparison.Ordinal);
        }

        [Fact]
        public async Task ReviewDiffAcceptsAnEmptyProposedString()
        {
            // F4: TryRequireString treated "" as absent. An emptied file is a
            // legal proposal (VS Code accepts it: typeof "" === "string"),
            // and the Go side maps isError to "review unavailable", silently
            // skipping the editor diff — so rejecting "" here silently broke
            // reviewing a file being emptied.
            ReviewRequest? captured = null;
            var host = new FakeEditorHost { OnReviewDiff = (req, ct) => { captured = req; return Task.FromResult(ReviewDecision.Accept); } };
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("review_diff", Args("{\"path\":\"a.txt\",\"proposed\":\"\"}"), new object(), CancellationToken.None);

            Assert.False(result.IsError);
            Assert.Equal("{\"decision\":\"accept\"}", result.Text);
            Assert.NotNull(captured);
            Assert.Equal("", captured!.Proposed);
        }

        [Fact]
        public async Task ReviewDiffStillRequiresPathNonEmpty()
        {
            var registry = new ToolRegistry(NewHost());

            var result = await registry.CallAsync("review_diff", Args("{\"path\":\"\",\"proposed\":\"new\"}"), new object(), CancellationToken.None);

            Assert.True(result.IsError);
            Assert.Contains("path", result.Text, StringComparison.Ordinal);
        }

        [Fact]
        public async Task ReviewCancelResolvesAPendingReviewDiffAsCancelledWhenTheHostThrowsOperationCanceledException()
        {
            // S1: the host may answer a cancelled review either by returning
            // ReviewDecision.Cancelled or by throwing
            // OperationCanceledException; ReviewTools must treat both the
            // same way.
            var hostCalled = new TaskCompletionSource<bool>();
            var host = new FakeEditorHost
            {
                OnReviewDiff = async (req, ct) =>
                {
                    hostCalled.SetResult(true);
                    await Task.Delay(Timeout.Infinite, ct);
                    return ReviewDecision.Reject; // unreachable
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
        public async Task ReviewCancelAndAFastAcceptingHostNeverDisagreeOnTheOutcome()
        {
            // F7 regression: pending.Resolved was set outside the lock that
            // ReviewCancel reads-and-claims it under, so a cancel racing a
            // fast "accept" answer could report {"cancelled":true} for a
            // review that had actually resolved "accept". Run many
            // genuinely-concurrent iterations (Task.Run onto the thread pool,
            // plus a real await point in the host) so the two calls can
            // actually interleave; the invariant below must hold every time.
            for (var i = 0; i < 500; i++)
            {
                var host = new FakeEditorHost
                {
                    OnReviewDiff = async (req, ct) =>
                    {
                        await Task.Yield();
                        return ReviewDecision.Accept;
                    },
                };
                var registry = new ToolRegistry(host);
                var conn = new object();
                var path = $"race-{i}.txt";

                var diffTask = Task.Run(() => registry.CallAsync("review_diff", Args($"{{\"path\":\"{path}\",\"proposed\":\"new\"}}"), conn, CancellationToken.None));
                var cancelTask = Task.Run(() => registry.CallAsync("review_cancel", Args($"{{\"path\":\"{path}\"}}"), conn, CancellationToken.None));

                var diffResult = await diffTask;
                var cancelResult = await cancelTask;

                if (cancelResult.Text == "{\"cancelled\":true}")
                {
                    Assert.Equal("{\"decision\":\"cancelled\"}", diffResult.Text);
                }
                else
                {
                    Assert.Equal("{\"decision\":\"accept\"}", diffResult.Text);
                }
            }
        }

        [Fact]
        public async Task DebugStartPropagatesCancellationFromTheStackReadInsteadOfSwallowingIt()
        {
            // F8: Describe's bare `catch` turned a cancelled connection into
            // ordinary "session ended before the stack could be read" success
            // text instead of letting the cancellation propagate.
            var host = NewHost();
            host.DebugHost.OnStart = (config, ct) => Task.FromResult(new StopResult(StopKind.Stopped, "step"));
            host.DebugHost.OnStack = (depth, ct) => throw new OperationCanceledException();
            var registry = new ToolRegistry(host);

            await Assert.ThrowsAsync<OperationCanceledException>(
                () => registry.CallAsync("debug_start", Args("{}"), new object(), CancellationToken.None));
        }

        [Fact]
        public async Task OpenPropagatesCancellationFromPathResolutionInsteadOfReportingAnError()
        {
            // F8: ToolPaths.ResolveAsync's blanket catch turned a cancelled
            // connection into isError:true "workspace folders: ..." instead
            // of letting the cancellation propagate so BridgeServer's own
            // cancelled-call handling (no reply at all) applies.
            var host = new FakeEditorHost { OnGetWorkspaceFolders = ct => throw new OperationCanceledException() };
            var registry = new ToolRegistry(host);

            await Assert.ThrowsAsync<OperationCanceledException>(
                () => registry.CallAsync("open", Args("{\"path\":\"sample.go\"}"), new object(), CancellationToken.None));
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
            // Ruling S3: StackFrameInfo.Path crosses the seam absolute; the
            // tool relativises it for output, exactly where vscode's own
            // debug_stack handler does.
            var host = NewHost();
            host.DebugHost.OnStart = (config, ct) => Task.FromResult(new StopResult(StopKind.Stopped, "breakpoint"));
            host.DebugHost.OnStack = (depth, ct) => Task.FromResult<IReadOnlyList<StackFrameInfo>>(new[]
            {
                new StackFrameInfo("main.main", Path.Combine(_workspace, "main.go"), 12, 1),
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
        public async Task DebugBreakpointRefusesWhenNoWorkspaceFolderIsOpen()
        {
            // Ruling S9, applied to every path-taking tool, not just open.
            var host = new FakeEditorHost { OnGetWorkspaceFolders = ct => Task.FromResult<IReadOnlyList<string>>(Array.Empty<string>()) };
            var called = false;
            host.DebugHost.OnSetBreakpoint = (path, line, action, condition, ct) => { called = true; return Task.FromResult<IReadOnlyList<BreakpointInfo>>(Array.Empty<BreakpointInfo>()); };
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("debug_breakpoint", Args("{\"path\":\"sample.go\",\"line\":1}"), new object(), CancellationToken.None);

            Assert.True(result.IsError);
            Assert.Equal("no solution or folder is open", result.Text);
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
                new StackFrameInfo("main.main", Path.Combine(_workspace, "main.go"), 12, 1),
                new StackFrameInfo("main.helper", null, 4, 2),
            }); };
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("debug_stack", Args("{}"), new object(), CancellationToken.None);

            Assert.Equal(10, seenDepth);
            Assert.Equal("#0 main.main main.go:12  [frame 1]\n#1 main.helper ?:4  [frame 2]", result.Text);
        }

        [Fact]
        public async Task DebugStackRelativisesAPathInASubdirectory()
        {
            // A path directly under the workspace root relativises to a
            // string identical to its own file name — the case above can't
            // by itself distinguish "the tool relativised this" from "the
            // tool passed it through unchanged". A subdirectory can.
            var host = NewHost();
            host.DebugHost.OnStack = (depth, ct) => Task.FromResult<IReadOnlyList<StackFrameInfo>>(new[]
            {
                new StackFrameInfo("pkg.Helper", Path.Combine(_workspace, "internal", "pkg", "helper.go"), 7, 1),
            });
            var registry = new ToolRegistry(host);

            var result = await registry.CallAsync("debug_stack", Args("{}"), new object(), CancellationToken.None);

            Assert.Equal("#0 pkg.Helper internal/pkg/helper.go:7  [frame 1]", result.Text);
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
