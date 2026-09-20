using System;
using System.Collections.Generic;
using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;

namespace BECode.Bridge.Tools
{
    /// <summary>
    /// The two hidden review tools (<c>review_diff</c>, <c>review_cancel</c>),
    /// callable through <c>ToolRegistry.CallAsync</c> but never returned by
    /// <c>List()</c> — the harness drives these, never the model. Ports
    /// vscode/src/tools/review.ts and its accept-all/pending-cancel state,
    /// keyed by connection identity per the task's carried obligation (drop
    /// it in <see cref="ConnectionClosed"/>).
    ///
    /// vscode implements the cancel race with a manual <c>Promise.race</c>
    /// because <c>showInformationMessage</c> has no built-in cancellation;
    /// here it is ordinary <see cref="CancellationToken"/> plumbing instead —
    /// <see cref="IEditorHost.ReviewDiffAsync"/> takes a token per connection
    /// and path, and <c>review_cancel</c> cancels it. A host is expected to
    /// answer <see cref="ReviewDecision.Cancelled"/> itself when it sees that
    /// token cancelled (matching the decision going out over the wire as an
    /// ordinary return value, not an exception), but a host that instead
    /// follows the standard .NET convention and throws
    /// <see cref="OperationCanceledException"/> is treated the same way, as a
    /// defensive fallback.
    /// </summary>
    public sealed class ReviewTools
    {
        private sealed class PendingReview
        {
            public readonly CancellationTokenSource Cts;
            public volatile bool Resolved;

            public PendingReview(CancellationTokenSource cts)
            {
                Cts = cts;
            }
        }

        private readonly IEditorHost _host;
        private readonly object _lock = new object();
        private readonly HashSet<object> _acceptAll = new HashSet<object>();
        private readonly Dictionary<object, Dictionary<string, PendingReview>> _pending = new Dictionary<object, Dictionary<string, PendingReview>>();

        public ReviewTools(IEditorHost host)
        {
            _host = host;
        }

        public async Task<ToolResult> ReviewDiff(JsonElement args, object connection, CancellationToken ct)
        {
            if (!ToolArgs.TryRequireString(args, "path", out var path, out var errPath))
            {
                return errPath!;
            }

            if (!ToolArgs.TryRequireAnyString(args, "proposed", out var proposed, out var errProposed))
            {
                return errProposed!;
            }

            var original = ToolArgs.GetString(args, "original");
            var summary = ToolArgs.GetString(args, "summary");
            var shared = ToolArgs.GetBool(args, "shared");

            lock (_lock)
            {
                if (_acceptAll.Contains(connection))
                {
                    return new ToolResult(Serialize(ReviewDecision.Accept), false);
                }
            }

            var cts = CancellationTokenSource.CreateLinkedTokenSource(ct);
            var pending = new PendingReview(cts);
            lock (_lock)
            {
                if (!_pending.TryGetValue(connection, out var byConn))
                {
                    byConn = new Dictionary<string, PendingReview>(StringComparer.Ordinal);
                    _pending[connection] = byConn;
                }

                byConn[path] = pending;
            }

            try
            {
                var request = new ReviewRequest(path, original, proposed, summary, shared);
                ReviewDecision decision;
                try
                {
                    decision = await _host.ReviewDiffAsync(request, cts.Token).ConfigureAwait(false);
                }
                catch (OperationCanceledException) when (cts.IsCancellationRequested)
                {
                    decision = ReviewDecision.Cancelled;
                }

                // Fix round 1, F7: Resolved must be read AND set atomically
                // with ReviewCancel's own read-and-claim — the same lock.
                // Setting it unlocked (the original code) let a
                // review_cancel racing a fast answer see Resolved still
                // false, claim the pending review as its own (reporting
                // {"cancelled":true} to ITS caller), while this call went on
                // to return the host's actual decision (e.g. "accept") to
                // ITS OWN caller — two callers asking about the exact same
                // review getting contradictory answers. Reading the OLD
                // value here (before overwriting it true) is what tells this
                // call whether review_cancel got there first: if so, it must
                // agree and answer "cancelled" too, regardless of what the
                // host actually decided.
                bool claimedByCancel;
                lock (_lock)
                {
                    claimedByCancel = pending.Resolved;
                    pending.Resolved = true;
                }

                if (claimedByCancel)
                {
                    decision = ReviewDecision.Cancelled;
                }
                else if (decision == ReviewDecision.AcceptAll && !cts.IsCancellationRequested)
                {
                    // Fix round 1, per the coordinator's note on the
                    // BridgeServer rework: ConnectionClosed may have
                    // cancelled this review's token directly (not via
                    // review_cancel — e.g. the socket simply closed) while a
                    // misbehaving host still answers AcceptAll despite that.
                    // A still-running review_diff must not re-create
                    // per-connection state for a connection already known
                    // gone.
                    lock (_lock)
                    {
                        _acceptAll.Add(connection);
                    }
                }

                return new ToolResult(Serialize(decision), false);
            }
            finally
            {
                lock (_lock)
                {
                    if (_pending.TryGetValue(connection, out var byConn)
                        && byConn.TryGetValue(path, out var existing)
                        && ReferenceEquals(existing, pending))
                    {
                        byConn.Remove(path);
                        if (byConn.Count == 0)
                        {
                            _pending.Remove(connection);
                        }
                    }
                }

                cts.Dispose();
            }
        }

