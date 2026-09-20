using System;
using System.IO;
using System.Threading;
using System.Threading.Tasks;
using BECode.Bridge;
using BECode.Bridge.Hosting;
using Microsoft.VisualStudio;
using Microsoft.VisualStudio.Imaging;
using Microsoft.VisualStudio.Shell;
using Microsoft.VisualStudio.Shell.Interop;
using Microsoft.VisualStudio.Threading;

namespace BECode.VisualStudio
{
    /// <summary>
    /// Host design §3.5, <c>ReviewDiffAsync</c> — "the most intricate
    /// member": shows a proposed write as a Visual Studio difference-viewer
    /// diff with an info bar (Accept / Accept all this session / Reject),
    /// and returns the user's decision. The editor DECIDES; it never writes
    /// the file (the harness does that on Accept, exactly as with VS Code).
    /// The diff is never modal, so a <c>review_cancel</c> can always close
    /// it.
    /// </summary>
    internal sealed class DiffReview
    {
        private readonly AsyncPackage _package;
        private readonly WorkspaceFolders _folders;

        public DiffReview(AsyncPackage package, WorkspaceFolders folders)
        {
            _package = package ?? throw new ArgumentNullException(nameof(package));
            _folders = folders ?? throw new ArgumentNullException(nameof(folders));
        }

        public Task<ReviewDecision> ReviewDiffAsync(ReviewRequest request, CancellationToken ct)
        {
            return Guard.RunAsync(nameof(ReviewDiffAsync), ct, async () =>
            {
                await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync(ct);
                return await ReviewOnMainThreadAsync(request, ct).ConfigureAwait(true);
            });
        }

