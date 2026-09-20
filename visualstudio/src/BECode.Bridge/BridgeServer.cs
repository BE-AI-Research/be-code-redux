using System;
using System.Collections.Generic;
using System.Net;
using System.Net.Sockets;
using System.Text.Json;
using System.Threading;
using System.Threading.Channels;
using System.Threading.Tasks;

namespace BECode.Bridge
{
    /// <summary>
    /// The wire: a TCP listener on 127.0.0.1, newline-delimited JSON-RPC 2.0
    /// per connection, token-gated by <c>initialize</c>. Tool behaviour is
    /// entirely delegated to <see cref="IToolDispatcher"/> — this class only
    /// knows how to frame, authenticate, order and reply.
    /// </summary>
    public sealed class BridgeServer : IAsyncDisposable
    {
        // A JsonElement default(JsonElement) has ValueKind Undefined and
        // cannot be queried; when a tools/call carries no "arguments", the
        // dispatcher is handed this empty object instead.
        private static readonly JsonElement EmptyArgs =
            JsonDocument.Parse("{}").RootElement.Clone();

        private readonly IToolDispatcher _tools;
        private readonly string _token;
        private readonly string _version;

        private readonly object _clientsLock = new object();
        private readonly List<TcpClient> _clients = new List<TcpClient>();

        private readonly object _tasksLock = new object();
        private readonly List<Task> _connectionTasks = new List<Task>();

        private TcpListener? _listener;
        private CancellationTokenSource? _cts;
        private Task? _acceptLoop;

        public BridgeServer(IToolDispatcher tools, string token, string version)
        {
            _tools = tools ?? throw new ArgumentNullException(nameof(tools));
            _token = token ?? throw new ArgumentNullException(nameof(token));
            _version = version ?? throw new ArgumentNullException(nameof(version));
        }

        public int ConnectionCount
        {
            get
            {
                lock (_clientsLock)
                {
                    return _clients.Count;
                }
            }
        }

        /// <summary>Starts listening on 127.0.0.1 and returns the bound port.</summary>
        public Task<int> StartAsync(int port = 0)
        {
            if (_listener != null)
            {
                throw new InvalidOperationException("BridgeServer is already started.");
            }

            _listener = new TcpListener(IPAddress.Loopback, port);
            _listener.Start();
            var boundPort = ((IPEndPoint)_listener.LocalEndpoint).Port;

            _cts = new CancellationTokenSource();
            _acceptLoop = AcceptLoopAsync(_listener, _cts.Token);
            return Task.FromResult(boundPort);
        }

        private async Task AcceptLoopAsync(TcpListener listener, CancellationToken ct)
        {
            while (!ct.IsCancellationRequested)
            {
                TcpClient client;
                try
                {
                    client = await listener.AcceptTcpClientAsync().ConfigureAwait(false);
                }
                catch (ObjectDisposedException)
                {
                    break;
                }
                catch (SocketException)
                {
                    break;
                }

                lock (_clientsLock)
                {
                    _clients.Add(client);
                }

                var task = HandleConnectionAsync(client, ct);
                lock (_tasksLock)
                {
                    _connectionTasks.Add(task);
                }
            }
        }

        private sealed class ConnectionState
        {
            // Authed is only ever touched by the single processor task.
            // ShouldClose is written there too but read from the read loop
            // on a different task, so it needs a visibility guarantee.
            public bool Authed;
            public volatile bool ShouldClose;
        }

