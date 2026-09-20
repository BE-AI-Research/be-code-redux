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
    /// throws <see cref="OperationCanceledException"/> — for that token being
    /// cancelled, or for any other reason (fix round 2, D4) — is treated the
    /// same way, unconditionally: <c>ReviewDiff</c>'s catch is not gated on
    /// the token's own state.
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
                // Fix round 2, D4: a host may throw OperationCanceledException
                // for a reason that has nothing to do with THIS token — Visual
                // Studio shutting down, the document closed underneath it —
                // and is not obliged to check `ct` before doing so. Gating this
                // catch on `cts.IsCancellationRequested` (fix round 1's
                // version) let such an OCE escape uncaught whenever the token
                // itself was not the reason, which propagated out of
                // ReviewDiff entirely: the Go side then waited out its own
                // timeout instead of getting an ordinary "cancelled" reply.
                // Any OCE from the host means "cancelled", unconditionally.
                catch (OperationCanceledException)
                {
                    decision = ReviewDecision.Cancelled;
                }

                // Fix round 2, F9: the entire decision — Resolved's read/write
                // (fix round 1, F7) AND whether an AcceptAll answer is honoured
                // — now happens in ONE lock acquisition, gated on this specific
                // pending registration still being present in _pending. Round
                // 1's version split this into two lock sections and checked
                // `cts.IsCancellationRequested` in between them, UNLOCKED:
                // ConnectionClosed removes the map entry under _lock, then
                // calls Cts.Cancel() OUTSIDE the lock (deliberately — see
                // ConnectionClosed's own comment) — so there is a real window,
                // under genuine thread concurrency, where the map entry is
                // already gone but the token has not been marked cancelled
                // yet. A review_diff whose host ignores cancellation and
                // answers AcceptAll in exactly that window read
                // `!cts.IsCancellationRequested` as still true and resurrected
                // accept-all for a connection ConnectionClosed had already
                // torn down (reviewer's probe: 582/5000). Checking "is this
                // pending object still the one registered under
                // (connection, path)?" — under the SAME lock ConnectionClosed
                // removes it under — is deterministic regardless of when
                // Cancel() itself runs, because map membership (not the
                // token) is what ConnectionClosed and this check now
                // synchronise on.
                lock (_lock)
                {
                    var claimedByCancel = pending.Resolved;
                    pending.Resolved = true;

                    if (claimedByCancel)
                    {
                        decision = ReviewDecision.Cancelled;
                    }
                    else if (decision == ReviewDecision.AcceptAll)
                    {
                        var stillRegistered = _pending.TryGetValue(connection, out var byConn)
                            && byConn.TryGetValue(path, out var existing)
                            && ReferenceEquals(existing, pending);
                        if (stillRegistered)
                        {
                            _acceptAll.Add(connection);
                        }
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

        /// <summary>
        /// Fix round 2, F9: a test-only probe of whether <paramref name="connection"/>
        /// currently has "accept all this session" recorded, so a regression
        /// test can assert the absence of state directly instead of inferring
        /// it indirectly through a second <c>review_diff</c> call.
        /// </summary>
        internal bool HasAcceptAll(object connection)
        {
            lock (_lock)
            {
                return _acceptAll.Contains(connection);
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
