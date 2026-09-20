using System;
using System.Collections.Generic;
using System.IO;
using System.Threading;
using System.Threading.Tasks;
using BECode.Bridge;
using EnvDTE;
using EnvDTE80;
using Microsoft.VisualStudio;
using Microsoft.VisualStudio.Shell;
using Microsoft.VisualStudio.Shell.Interop;
using Microsoft.VisualStudio.TextManager.Interop;

namespace BECode.VisualStudio
{
    /// <summary>
    /// The Visual Studio <see cref="IEditorHost"/>: <see
    /// cref="GetContextAsync"/> and <see cref="OpenAsync"/> (host design
    /// §3.1/§3.2) are implemented directly here over <c>DTE2</c>; every
    /// other member delegates to the class that owns its own piece
    /// (<see cref="WorkspaceFolders"/>, <see cref="RoslynNavigator"/>,
    /// <see cref="ErrorListReader"/>, <see cref="DiffReview"/>,
    /// <see cref="VisualStudioDebugHost"/>) — see <see cref="BECodePackage.InitializeCoreAsync"/>
    /// for how those are built and wired together.
    /// </summary>
    internal sealed class VisualStudioEditorHost : IEditorHost
    {
        private readonly AsyncPackage _package;
        private readonly WorkspaceFolders _folders;
        private readonly RoslynNavigator _roslyn;
        private readonly ErrorListReader _errorList;
        private readonly DiffReview _diffReview;

        public VisualStudioEditorHost(
            AsyncPackage package,
            WorkspaceFolders folders,
            RoslynNavigator roslyn,
            ErrorListReader errorList,
            DiffReview diffReview,
            VisualStudioDebugHost debugHost)
        {
            _package = package ?? throw new ArgumentNullException(nameof(package));
            _folders = folders ?? throw new ArgumentNullException(nameof(folders));
            _roslyn = roslyn ?? throw new ArgumentNullException(nameof(roslyn));
            _errorList = errorList ?? throw new ArgumentNullException(nameof(errorList));
            _diffReview = diffReview ?? throw new ArgumentNullException(nameof(diffReview));
            Debug = debugHost ?? throw new ArgumentNullException(nameof(debugHost));
        }

        public IDebugHost Debug { get; }

        public Task<EditorContext> GetContextAsync(CancellationToken ct)
        {
            return Guard.RunAsync(nameof(GetContextAsync), ct, async () =>
            {
                await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync(ct);

                var dte = await _package.GetServiceAsync(typeof(SDTE)).ConfigureAwait(true) as DTE2;
                if (dte == null)
                {
                    return new EditorContext(string.Empty, 0, 0, 0, string.Empty, Array.Empty<string>());
                }

                var open = new List<string>();
                try
                {
                    foreach (Document doc in dte.Documents)
                    {
                        if (IsRootedExistingFile(doc.FullName))
                        {
                            open.Add(doc.FullName);
                        }
                    }
                }
                catch (Exception ex)
                {
                    ActivityLog.LogWarning(nameof(GetContextAsync), ex.ToString());
                }

                var active = dte.ActiveDocument;
                string? activeFull = null;
                try
                {
                    activeFull = active?.FullName;
                }
                catch
                {
                    active = null;
                }

                if (active == null || !IsRootedExistingFile(activeFull))
                {
                    return new EditorContext(string.Empty, 0, 0, 0, string.Empty, open);
                }

                TextSelection? selection;
                try
                {
                    selection = active.Selection as TextSelection;
                }
                catch
                {
                    selection = null;
                }

                if (selection == null)
                {
                    // A non-text document (a designer): treat as no
                    // selection (host design §3.1). File is non-empty here,
                    // so Line stays non-zero per EditorContext's own
                    // invariant ("0 only alongside an empty File") — there
                    // is no real caret, so 1 is the least-wrong default.
                    return new EditorContext(activeFull!, 1, 0, 0, string.Empty, open);
                }

                var line = selection.ActivePoint.Line;

                if (selection.IsEmpty)
                {
                    return new EditorContext(activeFull!, line, 0, 0, string.Empty, open);
                }

                var selStart = selection.TopPoint.Line;
                var selEnd = selection.BottomPoint.Line;
                var text = selection.Text ?? string.Empty;

                return new EditorContext(activeFull!, line, selStart, selEnd, text, open);
            });
        }

        public Task<IReadOnlyList<string>> GetWorkspaceFoldersAsync(CancellationToken ct)
        {
            // Never touches the UI thread (host design §2.2) — no Guard
            // wrapping needed, there is nothing here that can throw.
            return Task.FromResult(_folders.Current);
        }

        public Task OpenAsync(string path, int? line, CancellationToken ct)
        {
            if (!File.Exists(path))
            {
                throw new FileNotFoundException(path, path);
            }

            return Guard.RunAsync(nameof(OpenAsync), ct, async () =>
            {
                await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync(ct);

                VsShellUtilities.OpenDocument(
                    _package,
                    path,
                    VSConstants.LOGVIEWID_Code,
                    out _,
                    out _,
                    out var frame,
                    out var view);

                if (line.HasValue && view != null)
                {
                    var zeroBased = Math.Max(0, line.Value - 1);
                    view.SetCaretPos(zeroBased, 0);
                    view.CenterLines(zeroBased, 1);
                }

                // Ruling D9: do not take keyboard focus away from wherever
                // the user is currently typing — ShowNoActivate rather than
                // Show().
                frame?.ShowNoActivate();
            });
        }

        public Task<IReadOnlyList<Location>?> DefinitionAsync(string path, int line, int col, CancellationToken ct)
            => _roslyn.DefinitionAsync(path, line, col, ct);

        public Task<IReadOnlyList<Location>?> ReferencesAsync(string path, int line, int col, CancellationToken ct)
            => _roslyn.ReferencesAsync(path, line, col, ct);

        public Task<string?> HoverAsync(string path, int line, int col, CancellationToken ct)
            => _roslyn.HoverAsync(path, line, col, ct);

        public Task<IReadOnlyList<Diagnostic>> DiagnosticsAsync(string? path, CancellationToken ct)
            => _errorList.DiagnosticsAsync(path, ct);

        public Task<ReviewDecision> ReviewDiffAsync(ReviewRequest request, CancellationToken ct)
            => _diffReview.ReviewDiffAsync(request, ct);

        private static bool IsRootedExistingFile(string? path)
        {
            return !string.IsNullOrEmpty(path) && Path.IsPathRooted(path) && File.Exists(path);
        }
    }
}
