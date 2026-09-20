using System;
using System.Collections.Generic;
using System.Threading;
using System.Threading.Tasks;
using BECode.Bridge.Hosting;
using Microsoft.CodeAnalysis;
using Microsoft.CodeAnalysis.FindSymbols;
using Microsoft.VisualStudio.ComponentModelHost;
using Microsoft.VisualStudio.LanguageServices;
using Microsoft.VisualStudio.Shell;
using Microsoft.VisualStudio.Threading;
using BridgeLocation = BECode.Bridge.Location;

namespace BECode.VisualStudio
{
    /// <summary>
    /// Host design §3.3: <c>DefinitionAsync</c>/<c>ReferencesAsync</c>/
    /// <c>HoverAsync</c> over Roslyn (<c>Microsoft.VisualStudio.LanguageServices</c>'s
    /// <see cref="VisualStudioWorkspace"/>, fetched from MEF via
    /// <see cref="IComponentModel"/>). <c>workspace.CurrentSolution.GetDocumentIdsWithFilePath(path)</c>
    /// empty means "not a Roslyn document" — the interface's null return
    /// (C++, JSON, anything Roslyn does not own); once a document is found,
    /// every further step runs off the UI thread (Roslyn is free-threaded).
    /// </summary>
    internal sealed class RoslynNavigator
    {
        private readonly AsyncPackage _package;

        public RoslynNavigator(AsyncPackage package)
        {
            _package = package ?? throw new ArgumentNullException(nameof(package));
        }

        public Task<IReadOnlyList<BridgeLocation>?> DefinitionAsync(string path, int line, int col, CancellationToken ct)
        {
            return Guard.RunAsync(nameof(DefinitionAsync), ct, async () =>
            {
                var lookup = await FindSymbolAsync(path, line, col, ct).ConfigureAwait(false);
                if (!lookup.DocumentAvailable)
                {
                    return (IReadOnlyList<BridgeLocation>?)null;
                }

                if (lookup.Symbol == null)
                {
                    return (IReadOnlyList<BridgeLocation>?)Array.Empty<BridgeLocation>();
                }

                var result = new List<BridgeLocation>();
                foreach (var loc in lookup.Symbol.Locations)
                {
                    // A metadata-only symbol (a framework type) has no
                    // source location to point at — skipped, not an error
                    // (host design §3.3).
                    if (!loc.IsInSource)
                    {
                        continue;
                    }

                    var span = loc.GetLineSpan();
                    result.Add(new BridgeLocation(
                        loc.SourceTree?.FilePath ?? string.Empty,
                        span.StartLinePosition.Line + 1,
                        span.StartLinePosition.Character + 1));
                }

                return (IReadOnlyList<BridgeLocation>?)result;
            });
        }

        public Task<IReadOnlyList<BridgeLocation>?> ReferencesAsync(string path, int line, int col, CancellationToken ct)
        {
            return Guard.RunAsync(nameof(ReferencesAsync), ct, async () =>
            {
                var lookup = await FindSymbolAsync(path, line, col, ct).ConfigureAwait(false);
                if (!lookup.DocumentAvailable)
                {
                    return (IReadOnlyList<BridgeLocation>?)null;
                }

                if (lookup.Symbol == null || lookup.Solution == null)
                {
                    return (IReadOnlyList<BridgeLocation>?)Array.Empty<BridgeLocation>();
                }

                // Unbounded by design (Ruling S5): every reference, the tool
                // counts and truncates.
                var referencedSymbols = await SymbolFinder.FindReferencesAsync(lookup.Symbol, lookup.Solution, ct).ConfigureAwait(false);

                var result = new List<BridgeLocation>();
                foreach (var referencedSymbol in referencedSymbols)
                {
                    foreach (var refLoc in referencedSymbol.Locations)
                    {
                        if (!refLoc.Location.IsInSource)
                        {
                            continue;
                        }

                        var span = refLoc.Location.GetLineSpan();
                        var doc = refLoc.Document;
                        var sourceText = await doc.GetTextAsync(ct).ConfigureAwait(false);
                        var lineIndex = span.StartLinePosition.Line;
                        var lineText = lineIndex >= 0 && lineIndex < sourceText.Lines.Count
                            ? sourceText.Lines[lineIndex].ToString().Trim()
                            : string.Empty;

                        result.Add(new BridgeLocation(
                            doc.FilePath ?? string.Empty,
                            span.StartLinePosition.Line + 1,
                            span.StartLinePosition.Character + 1,
                            lineText));
                    }
                }

                return (IReadOnlyList<BridgeLocation>?)result;
            });
        }

