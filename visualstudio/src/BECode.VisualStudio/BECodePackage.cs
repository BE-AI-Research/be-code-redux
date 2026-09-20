using System;
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

            var port = await server.StartAsync().ConfigureAwait(true);
            _server = server;

            _home = Environment.GetFolderPath(Environment.SpecialFolder.UserProfile);
            _pid = Process.GetCurrentProcess().Id;

            // The lock file must exist before anything else can find this
            // host — written last, once the server is actually listening.
            await LockFile.WriteAsync(_home, new LockInfo(
                _pid,
                port,
                token,
                folders.Current,
                "visualstudio",
                Version)).ConfigureAwait(true);
        }

        protected override void Dispose(bool disposing)
        {
            if (disposing)
            {
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
