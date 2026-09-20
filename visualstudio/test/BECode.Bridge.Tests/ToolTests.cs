using System;
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
    }
}
