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

            public List<object> ClosedConnections { get; } = new List<object>();

            public IReadOnlyList<ToolInfo> List() => _tools;

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

        private static async Task<(BridgeServer server, int port)> StartServerAsync(IToolDispatcher? dispatcher = null)
        {
            var server = new BridgeServer(dispatcher ?? new FakeToolDispatcher(), Token, "1.0.0-test");
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

        [Fact]
        public async Task UnknownToolIsErrorWithUnknownToolMessage()
        {
            var (server, port) = await StartServerAsync(); // default dispatcher: everything is "unknown tool"
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

        [Fact]
        public async Task PipelinedCallsReplyInRequestOrderDespiteADelayedFirstCall()
        {
            var dispatcher = new FakeToolDispatcher
            {
                OnCall = async (name, args, conn, ct) =>
                {
                    if (name == "slow")
                    {
                        await Task.Delay(200, ct);
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

            // Both requests are written before either reply is read: this is
            // the pipelining the ordering guarantee has to survive.
            await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":10,\"method\":\"tools/call\",\"params\":{\"name\":\"slow\",\"arguments\":{}}}");
            await SendLineAsync(stream, "{\"jsonrpc\":\"2.0\",\"id\":11,\"method\":\"tools/call\",\"params\":{\"name\":\"fast\",\"arguments\":{}}}");

            var stopwatch = Stopwatch.StartNew();
            var firstLine = await ReadLineWithTimeoutAsync(reader, ReplyTimeout);
            var secondLine = await ReadLineWithTimeoutAsync(reader, ReplyTimeout);
            stopwatch.Stop();

            Assert.NotNull(firstLine);
            Assert.NotNull(secondLine);
            var firstRoot = JsonDocument.Parse(firstLine!).RootElement;
            var secondRoot = JsonDocument.Parse(secondLine!).RootElement;

            Assert.Equal(10, firstRoot.GetProperty("id").GetInt32());
            Assert.Equal(11, secondRoot.GetProperty("id").GetInt32());
            // The 200ms delay on the *first* call is the behaviour under
            // test, not a synchronisation sleep: it proves the second
            // request's processing genuinely waited for the first to finish
            // rather than the two racing to reply.
            Assert.True(stopwatch.ElapsedMilliseconds >= 180, $"expected the delayed first call to gate the second reply, elapsed={stopwatch.ElapsedMilliseconds}ms");
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
    }
}
