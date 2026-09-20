using System;
using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;
using BECode.Bridge;
using BECode.Bridge.Tools;
using Xunit;

namespace BECode.Bridge.Tests
{
    /// <summary>
    /// Fix round 2, F9: <see cref="ReviewTools"/> instantiated directly
    /// (its constructor is public) rather than through <see cref="ToolRegistry"/>,
    /// so these tests can reach the internal <c>HasAcceptAll</c> probe and
    /// force the exact interleaving the finding describes — a still-running
    /// <c>review_diff</c> racing <c>ConnectionClosed</c> for the SAME
    /// connection, with a host that never looks at its token.
    /// </summary>
    public class ReviewToolsRaceTests
    {
        private static JsonElement Args(string json)
        {
            using var doc = JsonDocument.Parse(json);
            return doc.RootElement.Clone();
        }

        [Fact]
        public async Task ConnectionClosedRacingAMisbehavingHostNeverResurrectsAcceptAll()
        {
            // Genuine thread-pool concurrency (Task.Run for both sides, a
            // real await Task.Yield() inside the host) is what actually
            // exercises the race — the round-1 test that only ever waits on
            // ct.Register cannot, by construction, ever see the token
            // un-cancelled at check time (see the finding). 200 iterations:
            // the reviewer measured roughly 12% of iterations hitting the
            // bad interleaving against the un-fixed code, so a 200-iteration
            // run fails essentially every time without the fix and never
            // with it.
            for (var i = 0; i < 200; i++)
            {
                var entered = new TaskCompletionSource<bool>();
                var host = new FakeEditorHost
                {
                    OnReviewDiff = async (req, ct) =>
                    {
                        entered.SetResult(true);
                        await Task.Yield();
                        return ReviewDecision.AcceptAll; // never looks at ct
                    },
                };
                var reviewTools = new ReviewTools(host);
                var conn = new object();

                var diffTask = Task.Run(() => reviewTools.ReviewDiff(
                    Args($"{{\"path\":\"r{i}.txt\",\"proposed\":\"new\"}}"), conn, CancellationToken.None));
                await entered.Task;

                var closeTask = Task.Run(() => reviewTools.ConnectionClosed(conn));

                await Task.WhenAll(diffTask, closeTask);

                Assert.False(reviewTools.HasAcceptAll(conn));
            }
        }

        [Fact]
        public async Task AnAcceptAllThatLosesToAReviewCancelRecordsNoAcceptAllState()
        {
            // The missing test the finding calls out: even a fully
            // deterministic (non-racy) loss to review_cancel must not leave
            // accept-all state behind.
            var hostCalled = new TaskCompletionSource<bool>();
            var host = new FakeEditorHost
            {
                OnReviewDiff = async (req, ct) =>
                {
                    var tcs = new TaskCompletionSource<bool>();
                    using (ct.Register(() => tcs.TrySetResult(true)))
                    {
                        hostCalled.SetResult(true);
                        await tcs.Task;
                    }

                    // Still answers AcceptAll despite having been told to stop.
                    return ReviewDecision.AcceptAll;
                },
            };
            var reviewTools = new ReviewTools(host);
            var conn = new object();

            var diffTask = reviewTools.ReviewDiff(Args("{\"path\":\"a.txt\",\"proposed\":\"new\"}"), conn, CancellationToken.None);
            await hostCalled.Task;

            var cancelResult = await reviewTools.ReviewCancel(Args("{\"path\":\"a.txt\"}"), conn, CancellationToken.None);
            Assert.Equal("{\"cancelled\":true}", cancelResult.Text);

            var diffResult = await diffTask;
            Assert.Equal("{\"decision\":\"cancelled\"}", diffResult.Text);
            Assert.False(reviewTools.HasAcceptAll(conn));
        }
    }
}
