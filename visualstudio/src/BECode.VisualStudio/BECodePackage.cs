using System;
using System.Collections.Generic;
using System.Diagnostics;
using System.Runtime.InteropServices;
using System.Threading;
using System.Threading.Tasks;
using BECode.Bridge;
using Microsoft.VisualStudio.Shell;
using Microsoft.VisualStudio.Shell.Interop;
using Microsoft.VisualStudio.Threading;

namespace BECode.VisualStudio
{
    /// <summary>
    /// Host design §2.1: load, lifetime, the lock file, solution events.
    /// <see cref="InitializeAsync"/>'s whole body is in one try; a failure
    /// is logged and swallowed, leaving Visual Studio with an extension
    /// that does nothing rather than one that breaks load (host design
    /// §1.3 — one of the three outcomes that must be impossible).
    /// </summary>
    [PackageRegistration(UseManagedResourcesOnly = true, AllowsBackgroundLoading = true)]
    [Guid(PackageGuidString)]
    [ProvideAutoLoad(UIContextGuids80.NoSolution, PackageAutoLoadFlags.BackgroundLoad)]
    [ProvideAutoLoad(UIContextGuids80.SolutionExists, PackageAutoLoadFlags.BackgroundLoad)]
    // Fix round 1, C-2: joins this package's own folder onto devenv's
    // assembly probing path, so BECode.Bridge.dll and the BCL assemblies
    // shipped alongside it (System.Text.Json and friends — see the csproj's
    // ProjectReference comment) are resolvable at load time. UseCodebase
    // (in the csproj) registers a CodeBase for BECode.VisualStudio.dll
    // ITSELF only; it does nothing for what that assembly references.
    [ProvideBindingPath]
    public sealed class BECodePackage : AsyncPackage
    {
        public const string PackageGuidString = "4f0c8f6a-3b2f-4c2b-9e6a-8a2c1c7a9d5e";

        // Host design §1.4/§4: the version string LockInfo/serverInfo carry.
        // Kept in step with source.extension.vsixmanifest's Identity
        // Version by hand — there is no single source of truth to read it
        // from at package-load time without adding a resource dependency,
        // and the manifest's own Version is itself hand-maintained too.
        private const string Version = "0.1.0";

        private BridgeServer? _server;
        private WorkspaceFolders? _folders;
        private int _pid;
        private string? _home;
        private int _port;
        private string? _token;

        // Fix round 1, I-2: republishing the lock file when
        // WorkspaceFolders.Changed fires. One lock (_lockWriteGate) guards
        // both the "is this the newest generation queued" decision and the
        // pending-folders slot together, so the two can never be updated
        // out of step with each other — see OnFoldersChanged's own comment
        // for why splitting them (a GenerationGate check followed by a
        // separate assignment) would reopen the exact race this exists to
        // close. Only one republish write is ever in flight
        // (_lockWriteInProgress); a burst of Changed firings while a write
        // is running collapses to whatever the LATEST one queued.
        private readonly object _lockWriteGate = new object();
        private long _lastQueuedLockGeneration = -1;
        private bool _lockWriteInProgress;
        private IReadOnlyList<string>? _pendingLockFolders;
        private volatile bool _lockRemoved;

        protected override async Task InitializeAsync(CancellationToken cancellationToken, IProgress<ServiceProgressData> progress)
        {
            try
            {
                await InitializeCoreAsync(cancellationToken).ConfigureAwait(false);
            }
            catch (Exception ex)
            {
                ActivityLog.LogError(nameof(InitializeAsync), ex.ToString());
            }
        }

        private async Task InitializeCoreAsync(CancellationToken ct)
        {
            // Fix round 1, I-9: resolved and the file mirror initialised as
            // early as possible, before anything else in this method can
            // log — neither call needs the UI thread.
            _home = Environment.GetFolderPath(Environment.SpecialFolder.UserProfile);
            _pid = Process.GetCurrentProcess().Id;
            ActivityLog.Initialize(_home);

            await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync(ct);

            var folders = new WorkspaceFolders(this);
            await folders.InitializeAsync(ct).ConfigureAwait(true);
            _folders = folders;

            var roslyn = new RoslynNavigator(this);
            var errorList = new ErrorListReader(this);
            var diffReview = new DiffReview(this, folders);
            var debugHost = await VisualStudioDebugHost.CreateAsync(this, ct).ConfigureAwait(true);
            var host = new VisualStudioEditorHost(this, folders, roslyn, errorList, diffReview, debugHost);

            var registry = new ToolRegistry(host);
            var token = LockFile.NewToken();
            var server = new BridgeServer(registry, token, Version);
            server.OnError = (context, ex) => ActivityLog.LogError(context, ex.ToString());

            // Still on the main thread here (needed for the DTE version
            // lookup below); host/registry/server construction above is all
            // main-thread VS service work too.
            string vsVersion;
            try
            {
                vsVersion = (await GetServiceAsync(typeof(SDTE)).ConfigureAwait(true) as EnvDTE80.DTE2)?.Version ?? "unknown";
            }
            catch
            {
                vsVersion = "unknown";
            }

            // Fix round 1, I-11 (M-9): BridgeServer.StartAsync (opens a TCP
            // listener) and LockFile.WriteAsync (file I/O) need nothing from
            // the UI thread — hop to the pool before either.
            await TaskScheduler.Default;

            var port = await server.StartAsync().ConfigureAwait(false);
            _server = server;
            _port = port;
            _token = token;

            // The lock file must exist before anything else can find this
            // host — written last, once the server is actually listening.
            await LockFile.WriteAsync(_home, new LockInfo(
                _pid,
                port,
                token,
                folders.Current,
                "visualstudio",
                Version)).ConfigureAwait(false);

            // Fix round 1, I-2: from here on, a folder-list change
            // republishes the lock (off the UI thread, serialised,
            // superseded generations dropped — see OnFoldersChanged).
            // Subscribed AFTER the initial write above so the recompute
            // WorkspaceFolders.InitializeAsync already ran does not race a
            // redundant republish against it.
            folders.Changed += OnFoldersChanged;

            ActivityLog.LogInformation(nameof(BECodePackage), "listening on port " + port + ", " + folders.Current.Count + " workspace folder(s), VS " + vsVersion);
        }

