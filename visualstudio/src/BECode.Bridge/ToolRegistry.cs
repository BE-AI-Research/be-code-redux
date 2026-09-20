using System;
using System.Collections.Generic;
using System.Linq;
using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;

namespace BECode.Bridge
{
    /// <summary>
    /// Binds every name in the embedded manifest (<see cref="ToolManifest"/>)
    /// to a handler, and is the <see cref="IToolDispatcher"/>
    /// <see cref="BridgeServer"/> dispatches to. A manifest entry with no
    /// handler, or a handler with no manifest entry, throws at construction —
    /// the two must never drift, in either direction.
    ///
    /// Checkpoint 1: every handler is a placeholder ("not implemented"); the
    /// eighteen names, their manifest binding and the unknown-tool contract
    /// are what this checkpoint proves. Later checkpoints replace the
    /// placeholders with the real tool families (<c>Tools/EditorTools.cs</c>,
    /// <c>DiagnosticsTools.cs</c>, <c>ReviewTools.cs</c>, <c>DebugTools.cs</c>)
    /// without touching this binding table's shape.
    /// </summary>
    public sealed class ToolRegistry : IToolDispatcher
    {
        internal delegate Task<ToolResult> ToolHandler(JsonElement args, object connection, CancellationToken ct);

        private readonly IReadOnlyList<ToolInfo> _list;
        private readonly IReadOnlyDictionary<string, ToolHandler> _handlers;

        public ToolRegistry(IEditorHost host)
        {
            var manifest = ToolManifest.Load();

            var handlers = BuildHandlers(host);

            var manifestNames = new HashSet<string>(manifest.Select(m => m.Name), StringComparer.Ordinal);
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
                .Select(m => new ToolInfo(m.Name, m.Description, m.InputSchema))
                .ToList();
        }

        private static Dictionary<string, ToolHandler> BuildHandlers(IEditorHost host)
        {
            // Checkpoint 1 placeholder: no family classes exist yet, so every
            // name resolves to the same "not implemented" handler. `host` is
            // deliberately unused here — nothing at this checkpoint calls it.
            _ = host;

            ToolHandler notImplemented = (args, connection, ct) =>
                Task.FromResult(new ToolResult("not implemented", true));

            return new Dictionary<string, ToolHandler>(StringComparer.Ordinal)
            {
                ["context"] = notImplemented,
                ["open"] = notImplemented,
                ["definition"] = notImplemented,
                ["references"] = notImplemented,
                ["hover"] = notImplemented,
                ["diagnostics"] = notImplemented,
                ["debug_configs"] = notImplemented,
                ["debug_start"] = notImplemented,
                ["debug_breakpoint"] = notImplemented,
                ["debug_continue"] = notImplemented,
                ["debug_step"] = notImplemented,
                ["debug_stack"] = notImplemented,
                ["debug_variables"] = notImplemented,
                ["debug_evaluate"] = notImplemented,
                ["debug_output"] = notImplemented,
                ["debug_stop"] = notImplemented,
                ["review_diff"] = notImplemented,
                ["review_cancel"] = notImplemented,
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
            // Checkpoint 3 wires this to ReviewTools' accept-all/pending
            // state; nothing at this checkpoint holds per-connection state.
        }
    }
}
