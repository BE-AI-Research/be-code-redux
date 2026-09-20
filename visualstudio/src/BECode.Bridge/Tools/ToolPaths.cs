using System.Threading;
using System.Threading.Tasks;

namespace BECode.Bridge.Tools
{
    /// <summary>
    /// Resolves a tool's raw path argument to an absolute path confined to
    /// one of the host's workspace folders (<see cref="Paths.AbsPath"/>),
    /// fetching those folders from <see cref="IEditorHost.GetContextAsync"/>
    /// since the brief's <see cref="IEditorHost"/> exposes no lighter-weight
    /// accessor. A path that escapes every folder never reaches the host:
    /// the caller gets <c>Error</c> back and must return it without calling
    /// anything else on the host.
    /// </summary>
    internal static class ToolPaths
    {
        public static async Task<(string? Abs, ToolResult? Error)> ResolveAsync(IEditorHost host, string rawPath, CancellationToken ct)
        {
            EditorContext context;
            try
            {
                context = await host.GetContextAsync(ct).ConfigureAwait(false);
            }
            catch (System.Exception ex)
            {
                return (null, new ToolResult($"context: {ex.Message}", true));
            }

            try
            {
                var abs = Paths.AbsPath(context.WorkspaceFolders, rawPath);
                return (abs, null);
            }
            catch (PathOutsideWorkspaceException ex)
            {
                return (null, new ToolResult(ex.Message, true));
            }
        }
    }
}
