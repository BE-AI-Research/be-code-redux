using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;

namespace BECode.Bridge.Tools
{
    /// <summary>
    /// The <c>diagnostics</c> tool. Ports vscode/src/tools/diagnostics.ts,
    /// except the default severity: vscode's own default (no argument given)
    /// is errors and warnings only, but the design brief for this project
    /// requires <c>diagnostics</c> to default to "all" here — a deliberate
    /// Visual Studio difference, not an oversight (see the task report).
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
            var severity = ToolArgs.GetString(args, "severity") ?? "all";
            var items = await _host.DiagnosticsAsync(path, severity, ct).ConfigureAwait(false);
            return new ToolResult(Format.Diagnostics(items), false);
        }
    }
}
