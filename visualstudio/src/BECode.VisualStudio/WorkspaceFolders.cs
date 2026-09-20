using System;
using System.Collections.Generic;
using System.Linq;
using System.Threading;
using System.Threading.Tasks;
using BECode.Bridge.Hosting;
using Microsoft.VisualStudio;
using Microsoft.VisualStudio.Shell;
using Microsoft.VisualStudio.Shell.Interop;
using Microsoft.VisualStudio.Threading;

namespace BECode.VisualStudio
{
    /// <summary>
    /// Host design §2.2: the solution/project (or, in Open Folder mode, the
    /// opened folder alone) roots <see cref="VisualStudioEditorHost.GetWorkspaceFoldersAsync"/>
    /// answers from. Read through <see cref="IVsSolution"/> (never
    /// <c>DTE.Solution.Projects</c> — that needs a recursive walk through
    /// solution folders and throws on an unloaded project), republished on
    /// <see cref="IVsSolutionEvents"/>/<see cref="IVsSolutionEvents7"/> and
    /// debounced 250ms (<see cref="Debouncer"/>) so the burst of events a
    /// solution load fires collapses into one recompute. <see cref="Current"/>
    /// reads a cached field and never touches the UI thread — <c>GetWorkspaceFoldersAsync</c>
    /// is called on every path-taking tool.
    /// </summary>
    internal sealed class WorkspaceFolders : IVsSolutionEvents, IVsSolutionEvents7, IDisposable
    {
        private static readonly TimeSpan DebounceDelay = TimeSpan.FromMilliseconds(250);

        private readonly AsyncPackage _package;
        private readonly Debouncer _debouncer;
        private IVsSolution? _solution;
        private uint _solutionEventsCookie;
        private string? _openFolder;
        private volatile IReadOnlyList<string> _current = Array.Empty<string>();

        public WorkspaceFolders(AsyncPackage package)
        {
            _package = package ?? throw new ArgumentNullException(nameof(package));
            _debouncer = new Debouncer(DebounceDelay, RecomputeAsync);
        }

        /// <summary>
        /// The current folder list — cached, never touches the UI thread
        /// (host design §2.2). Empty when no solution and no folder is
        /// open; the tools already answer "no solution or folder is open"
        /// for that case.
        /// </summary>
        public IReadOnlyList<string> Current => _current;

        /// <summary>Called once from <see cref="BECodePackage.InitializeAsync(CancellationToken, IProgress{ServiceProgressData})"/>.</summary>
        public async Task InitializeAsync(CancellationToken ct)
        {
            await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync(ct);

            _solution = await _package.GetServiceAsync(typeof(SVsSolution)).ConfigureAwait(true) as IVsSolution;
            if (_solution != null)
            {
                _solution.AdviseSolutionEvents(this, out _solutionEventsCookie);
            }

            await RecomputeAsync().ConfigureAwait(true);
        }

        private void Republish()
        {
            _debouncer.Trigger();
        }

        private async Task RecomputeAsync()
        {
            await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync(CancellationToken.None);

            IReadOnlyList<string> next;
            try
            {
                next = ComputeOnMainThread();
            }
            catch (Exception ex)
            {
                ActivityLog.LogError(nameof(WorkspaceFolders), ex.ToString());
                next = Array.Empty<string>();
            }

            await TaskScheduler.Default;
            _current = next;
        }

