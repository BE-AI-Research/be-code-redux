using System;
using System.Threading;
using System.Threading.Tasks;

namespace BECode.Bridge.Hosting
{
    /// <summary>
    /// Coalesces a burst of <see cref="Trigger"/> calls into a single firing
    /// of the action passed at construction, once no further trigger has
    /// arrived for <paramref name="delay"/> — the visual-studio-host-design's
    /// §2.2 republish debounce (250ms, for <c>WorkspaceFolders</c>'s
    /// <c>IVsSolutionEvents</c> sink), pulled out here because the
    /// coalescing itself needs no Visual Studio type, so it can be unit-tested
    /// on its own without a Visual Studio host.
    /// Takes a delay function instead of calling <see cref="Task.Delay(TimeSpan,
    /// CancellationToken)"/> directly so a test can substitute a
    /// deterministic one and assert the coalescing behaviour without
    /// sleeping for real wall-clock time.
    /// </summary>
    public sealed class Debouncer : IDisposable
    {
        private readonly TimeSpan _delay;
        private readonly Func<Task> _action;
        private readonly Func<TimeSpan, CancellationToken, Task> _delayFn;
        private readonly object _gate = new object();
        private CancellationTokenSource? _pending;
        private bool _disposed;

        public Debouncer(TimeSpan delay, Func<Task> action, Func<TimeSpan, CancellationToken, Task>? delayFn = null)
        {
            _delay = delay;
            _action = action ?? throw new ArgumentNullException(nameof(action));
            _delayFn = delayFn ?? Task.Delay;
        }

        /// <summary>
        /// Records a new trigger, cancelling (and abandoning) whatever
        /// pending firing a previous trigger scheduled. Safe to call
        /// concurrently and re-entrantly (an event sink may fire on the
        /// same thread that is itself inside a previous <see
        /// cref="Trigger"/> call's continuation).
        /// </summary>
        public void Trigger()
        {
            CancellationTokenSource cts;
            lock (_gate)
            {
                if (_disposed)
                {
                    return;
                }

                _pending?.Cancel();
                _pending?.Dispose();
                cts = new CancellationTokenSource();
                _pending = cts;
            }

            _ = RunAsync(cts);
        }

        private async Task RunAsync(CancellationTokenSource cts)
        {
            try
            {
                await _delayFn(_delay, cts.Token).ConfigureAwait(false);
            }
            catch (OperationCanceledException)
            {
                return;
            }

            if (cts.IsCancellationRequested)
            {
                return;
            }

            lock (_gate)
            {
                // A newer Trigger() already replaced _pending with its own
                // source while this delay was in flight but resolved
                // without observing cancellation (a delayFn that ignores its
                // token, e.g. in a careless test double): only the CURRENT
                // pending source's firing is allowed to run.
                if (!ReferenceEquals(_pending, cts))
                {
                    return;
                }
            }

            try
            {
                await _action().ConfigureAwait(false);
            }
            catch
            {
                // The caller's action owns its own error handling/logging;
                // a throwing action must not take down the debouncer or
                // propagate to whatever thread happens to run this
                // fire-and-forget continuation.
            }
        }

        public void Dispose()
        {
            lock (_gate)
            {
                _disposed = true;
                _pending?.Cancel();
                _pending?.Dispose();
                _pending = null;
            }
        }
    }
}
