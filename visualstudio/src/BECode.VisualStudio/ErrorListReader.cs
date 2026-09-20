using System;
using System.Collections.Generic;
using System.IO;
using System.Threading;
using System.Threading.Tasks;
using BECode.Bridge;
using BECode.Bridge.Hosting;
using EnvDTE;
using EnvDTE80;
using Microsoft.VisualStudio;
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
    /// itself — fix round 1, I-10, AFTER rooting a non-absolute
    /// <c>ErrorItem.FileName</c> (build errors from MSBuild often carry a
    /// project-relative or bare name), since the exact-absolute-path match
    /// was silently dropping every one of those. Severity filtering is
    /// explicitly NOT this host's job (Ruling S4) — every diagnostic is
    /// returned regardless of path filtering's outcome, and
    /// <c>DiagnosticsTools</c> filters by severity.
    /// </summary>
    internal sealed class ErrorListReader
    {
        /// <summary>Fix round 1, I-11: the Error List is capped here so an enormous solution cannot make one call read tens of thousands of COM items.</summary>
        private const int MaxItems = 5000;

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

                // Fix round 1, I-10: candidate directories to root a
                // non-absolute FileName against — the owning project's
                // directory first, then the solution directory — via the
                // SAME enumeration WorkspaceFolders uses, so no separate
                // solution walk is written here.
                var solutionService = await _package.GetServiceAsync(typeof(SVsSolution)).ConfigureAwait(true) as IVsSolution;
                var projectDirsByName = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);
                foreach (var project in WorkspaceFolders.EnumerateLoadedProjects(solutionService))
                {
                    if (!string.IsNullOrEmpty(project.Dir) && !projectDirsByName.ContainsKey(project.Name))
                    {
                        projectDirsByName[project.Name] = project.Dir;
                    }
                }

                string? solutionDir = null;
                try
                {
                    if (solutionService != null
                        && ErrorHandler.Succeeded(solutionService.GetSolutionInfo(out var dir, out _, out _))
                        && !string.IsNullOrEmpty(dir))
                    {
                        solutionDir = dir;
                    }
                }
                catch (Exception ex)
                {
                    ActivityLog.LogWarning(nameof(DiagnosticsAsync), ex.ToString());
                }

                var result = new List<Diagnostic>();
                var count = errorItems.Count;
                var limit = Math.Min(count, MaxItems);
                var truncated = count > MaxItems;

                // COM collections are 1-based (host design §2.3).
                for (var i = 1; i <= limit; i++)
                {
                    ct.ThrowIfCancellationRequested();

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

                    var rawFileName = item.FileName ?? string.Empty;
                    var projectName = item.Project;

                    var candidates = new List<string?>(2);
                    if (!string.IsNullOrEmpty(projectName) && projectDirsByName.TryGetValue(projectName, out var projectDir))
                    {
                        candidates.Add(projectDir);
                    }

                    candidates.Add(solutionDir);

                    var fileName = PathRooting.Root(rawFileName, candidates, File.Exists) ?? rawFileName;

                    if (path != null && !string.Equals(fileName, path, StringComparison.OrdinalIgnoreCase))
                    {
                        continue;
                    }

                    var description = item.Description ?? string.Empty;
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

                if (truncated)
                {
                    // Fix round 1, I-11: a synthetic diagnostic so the model
                    // sees the truncation rather than silently getting a
                    // partial answer — Source "be-code" distinguishes it
                    // from anything the Error List itself produced.
                    result.Add(new Diagnostic(
                        solutionDir ?? string.Empty,
                        1,
                        1,
                        "info",
                        "be-code",
                        "Error List truncated at " + MaxItems + " of " + count + " items"));
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