        // Called only from RecomputeAsync/InitializeAsync, both of which
        // switch to the main thread with a literal
        // "await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync(...)"
        // immediately before calling this — the VSTHRD010/VSTHRD108
        // analyzers only recognise that EXACT call shape in the CALLING
        // method's own body to prove thread affinity; routing it through
        // Threading.SwitchToMainThreadAsync (a wrapper) or asserting with
        // ThreadHelper.ThrowIfNotOnUIThread() here instead both left this
        // flagged (design correction — see the report).
        private IReadOnlyList<string> ComputeOnMainThread()
        {
            ThreadHelper.ThrowIfNotOnUIThread();

            if (_openFolder != null)
            {
                return new[] { _openFolder };
            }

            if (_solution == null)
            {
                return Array.Empty<string>();
            }

            var folders = new List<string>();

            if (ErrorHandler.Succeeded(_solution.GetSolutionInfo(out var solutionDir, out _, out _)) && !string.IsNullOrEmpty(solutionDir))
            {
                folders.Add(solutionDir);
            }

            var guid = Guid.Empty;
            if (ErrorHandler.Succeeded(_solution.GetProjectEnum((uint)__VSENUMPROJFLAGS.EPF_LOADEDINSOLUTION, ref guid, out var enumHierarchies)) && enumHierarchies != null)
            {
                var buffer = new IVsHierarchy[1];
                while (enumHierarchies.Next(1, buffer, out var fetched) == VSConstants.S_OK && fetched == 1)
                {
                    var hierarchy = buffer[0];
                    if (hierarchy == null)
                    {
                        continue;
                    }

                    if (ErrorHandler.Succeeded(hierarchy.GetProperty(VSConstants.VSITEMID_ROOT, (int)__VSHPROPID.VSHPROPID_ProjectDir, out var value))
                        && value is string dir && !string.IsNullOrEmpty(dir))
                    {
                        folders.Add(dir);
                    }
                }
            }

            return folders
                .Select(f => f.TrimEnd('\\', '/'))
                .Where(f => f.Length > 0)
                .Distinct(StringComparer.OrdinalIgnoreCase)
                .ToArray();
        }

        // IVsSolutionEvents — every mutation republishes (debounced); every
        // "query" callback is a plain S_OK/no-op, this sink never vetoes
        // anything.
        public int OnAfterOpenProject(IVsHierarchy pHierarchy, int fAdded)
        {
            Republish();
            return VSConstants.S_OK;
        }

        public int OnQueryCloseProject(IVsHierarchy pHierarchy, int fRemoving, ref int pfCancel) => VSConstants.S_OK;

        public int OnBeforeCloseProject(IVsHierarchy pHierarchy, int fRemoved)
        {
            Republish();
            return VSConstants.S_OK;
        }

        public int OnAfterLoadProject(IVsHierarchy pStubHierarchy, IVsHierarchy pRealHierarchy)
        {
            Republish();
            return VSConstants.S_OK;
        }

        public int OnQueryUnloadProject(IVsHierarchy pRealHierarchy, ref int pfCancel) => VSConstants.S_OK;

        public int OnBeforeUnloadProject(IVsHierarchy pRealHierarchy, IVsHierarchy pStubHierarchy)
        {
            Republish();
            return VSConstants.S_OK;
        }

        public int OnAfterOpenSolution(object pUnkReserved, int fNewSolution)
        {
            Republish();
            return VSConstants.S_OK;
        }

        public int OnQueryCloseSolution(object pUnkReserved, ref int pfCancel) => VSConstants.S_OK;

        public int OnBeforeCloseSolution(object pUnkReserved) => VSConstants.S_OK;

        public int OnAfterCloseSolution(object pUnkReserved)
        {
            Republish();
            return VSConstants.S_OK;
        }

        // IVsSolutionEvents7 — Open Folder mode.
        public void OnAfterOpenFolder(string folderPath)
        {
            _openFolder = folderPath;
            Republish();
        }

        public void OnBeforeCloseFolder(string folderPath)
        {
        }

        public void OnAfterCloseFolder(string folderPath)
        {
            _openFolder = null;
            Republish();
        }

        public void OnQueryCloseFolder(string folderPath, ref int pfCancel)
        {
        }

        public void OnAfterLoadAllDeferredProjects()
        {
            Republish();
        }

        /// <summary>
        /// Non-blocking: unadvising touches the main thread, but a plain
        /// <see cref="IDisposable.Dispose"/> may run on any thread (host
        /// design §1.3/rule list — never <c>JoinableTaskFactory.Run</c>, so
        /// this cannot simply block until it is done). Fires the unadvise on
        /// the JTF and returns immediately, same shape as
        /// <see cref="BECodePackage.Dispose(bool)"/>'s own bounded,
        /// non-blocking server teardown.
        /// </summary>
        public void Dispose()
        {
            _debouncer.Dispose();

            var solution = _solution;
            var cookie = _solutionEventsCookie;
            _solution = null;
            _solutionEventsCookie = 0;

            if (solution != null && cookie != 0)
            {
                ThreadHelper.JoinableTaskFactory.RunAsync(async () =>
                {
                    await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync();
                    try
                    {
                        solution.UnadviseSolutionEvents(cookie);
                    }
                    catch (Exception ex)
                    {
                        ActivityLog.LogError(nameof(WorkspaceFolders), ex.ToString());
                    }
                }).FileAndForget("becode/workspacefolders/dispose");
            }
        }
    }
}