        private async Task<ReviewDecision> ReviewOnMainThreadAsync(ReviewRequest request, CancellationToken ct)
        {
            // Redundant with the caller's own switch, but literal here too:
            // VSTHRD109 flags a
            // ThreadHelper.ThrowIfNotOnUIThread() assertion inside an async
            // method and wants an actual switch instead, even one that is a
            // main-thread no-op because the caller already switched.
            await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync(ct);

            // The path is computed here, but the directory
            // itself is created INSIDE the try below — TryDeleteDirectory in
            // the finally is a no-op (via its own catch-all) against a path
            // that was never created, so this is still safe to reference
            // there even if CreateDirectory itself never ran.
            var tempDir = Path.Combine(Path.GetTempPath(), "becode-review-" + Guid.NewGuid().ToString("N"));

            IVsWindowFrame? frame = null;
            IVsInfoBarUIElement? infoBarElement = null;
            uint infoBarCookie = 0;
            uint frameNotifyCookie = 0;
            // Without RunContinuationsAsynchronously, a
            // TrySetResult from an info-bar click handler (itself running on
            // the UI thread — host design §2.3) runs every continuation of
            // tcs.Task INLINE on that same call stack: the unadvises, a
            // re-entrant CloseFrame, a recursive Directory.Delete, then the
            // bridge's own JSON serialisation and socket write, all inside
            // OnActionItemClicked. ConfigureAwait(false) on the await below
            // does not help — it only controls which context is preferred
            // for resuming, not whether TrySetResult itself runs the
            // continuation inline.
            var tcs = new TaskCompletionSource<ReviewDecision>(TaskCreationOptions.RunContinuationsAsynchronously);

            try
            {
                // The directory and both temp files are
                // created HERE, inside the try, so a failed write (or the
                // CreateDirectory call itself) cannot leak a directory the
                // finally never learns to clean up.
                Directory.CreateDirectory(tempDir);

                string leftPath;
                bool leftIsTemp;
                if (request.Original != null)
                {
                    // Left side: the ORIGINAL text supplied, written to a temp
                    // file named after the real file so the language service
                    // still colours it.
                    leftPath = Path.Combine(tempDir, Path.GetFileName(request.Path));
                    File.WriteAllText(leftPath, request.Original);
                    leftIsTemp = true;
                }
                else if (File.Exists(request.Path))
                {
                    // Else: the file on disk, unmodified.
                    leftPath = request.Path;
                    leftIsTemp = false;
                }
                else
                {
                    // Else (a new file): an empty temp file.
                    leftPath = Path.Combine(tempDir, Path.GetFileName(request.Path));
                    File.WriteAllText(leftPath, string.Empty);
                    leftIsTemp = true;
                }

                var rightPath = Path.Combine(tempDir, DiffTempFiles.ProposedFileName(request.Path));
                File.WriteAllText(rightPath, request.Proposed);

                var diffService = await _package.GetServiceAsync(typeof(SVsDifferenceService)).ConfigureAwait(true) as IVsDifferenceService;
                if (diffService == null)
                {
                    throw new InvalidOperationException("the difference viewer service is unavailable");
                }

                var options = __VSDIFFSERVICEOPTIONS.VSDIFFOPT_RightFileIsTemporary;
                if (leftIsTemp)
                {
                    options |= __VSDIFFSERVICEOPTIONS.VSDIFFOPT_LeftFileIsTemporary;
                }

                var caption = "BE-Code: " + RelativePath(request.Path);

                frame = diffService.OpenComparisonWindow2(
                    leftPath,
                    rightPath,
                    caption,
                    caption,
                    "Current",
                    "Proposed",
                    string.Empty,
                    null,
                    (uint)options);

                if (frame == null)
                {
                    throw new InvalidOperationException("the difference viewer did not open a window");
                }

                // Never modal (host design §3.5) — a review_cancel must
                // always be able to close it.
                frame.Show();

                // The relative path is UNCONDITIONALLY
                // part of the bar's text — several concurrent reviews (one
                // per file) are otherwise indistinguishable, since the
                // summary alone ("Apply this change?") looks identical on
                // every bar.
                var infoBarText = RelativePath(request.Path) + ": " + (request.Summary ?? "Apply this change?");
                if (request.Shared)
                {
                    infoBarText += " — also waiting in the terminal";
                }

                var infoBarFactory = await _package.GetServiceAsync(typeof(SVsInfoBarUIFactory)).ConfigureAwait(true) as IVsInfoBarUIFactory;
                var infoBarHost = GetInfoBarHost(frame) ?? await GetMainWindowInfoBarHostAsync().ConfigureAwait(true);

                if (infoBarFactory != null && infoBarHost != null)
                {
                    var model = new InfoBarModel(
                        new[] { new InfoBarTextSpan(infoBarText) },
                        new InfoBarActionItem[]
                        {
                            new InfoBarButton("Accept", ReviewDecision.Accept),
                            new InfoBarButton("Accept all this session", ReviewDecision.AcceptAll),
                            new InfoBarButton("Reject", ReviewDecision.Reject),
                        },
                        KnownMonikers.StatusInformation);

                    infoBarElement = infoBarFactory.CreateInfoBar(model);
                    var sink = new InfoBarSink(tcs);
                    infoBarElement.Advise(sink, out infoBarCookie);
                    infoBarHost.AddInfoBar(infoBarElement);
                }
                else
                {
                    // Host design §3.5's named Risk: no info bar host
                    // anywhere reachable. There is no other UI surface this
                    // member is allowed to raise (fs.go's own approval takes
                    // over when the host answers ReviewUnavailable) — throw
                    // so Guard turns it into an isError result rather than
                    // hanging forever waiting for an answer nothing can give.
                    throw new InvalidOperationException("no info bar host is available to ask for a review decision");
                }

                if (frame is IVsWindowFrame2 frame2)
                {
                    var closeSink = new FrameCloseSink(tcs);
                    frame2.Advise(closeSink, out frameNotifyCookie);
                }

                // Cancellation (review_cancel, or the connection dropping):
                // close the frame on the main thread and resolve Cancelled.
                // Fire-and-forget via RunAsync — never JoinableTaskFactory.Run
                // — a registered callback must not block whatever cancelled
                // ct.
                using (ct.Register(() =>
                {
                    ThreadHelper.JoinableTaskFactory.RunAsync(async () =>
                    {
                        await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync();
                        try
                        {
                            frame?.CloseFrame((uint)__FRAMECLOSE.FRAMECLOSE_NoSave);
                        }
                        catch (Exception ex)
                        {
                            ActivityLog.LogError(nameof(ReviewDiffAsync), ex.ToString());
                        }

                        tcs.TrySetResult(ReviewDecision.Cancelled);
                    }).FileAndForget("becode/diffreview/cancel");
                }))
                {
                    await TaskScheduler.Default;
                    return await tcs.Task.ConfigureAwait(false);
                }
            }
            finally
            {
                await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync();

                if (infoBarElement != null && infoBarCookie != 0)
                {
                    try
                    {
                        infoBarElement.Unadvise(infoBarCookie);
                    }
                    catch (Exception ex)
                    {
                        ActivityLog.LogError(nameof(ReviewDiffAsync), ex.ToString());
                    }
                }

                if (infoBarElement != null)
                {
                    // The finally must unadvise AND close the bar. On the
                    // main-window fallback host
                    // (used when the comparison frame itself has no info bar
                    // host), unadvising without closing leaves a dead "Apply
                    // this change?" bar pinned to the top of Visual Studio for
                    // every cancelled review, accumulating across reviews.
                    // Close() after the bar
                    // already closed itself (the ordinary Accept/Reject path,
                    // which calls Close() in OnActionItemClicked) is expected
                    // to be a harmless no-op — same defensive shape as
                    // frame.CloseFrame() below.
                    try
                    {
                        infoBarElement.Close();
                    }
                    catch
                    {
                        // already closed
                    }
                }

                if (frame is IVsWindowFrame2 frame2Cleanup && frameNotifyCookie != 0)
                {
                    try
                    {
                        frame2Cleanup.Unadvise(frameNotifyCookie);
                    }
                    catch (Exception ex)
                    {
                        ActivityLog.LogError(nameof(ReviewDiffAsync), ex.ToString());
                    }
                }

                if (frame != null)
                {
                    try
                    {
                        frame.CloseFrame((uint)__FRAMECLOSE.FRAMECLOSE_NoSave);
                    }
                    catch
                    {
                        // already closed
                    }
                }

                TryDeleteDirectory(tempDir);
            }
        }

