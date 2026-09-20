using System;
using System.Net;
using System.Net.Sockets;
using System.Text;
using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;
using BECode.Bridge;
using Xunit;

namespace BECode.Bridge.Tests
{
    /// <summary>
    /// F2: an end-to-end regression test through the real <see cref="BridgeServer"/>
    /// (not just <see cref="ToolRegistry"/>/<see cref="FakeEditorHost"/> called
    /// in-process), since the server-level deadlock the round-1 review found
    /// (review_cancel queued behind review_diff on the same connection) could
    /// only be seen with a real socket and the server's actual per-connection
    /// dispatch — an in-process call to <c>ToolRegistry.CallAsync</c> can
    /// never reproduce "two requests pipelined on one socket".
    /// </summary>
    public class ReviewOverTheWireTests
    {
        private const string Token = "review-wire-token";
        private static readonly TimeSpan ReplyTimeout = TimeSpan.FromSeconds(5);

        private static async Task<(BridgeServer Server, int Port)> StartServerAsync(FakeEditorHost host)
        {
            var registry = new ToolRegistry(host);
            var server = new BridgeServer(registry, Token, "1.0.0-test");
            var port = await server.StartAsync(0);
            return (server, port);
        }

        private static async Task<TcpClient> ConnectAsync(int port)
        {
            var client = new TcpClient();
            await client.ConnectAsync(IPAddress.Loopback, port);
            return client;
        }

        private static async Task SendLineAsync(NetworkStream stream, string json)
        {
            var bytes = Encoding.UTF8.GetBytes(json + "\n");
            await stream.WriteAsync(bytes, 0, bytes.Length);
        }

        private static async Task<string?> ReadLineWithTimeoutAsync(System.IO.StreamReader reader, TimeSpan timeout)
        {
            using var cts = new CancellationTokenSource(timeout);
            try
            {
                return await reader.ReadLineAsync(cts.Token);
            }
            catch (OperationCanceledException)
            {
                throw new TimeoutException($"no reply within {timeout}");
            }
        }

        private static async Task InitializeAsync(NetworkStream stream, System.IO.StreamReader reader)
        {
            await SendLineAsync(stream, $"{{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{{\"auth\":{{\"token\":\"{Token}\"}}}}}}");
            var line = await ReadLineWithTimeoutAsync(reader, ReplyTimeout);
            Assert.NotNull(line);
        }

        private static async Task WaitForAsync(Func<bool> condition, TimeSpan timeout)
        {
            var deadline = DateTime.UtcNow + timeout;
            while (!condition())
            {
                if (DateTime.UtcNow >= deadline)
                {
                    throw new TimeoutException($"condition not met within {timeout}");
                }

                await Task.Delay(20);
            }
        }

        [Fact]
        public async Task ReviewCancelOnTheSameSocketResolvesAPendingReviewDiffToCancelled()
        {
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

                    return ReviewDecision.Cancelled;
                },
            };

            var (server, port) = await StartServerAsync(host);
            await using var serverLifetime = server;

            using var client = await ConnectAsync(port);
            using var stream = client.GetStream();
            using var reader = new System.IO.StreamReader(stream, Encoding.UTF8);
            await InitializeAsync(stream, reader);

            // review_diff first; it blocks the FAKE host (not the connection
            // — tools/call now runs on its own task per connection) until its
            // token fires.
            await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":10,\"method\":\"tools/call\",\"params\":{\"name\":\"review_diff\",\"arguments\":{\"path\":\"a.txt\",\"proposed\":\"new\"}}}");
            await hostCalled.Task.WaitAsync(ReplyTimeout);

            // review_cancel for the SAME path, on the SAME socket, while
            // review_diff is still outstanding: this is exactly the shape
            // the round-1 review found deadlocked under the old
            // one-call-at-a-time-per-connection dispatch.
            await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":11,\"method\":\"tools/call\",\"params\":{\"name\":\"review_cancel\",\"arguments\":{\"path\":\"a.txt\"}}}");

            JsonElement? diffReply = null;
            JsonElement? cancelReply = null;
            for (var i = 0; i < 2; i++)
            {
                var line = await ReadLineWithTimeoutAsync(reader, ReplyTimeout);
                Assert.NotNull(line);
                var root = JsonDocument.Parse(line!).RootElement.Clone();
                var id = root.GetProperty("id").GetInt32();
                if (id == 10)
                {
                    diffReply = root;
                }
                else if (id == 11)
                {
                    cancelReply = root;
                }
            }

            Assert.NotNull(diffReply);
            Assert.NotNull(cancelReply);

            var diffText = diffReply!.Value.GetProperty("result").GetProperty("content")[0].GetProperty("text").GetString();
            var diffIsError = diffReply!.Value.GetProperty("result").GetProperty("isError").GetBoolean();
            var cancelText = cancelReply!.Value.GetProperty("result").GetProperty("content")[0].GetProperty("text").GetString();
            var cancelIsError = cancelReply!.Value.GetProperty("result").GetProperty("isError").GetBoolean();

            Assert.False(diffIsError);
            Assert.Equal("{\"decision\":\"cancelled\"}", diffText);
            Assert.False(cancelIsError);
            Assert.Equal("{\"cancelled\":true}", cancelText);
        }

        [Fact]
        public async Task ClosingTheSocketWithAReviewPendingCancelsTheHostsTokenAndDropsTheConnection()
        {
            var hostCalled = new TaskCompletionSource<bool>();
            var hostSawCancellation = new TaskCompletionSource<bool>();
            var host = new FakeEditorHost
            {
                OnReviewDiff = async (req, ct) =>
                {
                    var tcs = new TaskCompletionSource<bool>();
                    using (ct.Register(() => { hostSawCancellation.TrySetResult(true); tcs.TrySetResult(true); }))
                    {
                        hostCalled.SetResult(true);
                        await tcs.Task;
                    }

                    return ReviewDecision.Cancelled;
                },
            };

            var (server, port) = await StartServerAsync(host);
            await using var serverLifetime = server;

            var client = await ConnectAsync(port);
            var stream = client.GetStream();
            var reader = new System.IO.StreamReader(stream, Encoding.UTF8);
            await InitializeAsync(stream, reader);
            await WaitForAsync(() => server.ConnectionCount == 1, TimeSpan.FromSeconds(5));

            await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":20,\"method\":\"tools/call\",\"params\":{\"name\":\"review_diff\",\"arguments\":{\"path\":\"b.txt\",\"proposed\":\"new\"}}}");
            await hostCalled.Task.WaitAsync(ReplyTimeout);

            client.Close();

            await hostSawCancellation.Task.WaitAsync(ReplyTimeout);
            await WaitForAsync(() => server.ConnectionCount == 0, TimeSpan.FromSeconds(5));
        }
    }
}
