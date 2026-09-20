using System;
using System.Collections.Generic;
using System.Diagnostics;
using System.Linq;
using System.Net;
using System.Net.NetworkInformation;
using System.Net.Sockets;
using System.Text;
using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;
using BECode.Bridge;
using Xunit;

namespace BECode.Bridge.Tests
{
    public class ServerTests
    {
        private const string Token = "test-token-abc123";
        private static readonly TimeSpan ReplyTimeout = TimeSpan.FromSeconds(5);

        private sealed class FakeToolDispatcher : IToolDispatcher
        {
            private readonly List<ToolInfo> _tools;

            public FakeToolDispatcher(IEnumerable<ToolInfo>? tools = null)
            {
                _tools = tools?.ToList() ?? new List<ToolInfo>();
            }

            public Func<string, JsonElement, object, CancellationToken, Task<ToolResult>>? OnCall { get; set; }

            // C1 (review round 1): lets a test make List() throw, to prove
            // the server survives it instead of zombifying the connection.
            public Func<IReadOnlyList<ToolInfo>>? OnList { get; set; }

            public List<object> ClosedConnections { get; } = new List<object>();

            public IReadOnlyList<ToolInfo> List() => OnList != null ? OnList() : _tools;

            public Task<ToolResult> CallAsync(string name, JsonElement args, object connection, CancellationToken ct)
            {
                if (OnCall != null)
                {
                    return OnCall(name, args, connection, ct);
                }

                return Task.FromResult(new ToolResult($"unknown tool {name}", true));
            }

            public void ConnectionClosed(object connection)
            {
                lock (ClosedConnections)
                {
                    ClosedConnections.Add(connection);
                }
            }
        }

        private static async Task<(BridgeServer server, int port)> StartServerAsync(
            IToolDispatcher? dispatcher = null,
            int maxLineBytes = 16 * 1024 * 1024)
        {
            var server = new BridgeServer(dispatcher ?? new FakeToolDispatcher(), Token, "1.0.0-test", maxLineBytes);
            var port = await server.StartAsync(0);
            return (server, port);
        }

        // Polls a condition up to a deadline rather than using a fixed sleep
        // as the synchronisation mechanism — the poll interval is just how
        // often it rechecks, not how long the test waits when the condition
        // is met early.
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

        // M6(a) (fix round 1): writes several JSON-RPC lines in ONE
        // WriteAsync call, so they genuinely pipeline on the wire (both
        // already sent, and available to be read/dequeued together) rather
        // than the second only being written after the first's reply is
        // read — which would never exercise a race between them.
        private static async Task SendLinesAsync(NetworkStream stream, params string[] jsonLines)
        {
            var sb = new StringBuilder();
            foreach (var json in jsonLines)
            {
                sb.Append(json).Append('\n');
            }

            var bytes = Encoding.UTF8.GetBytes(sb.ToString());
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

        private static async Task<JsonElement> InitializeAsync(NetworkStream stream, System.IO.StreamReader reader, string token, int id = 1)
        {
            await SendLineAsync(stream, $"{{\"jsonrpc\":\"2.0\",\"id\":{id},\"method\":\"initialize\",\"params\":{{\"auth\":{{\"token\":\"{token}\"}}}}}}");
            var line = await ReadLineWithTimeoutAsync(reader, ReplyTimeout);
            Assert.NotNull(line);
            return JsonDocument.Parse(line!).RootElement.Clone();
        }

        [Fact]
        public async Task WrongTokenGetsBadTokenErrorAndSocketCloses()
        {
            var (server, port) = await StartServerAsync();
            await using var serverLifetime = server;

            using var client = await ConnectAsync(port);
            using var stream = client.GetStream();
            using var reader = new System.IO.StreamReader(stream, Encoding.UTF8);

            await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"auth\":{\"token\":\"wrong\"}}}");
            var line = await ReadLineWithTimeoutAsync(reader, ReplyTimeout);
            Assert.NotNull(line);
            var root = JsonDocument.Parse(line!).RootElement;

            Assert.Equal(-32001, root.GetProperty("error").GetProperty("code").GetInt32());
            Assert.Equal("bad token", root.GetProperty("error").GetProperty("message").GetString());

            // The server closes the socket after a bad token: the next read
            // must observe end-of-stream (null), not merely time out.
            var next = await ReadLineWithTimeoutAsync(reader, ReplyTimeout);
            Assert.Null(next);
        }

        [Fact]
        public async Task ToolsListBeforeInitializeGetsNotInitialized()
        {
            var (server, port) = await StartServerAsync();
            await using var serverLifetime = server;

            using var client = await ConnectAsync(port);
            using var stream = client.GetStream();
            using var reader = new System.IO.StreamReader(stream, Encoding.UTF8);

            await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}");
            var line = await ReadLineWithTimeoutAsync(reader, ReplyTimeout);
            Assert.NotNull(line);
            var root = JsonDocument.Parse(line!).RootElement;

            Assert.Equal(-32002, root.GetProperty("error").GetProperty("code").GetInt32());
            Assert.Equal("not initialized", root.GetProperty("error").GetProperty("message").GetString());
        }

