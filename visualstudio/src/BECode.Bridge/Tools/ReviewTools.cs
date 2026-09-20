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

            if (!ToolArgs.TryRequireString(args, "proposed", out var proposed, out var errProposed))
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

                // Whichever way the host answered, the decision is now final:
                // a review_cancel arriving after this point must not claim to
                // have changed it (mirrors vscode's `handle.resolved = true`,
                // set the instant the race is decided).
                pending.Resolved = true;

                if (decision == ReviewDecision.AcceptAll)
                {
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
            lock (_lock)
            {
                _acceptAll.Remove(connection);
                _pending.Remove(connection);
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
