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

            await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"x\",\"arguments\":{}}}");
            var first = await ReadLineWithTimeoutAsync(reader, ReplyTimeout);
            Assert.NotNull(first);
            var firstRoot = JsonDocument.Parse(first!).RootElement;
            Assert.Equal(-32002, firstRoot.GetProperty("error").GetProperty("code").GetInt32());

            var initRoot = await InitializeAsync(stream, reader, Token, id: 2);
            Assert.False(initRoot.TryGetProperty("error", out _));
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
            var gate = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
            var callReturned = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
            var dispatcher = new FakeToolDispatcher
            {
                OnCall = async (name, args, conn, ct) =>
                {
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

            var client = await ConnectAsync(port);
            var stream = client.GetStream();
            using var reader = new System.IO.StreamReader(stream, Encoding.UTF8);
            await InitializeAsync(stream, reader, Token);

            await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"stubborn\",\"arguments\":{}}}");
            await WaitForAsync(() => server.ConnectionCount == 1, TimeSpan.FromSeconds(5));

            client.Close();

            await WaitForAsync(() => dispatcher.ClosedConnections.Count == 1, TimeSpan.FromSeconds(5));
            Assert.Single(dispatcher.ClosedConnections);
            await WaitForAsync(() => server.ConnectionCount == 0, TimeSpan.FromSeconds(5));

            await WaitForAsync(
                () => errors.Any(e => e.Context.IndexOf("in-flight", StringComparison.OrdinalIgnoreCase) >= 0),
                TimeSpan.FromSeconds(5));

            var disposeTask = server.DisposeAsync().AsTask();
            var completed = await Task.WhenAny(disposeTask, Task.Delay(TimeSpan.FromSeconds(5)));
            Assert.Same(disposeTask, completed);
            await disposeTask;

            // Only now let the abandoned call finish — proving it does not
            // hang or throw once teardown has already moved on without it.
            gate.TrySetResult(true);
            var completedCall = await Task.WhenAny(callReturned.Task, Task.Delay(TimeSpan.FromSeconds(5)));
            Assert.Same(callReturned.Task, completedCall);
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

            Assert.Contains(errors, e => e.Exception.Message == "boom-list");
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

            await WaitForAsync(() => errors.Count > 0, TimeSpan.FromSeconds(5));
            Assert.Contains(errors, e => e.Exception is System.IO.InvalidDataException);
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

            await WaitForAsync(() => errors.Any(e => e.Context.Contains("write", StringComparison.OrdinalIgnoreCase)), TimeSpan.FromSeconds(5));
            Assert.Contains(errors, e => e.Context.Contains("write", StringComparison.OrdinalIgnoreCase));
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
