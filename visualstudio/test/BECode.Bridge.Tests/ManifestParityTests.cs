using System;
using System.Collections.Generic;
using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;
using BECode.Bridge;
using Xunit;

namespace BECode.Bridge.Tests
{
    /// <summary>
    /// A host that throws if any of its members are actually invoked. Used
    /// only to construct a <see cref="ToolRegistry"/> for tests that check
    /// its manifest binding and never dispatch a call through a handler that
    /// would touch the host (checkpoint 1: every handler is a placeholder
    /// that ignores its host anyway). Behavioural tests over a real,
    /// configurable host live in <c>FakeEditorHost.cs</c> / <c>ToolTests.cs</c>.
    /// </summary>
    internal sealed class ThrowingEditorHost : IEditorHost
    {
        private static Exception NotExpected([System.Runtime.CompilerServices.CallerMemberName] string member = "")
            => new InvalidOperationException($"ThrowingEditorHost.{member} should not be called by this test");

        public Task<EditorContext> GetContextAsync(CancellationToken ct) => throw NotExpected();
        public Task OpenAsync(string path, int? line, CancellationToken ct) => throw NotExpected();
        public Task<IReadOnlyList<Location>?> DefinitionAsync(string path, int line, int col, CancellationToken ct) => throw NotExpected();
        public Task<IReadOnlyList<Location>?> ReferencesAsync(string path, int line, int col, int max, CancellationToken ct) => throw NotExpected();
        public Task<string?> HoverAsync(string path, int line, int col, CancellationToken ct) => throw NotExpected();
        public Task<IReadOnlyList<Diagnostic>> DiagnosticsAsync(string? path, CancellationToken ct) => throw NotExpected();
        public IDebugHost Debug => throw NotExpected();
        public Task<ReviewDecision> ReviewDiffAsync(ReviewRequest request, CancellationToken ct) => throw NotExpected();
        public Task<bool> ReviewCancelAsync(string path) => throw NotExpected();
    }

    public class ManifestParityTests
    {
        private static readonly string[] ExpectedNames =
        {
            "context", "open", "definition", "references", "hover", "diagnostics",
            "debug_configs", "debug_start", "debug_breakpoint", "debug_continue", "debug_step",
            "debug_stack", "debug_variables", "debug_evaluate", "debug_output", "debug_stop",
            "review_diff", "review_cancel",
        };

        private static readonly string[] HiddenNames = { "review_diff", "review_cancel" };

        [Fact]
        public void ManifestHasEighteenEntriesInTheDocumentedOrder()
        {
            var manifest = ToolManifest.Load();
            Assert.Equal(18, manifest.Count);

            var names = new List<string>();
            foreach (var entry in manifest)
            {
                names.Add(entry.Name);
            }

            Assert.Equal(ExpectedNames, names);
        }

        [Fact]
        public void ManifestHiddenSetIsExactlyTheReviewTools()
        {
            var manifest = ToolManifest.Load();
            var hidden = new List<string>();
            foreach (var entry in manifest)
            {
                if (entry.Hidden)
                {
                    hidden.Add(entry.Name);
                }
            }

            Assert.Equal(HiddenNames, hidden);
        }

        [Fact]
        public void EveryManifestEntryHasAHandlerAndViceVersa()
        {
            // ToolRegistry's constructor itself throws on any mismatch; a
            // registry constructed here without throwing already proves the
            // parity the name says. The assertion below is belt-and-braces
            // against a future change that silently swallows the exception.
            var ex = Record.Exception(() => new ToolRegistry(new ThrowingEditorHost()));
            Assert.Null(ex);
        }

        [Fact]
        public void ListOmitsExactlyTheHiddenTools()
        {
            var registry = new ToolRegistry(new ThrowingEditorHost());
            var listed = new List<string>();
            foreach (var tool in registry.List())
            {
                listed.Add(tool.Name);
            }

            Assert.Equal(16, listed.Count);
            foreach (var hidden in HiddenNames)
            {
                Assert.DoesNotContain(hidden, listed);
            }
        }

        [Fact]
        public void ListedSchemasAreByteEqualToTheManifestAfterJsonNormalisation()
        {
            var manifest = ToolManifest.Load();
            var registry = new ToolRegistry(new ThrowingEditorHost());
            var listed = registry.List();

            var byName = new Dictionary<string, ToolInfo>(StringComparer.Ordinal);
            foreach (var tool in listed)
            {
                byName[tool.Name] = tool;
            }

            foreach (var entry in manifest)
            {
                if (entry.Hidden)
                {
                    Assert.False(byName.ContainsKey(entry.Name));
                    continue;
                }

                Assert.True(byName.TryGetValue(entry.Name, out var tool), $"'{entry.Name}' missing from List()");
                Assert.Equal(entry.Description, tool!.Description);

                var expectedSchema = JsonSerializer.Serialize(entry.InputSchema);
                var actualSchema = JsonSerializer.Serialize(tool.InputSchema);
                Assert.Equal(expectedSchema, actualSchema);
            }
        }

        [Fact]
        public async Task CallAsyncOfAnUnknownToolIsErrorTrueWithTheExactContractText()
        {
            var registry = new ToolRegistry(new ThrowingEditorHost());
            var args = JsonDocument.Parse("{}").RootElement;

            var result = await registry.CallAsync("does_not_exist", args, new object(), CancellationToken.None);

            Assert.True(result.IsError);
            Assert.Equal("unknown tool does_not_exist", result.Text);
        }

        [Fact]
        public async Task ReviewDiffAndReviewCancelAreCallableThroughCallAsyncDespiteBeingHidden()
        {
            var registry = new ToolRegistry(new ThrowingEditorHost());
            var args = JsonDocument.Parse("{}").RootElement;

            // Checkpoint 1: these still resolve to a handler (the "not
            // implemented" placeholder), not "unknown tool" — proving they
            // are bound like any other name, only List() treats them
            // differently. Real behaviour arrives in checkpoint 3.
            var diff = await registry.CallAsync("review_diff", args, new object(), CancellationToken.None);
            var cancel = await registry.CallAsync("review_cancel", args, new object(), CancellationToken.None);

            Assert.NotEqual("unknown tool review_diff", diff.Text);
            Assert.NotEqual("unknown tool review_cancel", cancel.Text);
        }
    }
}
