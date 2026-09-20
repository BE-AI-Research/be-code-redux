using System;
using System.Collections.Generic;
using System.Threading;
using System.Threading.Tasks;
using BECode.Bridge;
using BECode.Bridge.Hosting;
using EnvDTE;
using EnvDTE80;
using Microsoft.VisualStudio.Shell;
using Microsoft.VisualStudio.Shell.Interop;

namespace BECode.VisualStudio
{
    /// <summary>
    /// Host design §3.4, <c>DiagnosticsAsync</c>: reads
    /// <c>((DTE2)dte).ToolWindows.ErrorList.ErrorItems</c> — a 1-based COM
    /// collection unchanged since Visual Studio 2005, chosen over the newer
    /// IErrorList/table API because it is five properties on a stable
    /// interface rather than something much easier to get wrong unrun.
    /// <c>Ruling D11</c> (<see cref="IEditorHost.DiagnosticsAsync"/>'s own
    /// doc comment) says a non-null path is matched EXACTLY here, not left
    /// to the tool: <see cref="DiagnosticsAsync"/> does that filtering
    /// itself. Severity filtering is explicitly NOT this host's job
    /// (Ruling S4) — every diagnostic is returned regardless of path
    /// filtering's outcome, and <c>DiagnosticsTools</c> filters by severity.
    /// </summary>
    internal sealed class ErrorListReader
    {
        private readonly AsyncPackage _package;

        public ErrorListReader(AsyncPackage package)
        {
            _package = package ?? throw new ArgumentNullException(nameof(package));
        }

        public Task<IReadOnlyList<Diagnostic>> DiagnosticsAsync(string? path, CancellationToken ct)
        {
            return Guard.RunAsync(nameof(DiagnosticsAsync), ct, async () =>
            {
                await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync(ct);

                // Host design §3.4's second caveat: reading ErrorItems
                // forces the Error List tool window to be created once —
                // that is expected, not a bug, if it has never been shown
                // this session.
                var dte = await _package.GetServiceAsync(typeof(SDTE)).ConfigureAwait(true) as DTE2;
                var errorItems = dte?.ToolWindows?.ErrorList?.ErrorItems;
                if (errorItems == null)
                {
                    return (IReadOnlyList<Diagnostic>)Array.Empty<Diagnostic>();
                }

                var result = new List<Diagnostic>();
                var count = errorItems.Count;

                // COM collections are 1-based (host design §2.3).
                for (var i = 1; i <= count; i++)
                {
                    ErrorItem? item;
                    try
                    {
                        item = errorItems.Item(i);
                    }
                    catch (ArgumentException)
                    {
                        // The Error List's own contents can change out from
                        // under us mid-enumeration (a build finishing, the
                        // user clearing it); a stale index is skipped, not
                        // fatal to the whole call.
                        continue;
                    }

                    if (item == null)
                    {
                        continue;
                    }

                    var fileName = item.FileName ?? string.Empty;

                    if (path != null && !string.Equals(fileName, path, StringComparison.OrdinalIgnoreCase))
                    {
                        continue;
                    }

                    var description = item.Description ?? string.Empty;
                    var projectName = item.Project;
                    var source = ErrorSource.ParseCode(description)
                        ?? (string.IsNullOrEmpty(projectName) ? "-" : projectName);

                    result.Add(new Diagnostic(
                        fileName,
                        item.Line,
                        item.Column,
                        SeverityFor(item.ErrorLevel),
                        source,
                        description));
                }

                return (IReadOnlyList<Diagnostic>)result;
            });
        }

        private static string SeverityFor(vsBuildErrorLevel level)
        {
            switch (level)
            {
                case vsBuildErrorLevel.vsBuildErrorLevelHigh:
                    return "error";
                case vsBuildErrorLevel.vsBuildErrorLevelMedium:
                    return "warning";
                case vsBuildErrorLevel.vsBuildErrorLevelLow:
                    return "info";
                default:
                    return "info";
            }
        }
    }
}
