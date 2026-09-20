using System;
using System.Threading;
using System.Threading.Tasks;
using Microsoft.VisualStudio.Shell;
using Microsoft.VisualStudio.Threading;

namespace BECode.VisualStudio
{
    /// <summary>
    /// Host design §2.3, rule 1: "every Visual Studio call happens after
    /// <c>await JoinableTaskFactory.SwitchToMainThreadAsync(ct)</c>, and the
    /// method leaves the main thread again (<c>await TaskScheduler.Default</c>)
    /// before any wait." <see cref="ThreadHelper.JoinableTaskFactory"/> is
    /// the package's own factory (set up by <see cref="AsyncPackage"/> at
    /// load); every host member below runs long after that, so using the
    /// static helper is always safe here. Nothing in this layer ever calls
    /// <c>.Result</c>, <c>.Wait()</c>, <c>JoinableTaskFactory.Run</c> or
    /// <c>Thread.Sleep</c> (host design §1.3/rule list).
    /// </summary>
    internal static class Threading
    {
        public static async Task SwitchToMainThreadAsync(CancellationToken ct)
        {
            await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync(ct);
        }

        /// <summary>
        /// Hops off the main thread before a wait that must not block the
        /// UI (a debugger stop, an info-bar click, ...). <c>await
        /// TaskScheduler.Default;</c> is the exact idiom the host design
        /// names (§2.3) — confirmed by compiling a throwaway probe: plain
        /// <see cref="TaskScheduler"/> has no <c>GetAwaiter</c> of its own;
        /// what makes it directly awaitable is
        /// <c>Microsoft.VisualStudio.Threading</c>'s own extension method
        /// (<c>TplExtensions.GetAwaiter(TaskScheduler)</c>), which the
        /// <c>using</c> above brings into scope. That package is already a
        /// transitive dependency of <c>Microsoft.VisualStudio.SDK</c>, so no
        /// extra reference was needed.
        /// </summary>
        public static async Task SwitchToBackgroundAsync()
        {
            await TaskScheduler.Default;
        }
    }

    /// <summary>
    /// Host design §2.3: wraps every <c>IEditorHost</c>/<c>IDebugHost</c>
    /// member. Switches to the main thread first (rule 1 above), runs the
    /// body, and on the way out: an <see cref="OperationCanceledException"/>
    /// caused by THIS call's own <paramref name="ct"/> passes through
    /// unchanged (<c>IEditorHost</c>'s Ruling D4); anything else — including
    /// an <see cref="OperationCanceledException"/> from some other cause —
    /// is logged to the Activity Log and rethrown as
    /// <see cref="InvalidOperationException"/>("&lt;name&gt;: &lt;message&gt;"),
    /// which the tools layer reports as <c>isError</c>.
    /// </summary>
    internal static class Guard
    {
        public static async Task<T> RunAsync<T>(string name, CancellationToken ct, Func<CancellationToken, Task<T>> body)
        {
            await Threading.SwitchToMainThreadAsync(ct).ConfigureAwait(true);
            try
            {
                return await body(ct).ConfigureAwait(true);
            }
            catch (OperationCanceledException) when (ct.IsCancellationRequested)
            {
                throw;
            }
            catch (Exception ex)
            {
                ActivityLog.LogError(name, ex.ToString());
                throw new InvalidOperationException(name + ": " + ex.Message, ex);
            }
        }

        public static Task RunAsync(string name, CancellationToken ct, Func<CancellationToken, Task> body)
        {
            return RunAsync<object?>(name, ct, async c =>
            {
                await body(c).ConfigureAwait(true);
                return null;
            });
        }
    }
}
