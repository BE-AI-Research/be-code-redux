using System;
using System.Collections.Generic;
using System.Linq;
using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;
using BECode.Bridge.Tools;

namespace BECode.Bridge
{
    /// <summary>
    /// Binds every name in the embedded manifest (<see cref="ToolManifest"/>)
    /// to a handler, and is the <see cref="IToolDispatcher"/>
    /// <see cref="BridgeServer"/> dispatches to. A manifest entry with no
    /// handler, or a handler with no manifest entry, throws at construction —
    /// the two must never drift, in either direction.
    ///
    /// The eighteen names bind to the four tool families in
    /// <c>Tools/</c> (<c>EditorTools</c>, <c>DiagnosticsTools</c>,
    /// <c>ReviewTools</c>, <c>DebugTools</c>).
    /// </summary>
    public sealed class ToolRegistry : IToolDispatcher
    {
        internal delegate Task<ToolResult> ToolHandler(JsonElement args, object connection, CancellationToken ct);

        private readonly IReadOnlyList<ToolInfo> _list;
        private readonly IReadOnlyDictionary<string, ToolHandler> _handlers;
        private readonly ReviewTools _reviewTools;

        public ToolRegistry(IEditorHost host) : this(host, ToolManifest.Load())
        {
        }

        /// <summary>
        /// Takes the manifest entries directly, so a test
        /// can inject a deliberately mismatched manifest and prove the
        /// parity guard actually throws — in both directions, naming the
        /// offender — rather than trusting that by never having deleted it.
        /// </summary>
        internal ToolRegistry(IEditorHost host, IReadOnlyList<ManifestEntry> manifest)
        {
            var manifestNames = new HashSet<string>(manifest.Select(m => m.Name), StringComparer.Ordinal);

            // Validated before the handler-parity check
            // below (which, given ToolOverrides only ever names real tools,
            // would also always catch the same missing entry) so a mismatch
            // here is reported as what it specifically is — the override
            // table has an entry this manifest does not — rather than only
            // ever being visible as an undifferentiated parity failure.
            var unknownOverrides = ToolOverrides.Descriptions.Keys
                .Where(n => !manifestNames.Contains(n))
                .OrderBy(n => n, StringComparer.Ordinal)
                .ToList();
            if (unknownOverrides.Count > 0)
            {
                throw new InvalidOperationException(
                    $"ToolRegistry / ToolOverrides mismatch: override(s) for name(s) not in the manifest [{string.Join(", ", unknownOverrides)}]");
            }

            _reviewTools = new ReviewTools(host);
            var handlers = BuildHandlers(host, _reviewTools);

            var missing = manifestNames.Where(n => !handlers.ContainsKey(n)).OrderBy(n => n, StringComparer.Ordinal).ToList();
            var extra = handlers.Keys.Where(n => !manifestNames.Contains(n)).OrderBy(n => n, StringComparer.Ordinal).ToList();
            if (missing.Count > 0 || extra.Count > 0)
            {
                throw new InvalidOperationException(
                    "ToolRegistry / manifest mismatch: "
                    + $"missing handlers for [{string.Join(", ", missing)}]; "
                    + $"handlers with no manifest entry [{string.Join(", ", extra)}]");
            }

            _handlers = handlers;
            _list = manifest
                .Where(m => !m.Hidden)
                .Select(m => new ToolInfo(
                    m.Name,
                    ToolOverrides.Descriptions.TryGetValue(m.Name, out var overrideText) ? overrideText : m.Description,
                    m.InputSchema))
                .ToList();
        }

        private static Dictionary<string, ToolHandler> BuildHandlers(IEditorHost host, ReviewTools reviewTools)
        {
            var editorTools = new EditorTools(host);
            var diagnosticsTools = new DiagnosticsTools(host);
            var debugTools = new DebugTools(host);

            return new Dictionary<string, ToolHandler>(StringComparer.Ordinal)
            {
                ["context"] = editorTools.Context,
                ["open"] = editorTools.Open,
                ["definition"] = editorTools.Definition,
                ["references"] = editorTools.References,
                ["hover"] = editorTools.Hover,
                ["diagnostics"] = diagnosticsTools.Diagnostics,
                ["debug_configs"] = debugTools.Configs,
                ["debug_start"] = debugTools.Start,
                ["debug_breakpoint"] = debugTools.Breakpoint,
                ["debug_continue"] = debugTools.Continue,
                ["debug_step"] = debugTools.Step,
                ["debug_stack"] = debugTools.Stack,
                ["debug_variables"] = debugTools.Variables,
                ["debug_evaluate"] = debugTools.Evaluate,
                ["debug_output"] = debugTools.Output,
                ["debug_stop"] = debugTools.Stop,
                ["review_diff"] = reviewTools.ReviewDiff,
                ["review_cancel"] = reviewTools.ReviewCancel,
            };
        }

        /// <summary>Never includes hidden tools (review_diff, review_cancel).</summary>
        public IReadOnlyList<ToolInfo> List() => _list;

        public Task<ToolResult> CallAsync(string name, JsonElement args, object connection, CancellationToken ct)
        {
            if (!_handlers.TryGetValue(name, out var handler))
            {
                return Task.FromResult(new ToolResult($"unknown tool {name}", true));
            }

            return handler(args, connection, ct);
        }

        public void ConnectionClosed(object connection)
        {
            _reviewTools.ConnectionClosed(connection);
        }
    }
}
