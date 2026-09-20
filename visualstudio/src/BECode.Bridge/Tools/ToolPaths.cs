using System;
using System.Collections.Generic;
using System.Threading;
using System.Threading.Tasks;

namespace BECode.Bridge.Tools
{
    /// <summary>
    /// Resolves a tool's raw path argument to an absolute path confined to
    /// one of the host's workspace folders (<see cref="Paths.AbsPath"/>),
    /// fetching those folders from <see cref="IEditorHost.GetWorkspaceFoldersAsync"/>.
    /// A path that escapes every folder never reaches the host:
    /// the caller gets <c>Error</c> back and must return it without calling
    /// anything else on the host. <see cref="Folders"/> is also handed back
    /// on success, so a caller that needs to relativise the host's response
    /// (<c>definition</c>, <c>references</c>, <c>debug_stack</c>,
    /// <c>diagnostics</c>) does not have to fetch the same list twice.
    /// </summary>
    internal static class ToolPaths
    {
        public static async Task<(string? Abs, IReadOnlyList<string>? Folders, ToolResult? Error)> ResolveAsync(IEditorHost host, string rawPath, CancellationToken ct)
        {
            IReadOnlyList<string> folders;
            try
            {
                folders = await host.GetWorkspaceFoldersAsync(ct).ConfigureAwait(false);
            }
            // Let cancellation propagate rather than
            // turning a connection going away into a synthesized tool error.
            catch (Exception ex) when (!(ex is OperationCanceledException))
            {
                return (null, null, new ToolResult($"workspace folders: {ex.Message}", true));
            }

            // An empty workspace-folder list (no solution or
            // folder open in Visual Studio) is refused here, before
            // Paths.AbsPath ever runs — AbsPath's own TS-parity fallback
            // (resolve against the process's current directory) would
            // resolve into whatever devenv.exe's arbitrary CWD happens to
            // be, never a directory the user meant.
            if (folders.Count == 0)
            {
                return (null, null, new ToolResult("no solution or folder is open", true));
            }

            try
            {
                var abs = Paths.AbsPath(folders, rawPath);
                return (abs, folders, null);
            }
            catch (PathOutsideWorkspaceException ex)
            {
                return (null, null, new ToolResult(ex.Message, true));
            }
            // A malformed path argument (e.g. an embedded
            // NUL character) makes Path.GetFullPath/Path.Combine throw
            // ArgumentException; left uncaught, it would escape
            // ToolRegistry.CallAsync entirely — an expected, caller-triggerable
            // failure must come back as an ordinary isError:true result, not
            // an unhandled exception. Cancellation still propagates.
            catch (Exception ex) when (!(ex is OperationCanceledException))
            {
                return (null, null, new ToolResult($"invalid path: {ex.Message}", true));
            }
        }
    }
}