        [Fact]
        public async Task NotificationGetsNoReplyThenARequestGetsExactlyOneReply()
        {
            var (server, port) = await StartServerAsync();
            await using var serverLifetime = server;

            using var client = await ConnectAsync(port);
            using var stream = client.GetStream();
            using var reader = new System.IO.StreamReader(stream, Encoding.UTF8);

            // A notification (no "id") for an unknown method: must produce
            // no reply at all, even though the method itself is bogus.
            await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"method\":\"whatever\"}");
            // Immediately follow with a real request on the same connection.
            await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":7,\"method\":\"tools/list\"}");

            var line = await ReadLineWithTimeoutAsync(reader, ReplyTimeout);
            Assert.NotNull(line);
            var root = JsonDocument.Parse(line!).RootElement;
            // The one reply that arrives must be the reply to id 7 (still
            // "not initialized", since we never authenticated) — proving the
            // notification produced nothing ahead of it.
            Assert.Equal(7, root.GetProperty("id").GetInt32());

            // And nothing further arrives.
            var extra = Record.ExceptionAsync(async () => await ReadLineWithTimeoutAsync(reader, TimeSpan.FromMilliseconds(300)));
            var ex = await extra;
            Assert.IsType<TimeoutException>(ex);
        }

        [Fact]
        public async Task InitializeReplyShapeIsExact()
        {
            var (server, port) = await StartServerAsync();
            await using var serverLifetime = server;

            using var client = await ConnectAsync(port);
            using var stream = client.GetStream();
            using var reader = new System.IO.StreamReader(stream, Encoding.UTF8);

            var root = await InitializeAsync(stream, reader, Token, id: 42);

            Assert.Equal("2.0", root.GetProperty("jsonrpc").GetString());
            Assert.Equal(42, root.GetProperty("id").GetInt32());
            var result = root.GetProperty("result");
            Assert.Equal("2024-11-05", result.GetProperty("protocolVersion").GetString());
            Assert.True(result.GetProperty("capabilities").GetProperty("tools").ValueKind == JsonValueKind.Object);
            Assert.Empty(result.GetProperty("capabilities").GetProperty("tools").EnumerateObject());
            var serverInfo = result.GetProperty("serverInfo");
            Assert.Equal("be-code-visualstudio", serverInfo.GetProperty("name").GetString());
            Assert.Equal("1.0.0-test", serverInfo.GetProperty("version").GetString());
            Assert.False(root.TryGetProperty("error", out _));
        }

        [Fact]
        public async Task ToolsCallWrapsAResultAsContentTextIsError()
        {
            var dispatcher = new FakeToolDispatcher
            {
                OnCall = (name, args, conn, ct) => Task.FromResult(new ToolResult("hello from " + name, false)),
            };
            var (server, port) = await StartServerAsync(dispatcher);
            await using var serverLifetime = server;

            using var client = await ConnectAsync(port);
            using var stream = client.GetStream();
            using var reader = new System.IO.StreamReader(stream, Encoding.UTF8);
            await InitializeAsync(stream, reader, Token);

            await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"ping\",\"arguments\":{}}}");
            var line = await ReadLineWithTimeoutAsync(reader, ReplyTimeout);
            Assert.NotNull(line);
            var root = JsonDocument.Parse(line!).RootElement;

            var result = root.GetProperty("result");
            Assert.False(result.GetProperty("isError").GetBoolean());
            var content = result.GetProperty("content");
            Assert.Equal(1, content.GetArrayLength());
            var first = content[0];
            Assert.Equal("text", first.GetProperty("type").GetString());
            Assert.Equal("hello from ping", first.GetProperty("text").GetString());
        }

        // I3 (review round 1): this does NOT prove the server recognises
        // "unknown tool nope" — there is no registry in this project at
        // all, that is Task 3's obligation (ToolRegistry, built against
        // IToolDispatcher). What it actually proves is narrower and still
        // load-bearing: BridgeServer passes a dispatcher's ToolResult
        // through to the wire unchanged (isError and text both), whatever
        // that result says. FakeToolDispatcher's default behaviour —
        // returning isError:true "unknown tool <name>" for anything with no
        // OnCall configured — merely mimics the shape Task 3's real
        // ToolRegistry is expected to produce for an unrecognised name, so
        // this test doubles as a fixture for that shape without asserting
        // the server itself implements it.
        [Fact]
        public async Task ToolsCallPassesADispatchersIsErrorResultThroughUnchanged()
        {
            var (server, port) = await StartServerAsync(); // default dispatcher stands in for Task 3's ToolRegistry answering "no such tool"
            await using var serverLifetime = server;

            using var client = await ConnectAsync(port);
            using var stream = client.GetStream();
            using var reader = new System.IO.StreamReader(stream, Encoding.UTF8);
            await InitializeAsync(stream, reader, Token);

            await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"nope\",\"arguments\":{}}}");
            var line = await ReadLineWithTimeoutAsync(reader, ReplyTimeout);
            Assert.NotNull(line);
            var result = JsonDocument.Parse(line!).RootElement.GetProperty("result");

            Assert.True(result.GetProperty("isError").GetBoolean());
            Assert.Equal("unknown tool nope", result.GetProperty("content")[0].GetProperty("text").GetString());
        }

        // Task 3a / R-7: the wire contract changed. "Requests on one
        // connection are handled in order" (the premise this test used to
        // assert for tools/call replies) is no longer true — tools/call now
        // runs on its own task and its reply may arrive out of request
        // order. REPLACES PipelinedCallsReplyInRequestOrderDespiteADelayedFirstCall;
        // see ASlowToolsCallDoesNotDelayALaterCallsReplyOnTheSameConnection
        // below, which asserts the opposite of what this test used to: the
        // later, faster call's reply arrives WHILE the slow one is still
        // pending.
        [Fact]
        public async Task ASlowToolsCallDoesNotDelayALaterCallsReplyOnTheSameConnection()
        {
            var gate = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
            var dispatcher = new FakeToolDispatcher
            {
                OnCall = async (name, args, conn, ct) =>
                {
                    if (name == "slow")
                    {
                        await gate.Task;
                    }

                    return new ToolResult(name, false);
                },
            };
            var (server, port) = await StartServerAsync(dispatcher);
            await using var serverLifetime = server;

            using var client = await ConnectAsync(port);
            using var stream = client.GetStream();
            using var reader = new System.IO.StreamReader(stream, Encoding.UTF8);
            await InitializeAsync(stream, reader, Token);

            // The gate is force-opened in `finally` regardless of outcome:
            // under today's sequential code the "slow" call is stuck on the
            // gate with nothing else able to cancel it, and DisposeAsync
            // (the `await using` above) would otherwise hang forever waiting
            // for it during test cleanup on a RED run — a hang, not a bounded
            // failure.
            try
            {
                // Both requests are written before either reply is read: this
                // is the pipelining the concurrency guarantee has to survive.
                await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":10,\"method\":\"tools/call\",\"params\":{\"name\":\"slow\",\"arguments\":{}}}");
                await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":11,\"method\":\"tools/call\",\"params\":{\"name\":\"fast\",\"arguments\":{}}}");

                // RED today (sequential processing): id 11 never arrives
                // before the gate opens — this read times out.
                var secondLine = await ReadLineWithTimeoutAsync(reader, ReplyTimeout);
                Assert.NotNull(secondLine);
                var secondRoot = JsonDocument.Parse(secondLine!).RootElement;
                Assert.Equal(11, secondRoot.GetProperty("id").GetInt32());

                gate.TrySetResult(true);

                var firstLine = await ReadLineWithTimeoutAsync(reader, ReplyTimeout);
                Assert.NotNull(firstLine);
                var firstRoot = JsonDocument.Parse(firstLine!).RootElement;
                Assert.Equal(10, firstRoot.GetProperty("id").GetInt32());
            }
            finally
            {
                gate.TrySetResult(true);
            }
        }

        // R-7: review_diff/review_cancel in miniature — call A blocks until
        // call B's own handler unblocks it, both dispatched on ONE
        // connection before either reply is read. RED today: sequential
        // processing means B never even gets dispatched (it queues behind
        // A's still-pending call), so this is a genuine deadlock — reads use
        // a 3s timeout here (not the default 5s ReplyTimeout) purely so RED
        // shows up as a bounded TimeoutException, never a hang.
        [Fact]
        public async Task ACallCanBeUnblockedByAnotherCallOnTheSameConnectionWhileBothArePending()
        {
            var deadlockTimeout = TimeSpan.FromSeconds(3);
            var gate = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
            var dispatcher = new FakeToolDispatcher
            {
                OnCall = async (name, args, conn, ct) =>
                {
                    if (name == "review_diff")
                    {
                        await gate.Task;
                        return new ToolResult("diff-result", false);
                    }

                    // review_cancel unblocks the pending review_diff call.
                    gate.TrySetResult(true);
                    return new ToolResult("cancel-result", false);
                },
            };
            var (server, port) = await StartServerAsync(dispatcher);
            await using var serverLifetime = server;

            using var client = await ConnectAsync(port);
            using var stream = client.GetStream();
            using var reader = new System.IO.StreamReader(stream, Encoding.UTF8);
            await InitializeAsync(stream, reader, Token);

            // Force-open the gate in `finally`: under today's sequential
            // code review_cancel never gets dispatched at all, so nothing
            // ever opens it — without this, DisposeAsync (the `await using`
            // above) would hang the test process forever during cleanup
            // instead of the RED failure being a bounded timeout.
            try
            {
                await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"review_diff\",\"arguments\":{}}}");
                await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"review_cancel\",\"arguments\":{}}}");

                var seenIds = new HashSet<int>();
                for (var i = 0; i < 2; i++)
                {
                    var line = await ReadLineWithTimeoutAsync(reader, deadlockTimeout);
                    Assert.NotNull(line);
                    var root = JsonDocument.Parse(line!).RootElement;
                    seenIds.Add(root.GetProperty("id").GetInt32());
                }

                Assert.Equal(new HashSet<int> { 1, 2 }, seenIds);
            }
            finally
            {
                gate.TrySetResult(true);
            }
        }

        // Ordering that must survive concurrent tools/call: everything else
        // is still handled, and replied to, strictly in dispatch order.
        [Fact]
        public async Task ToolsListAfterAPendingSlowCallIsAnsweredImmediately()
        {
            var gate = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
            var dispatcher = new FakeToolDispatcher
            {
                OnCall = async (name, args, conn, ct) =>
                {
                    await gate.Task;
                    return new ToolResult(name, false);
                },
            };
            var (server, port) = await StartServerAsync(dispatcher);
            await using var serverLifetime = server;

            using var client = await ConnectAsync(port);
            using var stream = client.GetStream();
            using var reader = new System.IO.StreamReader(stream, Encoding.UTF8);
            await InitializeAsync(stream, reader, Token);

            // Force-open the gate in `finally`: under today's sequential
            // code tools/list never gets dispatched (it queues behind the
            // still-pending slow call), so nothing else would ever open it —
            // without this, DisposeAsync (the `await using` above) would
            // hang the test process during cleanup instead of the RED
            // failure being a bounded timeout.
            try
            {
                await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"slow\",\"arguments\":{}}}");
                await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/list\"}");

                var line = await ReadLineWithTimeoutAsync(reader, ReplyTimeout);
                Assert.NotNull(line);
                var root = JsonDocument.Parse(line!).RootElement;
                Assert.Equal(2, root.GetProperty("id").GetInt32());
                Assert.True(root.TryGetProperty("result", out _));

                gate.TrySetResult(true);
                var slowLine = await ReadLineWithTimeoutAsync(reader, ReplyTimeout);
                Assert.NotNull(slowLine);
                Assert.Equal(1, JsonDocument.Parse(slowLine!).RootElement.GetProperty("id").GetInt32());
            }
            finally
            {
                gate.TrySetResult(true);
            }
        }

        [Fact]
        public async Task InitializeThenToolsListStillReplyInOrder()
        {
            var (server, port) = await StartServerAsync();
            await using var serverLifetime = server;

            using var client = await ConnectAsync(port);
            using var stream = client.GetStream();
            using var reader = new System.IO.StreamReader(stream, Encoding.UTF8);

            await SendLineAsync(stream, $"{{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{{\"auth\":{{\"token\":\"{Token}\"}}}}}}");
            await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/list\"}");

            var first = await ReadLineWithTimeoutAsync(reader, ReplyTimeout);
            var second = await ReadLineWithTimeoutAsync(reader, ReplyTimeout);
            Assert.NotNull(first);
            Assert.NotNull(second);
            Assert.Equal(1, JsonDocument.Parse(first!).RootElement.GetProperty("id").GetInt32());
            Assert.Equal(2, JsonDocument.Parse(second!).RootElement.GetProperty("id").GetInt32());
        }

        [Fact]
        public async Task ToolsCallBeforeInitializeIsRefusedEvenThoughALaterInitializeSucceeds()
        {
            var (server, port) = await StartServerAsync();
            await using var serverLifetime = server;

            using var client = await ConnectAsync(port);
            using var stream = client.GetStream();
            using var reader = new System.IO.StreamReader(stream, Encoding.UTF8);

            // M6(a) (fix round 1): both lines written in ONE WriteAsync, so
            // they genuinely pipeline — reading the first reply before
            // sending the second (the original shape) never exercises
            // whether auth-gating happens before a tools/call is forked,
            // since the second line would not even be on the wire yet.
            await SendLinesAsync(
                stream,
                "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"x\",\"arguments\":{}}}",
                $"{{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"initialize\",\"params\":{{\"auth\":{{\"token\":\"{Token}\"}}}}}}");

            var first = await ReadLineWithTimeoutAsync(reader, ReplyTimeout);
            Assert.NotNull(first);
            var firstRoot = JsonDocument.Parse(first!).RootElement;
            Assert.Equal(1, firstRoot.GetProperty("id").GetInt32());
            Assert.Equal(-32002, firstRoot.GetProperty("error").GetProperty("code").GetInt32());

            var second = await ReadLineWithTimeoutAsync(reader, ReplyTimeout);
            Assert.NotNull(second);
            var secondRoot = JsonDocument.Parse(second!).RootElement;
            Assert.Equal(2, secondRoot.GetProperty("id").GetInt32());
            Assert.False(secondRoot.TryGetProperty("error", out _));
        }

        // Per-connection cancellation + prompt teardown: a call that blocks
        // ONLY on its CancellationToken (never on a test gate) must be told
        // as soon as the client socket closes, and teardown must not wait
        // for it. RED today: ConnectionCount stays 1 (HandleConnectionAsync's
        // finally awaits the processor, which awaits this call, before ever
        // removing the client or calling ConnectionClosed).
        [Fact]
        public async Task ClosingTheSocketCancelsAnInFlightCallsTokenAndTearsDownPromptly()
        {
            var cancelled = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
            var dispatcher = new FakeToolDispatcher
            {
                OnCall = async (name, args, conn, ct) =>
                {
                    try
                    {
                        await Task.Delay(Timeout.Infinite, ct);
                    }
                    catch (OperationCanceledException)
                    {
                        cancelled.TrySetResult(true);
                        throw;
                    }

                    return new ToolResult("unreachable", false);
                },
            };
            var (server, port) = await StartServerAsync(dispatcher);
            await using var serverLifetime = server;

            var client = await ConnectAsync(port);
            var stream = client.GetStream();
            using var reader = new System.IO.StreamReader(stream, Encoding.UTF8);
            await InitializeAsync(stream, reader, Token);

            await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"blocking\",\"arguments\":{}}}");
            await WaitForAsync(() => server.ConnectionCount == 1, TimeSpan.FromSeconds(5));

            // No gate is ever opened: the token itself is what unblocks the call.
            client.Close();

            var cancelledInTime = await Task.WhenAny(cancelled.Task, Task.Delay(TimeSpan.FromSeconds(5))) == cancelled.Task;
            Assert.True(cancelledInTime, "the in-flight call's token was never cancelled");

            await WaitForAsync(() => dispatcher.ClosedConnections.Count == 1, TimeSpan.FromSeconds(5));
            Assert.Single(dispatcher.ClosedConnections);

            await WaitForAsync(() => server.ConnectionCount == 0, TimeSpan.FromSeconds(5));
            Assert.Equal(0, server.ConnectionCount);
        }

        // Dispose with an in-flight call: DisposeAsync itself must cancel
        // the token (via the server-wide CTS the connection's token is
        // linked to) and return, having called ConnectionClosed exactly
        // once.
        [Fact]
        public async Task DisposeAsyncCancelsAnInFlightCallsTokenAndReturns()
        {
            var cancelled = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
            var dispatcher = new FakeToolDispatcher
            {
                OnCall = async (name, args, conn, ct) =>
                {
                    try
                    {
                        await Task.Delay(Timeout.Infinite, ct);
                    }
                    catch (OperationCanceledException)
                    {
                        cancelled.TrySetResult(true);
                        throw;
                    }

                    return new ToolResult("unreachable", false);
                },
            };
            var server = new BridgeServer(dispatcher, Token, "1.0.0-test");
            var port = await server.StartAsync(0);

            using var client = await ConnectAsync(port);
            using var stream = client.GetStream();
            using var reader = new System.IO.StreamReader(stream, Encoding.UTF8);
            await InitializeAsync(stream, reader, Token);

            await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"blocking\",\"arguments\":{}}}");
            await WaitForAsync(() => server.ConnectionCount == 1, TimeSpan.FromSeconds(5));

            var disposeTask = server.DisposeAsync().AsTask();
            var completed = await Task.WhenAny(disposeTask, Task.Delay(TimeSpan.FromSeconds(5)));
            Assert.Same(disposeTask, completed);
            await disposeTask;

            Assert.True(await cancelled.Task);
            Assert.Single(dispatcher.ClosedConnections);
        }

        // A call that ignores its token entirely (blocks on a gate the test
        // controls, opened only at the very end): teardown must not hang on
        // it. ConnectionClosed/ConnectionCount must still resolve promptly,
        // and both DisposeAsync and the connection's own teardown must give
        // up on the straggler within a bound and report it through OnError —
        // using a short, test-only bound via the public InFlightDrainTimeout
        // member so this test does not take 5s.
        [Fact]
        public async Task AStragglerThatIgnoresItsTokenIsAbandonedAndReportedThroughOnError()
        {
            // ConnectionCount == 1 is true from the moment the TCP connect
            // is accepted, well before this connection's tools/call handler
            // has necessarily started running — fix round 1, M1 added an
            // extra await (CallSlots.WaitAsync) before a call is even
            // forked, so relying on ConnectionCount alone now genuinely
            // races client.Close() against the dispatcher ever being
            // invoked at all. started is the real synchronisation point.
            var started = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
            var gate = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
            var callReturned = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
            var dispatcher = new FakeToolDispatcher
            {
                OnCall = async (name, args, conn, ct) =>
                {
                    started.TrySetResult(true);
                    await gate.Task; // deliberately ignores ct
                    callReturned.TrySetResult(true);
                    return new ToolResult("late", false);
                },
            };
            var errors = new List<(string Context, Exception Exception)>();
            var (server, port) = await StartServerAsync(dispatcher);
            server.InFlightDrainTimeout = TimeSpan.FromMilliseconds(200);
            server.OnError = (ctx, ex) =>
            {
                lock (errors)
                {
                    errors.Add((ctx, ex));
                }
            };
            // M6(c) (fix round 1): dispose the server via `await using` and
            // force-open the gate in `finally` — an early assertion failure
            // used to leave the server undisposed and the dispatcher's
            // OnCall permanently blocked on the gate, leaking a pooled task
            // for the rest of the test run.
            await using var serverLifetime = server;

            var client = await ConnectAsync(port);
            var stream = client.GetStream();
            using var reader = new System.IO.StreamReader(stream, Encoding.UTF8);
            await InitializeAsync(stream, reader, Token);

            try
            {
                await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"stubborn\",\"arguments\":{}}}");
                var startedInTime = await Task.WhenAny(started.Task, Task.Delay(TimeSpan.FromSeconds(5))) == started.Task;
                Assert.True(startedInTime, "the tools/call handler never started");

                client.Close();

                await WaitForAsync(() => dispatcher.ClosedConnections.Count == 1, TimeSpan.FromSeconds(5));
                Assert.Single(dispatcher.ClosedConnections);
                await WaitForAsync(() => server.ConnectionCount == 0, TimeSpan.FromSeconds(5));

                // M6(b) (fix round 1): snapshot under the lock — errors is
                // written from the server's own thread(s) concurrently with
                // this one polling it.
                await WaitForAsync(
                    () =>
                    {
                        lock (errors)
                        {
                            return errors.Any(e => e.Context.IndexOf("in-flight", StringComparison.OrdinalIgnoreCase) >= 0);
                        }
                    },
                    TimeSpan.FromSeconds(5));

                var disposeTask = server.DisposeAsync().AsTask();
                var completed = await Task.WhenAny(disposeTask, Task.Delay(TimeSpan.FromSeconds(5)));
                Assert.Same(disposeTask, completed);
                await disposeTask;

                // Only now let the abandoned call finish — proving it does
                // not hang or throw once teardown has already moved on
                // without it.
                gate.TrySetResult(true);
                var completedCall = await Task.WhenAny(callReturned.Task, Task.Delay(TimeSpan.FromSeconds(5)));
                Assert.Same(callReturned.Task, completedCall);
            }
            finally
            {
                gate.TrySetResult(true);
            }
        }

        // Replies after close: once teardown has fully finished (the client
        // observed ConnectionClosed and ConnectionCount 0), a call that only
        // then completes must not throw out of its task and must not be
        // reported through OnError with a write context — "a client that
        // went away is ordinary". Uses the dispatcher's own completion
        // signal (callCompleted) rather than an unreliable subscription to
        // TaskScheduler.UnobservedTaskException.
        [Fact]
        public async Task ALateReplyAfterTheConnectionIsGoneIsDroppedQuietly()
        {
            // See AStragglerThatIgnoresItsTokenIsAbandonedAndReportedThroughOnError's
            // comment on started: ConnectionCount alone is not a safe proxy
            // for "the tools/call handler has started", now that fix round
            // 1, M1 puts an extra await (CallSlots.WaitAsync) between a line
            // being dequeued and its call being forked.
            var started = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
            var gate = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
            var callCompleted = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
            var dispatcher = new FakeToolDispatcher
            {
                OnCall = async (name, args, conn, ct) =>
                {
                    started.TrySetResult(true);
                    await gate.Task; // deliberately ignores ct, like the straggler above
                    var result = new ToolResult("too-late", false);
                    callCompleted.TrySetResult(true);
                    return result;
                },
            };
            var errors = new List<(string Context, Exception Exception)>();
            var (server, port) = await StartServerAsync(dispatcher);
            server.InFlightDrainTimeout = TimeSpan.FromMilliseconds(200);
            server.OnError = (ctx, ex) =>
            {
                lock (errors)
                {
                    errors.Add((ctx, ex));
                }
            };
            // M6(d) (fix round 1): a deterministic signal for "this call's
            // own reply write has been attempted" — set only once, on the
            // straggler's eventual (post-abandonment) completion, since
            // that is the settlement this test actually needs to wait for.
            var settled = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
            server.OnCallSettled = () => settled.TrySetResult(true);
            await using var serverLifetime = server;

            var client = await ConnectAsync(port);
            var stream = client.GetStream();
            using var reader = new System.IO.StreamReader(stream, Encoding.UTF8);
            await InitializeAsync(stream, reader, Token);

            // Force-open the gate in `finally`: on a RED run against today's
            // sequential code the earlier WaitForAsync calls below never
            // resolve (ConnectionClosed never fires while the stubborn call
            // is stuck on the gate), so nothing else would ever open it —
            // without this, DisposeAsync (the `await using` above) would
            // hang the test process during cleanup instead of the RED
            // failure being a bounded timeout.
            try
            {
                await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"stubborn\",\"arguments\":{}}}");
                var startedInTime = await Task.WhenAny(started.Task, Task.Delay(TimeSpan.FromSeconds(5))) == started.Task;
                Assert.True(startedInTime, "the tools/call handler never started");

                client.Close();

                await WaitForAsync(() => dispatcher.ClosedConnections.Count == 1, TimeSpan.FromSeconds(5));
                await WaitForAsync(() => server.ConnectionCount == 0, TimeSpan.FromSeconds(5));
                // Let the abandonment bound elapse (reported through
                // OnError) so the connection is fully torn down — state
                // disposed — before the dispatcher's own gate is opened.
                // M6(b) (fix round 1): snapshot under the lock — errors is
                // written from the server's own thread(s) concurrently with
                // this one polling it.
                await WaitForAsync(
                    () =>
                    {
                        lock (errors)
                        {
                            return errors.Any(e => e.Context.IndexOf("in-flight", StringComparison.OrdinalIgnoreCase) >= 0);
                        }
                    },
                    TimeSpan.FromSeconds(5));

                lock (errors)
                {
                    errors.Clear();
                }

                gate.TrySetResult(true);
                var completedInTime = await Task.WhenAny(callCompleted.Task, Task.Delay(TimeSpan.FromSeconds(5))) == callCompleted.Task;
                Assert.True(completedInTime, "the abandoned call never completed via the dispatcher's own signal");

                // M6(d) (fix round 1): wait for the deterministic
                // OnCallSettled signal — the straggler's own reply write has
                // now actually been attempted (and dropped quietly) —
                // instead of guessing at a fixed grace period.
                var settledInTime = await Task.WhenAny(settled.Task, Task.Delay(TimeSpan.FromSeconds(5))) == settled.Task;
                Assert.True(settledInTime, "the straggler's call task never settled");

                List<(string Context, Exception Exception)> errorsSnapshot;
                lock (errors)
                {
                    errorsSnapshot = new List<(string, Exception)>(errors);
                }

                Assert.DoesNotContain(errorsSnapshot, e => e.Context.IndexOf("write", StringComparison.OrdinalIgnoreCase) >= 0);
            }
            finally
            {
                gate.TrySetResult(true);
            }
        }

        // Frames never interleave: 20 concurrent tools/call replies, each a
        // large (64 KiB) text payload, must each still land as exactly one
        // parseable JSON line — proving the per-connection write lock still
        // serialises writes under genuine concurrency.
        [Fact]
        public async Task TwentyConcurrentCallsNeverInterleaveFramesAndEachIdArrivesExactlyOnce()
        {
            var dispatcher = new FakeToolDispatcher
            {
                OnCall = async (name, args, conn, ct) =>
                {
                    await Task.Yield();
                    return new ToolResult(new string('x', 64 * 1024), false);
                },
            };
            var (server, port) = await StartServerAsync(dispatcher);
            await using var serverLifetime = server;

            using var client = await ConnectAsync(port);
            using var stream = client.GetStream();
            using var reader = new System.IO.StreamReader(stream, Encoding.UTF8);
            await InitializeAsync(stream, reader, Token);

            for (var i = 1; i <= 20; i++)
            {
                await SendLineAsync(stream, $"{{\"jsonrpc\":\"2.0\",\"id\":{i},\"method\":\"tools/call\",\"params\":{{\"name\":\"big\",\"arguments\":{{}}}}}}");
            }

            var seenIds = new HashSet<int>();
            for (var i = 0; i < 20; i++)
            {
                var line = await ReadLineWithTimeoutAsync(reader, ReplyTimeout);
                Assert.NotNull(line);
                var root = JsonDocument.Parse(line!).RootElement; // throws if frames interleaved into invalid JSON
                var id = root.GetProperty("id").GetInt32();
                Assert.True(seenIds.Add(id), $"id {id} seen more than once");
                Assert.Equal(64 * 1024, root.GetProperty("result").GetProperty("content")[0].GetProperty("text").GetString()!.Length);
            }

            Assert.Equal(new HashSet<int>(Enumerable.Range(1, 20)), seenIds);
        }

        // Fix round 1, M1 (Ruling R-10): before this task's concurrency
        // rework, one connection could hold at most one in-flight call; the
        // cap restores an equivalent bound instead of letting a client that
        // pipelines many thousands of tools/call lines get that many live
        // dispatcher calls at once. With the cap shrunk to 2, three gated
        // calls dispatched back to back must leave the third's handler
        // un-started while the first two are still pending — the ordered
        // processor itself is blocked acquiring a slot, back-pressure, not
        // an error — and only once a slot frees up (one gate opened) does
        // the third handler start.
        [Fact]
        public async Task ConcurrentCallsPerConnectionAreCappedAndBackPressured()
        {
            var gate = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
            var startedLock = new object();
            var startedIds = new List<int>();
            var dispatcher = new FakeToolDispatcher
            {
                OnCall = async (name, args, conn, ct) =>
                {
                    lock (startedLock)
                    {
                        startedIds.Add(int.Parse(name));
                    }

                    await gate.Task;
                    return new ToolResult(name, false);
                },
            };
            var (server, port) = await StartServerAsync(dispatcher);
            server.MaxConcurrentCallsPerConnection = 2;
            await using var serverLifetime = server;

            using var client = await ConnectAsync(port);
            using var stream = client.GetStream();
            using var reader = new System.IO.StreamReader(stream, Encoding.UTF8);
            await InitializeAsync(stream, reader, Token);

            for (var i = 1; i <= 3; i++)
            {
                await SendLineAsync(stream, $"{{\"jsonrpc\":\"2.0\",\"id\":{i},\"method\":\"tools/call\",\"params\":{{\"name\":\"{i}\",\"arguments\":{{}}}}}}");
            }

            await WaitForAsync(
                () =>
                {
                    lock (startedLock)
                    {
                        return startedIds.Count == 2;
                    }
                },
                TimeSpan.FromSeconds(5));

            // Give the third every reasonable chance to (incorrectly) start
            // anyway before asserting it hasn't — a generous but bounded
            // grace period, not a synchronisation sleep for the positive
            // case above.
            await Task.Delay(TimeSpan.FromMilliseconds(300));

            List<int> startedSnapshot;
            lock (startedLock)
            {
                startedSnapshot = new List<int>(startedIds);
            }

            Assert.Equal(2, startedSnapshot.Count);
            Assert.DoesNotContain(3, startedSnapshot);

            gate.TrySetResult(true);

            await WaitForAsync(
                () =>
                {
                    lock (startedLock)
                    {
                        return startedIds.Count == 3;
                    }
                },
                TimeSpan.FromSeconds(5));

            var seenIds = new HashSet<int>();
            for (var i = 0; i < 3; i++)
            {
                var line = await ReadLineWithTimeoutAsync(reader, ReplyTimeout);
                Assert.NotNull(line);
                seenIds.Add(JsonDocument.Parse(line!).RootElement.GetProperty("id").GetInt32());
            }

            Assert.Equal(new HashSet<int> { 1, 2, 3 }, seenIds);
        }

        [Fact]
        public async Task ThrownExceptionInAToolIsErrorNotAJsonRpcError()
        {
            var dispatcher = new FakeToolDispatcher
            {
                OnCall = (name, args, conn, ct) => throw new InvalidOperationException("boom"),
            };
            var (server, port) = await StartServerAsync(dispatcher);
            await using var serverLifetime = server;

            using var client = await ConnectAsync(port);
            using var stream = client.GetStream();
            using var reader = new System.IO.StreamReader(stream, Encoding.UTF8);
            await InitializeAsync(stream, reader, Token);

            await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":5,\"method\":\"tools/call\",\"params\":{\"name\":\"boomtool\",\"arguments\":{}}}");
            var line = await ReadLineWithTimeoutAsync(reader, ReplyTimeout);
            Assert.NotNull(line);
            var root = JsonDocument.Parse(line!).RootElement;

            Assert.False(root.TryGetProperty("error", out _));
            var result = root.GetProperty("result");
            Assert.True(result.GetProperty("isError").GetBoolean());
        }

        [Fact]
        public async Task ConnectionClosedIsCalledWhenTheClientDisconnects()
        {
            var dispatcher = new FakeToolDispatcher();
            var (server, port) = await StartServerAsync(dispatcher);
            await using var serverLifetime = server;

            var client = await ConnectAsync(port);
            var stream = client.GetStream();
            using var reader = new System.IO.StreamReader(stream, Encoding.UTF8);
            await InitializeAsync(stream, reader, Token);

            client.Close();

            var deadline = DateTime.UtcNow.AddSeconds(5);
            while (dispatcher.ClosedConnections.Count == 0 && DateTime.UtcNow < deadline)
            {
                await Task.Delay(20);
            }

            Assert.Single(dispatcher.ClosedConnections);
        }

        [Fact]
        public async Task ListenerIsBoundToLoopbackOnly()
        {
            var (server, port) = await StartServerAsync();
            await using var serverLifetime = server;

            var listeners = IPGlobalProperties.GetIPGlobalProperties().GetActiveTcpListeners();
            var matches = listeners.Where(ep => ep.Port == port).ToArray();

            Assert.NotEmpty(matches);
            Assert.All(matches, ep => Assert.Equal(IPAddress.Loopback, ep.Address));
        }

        // C1 (review round 1): a dispatcher whose List() throws must not
        // zombify the connection — the request that hit it gets an internal
        // error, and a later request on the same connection is still
        // answered normally.
        [Fact]
        public async Task AThrowingToolListGetsInternalErrorAndTheConnectionKeepsWorking()
        {
            var shouldThrow = true;
            var dispatcher = new FakeToolDispatcher
            {
                OnList = () => shouldThrow
                    ? throw new InvalidOperationException("boom-list")
                    : (IReadOnlyList<ToolInfo>)new List<ToolInfo>(),
            };
            var errors = new List<(string Context, Exception Exception)>();
            var (server, port) = await StartServerAsync(dispatcher);
            server.OnError = (ctx, ex) =>
            {
                lock (errors)
                {
                    errors.Add((ctx, ex));
                }
            };
            await using var serverLifetime = server;

            using var client = await ConnectAsync(port);
            using var stream = client.GetStream();
            using var reader = new System.IO.StreamReader(stream, Encoding.UTF8);
            await InitializeAsync(stream, reader, Token);

            await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":20,\"method\":\"tools/list\"}");
            var firstLine = await ReadLineWithTimeoutAsync(reader, ReplyTimeout);
            Assert.NotNull(firstLine);
            var firstRoot = JsonDocument.Parse(firstLine!).RootElement;
            Assert.Equal(-32603, firstRoot.GetProperty("error").GetProperty("code").GetInt32());
            Assert.Contains("boom-list", firstRoot.GetProperty("error").GetProperty("message").GetString());

            // The connection itself must still be usable afterward.
            shouldThrow = false;
            await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":21,\"method\":\"tools/list\"}");
            var secondLine = await ReadLineWithTimeoutAsync(reader, ReplyTimeout);
            Assert.NotNull(secondLine);
            var secondRoot = JsonDocument.Parse(secondLine!).RootElement;
            Assert.False(secondRoot.TryGetProperty("error", out _));
            Assert.Equal(0, secondRoot.GetProperty("result").GetProperty("tools").GetArrayLength());

            // M6(b) (fix round 1): snapshot under the lock before asserting
            // — errors is written from the server's own thread(s), not just
            // this test's.
            List<(string Context, Exception Exception)> errorsSnapshot;
            lock (errors)
            {
                errorsSnapshot = new List<(string, Exception)>(errors);
            }

            Assert.Contains(errorsSnapshot, e => e.Exception.Message == "boom-list");
        }

        // I1 (review round 1), server-level: the per-connection LineFramer
        // cap is wired through the optional trailing BridgeServer
        // constructor parameter, and exceeding it closes the connection
        // rather than growing the buffer forever.
        [Fact]
        public async Task PendingBytesBeyondTheServerSideCapClosesTheConnection()
        {
            var errors = new List<(string Context, Exception Exception)>();
            var (server, port) = await StartServerAsync(maxLineBytes: 64);
            server.OnError = (ctx, ex) =>
            {
                lock (errors)
                {
                    errors.Add((ctx, ex));
                }
            };
            await using var serverLifetime = server;

            using var client = await ConnectAsync(port);
            using var stream = client.GetStream();
            using var reader = new System.IO.StreamReader(stream, Encoding.UTF8);

            // No newline at all: an unterminated "line" well past the
            // 64-byte cap, sent before authentication (the cap has to apply
            // there too, since the framer runs before auth).
            var chunk = Encoding.UTF8.GetBytes(new string('x', 200));
            await stream.WriteAsync(chunk, 0, chunk.Length);

            var line = await ReadLineWithTimeoutAsync(reader, ReplyTimeout);
            Assert.Null(line); // closed, not merely idle

            await WaitForAsync(
                () =>
                {
                    lock (errors)
                    {
                        return errors.Count > 0;
                    }
                },
                TimeSpan.FromSeconds(5));

            // M6(b) (fix round 1): snapshot under the lock before asserting.
            List<(string Context, Exception Exception)> errorsSnapshot;
            lock (errors)
            {
                errorsSnapshot = new List<(string, Exception)>(errors);
            }

            Assert.Contains(errorsSnapshot, e => e.Exception is System.IO.InvalidDataException);
        }

        // Test gap closed (review round 1): ConnectionCount across a real
        // connect/disconnect, polled with a deadline rather than a sleep.
        [Fact]
        public async Task ConnectionCountTracksConnectAndDisconnect()
        {
            var (server, port) = await StartServerAsync();
            await using var serverLifetime = server;

            Assert.Equal(0, server.ConnectionCount);

            var client = await ConnectAsync(port);
            await WaitForAsync(() => server.ConnectionCount == 1, TimeSpan.FromSeconds(5));
            Assert.Equal(1, server.ConnectionCount);

            client.Close();
            await WaitForAsync(() => server.ConnectionCount == 0, TimeSpan.FromSeconds(5));
            Assert.Equal(0, server.ConnectionCount);
        }

        // Test gap closed: DisposeAsync on a server with a live connection
        // must close it, and by the time DisposeAsync returns,
        // ConnectionClosed must already have fired — not "eventually".
        [Fact]
        public async Task DisposeAsyncClosesALiveConnectionAndConnectionClosedHasFiredByTheTimeItReturns()
        {
            var dispatcher = new FakeToolDispatcher();
            var server = new BridgeServer(dispatcher, Token, "1.0.0-test");
            var port = await server.StartAsync(0);

            using var client = await ConnectAsync(port);
            using var stream = client.GetStream();
            using var reader = new System.IO.StreamReader(stream, Encoding.UTF8);
            await InitializeAsync(stream, reader, Token);
            await WaitForAsync(() => server.ConnectionCount == 1, TimeSpan.FromSeconds(5));

            await server.DisposeAsync();

            Assert.Single(dispatcher.ClosedConnections);
        }

        // Test gap closed: the bad-token path must call ConnectionClosed
        // exactly once, and a later DisposeAsync must not call it again.
        [Fact]
        public async Task BadTokenClosesTheConnectionExactlyOnceAndALaterDisposeDoesNotCallItAgain()
        {
            var dispatcher = new FakeToolDispatcher();
            var server = new BridgeServer(dispatcher, Token, "1.0.0-test");
            var port = await server.StartAsync(0);

            using var client = await ConnectAsync(port);
            using var stream = client.GetStream();
            using var reader = new System.IO.StreamReader(stream, Encoding.UTF8);
            await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"auth\":{\"token\":\"wrong\"}}}");
            var line = await ReadLineWithTimeoutAsync(reader, ReplyTimeout);
            Assert.NotNull(line);

            await WaitForAsync(() => dispatcher.ClosedConnections.Count > 0, TimeSpan.FromSeconds(5));
            Assert.Single(dispatcher.ClosedConnections);

            await server.DisposeAsync();

            Assert.Single(dispatcher.ClosedConnections);
        }

        // Test gap closed: a JSON-RPC id can be any JSON value, and a
        // string id specifically must come back as a string, not be
        // coerced.
        [Fact]
        public async Task StringIdIsEchoedAsAString()
        {
            var (server, port) = await StartServerAsync();
            await using var serverLifetime = server;

            using var client = await ConnectAsync(port);
            using var stream = client.GetStream();
            using var reader = new System.IO.StreamReader(stream, Encoding.UTF8);

            await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":\"abc-123\",\"method\":\"initialize\",\"params\":{\"auth\":{\"token\":\"" + Token + "\"}}}");
            var line = await ReadLineWithTimeoutAsync(reader, ReplyTimeout);
            Assert.NotNull(line);
            var root = JsonDocument.Parse(line!).RootElement;

            Assert.Equal(JsonValueKind.String, root.GetProperty("id").ValueKind);
            Assert.Equal("abc-123", root.GetProperty("id").GetString());
        }

        // M4: DisposeAsync must not throw when called more than once, or
        // when two calls race concurrently.
        [Fact]
        public async Task DisposeAsyncIsIdempotent()
        {
            var (server, _) = await StartServerAsync();

            await server.DisposeAsync();
            var ex = await Record.ExceptionAsync(async () => await server.DisposeAsync());

            Assert.Null(ex);
        }

        [Fact]
        public async Task DisposeAsyncIsSafeUnderConcurrentCalls()
        {
            var (server, _) = await StartServerAsync();

            var first = server.DisposeAsync().AsTask();
            var second = server.DisposeAsync().AsTask();
            var ex = await Record.ExceptionAsync(async () => await Task.WhenAll(first, second));

            Assert.Null(ex);
        }

        // "Also fix" (review round 1): a failed write must be reported
        // through OnError and stop the connection, not silently keep trying
        // to write into a dead socket. Forcing a real write failure
        // deterministically: the dispatcher itself closes the connection's
        // socket (the same TcpClient CallAsync receives as `connection`)
        // before returning, so the reply HandleToolsCallAsync then tries to
        // write is guaranteed to fail.
        [Fact]
        public async Task AWriteFailureIsReportedThroughOnError()
        {
            var dispatcher = new FakeToolDispatcher
            {
                OnCall = (name, args, conn, ct) =>
                {
                    ((TcpClient)conn).Close();
                    return Task.FromResult(new ToolResult("won't be delivered", false));
                },
            };
            var errors = new List<(string Context, Exception Exception)>();
            var (server, port) = await StartServerAsync(dispatcher);
            server.OnError = (ctx, ex) =>
            {
                lock (errors)
                {
                    errors.Add((ctx, ex));
                }
            };
            await using var serverLifetime = server;

            using var client = await ConnectAsync(port);
            using var stream = client.GetStream();
            using var reader = new System.IO.StreamReader(stream, Encoding.UTF8);
            await InitializeAsync(stream, reader, Token);

            await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":40,\"method\":\"tools/call\",\"params\":{\"name\":\"x\",\"arguments\":{}}}");

            // M6(b) (fix round 1): snapshot under the lock before asserting
            // — errors is written from the dispatcher's/server's own
            // threads concurrently with this one enumerating it.
            await WaitForAsync(
                () =>
                {
                    lock (errors)
                    {
                        return errors.Any(e => e.Context.Contains("write", StringComparison.OrdinalIgnoreCase));
                    }
                },
                TimeSpan.FromSeconds(5));

            List<(string Context, Exception Exception)> errorsSnapshot;
            lock (errors)
            {
                errorsSnapshot = new List<(string, Exception)>(errors);
            }

            Assert.Contains(errorsSnapshot, e => e.Context.Contains("write", StringComparison.OrdinalIgnoreCase));
        }

        // Fix round 1, M5: ShouldClose alone only closes the socket the next
        // time ProcessQueueAsync happens to recheck it, after processing
        // another line — but tools/call's write now happens on its own
        // forked task (R-7), so if the client never sends anything further,
        // nothing ever rechecks the flag. Same deterministic write-failure
        // setup as AWriteFailureIsReportedThroughOnError (the dispatcher
        // itself closes the socket), asserting the connection still tears
        // down promptly WITHOUT the client ever sending anything more.
        //
        // Caveat, recorded honestly rather than glossed over: this
        // particular setup does not isolate the fix, because on this
        // runtime a Socket.Close() (and, empirically, even a
        // Socket.Shutdown(SocketShutdown.Send) — tried first, and it also
        // aborts the read loop's own pending ReadAsync with an
        // "Operation canceled" IOException) already faults the read loop's
        // pending ReadAsync on its own, which tears the connection down
        // through the ordinary disconnect path regardless of whether
        // WriteBytesAsync additionally closes the socket on a write
        // failure. I could not find a standard Socket API that fails a
        // pending write while leaving a concurrently pending read on the
        // very same socket unaffected, to construct a case that would
        // actually hang pre-fix. The fix itself is still correct per M5's
        // own reasoning (a write failure from a cause that does NOT also
        // trip the read side — e.g. one BridgeServer detects before the OS
        // does — must not leave the connection to linger), and this test
        // guards the passing behaviour going forward even though it does
        // not demonstrate a pre-fix hang.
        [Fact]
        public async Task AWriteFailureClosesTheConnectionWithoutWaitingForMoreInput()
        {
            var dispatcher = new FakeToolDispatcher
            {
                OnCall = (name, args, conn, ct) =>
                {
                    ((TcpClient)conn).Close();
                    return Task.FromResult(new ToolResult("won't be delivered", false));
                },
            };
            var (server, port) = await StartServerAsync(dispatcher);
            await using var serverLifetime = server;

            using var client = await ConnectAsync(port);
            using var stream = client.GetStream();
            using var reader = new System.IO.StreamReader(stream, Encoding.UTF8);
            await InitializeAsync(stream, reader, Token);

            await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":41,\"method\":\"tools/call\",\"params\":{\"name\":\"x\",\"arguments\":{}}}");

            // No further input is ever sent on this connection.
            await WaitForAsync(() => server.ConnectionCount == 0, TimeSpan.FromSeconds(5));
            Assert.Equal(0, server.ConnectionCount);
        }

        // Fix round 1, C1: requirement 4 says an ORDINARY late reply — a
        // call that honours its token, takes a little real unwind time, then
        // returns a normal result (exactly what ReviewTools.ReviewDiff does:
        // it returns Cancelled as an ordinary result, never a thrown
        // exception) — must be quiet, not reported through OnError. The
        // reviewer's probe found the original per-call Abandoned marker
        // regressed this: 100/100 for any unwind >= 1ms, 0/250 on the
        // pre-task-3a baseline. ct.IsCancellationRequested at write time is
        // NOT a safe discriminator (that race is exactly what broke
        // AWriteFailureIsReportedThroughOnError the first time); this test
        // is looped 50 times because the fix depends on a lock-based
        // happens-before edge, not a fixed delay, and a flaky ordering bug
        // would not necessarily show up on the first iteration.
        [Fact]
        public async Task AnOrdinaryLateReplyAfterTeardownNeverReportsAWriteFailure()
        {
            for (var iteration = 0; iteration < 50; iteration++)
            {
                // ConnectionCount alone is not a safe proxy for "the
                // tools/call handler has started" — see
                // AStragglerThatIgnoresItsTokenIsAbandonedAndReportedThroughOnError's
                // comment on the same pattern (fix round 1, M1 added an
                // extra await, CallSlots.WaitAsync, before a call is forked).
                var started = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
                var callCompleted = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
                var dispatcher = new FakeToolDispatcher
                {
                    OnCall = async (name, args, conn, ct) =>
                    {
                        started.TrySetResult(true);
                        try
                        {
                            await Task.Delay(Timeout.Infinite, ct);
                        }
                        catch (OperationCanceledException)
                        {
                            // The delay here is the behaviour under test — an
                            // ordinary unwind time — not a synchronisation
                            // sleep.
                            await Task.Delay(20);
                        }

                        callCompleted.TrySetResult(true);
                        return new ToolResult("cancelled-ordinarily", false);
                    },
                };
                var errors = new List<(string Context, Exception Exception)>();
                var (server, port) = await StartServerAsync(dispatcher);
                server.OnError = (ctx, ex) =>
                {
                    lock (errors)
                    {
                        errors.Add((ctx, ex));
                    }
                };
                await using var serverLifetime = server;

                var client = await ConnectAsync(port);
                var stream = client.GetStream();
                using var reader = new System.IO.StreamReader(stream, Encoding.UTF8);
                await InitializeAsync(stream, reader, Token);

                await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"slow-unwind\",\"arguments\":{}}}");
                var startedInTime = await Task.WhenAny(started.Task, Task.Delay(TimeSpan.FromSeconds(5))) == started.Task;
                Assert.True(startedInTime, $"iteration {iteration}: the tools/call handler never started");

                client.Close();

                var completedInTime = await Task.WhenAny(callCompleted.Task, Task.Delay(TimeSpan.FromSeconds(5))) == callCompleted.Task;
                Assert.True(completedInTime, $"iteration {iteration}: the call never completed");

                await WaitForAsync(() => dispatcher.ClosedConnections.Count == 1, TimeSpan.FromSeconds(5));
                Assert.Single(dispatcher.ClosedConnections);

                await WaitForAsync(() => server.ConnectionCount == 0, TimeSpan.FromSeconds(5));
                Assert.Equal(0, server.ConnectionCount);

                List<(string Context, Exception Exception)> errorsSnapshot;
                lock (errors)
                {
                    errorsSnapshot = new List<(string, Exception)>(errors);
                }

                Assert.True(errorsSnapshot.Count == 0, $"iteration {iteration}: OnError was invoked: {string.Join(", ", errorsSnapshot.Select(e => e.Context))}");
            }
        }

        // Fix round 1, I1: CancellationTokenSource.Cancel() rethrows
        // (aggregated) any exception a registered callback throws — a
        // callback registered by dispatcher/tool code, not by BridgeServer.
        // Unfenced, the reviewer's probe found this skipped the entire rest
        // of teardown: ConnectionClosed zero times, ConnectionCount stuck at
        // 1 forever, client.Close() and state.Dispose() never ran, nothing
        // reported.
        [Fact]
        public async Task AThrowingCancellationCallbackDoesNotSkipTeardown()
        {
            // ConnectionCount == 1 is true from the moment the TCP connect
            // is accepted, well before the tools/call handler below has
            // necessarily started running — waiting on it alone would race
            // client.Close() against ct.Register below actually registering
            // the callback. registered is the real synchronisation point.
            var registered = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
            var dispatcher = new FakeToolDispatcher
            {
                OnCall = async (name, args, conn, ct) =>
                {
                    ct.Register(() => throw new InvalidOperationException("boom-cancel"));
                    registered.TrySetResult(true);
                    try
                    {
                        await Task.Delay(Timeout.Infinite, ct);
                    }
                    catch (OperationCanceledException)
                    {
                    }

                    return new ToolResult("unreachable", false);
                },
            };
            var errors = new List<(string Context, Exception Exception)>();
            var (server, port) = await StartServerAsync(dispatcher);
            server.OnError = (ctx, ex) =>
            {
                lock (errors)
                {
                    errors.Add((ctx, ex));
                }
            };
            await using var serverLifetime = server;

            var client = await ConnectAsync(port);
            var stream = client.GetStream();
            using var reader = new System.IO.StreamReader(stream, Encoding.UTF8);
            await InitializeAsync(stream, reader, Token);

            await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"blocking\",\"arguments\":{}}}");
            var registeredInTime = await Task.WhenAny(registered.Task, Task.Delay(TimeSpan.FromSeconds(5))) == registered.Task;
            Assert.True(registeredInTime, "the cancellation callback was never registered");

            client.Close();

            await WaitForAsync(() => dispatcher.ClosedConnections.Count == 1, TimeSpan.FromSeconds(5));
            Assert.Single(dispatcher.ClosedConnections);

            await WaitForAsync(() => server.ConnectionCount == 0, TimeSpan.FromSeconds(5));
            Assert.Equal(0, server.ConnectionCount);

            List<(string Context, Exception Exception)> errorsSnapshot;
            lock (errors)
            {
                errorsSnapshot = new List<(string, Exception)>(errors);
            }

            Assert.Contains(errorsSnapshot, e => e.Context.IndexOf("cancel", StringComparison.OrdinalIgnoreCase) >= 0);
        }

        // Fix round 1, M4: the "a request with an id always gets a reply"
        // guard in ProcessLineAsync only covers the inline methods
        // (initialize, tools/list, errors) — tools/call runs on its own
        // task, unawaited there, so a fault in HandleToolsCallAsync's tail
        // (the JSON encode + write, outside CallAsync's own try) used to
        // fault the forked task silently: no reply for that id, no OnError,
        // since nothing awaits that task except TrackInFlight's fire-and-
        // forget continuation, which only observes the exception. Forces a
        // genuine fault in that tail deterministically: the dispatcher
        // returns a null ToolResult (a legal value for the CallAsync
        // signature — the try/catch around CallAsync itself only guards
        // against a THROWN exception, not a null return), so
        // `result.Text`/`result.IsError` in the payload-construction step
        // right after — which is the tail this finding is about, not
        // CallAsync itself — throws NullReferenceException. (An earlier
        // attempt using a lone UTF-16 surrogate in the text did not work:
        // System.Text.Json silently substitutes U+FFFD for it rather than
        // throwing, and a deliberately over-deep id fails to PARSE in the
        // first place, before ever reaching dispatch — there is no window
        // where encoding a legally-parsed id can fail on the reply side but
        // not the request side, since both wrap it in exactly one more
        // level of nesting.)
        [Fact]
        public async Task AFaultInTheReplyTailIsReportedAndStillGetsAReply()
        {
            var dispatcher = new FakeToolDispatcher
            {
                OnCall = (name, args, conn, ct) => Task.FromResult<ToolResult>(null!),
            };
            var errors = new List<(string Context, Exception Exception)>();
            var (server, port) = await StartServerAsync(dispatcher);
            server.OnError = (ctx, ex) =>
            {
                lock (errors)
                {
                    errors.Add((ctx, ex));
                }
            };
            await using var serverLifetime = server;

            using var client = await ConnectAsync(port);
            using var stream = client.GetStream();
            using var reader = new System.IO.StreamReader(stream, Encoding.UTF8);
            await InitializeAsync(stream, reader, Token);

            await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":50,\"method\":\"tools/call\",\"params\":{\"name\":\"null-result\",\"arguments\":{}}}");

            var line = await ReadLineWithTimeoutAsync(reader, ReplyTimeout);
            Assert.NotNull(line);
            var root = JsonDocument.Parse(line!).RootElement;
            Assert.Equal(50, root.GetProperty("id").GetInt32());
            Assert.Equal(-32603, root.GetProperty("error").GetProperty("code").GetInt32());

            List<(string Context, Exception Exception)> errorsSnapshot;
            lock (errors)
            {
                errorsSnapshot = new List<(string, Exception)>(errors);
            }

            Assert.Contains(errorsSnapshot, e => e.Context.IndexOf("tools/call reply", StringComparison.OrdinalIgnoreCase) >= 0);
        }

        // M10: the default JSON encoder escapes '<', '>', '&' and every
        // non-ASCII character as \uXXXX. Confirms the relaxed encoder is in
        // effect (content is readable on the wire, not just round-trippable
        // — \uXXXX also round-trips, so that alone would not catch a
        // regression back to the default encoder) and that this still frames
        // as exactly one line.
        [Fact]
        public async Task ReplyWithNonAsciiAndEmbeddedNewlineStaysOneLineAndRoundTrips()
        {
            const string text = "<é>\nmore text & more <tags>"; // "<é>\nmore text & more <tags>"
            var dispatcher = new FakeToolDispatcher
            {
                OnCall = (name, args, conn, ct) => Task.FromResult(new ToolResult(text, false)),
            };
            var (server, port) = await StartServerAsync(dispatcher);
            await using var serverLifetime = server;

            using var client = await ConnectAsync(port);
            using var stream = client.GetStream();
            using var reader = new System.IO.StreamReader(stream, Encoding.UTF8);
            await InitializeAsync(stream, reader, Token);

            await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":30,\"method\":\"tools/call\",\"params\":{\"name\":\"echo\",\"arguments\":{}}}");
            var line = await ReadLineWithTimeoutAsync(reader, ReplyTimeout);
            Assert.NotNull(line);

            Assert.Contains("<é>", line, StringComparison.Ordinal);
            Assert.DoesNotContain("\\u003C", line, StringComparison.OrdinalIgnoreCase);
            Assert.DoesNotContain("\\u00e9", line, StringComparison.OrdinalIgnoreCase);

            var decoded = JsonDocument.Parse(line!).RootElement
                .GetProperty("result").GetProperty("content")[0].GetProperty("text").GetString();
            Assert.Equal(text, decoded);

            // And it really is exactly one line: nothing more arrives.
            var extra = await Record.ExceptionAsync(async () => await ReadLineWithTimeoutAsync(reader, TimeSpan.FromMilliseconds(300)));
            Assert.IsType<TimeoutException>(extra);
        }
    }
}
