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
        /// pending-bytes cap. Additive trailing
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
        /// An error surface for failures the server
        /// would otherwise only swallow — an exception that escapes request
        /// dispatch (e.g. <see cref="IToolDispatcher.List"/> throwing),
        /// <see cref="IToolDispatcher.ConnectionClosed"/> itself throwing,
        /// or a failed write. Invoking it is fenced: a throwing callback
        /// cannot break the server. Additive public member; nothing pinned
        /// changes shape.
        /// </summary>
        public Action<string, Exception>? OnError { get; set; }

        /// <summary>
        /// How long connection teardown waits for in-flight
        /// <c>tools/call</c> tasks to finish, once the connection's token has
        /// already been cancelled, before abandoning them and reporting a
        /// straggler through <see cref="OnError"/> instead of blocking on it.
        /// Default 5 s per the wire contract. Internal, not
        /// public — a caller has no business tuning this outside a test —
        /// visible to BECode.Bridge.Tests via the assembly's
        /// InternalsVisibleTo (AssemblyInfo.cs). None of the pinned
        /// signatures change.
        /// </summary>
        internal TimeSpan InFlightDrainTimeout { get; set; } = TimeSpan.FromSeconds(5);

        /// <summary>
        /// How long connection teardown will wait to
        /// acquire the per-connection write lock (to set
        /// <c>ConnectionState.TearingDown</c>) before giving up and closing
        /// the socket itself. A write already holding the lock ordinarily
        /// releases it in microseconds; the only realistic way to hold it
        /// this long is a write blocked on TCP backpressure from a dead or
        /// unresponsive peer, which will otherwise never clear. Internal and
        /// settable so a test can shorten it; default 1 s.
        /// </summary>
        internal TimeSpan WriteLockTeardownTimeout { get; set; } = TimeSpan.FromSeconds(1);

        /// <summary>
        /// The most <c>tools/call</c>
        /// dispatcher calls one connection may have running at once. Without
        /// this cap, a client that pipelines many
        /// thousands of <c>tools/call</c> lines back to back would get
        /// that many live dispatcher calls at once. The ORDERED processor
        /// (ProcessLineAsync) acquires one slot per call before forking it,
        /// so at the cap the connection simply stops dispatching further
        /// lines until a call finishes and frees a slot — back-pressure, not
        /// an error. Internal and settable so a test can shrink it; default
        /// 64.
        /// </summary>
        internal int MaxConcurrentCallsPerConnection { get; set; } = 64;

        /// <summary>
        /// Test-only hook invoked once every forked
        /// <c>tools/call</c> task — including its own reply write attempt,
        /// quiet or not — has fully settled. Lets a test replace a fixed
        /// delay ("give the write path a moment to run") with a genuine
        /// deterministic signal instead of guessing how long is enough.
        /// Fenced like OnError: a throwing hook cannot break the server.
        /// </summary>
        internal Action? OnCallSettled { get; set; }

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

                // Prune this bookkeeping entry as soon
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
        /// dispatch state, and the lock serialising writes to it. Folds
        /// stream/writeLock/client/state (previously five
        /// separate parameters threaded through every handler) into this
        /// one object.
        ///
        /// <see cref="Cts"/> is this connection's own
        /// cancellation source, linked to the server's — it is the token
        /// handed to <see cref="IToolDispatcher.CallAsync"/>, so a socket
        /// close, a bad token, the line cap or server disposal can tell an
        /// in-flight <c>tools/call</c> without waiting for it. In-flight call
        /// tasks are tracked here (<see cref="TrackInFlight"/>) and pruned as
        /// they complete, so teardown can await them bounded rather than
        /// forever.
        ///
        /// <see cref="TearingDown"/> is not a plain flag read with no
        /// synchronisation at write time, because that is exactly the race
        /// that would make an ORDINARY unwind (a call that honours its
        /// token, then takes a little real time before returning a normal
        /// result — what <c>ReviewTools.ReviewDiff</c> actually does)
        /// spuriously report <c>OnError("write failed")</c>: teardown could
        /// set the flag on a different thread before the ordinary write's
        /// own exception handler ever read it. Instead it is only
        /// ever read and written while holding <see cref="WriteLock"/> —
        /// the one place with a genuine happens-before edge — so
        /// ANY write attempted
        /// after teardown has set it, whether from a straggler
        /// <see cref="DrainInFlightAsync"/> already gave up on or from an
        /// ordinary call that simply finished a little late, is provably
        /// post-teardown and drops itself quietly.
        /// </summary>
        private sealed class ConnectionState : IDisposable
        {
            public readonly TcpClient Client;
            public readonly NetworkStream Stream;
            public readonly SemaphoreSlim WriteLock = new SemaphoreSlim(1, 1);
            public readonly CancellationTokenSource Cts;

            // One ticket per in-flight tools/call, sized by
            // the server's MaxConcurrentCallsPerConnection. Acquired by the
            // ordered processor before forking a call's task, released once
            // that task completes (see TrackInFlight below) — at the cap,
            // acquiring simply blocks the processor from dispatching further
            // lines until a slot frees up.
            public readonly SemaphoreSlim CallSlots;

            // Authed is only ever touched by the single processor task.
            // ShouldClose is written there too but read from the read loop
            // on a different task, so it needs a visibility guarantee.
            public bool Authed;
            public volatile bool ShouldClose;

            // Read and written only while holding
            // WriteLock (see the class comment above for why that matters);
            // volatile anyway so the one unlocked write in
            // MarkTearingDownAsync's dead-peer fallback is still visible.
            public volatile bool TearingDown;

            private readonly object _inFlightLock = new object();
            private readonly HashSet<Task> _inFlight = new HashSet<Task>();

            public ConnectionState(TcpClient client, NetworkStream stream, CancellationTokenSource cts, int maxConcurrentCalls)
            {
                Client = client;
                Stream = stream;
                Cts = cts;
                CallSlots = new SemaphoreSlim(maxConcurrentCalls, maxConcurrentCalls);
            }

            /// <summary>
            /// Registers a forked <c>tools/call</c> task and prunes it the
            /// instant it completes, so a long session never accumulates an
            /// unbounded list of finished calls. Also releases the
            /// <see cref="CallSlots"/> ticket the caller acquired before
            /// forking this task — every path into TrackInFlight has already
            /// acquired exactly one, so releasing here, once, on every
            /// completion (success, fault or cancellation) is exact.
            /// </summary>
            public void TrackInFlight(Task task)
            {
                lock (_inFlightLock)
                {
                    _inFlight.Add(task);
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

                        try
                        {
                            CallSlots.Release();
                        }
                        catch (ObjectDisposedException)
                        {
                            // An abandoned straggler finishing after
                            // teardown disposed the semaphore: there is no
                            // slot left to give back. Nothing awaits this
                            // continuation, so letting it throw would only
                            // surface on the finalizer thread.
                        }
                    },
                    CancellationToken.None,
                    TaskContinuationOptions.ExecuteSynchronously,
                    TaskScheduler.Default);
            }

            public List<Task> SnapshotInFlight()
            {
                lock (_inFlightLock)
                {
                    return new List<Task>(_inFlight);
                }
            }

            public void Dispose()
            {
                WriteLock.Dispose();
                Cts.Dispose();
                CallSlots.Dispose();
            }
        }

        private async Task HandleConnectionAsync(TcpClient client, CancellationToken serverCt)
        {
            // Everything that can throw — including
            // client.GetStream() itself — happens inside the try, so a
            // failure here still reaches the finally below and removes the
            // client from _clients / calls ConnectionClosed, rather than
            // leaking the connection out of the bookkeeping entirely.
            ConnectionState? state = null;
            Channel<string>? channel = null;
            Task? processor = null;

            try
            {
                var stream = client.GetStream();

                // This connection's own cancellation source,
                // linked to the server's — cancelling either cancels it. It
                // is the token IToolDispatcher.CallAsync receives, so a
                // socket close, a bad token, the line cap or server disposal
                // can tell an in-flight tools/call without waiting for it.
                var cts = CancellationTokenSource.CreateLinkedTokenSource(serverCt);
                state = new ConnectionState(client, stream, cts, MaxConcurrentCallsPerConnection);
                var framer = new LineFramer(_maxLineBytes);

                // Requests are READ and DISPATCHED in order: the read loop
                // only frames lines and enqueues them, and a single consumer
                // task (ProcessQueueAsync) works through them one at a time
                // in order. initialize/tools/list/errors are handled inline
                // there and so still reply in order too. tools/call is the
                // one exception: it is dispatched in order but RUNS on
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
                        // Pending bytes without a newline exceeded the
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
                // Cancel the connection's
                // token FIRST — before anything else — so any in-flight
                // tools/call is told immediately, on every path that reaches
                // here (normal disconnect, bad token, line cap, a write
                // failure, or the server disposing with this connection
                // still live).
                //
                // Fenced: CancellationTokenSource.Cancel()
                // rethrows (aggregated) any exception a registered callback
                // throws — callbacks registered by dispatcher/tool code, not
                // by us. Left unfenced, a throwing callback would skip every
                // remaining teardown step below: ConnectionClosed would never
                // fire, the client would never be removed from _clients (so
                // ConnectionCount stays stuck), client.Close() and
                // state.Dispose() would never run, and nothing would be
                // reported.
                if (state != null)
                {
                    try
                    {
                        state.Cts.Cancel();
                    }
                    catch (Exception ex)
                    {
                        ReportError("cancelling the connection token threw", ex);
                    }
                }

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

                // Mark the connection as tearing down —
                // under the write lock — BEFORE closing the socket, so any
                // write racing this point resolves deterministically. See
                // MarkTearingDownAsync and ConnectionState.TearingDown's own
                // comments for the full reasoning.
                if (state != null)
                {
                    await MarkTearingDownAsync(state).ConfigureAwait(false);
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

                // Dispose the per-connection SemaphoreSlim (and
                // the connection's CancellationTokenSource) only after
                // the in-flight tasks above are done or abandoned.
                state?.Dispose();
            }
        }

        /// <summary>
        /// The one place with a genuine happens-before edge
        /// for "has this connection's teardown begun" is the write lock
        /// itself, not a bare flag read with no synchronisation (a bare flag
        /// with no lock is racy: see ConnectionState's class comment).
        /// Acquires WriteLock, sets
        /// <see cref="ConnectionState.TearingDown"/>, releases — the caller
        /// (HandleConnectionAsync's finally) does this BEFORE closing the
        /// socket. A write that already holds the lock when this is called
        /// finishes — or fails and reports — first, like any ordinary
        /// write; a write that only acquires the lock after this releases it
        /// is provably post-teardown, and WriteBytesAsync (which checks
        /// TearingDown itself immediately after acquiring the same lock)
        /// drops it quietly without ever touching the stream.
        ///
        /// Bounded: a write already in flight against a dead/unresponsive
        /// peer can hold the lock indefinitely — blocked on TCP backpressure
        /// that will never clear, since an ordinary write to a healthy
        /// socket releases the lock in microseconds. If the lock is not free
        /// within <see cref="WriteLockTeardownTimeout"/>, close the socket
        /// now instead of waiting longer: this faults the stuck write, which
        /// reports through its own ordinary "write failed" path exactly as
        /// it always has and releases the lock in its own finally, and set
        /// the flag without the lock — TearingDown is volatile, so the write
        /// is still visible to whichever thread reads it next.
        /// </summary>
        private async Task MarkTearingDownAsync(ConnectionState state)
        {
            bool acquired;
            try
            {
                acquired = await state.WriteLock.WaitAsync(WriteLockTeardownTimeout).ConfigureAwait(false);
            }
            catch (ObjectDisposedException)
            {
                // Nothing else disposes this before teardown reaches here,
                // but tolerate it rather than assume.
                state.TearingDown = true;
                return;
            }

            if (acquired)
            {
                try
                {
                    state.TearingDown = true;
                }
                finally
                {
                    state.WriteLock.Release();
                }

                return;
            }

            // Gave up waiting for the lock: almost certainly a write stuck
            // on a dead peer. Close the socket to unblock it — its own
            // WriteBytesAsync catch/finally then reports the failure and
            // releases the lock — rather than let teardown hang here.
            try
            {
                state.Client.Close();
            }
            catch
            {
                // already closed
            }

            state.TearingDown = true;
        }

        /// <summary>
        /// Waits for a connection's in-flight
        /// tools/call tasks up to <see cref="InFlightDrainTimeout"/> (the
        /// token was already cancelled by the caller before this runs, so a
        /// well-behaved call should already be unwinding). A call that
        /// ignores its token and is still running when the bound elapses is
        /// abandoned — not awaited further — and reported once through
        /// <see cref="OnError"/> as a straggler, rather than blocking
        /// connection teardown (and, transitively, <see cref="DisposeAsync"/>)
        /// on it. Marks nothing on the straggler
        /// itself — <see cref="MarkTearingDownAsync"/> (called before this,
        /// in HandleConnectionAsync's finally) already told WriteBytesAsync
        /// every subsequent write on this connection is quiet, straggler or
        /// not.
        /// </summary>
        private async Task DrainInFlightAsync(ConnectionState state)
        {
            var stragglers = state.SnapshotInFlight();
            if (stragglers.Count == 0)
            {
                return;
            }

            var stragglerTasks = stragglers.ToArray();
            var allTask = Task.WhenAll(stragglerTasks);
            var timeoutTask = Task.Delay(InFlightDrainTimeout);
            var completed = await Task.WhenAny(allTask, timeoutTask).ConfigureAwait(false);

            if (completed != allTask)
            {
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
                            // The ORDERED
                            // processor acquires a call slot before forking
                            // — at MaxConcurrentCallsPerConnection already
                            // in flight, this simply blocks (back-pressure,
                            // no error) until one finishes and releases its
                            // slot (TrackInFlight). The acquire observes the
                            // connection's own token so teardown can never
                            // hang waiting on it.
                            try
                            {
                                await state.CallSlots.WaitAsync(state.Cts.Token).ConfigureAwait(false);
                            }
                            catch (OperationCanceledException) when (state.Cts.IsCancellationRequested)
                            {
                                // Connection tearing down while waiting for a
                                // slot: the request is simply abandoned, same
                                // as any other in-flight call caught by
                                // cancellation — no reply, no OnError.
                                break;
                            }

                            // Dispatched in order (we reach this line in
                            // strict per-connection order, same as every
                            // other case), but NOT awaited here — it runs on
                            // its own task and replies whenever that task
                            // completes, so a slow call never delays a later
                            // request's reply on this connection. The
                            // connection's own token (not the server's) is
                            // what IToolDispatcher.CallAsync receives, and
                            // the task is tracked so teardown can await it
                            // bounded instead of forever.
                            var callTask = HandleToolsCallAsync(state, id, paramsElement, state.Cts.Token);
                            state.TrackInFlight(callTask);

                            // Fires after the call's own reply write
                            // (quiet or not) has been attempted, not just
                            // after CallAsync itself returns — see
                            // OnCallSettled's own comment.
                            if (OnCallSettled != null)
                            {
                                _ = callTask.ContinueWith(
                                    _ => ReportCallSettled(),
                                    CancellationToken.None,
                                    TaskContinuationOptions.ExecuteSynchronously,
                                    TaskScheduler.Default);
                            }

                            break;

                        default:
                            await WriteErrorAsync(state, id, JsonRpcCodes.MethodNotFound, $"unknown method {method}").ConfigureAwait(false);
                            break;
                    }
                }
                catch (Exception ex)
                {
                    // A request with an id must always get a reply, even
                    // when something above threw that nothing here
                    // anticipated (e.g. IToolDispatcher.List() throwing).
                    // Without this guard the exception faults the single
                    // processor task: the read loop stays parked in
                    // ReadAsync forever, no later request on this
                    // connection is ever answered, and ConnectionClosed
                    // never fires. Reply with an internal-error frame and
                    // keep the connection alive instead. (tools/call itself
                    // cannot land here since it isn't awaited above —
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
            catch (OperationCanceledException) when (ct.IsCancellationRequested)
            {
                // The connection's own token fired —
                // this connection is tearing down. No reply, no OnError: a
                // cancelled in-flight call is expected, not a failure.
                return;
            }
            catch (Exception ex)
            {
                // A tool failure is isError:true, never a JSON-RPC error.
                result = new ToolResult($"{name}: {ex.Message}", true);
            }

            // The "a request with an id always gets a
            // reply" guard in ProcessLineAsync only covers the inline
            // methods (initialize, tools/list, errors) — tools/call runs on
            // its own task, never awaited there, so a fault in this tail
            // (the encode + write, outside CallAsync's own try above), left
            // unguarded, would fault the forked task silently: no reply for
            // this id, and no OnError, since nothing awaits this task except
            // TrackInFlight's fire-and-forget continuation, which only
            // observes the exception. Wrapped the same way ProcessLineAsync
            // wraps its own inline methods.
            try
            {
                var payload = new
                {
                    content = new[] { new { type = "text", text = result.Text } },
                    isError = result.IsError,
                };

                await WriteResultAsync(state, id, payload).ConfigureAwait(false);
            }
            catch (Exception ex)
            {
                ReportError("unhandled exception writing tools/call reply", ex);
                try
                {
                    await WriteErrorAsync(state, id, JsonRpcCodes.InternalError, ex.Message).ConfigureAwait(false);
                }
                catch
                {
                    // WriteErrorAsync already reports its own write
                    // failures via WriteBytesAsync; nothing more to add.
                }
            }
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
            try
            {
                await state.WriteLock.WaitAsync().ConfigureAwait(false);
            }
            catch (ObjectDisposedException)
            {
                // Teardown already disposed the write lock (only happens
                // after MarkTearingDownAsync has already set TearingDown and
                // DrainInFlightAsync has finished or given up) — drop it
                // quietly, no OnError. "A client that went away is
                // ordinary."
                return;
            }

            Exception? writeFailure = null;
            try
            {
                // Checked immediately after acquiring the
                // lock, before ever touching the stream — this is the one
                // place with a genuine happens-before edge against
                // MarkTearingDownAsync (see its own comment, and
                // ConnectionState.TearingDown's). A write that reaches this
                // point holding the lock either started before teardown
                // marked it (TearingDown still false: proceed exactly as
                // below, ordinary success or ordinary reported failure) or
                // only got the lock after teardown released it with the
                // flag already set (provably post-teardown: drop quietly
                // without touching the stream at all).
                if (state.TearingDown)
                {
                    return;
                }

                await state.Stream.WriteAsync(bytes, 0, bytes.Length).ConfigureAwait(false);
            }
            catch (Exception ex)
            {
                // A failed write must be reported and ShouldClose set:
                // left unset, later replies on
                // this connection would just keep failing into a dead
                // socket.
                //
                // ShouldClose alone only closes the socket
                // the next time ProcessQueueAsync happens to check it, after
                // processing another line — but tools/call's write
                // happens on its own forked task, so if the client
                // never sends anything else, nothing ever rechecks the flag
                // and the connection lingers with a dead write forever.
                // Close the socket directly instead: this faults the read
                // loop's pending ReadAsync and runs the ordinary teardown
                // (ConnectionClosed exactly once, same as any other
                // disconnect), promptly, without waiting for more input.
                state.ShouldClose = true;
                writeFailure = ex;
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

            if (writeFailure != null)
            {
                // Reported and closed only once the write lock is released:
                // OnError is somebody else's code, and the close starts a
                // teardown whose first step is to take that same lock.
                ReportError("write failed", writeFailure);
                try
                {
                    state.Client.Close();
                }
                catch
                {
                    // already closed
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

        private void ReportCallSettled()
        {
            try
            {
                OnCallSettled?.Invoke();
            }
            catch
            {
                // a throwing test hook must not break the server
            }
        }

        public async ValueTask DisposeAsync()
        {
            // Idempotent under concurrent calls. Only the caller that
            // wins the exchange (sees the non-null CancellationTokenSource)
            // runs teardown; every other caller — concurrent or later —
            // observes null and returns immediately rather than risking an
            // NRE against a field another call already cleared.
            var cts = Interlocked.Exchange(ref _cts, null);
            if (cts == null)
            {
                return;
            }

            // Fenced, same reasoning as the per-connection
            // Cancel() in HandleConnectionAsync's finally — a throwing
            // callback must not skip the rest of DisposeAsync's teardown.
            try
            {
                cts.Cancel();
            }
            catch (Exception ex)
            {
                ReportError("cancelling the server token threw", ex);
            }

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