        private static IVsInfoBarHost? GetInfoBarHost(IVsWindowFrame frame)
        {
            ThreadHelper.ThrowIfNotOnUIThread();

            try
            {
                if (ErrorHandler.Succeeded(frame.GetProperty((int)__VSFPROPID7.VSFPROPID_InfoBarHost, out var value)))
                {
                    return value as IVsInfoBarHost;
                }
            }
            catch (Exception ex)
            {
                ActivityLog.LogWarning(nameof(GetInfoBarHost), ex.ToString());
            }

            return null;
        }

        private async Task<IVsInfoBarHost?> GetMainWindowInfoBarHostAsync()
        {
            await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync();

            try
            {
                var shell = await _package.GetServiceAsync(typeof(SVsShell)).ConfigureAwait(true) as IVsShell;
                if (shell != null && ErrorHandler.Succeeded(shell.GetProperty((int)__VSSPROPID7.VSSPROPID_MainWindowInfoBarHost, out var value)))
                {
                    return value as IVsInfoBarHost;
                }
            }
            catch (Exception ex)
            {
                ActivityLog.LogWarning(nameof(GetMainWindowInfoBarHostAsync), ex.ToString());
            }

            return null;
        }

        private string RelativePath(string path)
        {
            foreach (var folder in _folders.Current)
            {
                var withSep = folder.TrimEnd('\\', '/') + Path.DirectorySeparatorChar;
                if (path.StartsWith(withSep, StringComparison.OrdinalIgnoreCase))
                {
                    return path.Substring(withSep.Length);
                }
            }

            return path;
        }

        private static void TryDeleteDirectory(string dir)
        {
            try
            {
                // Best effort (host design §3.5): a file the diff viewer
                // still holds open is left for the OS temp cleaner.
                Directory.Delete(dir, recursive: true);
            }
            catch
            {
            }
        }

        /// <summary>Matches an info bar button click back to the <see cref="ReviewDecision"/> it was created with (<c>ActionContext</c>).</summary>
        private sealed class InfoBarSink : IVsInfoBarUIEvents
        {
            private readonly TaskCompletionSource<ReviewDecision> _tcs;

            public InfoBarSink(TaskCompletionSource<ReviewDecision> tcs)
            {
                _tcs = tcs;
            }

            public void OnActionItemClicked(IVsInfoBarUIElement infoBarUIElement, IVsInfoBarActionItem actionItem)
            {
                // Visual Studio always raises info bar UI events on the main
                // thread; this assertion documents that (host design §2.3:
                // assert rather than switch in a
                // synchronous callback Visual Studio itself invokes).
                ThreadHelper.ThrowIfNotOnUIThread();

                // The whole body is fenced — a callback
                // Visual Studio itself invokes must never let an exception
                // escape back into its own dispatch.
                try
                {
                    // ActionContext may marshal across
                    // this COM boundary as the raw underlying int rather
                    // than the ReviewDecision enum value; fall back to the
                    // int, then to the button's own Text — a click must
                    // never do nothing.
                    var decision = ResolveDecision(actionItem);

                    // Decide -> Close() the bar -> TrySetResult,
                    // in that order: TrySetResult running BEFORE
                    // Close() would let tcs.Task's continuations — everything
                    // the caller does after its own await — run while the
                    // bar was still showing.
                    infoBarUIElement.Close();

                    if (decision.HasValue)
                    {
                        _tcs.TrySetResult(decision.Value);
                    }
                    else
                    {
                        ActivityLog.LogWarning(nameof(OnActionItemClicked), "could not resolve a ReviewDecision from ActionContext or Text=\"" + actionItem.Text + "\"");
                    }
                }
                catch (Exception ex)
                {
                    ActivityLog.LogError(nameof(OnActionItemClicked), ex.ToString());
                }
            }

