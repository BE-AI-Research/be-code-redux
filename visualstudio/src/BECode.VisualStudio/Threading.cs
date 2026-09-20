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
        ///
        /// There is deliberately NO
        /// <c>Threading.SwitchToMainThreadAsync</c> counterpart to this
        /// method any more. The VSTHRD010/VSTHRD108 analyzers only accept a
        /// LITERAL <c>await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync(ct)</c>
        /// in the same lexical scope that then touches a main-thread-only
        /// COM type as proof the thread was actually switched — routing it
        /// through a wrapper method (as this file originally did, and as
        /// the host design's prose reads) hides that literal call from the
        /// analyzer and every caller then gets flagged. So every member
        /// below calls <c>ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync(ct)</c>
        /// directly, in its own body (or the <see cref="Guard"/> lambda that
        /// is its body) — never through this class.
        /// </summary>
        public static async Task SwitchToBackgroundAsync()
        {
            await TaskScheduler.Default;
        }
    }

    /// <summary>
    /// Host design §2.3: wraps every <c>IEditorHost</c>/<c>IDebugHost</c>
    /// member's body for exception handling only (see
    /// <see cref="Threading.SwitchToBackgroundAsync"/>'s doc comment for why
    /// it no longer also performs the main-thread switch): an
    /// <see cref="OperationCanceledException"/> caused by THIS call's own
    /// <paramref name="ct"/> passes through unchanged (see <c>IEditorHost</c>'s
    /// cancellation contract); anything else — including an
    /// <see cref="OperationCanceledException"/> from some other cause — is
    /// logged to the Activity Log and rethrown as
    /// <see cref="InvalidOperationException"/>("&lt;name&gt;: &lt;message&gt;"),
    /// which the tools layer reports as <c>isError</c>. Every caller's own
    /// <paramref name="body"/> lambda still begins with the literal
    /// <c>await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync(ct)</c>
    /// the design's rule 1 asks for — Guard does not do it for them.
    /// </summary>
    internal static class Guard
    {
        public static async Task<T> RunAsync<T>(string name, CancellationToken ct, Func<Task<T>> body)
        {
            try
            {
                var result = await body().ConfigureAwait(true);

                // Every host member's body runs to
                // completion on whatever thread it left off on — usually
                // the UI thread, since the last thing most bodies do is a
                // literal SwitchToMainThreadAsync before touching a COM
                // type. Without this hop, the bridge's JSON serialisation
                // and socket write for the reply run ON the UI thread too.
                // SwitchToBackgroundAsync is a plain background hop (no
                // analyzer rule requires it to be a literal call the way
                // SwitchToMainThreadAsync does), so routing it through the
                // shared helper is fine here.
                await Threading.SwitchToBackgroundAsync();
                return result;
            }
            catch (OperationCanceledException) when (ct.IsCancellationRequested)
            {
                await Threading.SwitchToBackgroundAsync();
                throw;
            }
            catch (Exception ex)
            {
                ActivityLog.LogError(name, ex.ToString());
                await Threading.SwitchToBackgroundAsync();
                throw new InvalidOperationException(name + ": " + ex.Message, ex);
            }
        }

        public static Task RunAsync(string name, CancellationToken ct, Func<Task> body)
        {
            return RunAsync<object?>(name, ct, async () =>
            {
                await body().ConfigureAwait(true);
                return null;
            });
        }
    }
}
