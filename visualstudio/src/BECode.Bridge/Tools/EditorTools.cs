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
        // Ruling S6: the 2048-character selection cut moves here from the
        // host — the host returns the raw, untruncated selection.
        private const int SelectionCap = 2048;

        private readonly IEditorHost _host;

        public EditorTools(IEditorHost host)
        {
            _host = host;
        }

        public async Task<ToolResult> Context(JsonElement args, object connection, CancellationToken ct)
        {
            var context = await _host.GetContextAsync(ct).ConfigureAwait(false);
            var folders = await _host.GetWorkspaceFoldersAsync(ct).ConfigureAwait(false);

            // Ruling S3: file/open cross the seam absolute; relativise here,
            // exactly where vscode's own context handler does (against
            // folders()). An empty file (no active editor) stays empty —
            // there is nothing to relativise.
            var file = context.File.Length == 0 ? context.File : Paths.RelPath(folders, context.File);
            var open = context.Open.Select(p => Paths.RelPath(folders, p)).ToList();
            var selection = Truncate(context.Selection, SelectionCap);

            var payload = new
            {
                file,
                line = context.Line,
                selStart = context.SelStart,
                selEnd = context.SelEnd,
                selection,
                open,
                workspaceFolders = folders,
            };
            return new ToolResult(JsonSerializer.Serialize(payload), false);
        }

        public async Task<ToolResult> Open(JsonElement args, object connection, CancellationToken ct)
        {
            if (!ToolArgs.TryRequireString(args, "path", out var path, out var err))
            {
                return err!;
            }

            var (abs, _, resolveErr) = await ToolPaths.ResolveAsync(_host, path, ct).ConfigureAwait(false);
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

            var (abs, folders, resolveErr) = await ToolPaths.ResolveAsync(_host, path, ct).ConfigureAwait(false);
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

            return new ToolResult(string.Join("\n", locations.Select(l => $"{Paths.RelPath(folders!, l.Path)}:{l.Line}:{l.Col}")), false);
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

            // Fix round 2, N1: a non-positive max used to still print the
            // "N reference(s)" header with nothing listed. Clamp first, then
            // — like vscode's editor.ts:64 — decide "nothing to show" from
            // the length AFTER slicing, not the total before it: with
            // max=0 there may be 200 real references and the answer is
            // still "no references found".
            var max = System.Math.Max(0, ToolArgs.GetInt(args, "max") ?? 50);

            var (abs, folders, resolveErr) = await ToolPaths.ResolveAsync(_host, path, ct).ConfigureAwait(false);
            if (resolveErr != null)
            {
                return resolveErr;
            }

            // Ruling S5: the host returns every reference; the TOTAL count
            // (before slicing) goes in the header, and only the first `max`
            // are printed — matching vscode's own res.length-before-slice.
            var locations = await _host.ReferencesAsync(abs!, line, col, ct).ConfigureAwait(false);
            if (locations == null)
            {
                return new ToolResult("not available for this file type", true);
            }

            var shown = locations.Take(max).Select(l => $"{Paths.RelPath(folders!, l.Path)}:{l.Line}: {(l.Text ?? "").Trim()}").ToList();
            if (shown.Count == 0)
            {
                return new ToolResult("no references found", false);
            }

            return new ToolResult($"{locations.Count} reference(s)\n" + string.Join("\n", shown), false);
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

            var (abs, _, resolveErr) = await ToolPaths.ResolveAsync(_host, path, ct).ConfigureAwait(false);
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

        private static string Truncate(string text, int max) => text.Length <= max ? text : text.Substring(0, max);
    }
}
