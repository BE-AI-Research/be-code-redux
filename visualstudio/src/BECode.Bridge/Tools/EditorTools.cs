using System.Linq;
using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;

namespace BECode.Bridge.Tools
{
    /// <summary>
    /// The five editor tools: context, open, definition, references, hover.
    /// Ports vscode/src/tools/editor.ts's handlers; where a language service
    /// is unavailable, <see cref="IEditorHost"/> answers with null and this
    /// class turns that into the design spec's "not available for this file
    /// type" (section 4), never an empty success.
    /// </summary>
    public sealed class EditorTools
    {
        private static readonly JsonSerializerOptions ContextOptions = new JsonSerializerOptions
        {
            PropertyNamingPolicy = JsonNamingPolicy.CamelCase,
        };

        private readonly IEditorHost _host;

        public EditorTools(IEditorHost host)
        {
            _host = host;
        }

        public async Task<ToolResult> Context(JsonElement args, object connection, CancellationToken ct)
        {
            var context = await _host.GetContextAsync(ct).ConfigureAwait(false);
            return new ToolResult(JsonSerializer.Serialize(context, ContextOptions), false);
        }

        public async Task<ToolResult> Open(JsonElement args, object connection, CancellationToken ct)
        {
            if (!ToolArgs.TryRequireString(args, "path", out var path, out var err))
            {
                return err!;
            }

            var (abs, resolveErr) = await ToolPaths.ResolveAsync(_host, path, ct).ConfigureAwait(false);
            if (resolveErr != null)
            {
                return resolveErr;
            }

            var line = ToolArgs.GetInt(args, "line");
            await _host.OpenAsync(abs!, line, ct).ConfigureAwait(false);
            return new ToolResult($"opened {path}{(line.HasValue ? ":" + line.Value : "")}", false);
        }

        public async Task<ToolResult> Definition(JsonElement args, object connection, CancellationToken ct)
        {
            if (!ToolArgs.TryRequireString(args, "path", out var path, out var errPath))
            {
                return errPath!;
            }

            if (!ToolArgs.TryRequireInt(args, "line", out var line, out var errLine))
            {
                return errLine!;
            }

            if (!ToolArgs.TryRequireInt(args, "col", out var col, out var errCol))
            {
                return errCol!;
            }

            var (abs, resolveErr) = await ToolPaths.ResolveAsync(_host, path, ct).ConfigureAwait(false);
            if (resolveErr != null)
            {
                return resolveErr;
            }

            var locations = await _host.DefinitionAsync(abs!, line, col, ct).ConfigureAwait(false);
            if (locations == null)
            {
                return new ToolResult("not available for this file type", true);
            }

            if (locations.Count == 0)
            {
                return new ToolResult("no definition found (is the language server running and the file saved?)", false);
            }

            return new ToolResult(string.Join("\n", locations.Select(l => $"{l.Path}:{l.Line}:{l.Col}")), false);
        }

        public async Task<ToolResult> References(JsonElement args, object connection, CancellationToken ct)
        {
            if (!ToolArgs.TryRequireString(args, "path", out var path, out var errPath))
            {
                return errPath!;
            }

            if (!ToolArgs.TryRequireInt(args, "line", out var line, out var errLine))
            {
                return errLine!;
            }

            if (!ToolArgs.TryRequireInt(args, "col", out var col, out var errCol))
            {
                return errCol!;
            }

            var max = ToolArgs.GetInt(args, "max") ?? 50;

            var (abs, resolveErr) = await ToolPaths.ResolveAsync(_host, path, ct).ConfigureAwait(false);
            if (resolveErr != null)
            {
                return resolveErr;
            }

            var locations = await _host.ReferencesAsync(abs!, line, col, max, ct).ConfigureAwait(false);
            if (locations == null)
            {
                return new ToolResult("not available for this file type", true);
            }

            if (locations.Count == 0)
            {
                return new ToolResult("no references found", false);
            }

            var lines = locations.Select(l => $"{l.Path}:{l.Line}: {(l.Text ?? "").Trim()}");
            return new ToolResult($"{locations.Count} reference(s)\n" + string.Join("\n", lines), false);
        }

        public async Task<ToolResult> Hover(JsonElement args, object connection, CancellationToken ct)
        {
            if (!ToolArgs.TryRequireString(args, "path", out var path, out var errPath))
            {
                return errPath!;
            }

            if (!ToolArgs.TryRequireInt(args, "line", out var line, out var errLine))
            {
                return errLine!;
            }

            if (!ToolArgs.TryRequireInt(args, "col", out var col, out var errCol))
            {
                return errCol!;
            }

            var (abs, resolveErr) = await ToolPaths.ResolveAsync(_host, path, ct).ConfigureAwait(false);
            if (resolveErr != null)
            {
                return resolveErr;
            }

            var text = await _host.HoverAsync(abs!, line, col, ct).ConfigureAwait(false);
            if (text == null)
            {
                return new ToolResult("not available for this file type", true);
            }

            var trimmed = text.Trim();
            return new ToolResult(trimmed.Length == 0 ? "no hover information" : trimmed, false);
        }
    }
}