        public Task<ToolResult> ReviewCancel(JsonElement args, object connection, CancellationToken ct)
        {
            if (!ToolArgs.TryRequireString(args, "path", out var path, out var errPath))
            {
                return Task.FromResult(errPath!);
            }

            PendingReview? found = null;
            lock (_lock)
            {
                if (_pending.TryGetValue(connection, out var byConn)
                    && byConn.TryGetValue(path, out var pending)
                    && !pending.Resolved)
                {
                    // Claim it now, under the lock: a concurrent second
                    // review_cancel for the same (connection, path) must see
                    // Resolved already true and report failure.
                    pending.Resolved = true;
                    found = pending;
                }
            }

            if (found == null)
            {
                return Task.FromResult(new ToolResult("{\"cancelled\":false}", false));
            }

            found.Cts.Cancel();
            return Task.FromResult(new ToolResult("{\"cancelled\":true}", false));
        }

        public void ConnectionClosed(object connection)
        {
            // Fix round 1, F1: this used to drop _pending[connection]
            // without cancelling those CancellationTokenSources, so a still-
            // running review_diff was never told the connection is gone —
            // the host's difference viewer stayed open and the tool call
            // hung until (if ever) the host's own logic gave up. Cancel every
            // pending CTS for this connection first, then drop the map entry;
            // ReviewDiff's own `finally` still runs and finds nothing left to
            // remove, which is fine — this is the one place responsible for
            // telling the host, so it must not skip a review by racing
            // ReviewDiff's cleanup. Cancelling is done OUTSIDE the lock:
            // CancellationTokenSource.Cancel() runs registered callbacks
            // synchronously, and a host's callback must never be invoked
            // while this lock is held.
            List<CancellationTokenSource>? toCancel = null;
            lock (_lock)
            {
                _acceptAll.Remove(connection);
                if (_pending.TryGetValue(connection, out var byConn))
                {
                    toCancel = new List<CancellationTokenSource>(byConn.Count);
                    foreach (var pending in byConn.Values)
                    {
                        toCancel.Add(pending.Cts);
                    }

                    _pending.Remove(connection);
                }
            }

            if (toCancel != null)
            {
                foreach (var cts in toCancel)
                {
                    try
                    {
                        cts.Cancel();
                    }
                    catch (ObjectDisposedException)
                    {
                        // ReviewDiff's own `finally` already disposed this
                        // CTS: its host call happened to finish (or it was
                        // separately cancelled by review_cancel) in the
                        // narrow window between the snapshot above and this
                        // call. Either way the review is already resolved;
                        // there is nothing left to cancel.
                    }
                }
            }
        }

        private static string Serialize(ReviewDecision decision) => decision switch
        {
            ReviewDecision.Accept => "{\"decision\":\"accept\"}",
            ReviewDecision.Reject => "{\"decision\":\"reject\"}",
            ReviewDecision.AcceptAll => "{\"decision\":\"accept_all\"}",
            ReviewDecision.Cancelled => "{\"decision\":\"cancelled\"}",
            _ => throw new ArgumentOutOfRangeException(nameof(decision), decision, null),
        };
    }
}
