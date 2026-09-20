using System;
using System.Diagnostics;
using System.IO;
using System.Runtime.InteropServices;
using System.Threading;
using System.Threading.Tasks;

namespace BECode.Bridge.FakeHost
{
    /// <summary>
    /// A fake Visual Studio host: a console app that runs the real
    /// <see cref="BridgeServer"/>/<see cref="ToolRegistry"/> over a
    /// <see cref="ScriptedEditorHost"/> instead of Visual Studio, so
    /// <c>internal/ide</c>'s real Go client can be driven against the real
    /// C# bridge end to end. Usage:
    ///
    /// <c>dotnet run --project visualstudio/src/BECode.Bridge.FakeHost --
    /// --home &lt;dir&gt; --workspace &lt;dir&gt;</c>
    ///
    /// Writes the lock file the Go side discovers through
    /// (&lt;home&gt;/.be-code/ide/&lt;pid&gt;.json), prints <c>READY &lt;port&gt;</c>
    /// on stdout once that lock is written and flushed, then waits for
    /// stdin EOF or SIGTERM (also Ctrl+C / process exit) before disposing
    /// the server, removing the lock, and exiting 0.
    /// </summary>
    internal static class Program
    {
        private const string Version = "0.0.0-fakehost";

        private static async Task<int> Main(string[] args)
        {
            string? home = null;
            string? workspace = null;
            for (var i = 0; i < args.Length; i++)
            {
                if (args[i] == "--home" && i + 1 < args.Length)
                {
                    home = args[++i];
                }
                else if (args[i] == "--workspace" && i + 1 < args.Length)
                {
                    workspace = args[++i];
                }
            }

            if (string.IsNullOrEmpty(home) || string.IsNullOrEmpty(workspace))
            {
                Console.Error.WriteLine("usage: BECode.Bridge.FakeHost --home <dir> --workspace <dir>");
                return 2;
            }

            var host = new ScriptedEditorHost(workspace);
            var registry = new ToolRegistry(host);
            var token = LockFile.NewToken();
            await using var server = new BridgeServer(registry, token, Version);
            server.OnError = (context, ex) => Console.Error.WriteLine($"fakehost: {context}: {ex}");

            var port = await server.StartAsync().ConfigureAwait(false);
            var pid = Process.GetCurrentProcess().Id;

            var cleanupGate = 0;

            async Task CleanupAsync()
            {
                if (Interlocked.Exchange(ref cleanupGate, 1) != 0)
                {
                    return;
                }

                try
                {
                    await server.DisposeAsync().ConfigureAwait(false);
                }
                catch
                {
                    // best effort: exiting regardless
                }

                LockFile.Remove(home!, pid);
            }

            using var cts = new CancellationTokenSource();

            ConsoleCancelEventHandler onCancelKey = (_, e) =>
            {
                e.Cancel = true;
                cts.Cancel();
            };
            Console.CancelKeyPress += onCancelKey;

            EventHandler onProcessExit = (_, __) =>
            {
                // Last-resort synchronous cleanup: ProcessExit handlers
                // cannot await, but CleanupAsync is idempotent (the gate
                // above), so this only does real work if nothing else
                // already ran it.
                CleanupAsync().GetAwaiter().GetResult();
            };
            AppDomain.CurrentDomain.ProcessExit += onProcessExit;

            PosixSignalRegistration? sigterm = null;
            try
            {
                sigterm = PosixSignalRegistration.Create(PosixSignal.SIGTERM, ctx =>
                {
                    ctx.Cancel = true;
                    cts.Cancel();
                });
            }
            catch (PlatformNotSupportedException)
            {
                // Not every platform supports this registration; stdin EOF,
                // Ctrl+C and ProcessExit still cover shutdown there.
            }

            // The lock file must exist before READY is printed — a client
            // racing the printed line must always find it.
            await LockFile.WriteAsync(home, new LockInfo(
                pid,
                port,
                token,
                new[] { workspace },
                "visualstudio",
                Version)).ConfigureAwait(false);

            Console.Out.Write("READY " + port.ToString() + "\n");
            Console.Out.Flush();

            // stdin EOF (the test closing its stdin pipe, or the parent
            // process going away with no explicit signal) is a shutdown
            // request too.
            _ = Task.Run(() =>
            {
                try
                {
                    while (Console.In.ReadLine() != null)
                    {
                        // ignore any input; only EOF (null) matters
                    }
                }
                catch
                {
                    // treat a broken stdin the same as EOF
                }

                cts.Cancel();
            });

            try
            {
                await Task.Delay(Timeout.Infinite, cts.Token).ConfigureAwait(false);
            }
            catch (OperationCanceledException)
            {
                // normal shutdown path
            }

            sigterm?.Dispose();
            Console.CancelKeyPress -= onCancelKey;
            await CleanupAsync().ConfigureAwait(false);
            AppDomain.CurrentDomain.ProcessExit -= onProcessExit;

            return 0;
        }
    }
}
