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

        /// <summary>The solution-folder project type GUID: skipped everywhere loaded projects are enumerated.</summary>
        private static readonly Guid SolutionFolderTypeGuid = new Guid("{2150E333-8FDC-42A3-9474-1A3956D46DE8}");

        private readonly AsyncPackage _package;
        private readonly Debouncer _debouncer;
        private readonly GenerationGate _generationGate = new GenerationGate();
        private IVsSolution? _solution;
        private uint _solutionEventsCookie;
        private string? _openFolder;
        private volatile IReadOnlyList<string> _current = Array.Empty<string>();
        private volatile IReadOnlyList<ProjectEntry> _currentProjects = Array.Empty<ProjectEntry>();
        private volatile bool _disposed;

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

        /// <summary>
        /// Every loaded project (solution folders
        /// already excluded), as of the same recompute that produced
        /// <see cref="Current"/> — shared by <c>VisualStudioDebugHost.ConfigsAsync</c>
        /// (listing startable projects) and <c>ErrorListReader</c> (rooting
        /// a diagnostic's project-relative <c>FileName</c>) so neither
        /// copies the <see cref="IVsSolution"/> enumeration in
        /// <see cref="ComputeOnMainThread"/>.
        /// </summary>
        public IReadOnlyList<ProjectEntry> CurrentProjects => _currentProjects;

        /// <summary>
        /// Fires after every recompute — including the
        /// initial one — with the generation that recompute claimed (via
        /// <see cref="GenerationGate.Next"/>, in RECOMPUTE-START order) and
        /// the folder list it produced. <c>BECodePackage</c> uses this to
        /// keep the lock file's <c>workspaceFolders</c> current; it owns its
        /// own <see cref="GenerationGate"/> to decide whether a given
        /// firing is still the newest one it has seen, since a later
        /// recompute's write can finish before an earlier one's. Always
        /// raised off the UI thread (after <see cref="RecomputeAsync"/>'s
        /// own hop) so a subscriber's own I/O never runs on it.
        /// </summary>
        public event Action<long, IReadOnlyList<string>>? Changed;

        /// <summary>Called once from <see cref="BECodePackage.InitializeAsync(CancellationToken, IProgress{ServiceProgressData})"/>.</summary>
        public async Task InitializeAsync(CancellationToken ct)
        {
            await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync(ct);

            _solution = await _package.GetServiceAsync(typeof(SVsSolution)).ConfigureAwait(true) as IVsSolution;
            if (_solution != null)
            {
                _solution.AdviseSolutionEvents(this, out _solutionEventsCookie);

                // Open Folder mode is otherwise detected
                // only by OnAfterOpenFolder, which never fires for a folder
                // that was ALREADY open when the package itself finishes
                // loading (the package can auto-load after Open Folder has
                // already finished opening — UIContext SolutionExists is
                // satisfied by Open Folder mode too).
                try
                {
                    if (ErrorHandler.Succeeded(_solution.GetProperty((int)__VSPROPID7.VSPROPID_IsInOpenFolderMode, out var isOpenFolderObj))
                        && isOpenFolderObj is bool isOpenFolder && isOpenFolder
                        && ErrorHandler.Succeeded(_solution.GetSolutionInfo(out var folderDir, out _, out _))
                        && !string.IsNullOrEmpty(folderDir))
                    {
                        _openFolder = folderDir;
                    }
                }
                catch (Exception ex)
                {
                    ActivityLog.LogError(nameof(InitializeAsync), ex.ToString());
                }
            }

            await RecomputeAsync().ConfigureAwait(true);
        }

        private void Republish()
        {
            _debouncer.Trigger();
        }

        private async Task RecomputeAsync()
        {
            // Claimed in RECOMPUTE-START order, before any
            // await — this is what lets BECodePackage's GenerationGate
            // reject a late-finishing write for a recompute that a
            // LATER-started one has already superseded, however the two
            // actually finish relative to each other.
            var generation = _generationGate.Next();

            await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync(CancellationToken.None);

            IReadOnlyList<string> next;
            IReadOnlyList<ProjectEntry> nextProjects;
            try
            {
                nextProjects = EnumerateLoadedProjects(_solution);
                next = ComputeOnMainThread(nextProjects);
            }
            catch (Exception ex)
            {
                ActivityLog.LogError(nameof(WorkspaceFolders), ex.ToString());
                next = Array.Empty<string>();
                nextProjects = Array.Empty<ProjectEntry>();
            }

            // Assign the volatile fields BEFORE hopping
            // off the UI thread — Current/CurrentProjects must reflect this
            // recompute's result as soon as it is known, not only once this
            // method has also finished hopping to the thread pool.
            _current = next;
            _currentProjects = nextProjects;

            await TaskScheduler.Default;

            if (!_disposed)
            {
                Changed?.Invoke(generation, next);
            }
        }

        // Called only from RecomputeAsync/InitializeAsync, both of which
        // switch to the main thread with a literal
        // "await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync(...)"
        // immediately before calling this — the VSTHRD010/VSTHRD108
        // analyzers only recognise that EXACT call shape in the CALLING
        // method's own body to prove thread affinity; routing it through
        // Threading.SwitchToMainThreadAsync (a wrapper) or asserting with
        // ThreadHelper.ThrowIfNotOnUIThread() here instead both left this
        // flagged by the analyzer.
        private IReadOnlyList<string> ComputeOnMainThread(IReadOnlyList<ProjectEntry> projects)
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

            foreach (var project in projects)
            {
                if (!string.IsNullOrEmpty(project.Dir))
                {
                    folders.Add(project.Dir);
                }
            }

            return folders
                .Select(f => f.TrimEnd('\\', '/'))
                .Where(f => f.Length > 0)
                .Distinct(StringComparer.OrdinalIgnoreCase)
                .ToArray();
        }

        /// <summary>
        /// Every loaded, non-solution-folder project,
        /// via <see cref="IVsSolution.GetProjectEnum"/> (never
        /// <c>DTE.Solution.Projects</c> — the enumeration the whole class's
        /// own doc comment already rules out) — shared by whichever caller
        /// needs project names/directories, so nobody else re-walks the
        /// hierarchy tree. A hierarchy with no usable <see cref="EnvDTE.Project"/>
        /// name is skipped, not included with an empty name.
        /// </summary>
        internal static IReadOnlyList<ProjectEntry> EnumerateLoadedProjects(IVsSolution? solution)
        {
            ThreadHelper.ThrowIfNotOnUIThread();

            var result = new List<ProjectEntry>();
            if (solution == null)
            {
                return result;
            }

            var guid = Guid.Empty;
            if (!ErrorHandler.Succeeded(solution.GetProjectEnum((uint)__VSENUMPROJFLAGS.EPF_LOADEDINSOLUTION, ref guid, out var enumHierarchies)) || enumHierarchies == null)
            {
                return result;
            }

            var buffer = new IVsHierarchy[1];
            while (enumHierarchies.Next(1, buffer, out var fetched) == VSConstants.S_OK && fetched == 1)
            {
                var hierarchy = buffer[0];
                if (hierarchy == null)
                {
                    continue;
                }

                if (ErrorHandler.Succeeded(hierarchy.GetGuidProperty(VSConstants.VSITEMID_ROOT, (int)__VSHPROPID.VSHPROPID_TypeGuid, out var typeGuid))
                    && typeGuid == SolutionFolderTypeGuid)
                {
                    continue;
                }

                string? dir = null;
                if (ErrorHandler.Succeeded(hierarchy.GetProperty(VSConstants.VSITEMID_ROOT, (int)__VSHPROPID.VSHPROPID_ProjectDir, out var dirValue))
                    && dirValue is string dirText && !string.IsNullOrEmpty(dirText))
                {
                    dir = dirText;
                }

                string? name = null;
                string? uniqueName = null;
                if (ErrorHandler.Succeeded(hierarchy.GetProperty(VSConstants.VSITEMID_ROOT, (int)__VSHPROPID.VSHPROPID_ExtObject, out var extObject))
                    && extObject is EnvDTE.Project dteProject)
                {
                    try
                    {
                        name = dteProject.Name;
                    }
                    catch
                    {
                        name = null;
                    }

                    try
                    {
                        uniqueName = dteProject.UniqueName;
                    }
                    catch
                    {
                        uniqueName = null;
                    }
                }

                if (string.IsNullOrEmpty(name))
                {
                    // No usable EnvDTE.Project identity (a project that
                    // failed to load its extensibility object, or a
                    // hierarchy this enumeration was never meant to see) —
                    // skipped rather than listed with an empty name.
                    continue;
                }

                result.Add(new ProjectEntry(name!, uniqueName ?? name!, dir ?? string.Empty));
            }

            return result;
        }

        // IVsSolutionEvents — every mutation republishes (debounced); every
        // "query" callback is a plain S_OK/no-op, this sink never vetoes
        // anything. Every method's body is fenced —
        // Visual Studio invokes these directly as part of its own solution
        // load/unload dispatch, and an unhandled exception there is exactly
        // the "fail package load" outcome host design §1.3 rules out.
        public int OnAfterOpenProject(IVsHierarchy pHierarchy, int fAdded)
        {
            SafeRepublish(nameof(OnAfterOpenProject));
            return VSConstants.S_OK;
        }

        public int OnQueryCloseProject(IVsHierarchy pHierarchy, int fRemoving, ref int pfCancel) => VSConstants.S_OK;

        public int OnBeforeCloseProject(IVsHierarchy pHierarchy, int fRemoved)
        {
            SafeRepublish(nameof(OnBeforeCloseProject));
            return VSConstants.S_OK;
        }

        public int OnAfterLoadProject(IVsHierarchy pStubHierarchy, IVsHierarchy pRealHierarchy)
        {
            SafeRepublish(nameof(OnAfterLoadProject));
            return VSConstants.S_OK;
        }

        public int OnQueryUnloadProject(IVsHierarchy pRealHierarchy, ref int pfCancel) => VSConstants.S_OK;

        public int OnBeforeUnloadProject(IVsHierarchy pRealHierarchy, IVsHierarchy pStubHierarchy)
        {
            SafeRepublish(nameof(OnBeforeUnloadProject));
            return VSConstants.S_OK;
        }

        public int OnAfterOpenSolution(object pUnkReserved, int fNewSolution)
        {
            SafeRepublish(nameof(OnAfterOpenSolution));
            return VSConstants.S_OK;
        }

        public int OnQueryCloseSolution(object pUnkReserved, ref int pfCancel) => VSConstants.S_OK;

        public int OnBeforeCloseSolution(object pUnkReserved) => VSConstants.S_OK;

        public int OnAfterCloseSolution(object pUnkReserved)
        {
            SafeRepublish(nameof(OnAfterCloseSolution));
            return VSConstants.S_OK;
        }

        // IVsSolutionEvents7 — Open Folder mode.
        public void OnAfterOpenFolder(string folderPath)
        {
            try
            {
                _openFolder = folderPath;
            }
            catch (Exception ex)
            {
                ActivityLog.LogError(nameof(OnAfterOpenFolder), ex.ToString());
            }

            SafeRepublish(nameof(OnAfterOpenFolder));
        }

        public void OnBeforeCloseFolder(string folderPath)
        {
        }

        public void OnAfterCloseFolder(string folderPath)
        {
            try
            {
                _openFolder = null;
            }
            catch (Exception ex)
            {
                ActivityLog.LogError(nameof(OnAfterCloseFolder), ex.ToString());
            }

            SafeRepublish(nameof(OnAfterCloseFolder));
        }

        public void OnQueryCloseFolder(string folderPath, ref int pfCancel)
        {
        }

        public void OnAfterLoadAllDeferredProjects()
        {
            SafeRepublish(nameof(OnAfterLoadAllDeferredProjects));
        }

        /// <summary>
        /// <see cref="Republish"/> itself only ever calls
        /// <see cref="Debouncer.Trigger"/>, which does not throw under
        /// ordinary use — this fences the call anyway so every one of the
        /// callbacks above stays true to "the whole body is fenced" even as
        /// this method's own implementation changes.
        /// </summary>
        private void SafeRepublish(string context)
        {
            try
            {
                Republish();
            }
            catch (Exception ex)
            {
                ActivityLog.LogError(context, ex.ToString());
            }
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
            // Stops Changed from firing for a recompute
            // that was already past the debounce and mid-flight when
            // Dispose was called — _debouncer.Dispose() only cancels a
            // still-PENDING (debounced) trigger, not one whose RecomputeAsync
            // is already running.
            _disposed = true;
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

    /// <summary>
    /// One loaded, non-solution-folder project, as
    /// <see cref="WorkspaceFolders.EnumerateLoadedProjects"/> produces it.
    /// <see cref="UniqueName"/> falls back to <see cref="Name"/> when
    /// <c>EnvDTE.Project.UniqueName</c> itself throws (some project systems
    /// do, for a project not fully loaded); <see cref="Dir"/> is empty
    /// (never null) when <c>VSHPROPID_ProjectDir</c> was unavailable.
    /// </summary>
    internal readonly struct ProjectEntry
    {
        public ProjectEntry(string name, string uniqueName, string dir)
        {
            Name = name;
            UniqueName = uniqueName;
            Dir = dir;
        }

        public string Name { get; }

        public string UniqueName { get; }

        public string Dir { get; }
    }
}
