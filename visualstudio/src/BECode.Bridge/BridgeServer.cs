using System;
using System.Collections.Generic;
using System.IO;
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
        private readonly int _maxLineBytes;

        private readonly object _clientsLock = new object();
        private readonly List<TcpClient> _clients = new List<TcpClient>();

        private readonly object _tasksLock = new object();
        private readonly List<Task> _connectionTasks = new List<Task>();

        private TcpListener? _listener;
        private CancellationTokenSource? _cts;
        private Task? _acceptLoop;

        /// <param name="maxLineBytes">
        /// Forwarded to each connection's <see cref="LineFramer"/> as its
        /// pending-bytes cap (review round 1, I1). Additive trailing
        /// parameter — the three pinned constructor parameters are
        /// unchanged.
        /// </param>
        public BridgeServer(IToolDispatcher tools, string token, string version, int maxLineBytes = 16 * 1024 * 1024)
        {
            _tools = tools ?? throw new ArgumentNullException(nameof(tools));
            _token = token ?? throw new ArgumentNullException(nameof(token));
            _version = version ?? throw new ArgumentNullException(nameof(version));
            _maxLineBytes = maxLineBytes;
        }

        /// <summary>
        /// Review round 1, C1: an error surface for failures the server
        /// would otherwise only swallow — an exception that escapes request
        /// dispatch (e.g. <see cref="IToolDispatcher.List"/> throwing),
        /// <see cref="IToolDispatcher.ConnectionClosed"/> itself throwing,
        /// or a failed write. Invoking it is fenced: a throwing callback
        /// cannot break the server. Additive public member; nothing pinned
        /// changes shape.
        /// </summary>
        public Action<string, Exception>? OnError { get; set; }

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

                // M3 (review round 1): prune this bookkeeping entry as soon
                // as the connection's own task finishes, rather than
                // retaining one completed Task per connection for the
                // server's entire life.
                _ = task.ContinueWith(
                    finished =>
                    {
                        lock (_tasksLock)
                        {
                            _connectionTasks.Remove(finished);
                        }
                    },
                    CancellationToken.None,
                    TaskContinuationOptions.ExecuteSynchronously,
                    TaskScheduler.Default);
            }
        }

        /// <summary>
        /// Everything one connection needs: the socket, the framing and
        /// dispatch state, and the lock serialising writes to it. Review
        /// round 1 folded stream/writeLock/client/state (previously five
        /// separate parameters threaded through every handler) into this
        /// one object — see M11.
        /// </summary>
        private sealed class ConnectionState : IDisposable
        {
            public readonly TcpClient Client;
            public readonly NetworkStream Stream;
            public readonly SemaphoreSlim WriteLock = new SemaphoreSlim(1, 1);

            // Authed is only ever touched by the single processor task.
            // ShouldClose is written there too but read from the read loop
            // on a different task, so it needs a visibility guarantee.
            public bool Authed;
            public volatile bool ShouldClose;

            public ConnectionState(TcpClient client, NetworkStream stream)
            {
                Client = client;
                Stream = stream;
            }

            public void Dispose()
            {
                WriteLock.Dispose();
            }
        }

        private async Task HandleConnectionAsync(TcpClient client, CancellationToken serverCt)
        {
            // M2 (review round 1): everything that can throw — including
            // client.GetStream() itself — now happens inside the try, so a
            // failure here still reaches the finally below and removes the
            // client from _clients / calls ConnectionClosed, rather than
            // leaking the connection out of the bookkeeping entirely.
            ConnectionState? state = null;
            Channel<string>? channel = null;
            Task? processor = null;

            try
            {
                var stream = client.GetStream();
                state = new ConnectionState(client, stream);
                var framer = new LineFramer(_maxLineBytes);

                // Requests on a connection are handled strictly in order:
                // the read loop only frames lines and enqueues them, a
                // single consumer task processes and replies to one at a
                // time, so a slow tool call never lets a later request's
                // reply overtake it.
                channel = Channel.CreateUnbounded<string>(new UnboundedChannelOptions
                {
                    SingleReader = true,
                    SingleWriter = true,
                });

                processor = ProcessQueueAsync(channel.Reader, state, serverCt);

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

                    IEnumerable<string> lines;
                    try
                    {
                        lines = framer.Push(buffer.AsSpan(0, read));
                    }
                    catch (InvalidDataException ex)
                    {
                        // I1: pending bytes without a newline exceeded the
                        // cap. This runs before authentication, on
                        // attacker-controlled input, so close rather than
                        // let it grow further.
                        ReportError("line framer: pending bytes exceeded the cap", ex);
                        break;
                    }

                    foreach (var line in lines)
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
                if (channel != null)
                {
                    channel.Writer.TryComplete();
                }

                if (processor != null)
                {
                    try
                    {
                        await processor.ConfigureAwait(false);
                    }
                    catch
                    {
                        // processing must never bubble into connection teardown
                    }
                }

                lock (_clientsLock)
                {
                    _clients.Remove(client);
                }

                try
                {
                    _tools.ConnectionClosed(client);
                }
                catch (Exception ex)
                {
                    ReportError("IToolDispatcher.ConnectionClosed threw", ex);
                }

                try
                {
                    client.Close();
                }
                catch
                {
                    // already closed
                }

                // M4: dispose the per-connection SemaphoreSlim.
                state?.Dispose();
            }
        }

        private async Task ProcessQueueAsync(ChannelReader<string> reader, ConnectionState state, CancellationToken ct)
        {
            try
            {
                while (await reader.WaitToReadAsync(ct).ConfigureAwait(false))
                {
                    while (reader.TryRead(out var line))
                    {
                        await ProcessLineAsync(line, state, ct).ConfigureAwait(false);
                        if (state.ShouldClose)
                        {
                            try
                            {
                                state.Client.Close();
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

        private async Task ProcessLineAsync(string line, ConnectionState state, CancellationToken ct)
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
                    await WriteErrorAsync(state, id, JsonRpcCodes.NotInitialized, "not initialized").ConfigureAwait(false);
                    return;
                }

                try
                {
                    switch (method)
                    {
                        case "initialize":
                            await HandleInitializeAsync(state, id, paramsElement).ConfigureAwait(false);
                            break;

                        case "tools/list":
                            await HandleToolsListAsync(state, id).ConfigureAwait(false);
                            break;

                        case "tools/call":
                            await HandleToolsCallAsync(state, id, paramsElement, ct).ConfigureAwait(false);
                            break;

                        default:
                            await WriteErrorAsync(state, id, JsonRpcCodes.MethodNotFound, $"unknown method {method}").ConfigureAwait(false);
                            break;
                    }
                }
                catch (Exception ex)
                {
                    // C1: a request with an id must always get a reply, even
                    // when something above threw that nothing here
                    // anticipated (e.g. IToolDispatcher.List() throwing).
                    // Without this guard the exception faults the single
                    // processor task: the read loop stays parked in
                    // ReadAsync forever, no later request on this
                    // connection is ever answered, and ConnectionClosed
                    // never fires. Reply with an internal-error frame and
                    // keep the connection alive instead.
                    ReportError($"unhandled exception processing method '{method}'", ex);
                    await WriteErrorAsync(state, id, JsonRpcCodes.InternalError, ex.Message).ConfigureAwait(false);
                }
            }
        }

        private async Task HandleInitializeAsync(ConnectionState state, JsonElement id, JsonElement paramsElement)
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
                await WriteErrorAsync(state, id, JsonRpcCodes.BadToken, "bad token").ConfigureAwait(false);
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
            await WriteResultAsync(state, id, result).ConfigureAwait(false);
        }

        private async Task HandleToolsListAsync(ConnectionState state, JsonElement id)
        {
            var tools = _tools.List();
            await WriteResultAsync(state, id, new { tools }).ConfigureAwait(false);
        }

        private async Task HandleToolsCallAsync(ConnectionState state, JsonElement id, JsonElement paramsElement, CancellationToken ct)
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
                result = await _tools.CallAsync(name, args, state.Client, ct).ConfigureAwait(false);
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
            await WriteResultAsync(state, id, payload).ConfigureAwait(false);
        }

        private Task WriteResultAsync(ConnectionState state, JsonElement id, object? result)
        {
            var bytes = JsonRpcWriter.EncodeResult(id, result);
            return WriteBytesAsync(state, bytes);
        }

        private Task WriteErrorAsync(ConnectionState state, JsonElement id, int code, string message)
        {
            var bytes = JsonRpcWriter.EncodeError(id, code, message);
            return WriteBytesAsync(state, bytes);
        }

        private async Task WriteBytesAsync(ConnectionState state, byte[] bytes)
        {
            await state.WriteLock.WaitAsync().ConfigureAwait(false);
            try
            {
                await state.Stream.WriteAsync(bytes, 0, bytes.Length).ConfigureAwait(false);
            }
            catch (Exception ex)
            {
                // Review round 1: a failed write used to be swallowed
                // silently, leaving ShouldClose unset — later replies on
                // this connection would just keep failing into a dead
                // socket. Now: report it and stop processing further
                // requests on this connection (the ShouldClose check in
                // ProcessQueueAsync closes the socket right after).
                state.ShouldClose = true;
                ReportError("write failed", ex);
            }
            finally
            {
                state.WriteLock.Release();
            }
        }

        private void ReportError(string context, Exception ex)
        {
            try
            {
                OnError?.Invoke(context, ex);
            }
            catch
            {
                // a throwing error callback must not break the server
            }
        }

        public async ValueTask DisposeAsync()
        {
            // M4: idempotent under concurrent calls. Only the caller that
            // wins the exchange (sees the non-null CancellationTokenSource)
            // runs teardown; every other caller — concurrent or later —
            // observes null and returns immediately rather than risking an
            // NRE against a field another call already cleared.
            var cts = Interlocked.Exchange(ref _cts, null);
            if (cts == null)
            {
                return;
            }

            cts.Cancel();

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

            cts.Dispose();
        }
    }
}