        private async Task HandleConnectionAsync(TcpClient client, CancellationToken serverCt)
        {
            var stream = client.GetStream();
            var framer = new LineFramer();
            var writeLock = new SemaphoreSlim(1, 1);
            var state = new ConnectionState();

            // Requests on a connection are handled strictly in order: the
            // read loop only frames lines and enqueues them, a single
            // consumer task processes and replies to one at a time, so a
            // slow tool call never lets a later request's reply overtake it.
            var channel = Channel.CreateUnbounded<string>(new UnboundedChannelOptions
            {
                SingleReader = true,
                SingleWriter = true,
            });

            var processor = ProcessQueueAsync(channel.Reader, client, stream, writeLock, state, serverCt);

            try
            {
                var buffer = new byte[8192];
                while (!serverCt.IsCancellationRequested)
                {
                    int read;
                    try
                    {
                        read = await stream.ReadAsync(buffer, 0, buffer.Length, serverCt).ConfigureAwait(false);
                    }
                    catch (Exception)
                    {
                        break;
                    }

                    if (read == 0)
                    {
                        break;
                    }

                    foreach (var line in framer.Push(buffer.AsSpan(0, read)))
                    {
                        try
                        {
                            await channel.Writer.WriteAsync(line, serverCt).ConfigureAwait(false);
                        }
                        catch (Exception)
                        {
                            break;
                        }
                    }

                    if (state.ShouldClose)
                    {
                        break;
                    }
                }
            }
            finally
            {
                channel.Writer.TryComplete();
                try
                {
                    await processor.ConfigureAwait(false);
                }
                catch
                {
                    // processing must never bubble into connection teardown
                }

                lock (_clientsLock)
                {
                    _clients.Remove(client);
                }

                try
                {
                    _tools.ConnectionClosed(client);
                }
                catch
                {
                    // a tool's cleanup must not break the server
                }

                try
                {
                    client.Close();
                }
                catch
                {
                    // already closed
                }
            }
        }

        private async Task ProcessQueueAsync(
            ChannelReader<string> reader,
            TcpClient client,
            NetworkStream stream,
            SemaphoreSlim writeLock,
            ConnectionState state,
            CancellationToken ct)
        {
            try
            {
                while (await reader.WaitToReadAsync(ct).ConfigureAwait(false))
                {
                    while (reader.TryRead(out var line))
                    {
                        await ProcessLineAsync(line, client, stream, writeLock, state, ct).ConfigureAwait(false);
                        if (state.ShouldClose)
                        {
                            try
                            {
                                client.Close();
                            }
                            catch
                            {
                                // already closed
                            }

                            return;
                        }
                    }
                }
            }
            catch (OperationCanceledException)
            {
            }
        }

        private async Task ProcessLineAsync(
            string line,
            TcpClient client,
            NetworkStream stream,
            SemaphoreSlim writeLock,
            ConnectionState state,
            CancellationToken ct)
        {
            JsonDocument doc;
            try
            {
                doc = JsonDocument.Parse(line);
            }
            catch (JsonException)
            {
                return; // malformed frame: nothing sensible to reply to
            }

            using (doc)
            {
                var root = doc.RootElement;

                if (root.ValueKind != JsonValueKind.Object || !root.TryGetProperty("id", out var idEl)
                    || idEl.ValueKind == JsonValueKind.Null || idEl.ValueKind == JsonValueKind.Undefined)
                {
                    return; // no id means a notification: no reply
                }

                var id = idEl.Clone();

                var method = root.TryGetProperty("method", out var methodEl) && methodEl.ValueKind == JsonValueKind.String
                    ? methodEl.GetString() ?? ""
                    : "";

                JsonElement paramsElement = default;
                if (root.TryGetProperty("params", out var p))
                {
                    paramsElement = p.Clone();
                }

                if (!string.Equals(method, "initialize", StringComparison.Ordinal) && !state.Authed)
                {
                    await WriteErrorAsync(stream, writeLock, id, JsonRpcCodes.NotInitialized, "not initialized").ConfigureAwait(false);
                    return;
                }

                switch (method)
                {
                    case "initialize":
                        await HandleInitializeAsync(stream, writeLock, id, paramsElement, state).ConfigureAwait(false);
                        break;

                    case "tools/list":
                        await HandleToolsListAsync(stream, writeLock, id).ConfigureAwait(false);
                        break;

                    case "tools/call":
                        await HandleToolsCallAsync(stream, writeLock, id, paramsElement, client, ct).ConfigureAwait(false);
                        break;

                    default:
                        await WriteErrorAsync(stream, writeLock, id, JsonRpcCodes.MethodNotFound, $"unknown method {method}").ConfigureAwait(false);
                        break;
                }
            }
        }