        /// <summary>
        /// Fix round 1, I-2: <see cref="WorkspaceFolders.Changed"/>'s
        /// handler. Runs off the UI thread already (the event is raised
        /// there). The single <see cref="_lockWriteGate"/> lock makes the
        /// "is this generation newer than everything already queued"
        /// decision and the pending-folders assignment ATOMIC together —
        /// doing them as two separate steps (e.g. a <c>GenerationGate.TryCommit</c>
        /// check followed by a plain field write) would let a later call
        /// win the commit but an earlier call win the race to actually set
        /// the pending value, which is the exact "older list overwrites a
        /// newer one" bug this exists to prevent.
        /// </summary>
        private void OnFoldersChanged(long generation, IReadOnlyList<string> folders)
        {
            lock (_lockWriteGate)
            {
                if (_lockRemoved || generation <= _lastQueuedLockGeneration)
                {
                    return;
                }

                _lastQueuedLockGeneration = generation;
                _pendingLockFolders = folders;

                if (_lockWriteInProgress)
                {
                    // A write loop is already draining; it will pick up
                    // this (now the latest) pending value once its current
                    // write finishes.
                    return;
                }

                _lockWriteInProgress = true;
            }

            ThreadHelper.JoinableTaskFactory.RunAsync(DrainLockWritesAsync).FileAndForget("becode/package/republish");
        }

        private async Task DrainLockWritesAsync()
        {
            // RunAsync starts on its caller's thread, and LockFile.WriteAsync
            // creates directories and files before its first real await:
            // leave whatever thread raised the change before touching disk.
            await TaskScheduler.Default;

            while (true)
            {
                IReadOnlyList<string>? folders;
                lock (_lockWriteGate)
                {
                    if (_pendingLockFolders == null || _lockRemoved)
                    {
                        _lockWriteInProgress = false;
                        return;
                    }

                    folders = _pendingLockFolders;
                    _pendingLockFolders = null;
                }

                try
                {
                    await LockFile.WriteAsync(_home!, new LockInfo(_pid, _port, _token!, folders!, "visualstudio", Version)).ConfigureAwait(false);

                    // The gate stops a write from STARTING after Dispose, not
                    // one already past it: if Dispose removed the lock while
                    // this write was in flight, the write has just put it
                    // back for a Visual Studio that is closing. Take it away
                    // again.
                    bool removedMeanwhile;
                    lock (_lockWriteGate)
                    {
                        removedMeanwhile = _lockRemoved;
                    }

                    if (removedMeanwhile)
                    {
                        LockFile.Remove(_home!, _pid);
                        continue;
                    }

                    ActivityLog.LogInformation(nameof(WorkspaceFolders), "republished lock: " + folders!.Count + " workspace folder(s)");
                }
                catch (Exception ex)
                {
                    ActivityLog.LogError(nameof(OnFoldersChanged), ex.ToString());
                }
            }
        }

        protected override void Dispose(bool disposing)
        {
            if (disposing)
            {
                // Fix round 1, I-2: stop republishing BEFORE the lock is
                // removed — set under the same lock OnFoldersChanged and
                // DrainLockWritesAsync check, so a write already queued or
                // in flight cannot land AFTER LockFile.Remove below.
                lock (_lockWriteGate)
                {
                    _lockRemoved = true;
                }

                if (_folders != null)
                {
                    _folders.Changed -= OnFoldersChanged;
                }

                // Host design §2.1: remove the lock FIRST, so the harness
                // stops finding a dying bridge before the server itself —
                // which may take a moment to drain in-flight calls — is
                // torn down.
                if (_home != null)
                {
                    LockFile.Remove(_home, _pid);
                }

                _folders?.Dispose();

                var server = _server;
                _server = null;
                if (server != null)
                {
                    // Bounded to 2s, never blocking the UI thread on it
                    // (host design §2.1/§1.3): JoinableTaskFactory.RunAsync
                    // — never .Run(), never .Wait()/.Result.
                    ThreadHelper.JoinableTaskFactory.RunAsync(async () =>
                    {
                        await TaskScheduler.Default;

                        var disposeTask = server.DisposeAsync().AsTask();
                        var timeoutTask = Task.Delay(TimeSpan.FromSeconds(2));
                        var finished = await Task.WhenAny(disposeTask, timeoutTask).ConfigureAwait(false);

                        if (finished != disposeTask)
                        {
                            ActivityLog.LogWarning(nameof(Dispose), "server disposal did not complete within 2s; abandoning it");
                            return;
                        }

                        try
                        {
                            await disposeTask.ConfigureAwait(false);
                        }
                        catch (Exception ex)
                        {
                            ActivityLog.LogError(nameof(Dispose), ex.ToString());
                        }
                    }).FileAndForget("becode/package/dispose");
                }
            }

            base.Dispose(disposing);
        }
    }
}
