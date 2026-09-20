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

        /// <summary>
        /// Task 3a: how long connection teardown waits for in-flight
        /// <c>tools/call</c> tasks to finish, once the connection's token has
        /// already been cancelled, before abandoning them and reporting a
        /// straggler through <see cref="OnError"/> instead of blocking on it.
        /// Default 5 s per the wire contract; additive public member, tests
        /// may shorten it. None of the pinned signatures change.
        /// </summary>
        public TimeSpan InFlightDrainTimeout { get; set; } = TimeSpan.FromSeconds(5);

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
        /// Per-<c>tools/call</c> marker, created before the call's task
        /// starts and handed into it. Set by <see cref="DrainInFlightAsync"/>
        /// only for a task it gave up waiting on at connection teardown — a
        /// straggler that ignored its cancellation token. A call's own reply
        /// write checks this immediately before writing: once true, a write
        /// failure is the ordinary "client went away" case (requirement 4),
        /// not a reportable one. This is deliberately per-call rather than a
        /// single connection-wide flag: connection teardown (cancel token,
        /// <c>ConnectionClosed</c>, remove from <c>_clients</c>) can genuinely
        /// race, on a different thread, against an ordinary in-flight call
        /// that is about to fail its own write for an unrelated reason (e.g.
        /// <see cref="AWriteFailureIsReportedThroughOnError"/>-shaped tests,
        /// where the dispatcher itself closes the socket) — that failure must
        /// still be reported. Only a call actually abandoned past
        /// <see cref="InFlightDrainTimeout"/> is quiet.
        /// </summary>
        private sealed class InFlightCall
        {
            public volatile bool Abandoned;
        }

        /// <summary>
        /// Everything one connection needs: the socket, the framing and
        /// dispatch state, and the lock serialising writes to it. Review
        /// round 1 folded stream/writeLock/client/state (previously five
        /// separate parameters threaded through every handler) into this
        /// one object — see M11.
        ///
        /// Task 3a additions: <see cref="Cts"/> is this connection's own
        /// cancellation source, linked to the server's — it is the token
        /// handed to <see cref="IToolDispatcher.CallAsync"/>, so a socket
        /// close, a bad token, the line cap or server disposal can tell an
        /// in-flight <c>tools/call</c> without waiting for it. In-flight call
        /// tasks are tracked here (<see cref="TrackInFlight"/>), paired with
        /// their <see cref="InFlightCall"/> marker, and pruned as they
        /// complete, so teardown can await them bounded rather than forever.
        /// </summary>
        private sealed class ConnectionState : IDisposable
        {
            public readonly TcpClient Client;
            public readonly NetworkStream Stream;
            public readonly SemaphoreSlim WriteLock = new SemaphoreSlim(1, 1);
            public readonly CancellationTokenSource Cts;

            // Authed is only ever touched by the single processor task.
            // ShouldClose is written there too but read from the read loop
            // on a different task, so it needs a visibility guarantee.
            public bool Authed;
            public volatile bool ShouldClose;

            private readonly object _inFlightLock = new object();
            private readonly Dictionary<Task, InFlightCall> _inFlight = new Dictionary<Task, InFlightCall>();

            public ConnectionState(TcpClient client, NetworkStream stream, CancellationTokenSource cts)
            {
                Client = client;
                Stream = stream;
                Cts = cts;
            }

            /// <summary>
            /// Registers a forked <c>tools/call</c> task (paired with the
            /// <see cref="InFlightCall"/> marker already handed into it) and
            /// prunes it the instant it completes, so a long session never
            /// accumulates an unbounded list of finished calls.
            /// </summary>
            public void TrackInFlight(Task task, InFlightCall handle)
            {
                lock (_inFlightLock)
                {
                    _inFlight[task] = handle;
                }

                task.ContinueWith(
                    t =>
                    {
                        lock (_inFlightLock)
                        {
                            _inFlight.Remove(t);
                        }

                        if (t.IsFaulted)
                        {
                            // Observe it: HandleToolsCallAsync already
                            // catches everything it can, so this should
                            // never actually be faulted, but a stray fault
                            // here must not become an unobserved task
                            // exception.
                            _ = t.Exception;
                        }
                    },
                    CancellationToken.None,
                    TaskContinuationOptions.ExecuteSynchronously,
                    TaskScheduler.Default);
            }

            public List<(Task Task, InFlightCall Handle)> SnapshotInFlight()
            {
                lock (_inFlightLock)
                {
                    var snapshot = new List<(Task, InFlightCall)>(_inFlight.Count);
                    foreach (var pair in _inFlight)
                    {
                        snapshot.Add((pair.Key, pair.Value));
                    }

                    return snapshot;
                }
            }

            public void Dispose()
            {
                WriteLock.Dispose();
                Cts.Dispose();
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

                // Task 3a / R-7: this connection's own cancellation source,
                // linked to the server's — cancelling either cancels it. It
                // is the token IToolDispatcher.CallAsync receives, so a
                // socket close, a bad token, the line cap or server disposal
                // can tell an in-flight tools/call without waiting for it.
                var cts = CancellationTokenSource.CreateLinkedTokenSource(serverCt);
                state = new ConnectionState(client, stream, cts);
                var framer = new LineFramer(_maxLineBytes);

                // Requests are READ and DISPATCHED in order: the read loop
                // only frames lines and enqueues them, and a single consumer
                // task (ProcessQueueAsync) works through them one at a time
                // in order. initialize/tools/list/errors are handled inline
                // there and so still reply in order too. tools/call is the
                // one exception (R-7): it is dispatched in order but RUNS on
                // its own task, so its reply is written whenever that task
                // completes — a slow tools/call no longer delays a later
                // request's reply on the same connection. Writes stay
                // serialised by the per-connection write lock, so frames
                // never interleave regardless.
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
                // Task 3a / requirement 2 and 3: cancel the connection's
                // token FIRST — before anything else — so any in-flight
                // tools/call is told immediately, on every path that reaches
                // here (normal disconnect, bad token, line cap, a write
                // failure, or the server disposing with this connection
                // still live).
                state?.Cts.Cancel();

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

                // ConnectionClosed exactly once, before the in-flight drain
                // below — teardown must not wait for a pending tool call to
                // finish before telling the dispatcher the connection is
                // gone.
                try
                {
                    _tools.ConnectionClosed(client);
                }
                catch (Exception ex)
                {
                    ReportError("IToolDispatcher.ConnectionClosed threw", ex);
                }

                lock (_clientsLock)
                {
                    _clients.Remove(client);
                }

                try
                {
                    client.Close();
                }
                catch
                {
                    // already closed
                }

                // Only now — after ConnectionClosed and removal from
                // _clients, both of which must not wait on it — await any
                // still-running tools/call tasks, bounded so a call that
                // ignores its token cannot hang this forever.
                if (state != null)
                {
                    await DrainInFlightAsync(state).ConfigureAwait(false);
                }

                // M4: dispose the per-connection SemaphoreSlim (and, task
                // 3a, the connection's CancellationTokenSource) only after
                // the in-flight tasks above are done or abandoned.
                state?.Dispose();
            }
        }

        /// <summary>
        /// Task 3a, requirement 3: waits for a connection's in-flight
        /// tools/call tasks up to <see cref="InFlightDrainTimeout"/> (the
        /// token was already cancelled by the caller before this runs, so a
        /// well-behaved call should already be unwinding). A call that
        /// ignores its token and is still running when the bound elapses is
        /// abandoned — not awaited further — and reported once through
        /// <see cref="OnError"/> as a straggler, rather than blocking
        /// connection teardown (and, transitively, <see cref="DisposeAsync"/>)
        /// on it.
        /// </summary>
        private async Task DrainInFlightAsync(ConnectionState state)
        {
            var stragglers = state.SnapshotInFlight();
            if (stragglers.Count == 0)
            {
                return;
            }

            var stragglerTasks = new Task[stragglers.Count];
            for (var i = 0; i < stragglers.Count; i++)
            {
                stragglerTasks[i] = stragglers[i].Task;
            }

            var allTask = Task.WhenAll(stragglerTasks);
            var timeoutTask = Task.Delay(InFlightDrainTimeout);
            var completed = await Task.WhenAny(allTask, timeoutTask).ConfigureAwait(false);

            if (completed != allTask)
            {
                // Mark only the ones still actually running: a call that
                // ignored its token and is still going when the bound
                // elapses. Its own eventual write (if any) checks this marker
                // and drops itself quietly instead of reporting through
                // OnError — "a client that went away is ordinary".
                foreach (var straggler in stragglers)
                {
                    if (!straggler.Task.IsCompleted)
                    {
                        straggler.Handle.Abandoned = true;
                    }
                }

                ReportError(
                    $"in-flight tool call(s) did not complete within {InFlightDrainTimeout}; abandoning at connection teardown",
                    new TimeoutException("in-flight tools/call task(s) ignored connection cancellation"));

                // Don't block on the stragglers, but don't leave their
                // eventual faults unobserved either.
                _ = allTask.ContinueWith(
                    t => { _ = t.Exception; },
                    CancellationToken.None,
                    TaskContinuationOptions.OnlyOnFaulted | TaskContinuationOptions.ExecuteSynchronously,
                    TaskScheduler.Default);
                return;
            }

            try
            {
                await allTask.ConfigureAwait(false);
            }
            catch
            {
                // Each call already handled its own exception internally
                // (HandleToolsCallAsync never lets one escape); this is a
                // last-resort guard, not a reportable teardown failure.
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
                        await ProcessLineAsync(line, state).ConfigureAwait(false);
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

        private async Task ProcessLineAsync(string line, ConnectionState state)
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
                            // R-7: dispatched in order (we reach this line in
                            // strict per-connection order, same as every
                            // other case), but NOT awaited here — it runs on
                            // its own task and replies whenever that task
                            // completes, so a slow call never delays a later
                            // request's reply on this connection. The
                            // connection's own token (not the server's) is
                            // what IToolDispatcher.CallAsync receives, and
                            // the task is tracked (with its own InFlightCall
                            // marker) so teardown can await it bounded
                            // instead of forever.
                            var callHandle = new InFlightCall();
                            var callTask = HandleToolsCallAsync(state, id, paramsElement, state.Cts.Token, callHandle);
                            state.TrackInFlight(callTask, callHandle);
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
                    // keep the connection alive instead. (tools/call itself
                    // can no longer land here since it isn't awaited above —
                    // any exception from it is handled inside its own task,
                    // by HandleToolsCallAsync.)
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

        private async Task HandleToolsCallAsync(ConnectionState state, JsonElement id, JsonElement paramsElement, CancellationToken ct, InFlightCall handle)
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
            catch (OperationCanceledException) when (ct.IsCancellationRequested)
            {
                // Task 3a, requirement 6: the connection's own token fired —
                // this connection is tearing down. No reply, no OnError: a
                // cancelled in-flight call is expected, not a failure.
                return;
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

            // Task 3a, requirement 4: checked right before writing, not at
            // dispatch time — a call only ever becomes Abandoned once
            // DrainInFlightAsync gives up on it at connection teardown, long
            // after any ordinary (non-straggler) call would already have
            // written its reply. See InFlightCall's own comment for why this
            // is per-call rather than a single connection-wide flag.
            await WriteResultAsync(state, id, payload, quiet: handle.Abandoned).ConfigureAwait(false);
        }

        private Task WriteResultAsync(ConnectionState state, JsonElement id, object? result, bool quiet = false)
        {
            var bytes = JsonRpcWriter.EncodeResult(id, result);
            return WriteBytesAsync(state, bytes, quiet);
        }

        private Task WriteErrorAsync(ConnectionState state, JsonElement id, int code, string message)
        {
            var bytes = JsonRpcWriter.EncodeError(id, code, message);
            return WriteBytesAsync(state, bytes);
        }

        private async Task WriteBytesAsync(ConnectionState state, byte[] bytes, bool quiet = false)
        {
            try
            {
                await state.WriteLock.WaitAsync().ConfigureAwait(false);
            }
            catch (ObjectDisposedException)
            {
                // Task 3a, requirement 4: teardown already disposed the
                // write lock. Structurally this can only happen to a call
                // DrainInFlightAsync already gave up on (the lock is only
                // disposed, in ConnectionState.Dispose, after that drain
                // completes or times out) — drop it quietly, no OnError. "A
                // client that went away is ordinary."
                return;
            }

            try
            {
                await state.Stream.WriteAsync(bytes, 0, bytes.Length).ConfigureAwait(false);
            }
            catch (Exception ex)
            {
                if (quiet)
                {
                    // Task 3a, requirement 4: this reply belongs to a call
                    // DrainInFlightAsync already abandoned at connection
                    // teardown — a late write losing the race against a
                    // closed/disposed stream is ordinary, not reportable.
                    return;
                }

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
                try
                {
                    state.WriteLock.Release();
                }
                catch (ObjectDisposedException)
                {
                    // Teardown disposed the lock while this write was in
                    // flight (a straggler); nothing left to release.
                }
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