            public void OnClosed(IVsInfoBarUIElement infoBarUIElement)
            {
                ThreadHelper.ThrowIfNotOnUIThread();

                try
                {
                    // The user dismissed the bar without clicking a button:
                    // the diff stays open (host design §3.5) — nothing to
                    // do here.
                }
                catch (Exception ex)
                {
                    ActivityLog.LogError(nameof(OnClosed), ex.ToString());
                }
            }

            /// <summary>
            /// <see cref="IVsInfoBarActionItem.ActionContext"/>
            /// is typed <c>object</c> and may marshal as the ReviewDecision
            /// enum value itself, as the plain <c>int</c> underneath it, or —
            /// if neither survives the COM round trip — not at all; the
            /// button's own <see cref="IVsInfoBarActionItem.Text"/> (set
            /// verbatim in <see cref="ReviewOnMainThreadAsync"/>'s
            /// <c>InfoBarButton</c> construction) is the last resort.
            /// </summary>
            private static ReviewDecision? ResolveDecision(IVsInfoBarActionItem actionItem)
            {
                // A synchronous private
                // method needs its OWN assertion — the analyzer does not
                // reason across the call from OnActionItemClicked, which is
                // itself only known to be on the main thread because Visual
                // Studio invokes it there.
                ThreadHelper.ThrowIfNotOnUIThread();

                if (actionItem.ActionContext is ReviewDecision decision)
                {
                    return decision;
                }

                if (actionItem.ActionContext is int intValue && Enum.IsDefined(typeof(ReviewDecision), intValue))
                {
                    return (ReviewDecision)intValue;
                }

                switch (actionItem.Text)
                {
                    case "Accept":
                        return ReviewDecision.Accept;
                    case "Accept all this session":
                        return ReviewDecision.AcceptAll;
                    case "Reject":
                        return ReviewDecision.Reject;
                    default:
                        return null;
                }
            }
        }

        /// <summary>Closing the diff window without answering resolves Cancelled (host design §3.5).</summary>
        private sealed class FrameCloseSink : IVsWindowFrameNotify
        {
            private readonly TaskCompletionSource<ReviewDecision> _tcs;

            public FrameCloseSink(TaskCompletionSource<ReviewDecision> tcs)
            {
                _tcs = tcs;
            }

            public int OnShow(int fShow)
            {
                // Visual Studio invokes this synchronously
                // on the main thread as part of the frame's own
                // notification dispatch — fence the body so an exception
                // here (e.g. TrySetResult never throws, but a future change
                // to this method might add something that can) cannot
                // escape into that dispatch.
                ThreadHelper.ThrowIfNotOnUIThread();

                try
                {
                    if (fShow == (int)__FRAMESHOW.FRAMESHOW_WinClosed)
                    {
                        _tcs.TrySetResult(ReviewDecision.Cancelled);
                    }
                }
                catch (Exception ex)
                {
                    ActivityLog.LogError(nameof(OnShow), ex.ToString());
                }

                return VSConstants.S_OK;
            }

            public int OnMove()
            {
                // Fenced like OnShow's siblings, even
                // though nothing in the body itself can throw today — a
                // future change here must not have to remember to add this.
                ThreadHelper.ThrowIfNotOnUIThread();

                try
                {
                }
                catch (Exception ex)
                {
                    ActivityLog.LogError(nameof(OnMove), ex.ToString());
                }

                return VSConstants.S_OK;
            }

            public int OnSize()
            {
                ThreadHelper.ThrowIfNotOnUIThread();

                try
                {
                }
                catch (Exception ex)
                {
                    ActivityLog.LogError(nameof(OnSize), ex.ToString());
                }

                return VSConstants.S_OK;
            }

            public int OnDockableChange(int fDockable)
            {
                ThreadHelper.ThrowIfNotOnUIThread();

                try
                {
                }
                catch (Exception ex)
                {
                    ActivityLog.LogError(nameof(OnDockableChange), ex.ToString());
                }

                return VSConstants.S_OK;
            }
        }
    }
}
