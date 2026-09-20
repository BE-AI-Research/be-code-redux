using System;
using System.Collections.Generic;
using System.Linq;
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
        public Task<IReadOnlyList<string>> GetWorkspaceFoldersAsync(CancellationToken ct) => throw NotExpected();
        public Task OpenAsync(string path, int? line, CancellationToken ct) => throw NotExpected();
        public Task<IReadOnlyList<Location>?> DefinitionAsync(string path, int line, int col, CancellationToken ct) => throw NotExpected();
        public Task<IReadOnlyList<Location>?> ReferencesAsync(string path, int line, int col, CancellationToken ct) => throw NotExpected();
        public Task<string?> HoverAsync(string path, int line, int col, CancellationToken ct) => throw NotExpected();
        public Task<IReadOnlyList<Diagnostic>> DiagnosticsAsync(string? path, CancellationToken ct) => throw NotExpected();
        public IDebugHost Debug => throw NotExpected();
        public Task<ReviewDecision> ReviewDiffAsync(ReviewRequest request, CancellationToken ct) => throw NotExpected();
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

                // Schemas are NEVER overridden (F6): always byte-equal to the
                // manifest's. Descriptions are, for exactly the two tools
                // ToolOverrides names — see
                // ListDescriptionsMatchTheManifestExceptTheTwoOverriddenDebugTools.
                if (!ToolOverrides.Descriptions.ContainsKey(entry.Name))
                {
                    Assert.Equal(entry.Description, tool!.Description);
                }

                var expectedSchema = JsonSerializer.Serialize(entry.InputSchema);
                var actualSchema = JsonSerializer.Serialize(tool.InputSchema);
                Assert.Equal(expectedSchema, actualSchema);
            }
        }

        [Fact]
        public void ListDescriptionsMatchTheManifestExceptTheTwoOverriddenDebugTools()
        {
            // F6 (Ruling R-9): debug_configs/debug_start's descriptions are
            // overridden because the manifest's own text tells the model to
            // use vscode's program+type launch shape, which this bridge
            // refuses. Every other tool's description is the manifest's own.
            var manifest = ToolManifest.Load();
            var registry = new ToolRegistry(new ThrowingEditorHost());
            var listed = registry.List().ToDictionary(t => t.Name, t => t.Description, StringComparer.Ordinal);

            foreach (var entry in manifest)
            {
                if (entry.Hidden)
                {
                    continue;
                }

                if (entry.Name == "debug_configs" || entry.Name == "debug_start")
                {
                    Assert.NotEqual(entry.Description, listed[entry.Name]);
                }
                else
                {
                    Assert.Equal(entry.Description, listed[entry.Name]);
                }
            }
        }

        [Fact]
        public void ToolOverridesTableContainsExactlyTheTwoDebugToolNames()
        {
            var names = ToolOverrides.Descriptions.Keys.OrderBy(n => n, StringComparer.Ordinal).ToList();
            Assert.Equal(new[] { "debug_configs", "debug_start" }, names);
        }

        [Fact]
        public void ToolOverridesTextIsPinnedLiterally()
        {
            // T2: a typo here previously failed no test — the override
            // table's own JSON-schema-adjacent nature (it's read as plain
            // text by the model) means the exact wording matters and
            // deserves the same literal-string protection a manifest entry
            // gets for free from ManifestHasEighteenEntriesInTheDocumentedOrder's
            // parity check. debug_start's text ends by naming the 60s wait
            // the tool actually enforces (DebugTools.Describe's Timeout case).
            Assert.Equal(
                "List what Visual Studio can debug: the solution's startup projects and their launch profiles. Pass a name to debug_start as config.",
                ToolOverrides.Descriptions["debug_configs"]);
            Assert.Equal(
                "Start debugging in Visual Studio. Use config (a startup project or launch profile name from debug_configs), or omit it to debug the current startup project. program, type and args are not supported here: Visual Studio debugs the startup project with its launch profile's arguments. Returns where execution stopped (60s max).",
                ToolOverrides.Descriptions["debug_start"]);
        }

        [Fact]
        public void AnOverrideForANameNotInTheManifestThrowsAtConstruction()
        {
            // debug_start is one of ToolOverrides' two names; a manifest
            // that omits it must be rejected specifically as an override
            // mismatch (checked ahead of the general handler-parity guard,
            // which would also catch this same removal — see
            // ToolRegistry's internal constructor).
            var manifest = ToolManifest.Load().Where(m => m.Name != "debug_start").ToList();

            var ex = Assert.Throws<InvalidOperationException>(() => new ToolRegistry(new ThrowingEditorHost(), manifest));

            Assert.Contains("ToolOverrides", ex.Message, StringComparison.Ordinal);
            Assert.Contains("debug_start", ex.Message, StringComparison.Ordinal);
        }

        [Fact]
        public void ConstructorThrowsNamingAManifestEntryWithNoHandler()
        {
            // F5: proves the parity guard actually throws for a manifest
            // entry with no handler, naming the offender — rather than only
            // ever being exercised by construction succeeding.
            var manifest = new List<ManifestEntry>(ToolManifest.Load())
            {
                new ManifestEntry("bogus_tool", "d", JsonDocument.Parse("{}").RootElement.Clone(), false),
            };

            var ex = Assert.Throws<InvalidOperationException>(() => new ToolRegistry(new ThrowingEditorHost(), manifest));

            Assert.Contains("bogus_tool", ex.Message, StringComparison.Ordinal);
        }

        [Fact]
        public void ConstructorThrowsNamingAHandlerWithNoManifestEntry()
        {
            // F5, the other direction: a manifest missing an entry for a
            // name ToolRegistry always has a handler for (any of the
            // fourteen non-override names avoids also tripping the
            // override-mismatch guard above).
            var manifest = ToolManifest.Load().Where(m => m.Name != "context").ToList();

            var ex = Assert.Throws<InvalidOperationException>(() => new ToolRegistry(new ThrowingEditorHost(), manifest));

            Assert.Contains("context", ex.Message, StringComparison.Ordinal);
        }

        [Fact]
        public void ToolManifestLoadThrowsNamingTheResourceWhenItIsMissingFromTheGivenAssembly()
        {
            // F5: "same for a missing embedded resource, if it can be
            // injected cheaply" — it can, via the internal Load(Assembly)
            // overload: the test assembly itself does not embed the
            // manifest resource.
            var ex = Assert.Throws<InvalidOperationException>(() => ToolManifest.Load(typeof(ManifestParityTests).Assembly));

            Assert.Contains("BECode.Bridge.tools.manifest.json", ex.Message, StringComparison.Ordinal);
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
            // ThrowingEditorHost throws if touched, so these calls omit the
            // required "path"/"proposed" arguments — proving they resolve to
            // a real handler (an argument-validation error), not "unknown
            // tool", without needing a working host.
            var registry = new ToolRegistry(new ThrowingEditorHost());
            var args = JsonDocument.Parse("{}").RootElement;

            var diff = await registry.CallAsync("review_diff", args, new object(), CancellationToken.None);
            var cancel = await registry.CallAsync("review_cancel", args, new object(), CancellationToken.None);

            Assert.NotEqual("unknown tool review_diff", diff.Text);
            Assert.NotEqual("unknown tool review_cancel", cancel.Text);
        }
    }
}