        public Task<string?> HoverAsync(string path, int line, int col, CancellationToken ct)
        {
            return Guard.RunAsync(nameof(HoverAsync), ct, async () =>
            {
                var lookup = await FindSymbolAsync(path, line, col, ct).ConfigureAwait(false);
                if (!lookup.DocumentAvailable)
                {
                    return (string?)null;
                }

                if (lookup.Symbol == null)
                {
                    return (string?)string.Empty;
                }

                // Deliberately not Visual Studio's Quick Info (host design
                // §3.3): that service needs a live text view under the
                // mouse and returns UI objects.
                var display = lookup.Symbol.ToDisplayString(SymbolDisplayFormat.MinimallyQualifiedFormat);
                var summary = DocCommentSummary.Extract(lookup.Symbol.GetDocumentationCommentXml());

                // Ruling D10: join with a BLANK line when there is more
                // than one part.
                return string.IsNullOrEmpty(summary) ? display : display + "\n\n" + summary;
            });
        }

        /// <summary>
        /// <see cref="SymbolLookup.DocumentAvailable"/> false means "not a
        /// Roslyn document" (the interface's null-return case); true with a
        /// null <see cref="SymbolLookup.Symbol"/> means the provider ran and
        /// found nothing at that position (the interface's empty-list/
        /// empty-string case).
        /// </summary>
        private async Task<SymbolLookup> FindSymbolAsync(string path, int line, int col, CancellationToken ct)
        {
            await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync(ct);

            var componentModel = await _package.GetServiceAsync(typeof(SComponentModel)).ConfigureAwait(true) as IComponentModel;
            var workspace = componentModel?.GetService<VisualStudioWorkspace>();
            if (workspace == null)
            {
                return SymbolLookup.NoDocument;
            }

            var solution = workspace.CurrentSolution;
            var ids = solution.GetDocumentIdsWithFilePath(path);
            if (ids.IsDefaultOrEmpty)
            {
                return SymbolLookup.NoDocument;
            }

            // Host design §3.3: "all off the UI thread — Roslyn is
            // free-threaded."
            await TaskScheduler.Default;

            var document = solution.GetDocument(ids[0]);
            if (document == null)
            {
                return SymbolLookup.NoDocument;
            }

            var text = await document.GetTextAsync(ct).ConfigureAwait(false);
            var lineIndex = Math.Max(0, line - 1);
            if (lineIndex >= text.Lines.Count)
            {
                return SymbolLookup.Found(null, solution);
            }

            // Position clamped to the line's length (host design §3.3);
            // incoming line/col are already 1-based per Ruling D1, and the
            // interface's own contract (IEditorHost's class doc comment)
            // says the TOOLS have already clamped them to a minimum of 1.
            var textLine = text.Lines[lineIndex];
            var offset = Math.Max(0, col - 1);
            var position = Math.Min(textLine.Start + offset, textLine.End);

            var semanticModel = await document.GetSemanticModelAsync(ct).ConfigureAwait(false);
            if (semanticModel == null)
            {
                return SymbolLookup.Found(null, solution);
            }

            var symbol = await SymbolFinder.FindSymbolAtPositionAsync(semanticModel, position, workspace, ct).ConfigureAwait(false);
            return SymbolLookup.Found(symbol, solution);
        }

        private readonly struct SymbolLookup
        {
            public static readonly SymbolLookup NoDocument = default;

            private SymbolLookup(bool documentAvailable, ISymbol? symbol, Solution? solution)
            {
                DocumentAvailable = documentAvailable;
                Symbol = symbol;
                Solution = solution;
            }

            public bool DocumentAvailable { get; }

            public ISymbol? Symbol { get; }

            public Solution? Solution { get; }

            public static SymbolLookup Found(ISymbol? symbol, Solution solution)
            {
                return new SymbolLookup(true, symbol, solution);
            }
        }
    }
}