        private async Task HandleInitializeAsync(
            NetworkStream stream,
            SemaphoreSlim writeLock,
            JsonElement id,
            JsonElement paramsElement,
            ConnectionState state)
        {
            string? token = null;
            if (paramsElement.ValueKind == JsonValueKind.Object
                && paramsElement.TryGetProperty("auth", out var authEl)
                && authEl.ValueKind == JsonValueKind.Object
                && authEl.TryGetProperty("token", out var tokenEl)
                && tokenEl.ValueKind == JsonValueKind.String)
            {
                token = tokenEl.GetString();
            }

            if (!string.Equals(token, _token, StringComparison.Ordinal))
            {
                await WriteErrorAsync(stream, writeLock, id, JsonRpcCodes.BadToken, "bad token").ConfigureAwait(false);
                state.ShouldClose = true;
                return;
            }

            state.Authed = true;
            var result = new
            {
                protocolVersion = "2024-11-05",
                capabilities = new { tools = new { } },
                serverInfo = new { name = "be-code-visualstudio", version = _version },
            };
            await WriteResultAsync(stream, writeLock, id, result).ConfigureAwait(false);
        }

        private async Task HandleToolsListAsync(NetworkStream stream, SemaphoreSlim writeLock, JsonElement id)
        {
            var tools = _tools.List();
            await WriteResultAsync(stream, writeLock, id, new { tools }).ConfigureAwait(false);
        }

        private async Task HandleToolsCallAsync(
            NetworkStream stream,
            SemaphoreSlim writeLock,
            JsonElement id,
            JsonElement paramsElement,
            TcpClient client,
            CancellationToken ct)
        {
            var name = "";
            var args = EmptyArgs;

            if (paramsElement.ValueKind == JsonValueKind.Object)
            {
                if (paramsElement.TryGetProperty("name", out var nameEl) && nameEl.ValueKind == JsonValueKind.String)
                {
                    name = nameEl.GetString() ?? "";
                }

                if (paramsElement.TryGetProperty("arguments", out var argsEl) && argsEl.ValueKind == JsonValueKind.Object)
                {
                    args = argsEl;
                }
            }

            ToolResult result;
            try
            {
                result = await _tools.CallAsync(name, args, client, ct).ConfigureAwait(false);
            }
            catch (Exception ex)
            {
                // A tool failure is isError:true, never a JSON-RPC error.
                result = new ToolResult($"{name}: {ex.Message}", true);
            }

            var payload = new
            {
                content = new[] { new { type = "text", text = result.Text } },
                isError = result.IsError,
            };
            await WriteResultAsync(stream, writeLock, id, payload).ConfigureAwait(false);
        }

        private static async Task WriteResultAsync(NetworkStream stream, SemaphoreSlim writeLock, JsonElement id, object? result)
        {
            var bytes = JsonRpcWriter.EncodeResult(id, result);
            await WriteBytesAsync(stream, writeLock, bytes).ConfigureAwait(false);
        }

        private static async Task WriteErrorAsync(NetworkStream stream, SemaphoreSlim writeLock, JsonElement id, int code, string message)
        {
            var bytes = JsonRpcWriter.EncodeError(id, code, message);
            await WriteBytesAsync(stream, writeLock, bytes).ConfigureAwait(false);
        }

        private static async Task WriteBytesAsync(NetworkStream stream, SemaphoreSlim writeLock, byte[] bytes)
        {
            await writeLock.WaitAsync().ConfigureAwait(false);
            try
            {
                await stream.WriteAsync(bytes, 0, bytes.Length).ConfigureAwait(false);
            }
            catch
            {
                // the client may already be gone; the read loop observes
                // the same disconnect and tears the connection down.
            }
            finally
            {
                writeLock.Release();
            }
        }

        public async ValueTask DisposeAsync()
        {
            if (_cts == null)
            {
                return;
            }

            _cts.Cancel();

            try
            {
                _listener?.Stop();
            }
            catch
            {
                // already stopped
            }

            List<TcpClient> clientsSnapshot;
            lock (_clientsLock)
            {
                clientsSnapshot = new List<TcpClient>(_clients);
            }

            foreach (var client in clientsSnapshot)
            {
                try
                {
                    client.Close();
                }
                catch
                {
                    // already closed
                }
            }

            if (_acceptLoop != null)
            {
                try
                {
                    await _acceptLoop.ConfigureAwait(false);
                }
                catch
                {
                    // teardown, not a reportable failure
                }
            }

            List<Task> tasksSnapshot;
            lock (_tasksLock)
            {
                tasksSnapshot = new List<Task>(_connectionTasks);
                _connectionTasks.Clear();
            }

            foreach (var task in tasksSnapshot)
            {
                try
                {
                    await task.ConfigureAwait(false);
                }
                catch
                {
                    // teardown, not a reportable failure
                }
            }

            _cts.Dispose();
            _cts = null;
        }
    }
}
