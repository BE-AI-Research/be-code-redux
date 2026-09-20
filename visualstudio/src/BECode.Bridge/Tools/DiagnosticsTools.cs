using System.Collections.Generic;
using System.Linq;
using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;

namespace BECode.Bridge.Tools
{
    /// <summary>
    /// The <c>diagnostics</c> tool. Ports vscode/src/tools/diagnostics.ts.
    /// The default severity is errors and
    /// warnings, matching vscode and the manifest's own description; do not
    /// change the default to "all".
    /// </summary>
    public sealed class DiagnosticsTools
    {
        private readonly IEditorHost _host;

        public DiagnosticsTools(IEditorHost host)
        {
            _host = host;
        }

        public async Task<ToolResult> Diagnostics(JsonElement args, object connection, CancellationToken ct)
        {
            var rawPath = ToolArgs.GetString(args, "path");
            var severityArg = ToolArgs.GetString(args, "severity");

            // IEditorHost.DiagnosticsAsync's path is absolute or
            // null, so an optional filter argument is resolved and confined
            // exactly like every other path
            // argument, before it reaches the host.
            string? absPath = null;
            IReadOnlyList<string>? folders = null;
            if (rawPath != null)
            {
                var (abs, resolvedFolders, resolveErr) = await ToolPaths.ResolveAsync(_host, rawPath, ct).ConfigureAwait(false);
                if (resolveErr != null)
                {
                    return resolveErr;
                }

                absPath = abs;
                folders = resolvedFolders;
            }

            var items = await _host.DiagnosticsAsync(absPath, ct).ConfigureAwait(false);

            // Only fetch workspace folders a second time when the first
            // resolution above didn't already give them to us (no `path`
            // filter argument was supplied).
            folders ??= await _host.GetWorkspaceFoldersAsync(ct).ConfigureAwait(false);

            var want = new HashSet<string>(Format.SeveritiesFor(severityArg), System.StringComparer.Ordinal);
            var display = items
                .Where(d => want.Contains(d.Severity))
                .Select(d => d with { Path = Paths.RelPath(folders, d.Path) })
                .ToList();

            return new ToolResult(Format.Diagnostics(display), false);
        }
    }
}
