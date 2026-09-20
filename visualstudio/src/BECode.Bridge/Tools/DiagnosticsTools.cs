using System.Collections.Generic;
using System.Linq;
using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;

namespace BECode.Bridge.Tools
{
    /// <summary>
    /// The <c>diagnostics</c> tool. Ports vscode/src/tools/diagnostics.ts.
    /// Fix round 1, F3 (Ruling R-6): the default severity is errors and
    /// warnings, matching vscode and the manifest's own description — the
    /// original "defaults to all" was a mistake in the task brief, not a
    /// deliberate Visual Studio difference.
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
            var path = ToolArgs.GetString(args, "path");
            var severityArg = ToolArgs.GetString(args, "severity");

            var items = await _host.DiagnosticsAsync(path, ct).ConfigureAwait(false);

            var want = new HashSet<string>(Format.SeveritiesFor(severityArg), System.StringComparer.Ordinal);
            var filtered = items.Where(d => want.Contains(d.Severity)).ToList();

            return new ToolResult(Format.Diagnostics(filtered), false);
        }
    }
}
