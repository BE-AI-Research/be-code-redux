using System;
using System.Diagnostics;
using System.IO;
using System.Threading.Tasks;
using BECode.Bridge.Hosting;

namespace BECode.VisualStudio
{
    /// <summary>
    /// Host design §2, "ActivityLog.cs — one logging seam": every failure in
    /// this layer (<see cref="Guard"/>'s catch-all, <see
    /// cref="Bridge.BridgeServer.OnError"/> wired in <see
    /// cref="BECodePackage"/>, a swallowed <see
    /// cref="BECodePackage.InitializeAsync"/> exception) goes through here
    /// rather than each call site touching
    /// <see cref="Microsoft.VisualStudio.Shell.ActivityLog"/> directly, so
    /// there is exactly one place that decides the source name and one place
    /// to change if the logging mechanism ever needs to change. Every method
    /// swallows its own failure: the Activity Log itself must never be the
    /// reason something else in this layer throws (host design §1.3, "no
    /// failure in this layer may take down Visual Studio").
    ///
    /// The Activity Log may need the UI
    /// thread, and it is easy to miss entirely if the owner never opens it —
    /// every entry is now ALSO mirrored to a plain file,
    /// <c>&lt;home&gt;\.be-code\visualstudio.log</c>, appended from a
    /// background thread only. <see cref="Initialize"/> must be called once,
    /// early, with the same <c>home</c> <see cref="BECodePackage"/> already
    /// resolves for the lock file; every call before that (or a failure
    /// inside it) simply skips the file mirror, the Activity Log call still
    /// happens either way.
    /// </summary>
    internal static class ActivityLog
    {
        private const string Source = "BE-Code";
        private static readonly int Pid = Process.GetCurrentProcess().Id;
        private static string? _logPath;

        /// <summary>
        /// Truncates <c>visualstudio.log</c> when it is already over 1 MiB
        /// (<see cref="DiagnosticsLog.ShouldTruncate"/>) so it never grows
        /// unbounded across sessions, then remembers its path for every
        /// later <see cref="Write"/>. Called once from
        /// <see cref="BECodePackage.InitializeCoreAsync"/>, off the UI
        /// thread; any failure here (the directory cannot be created, the
        /// file cannot be stat'd/truncated) leaves the file mirror disabled
        /// for the rest of the session rather than throwing.
        /// </summary>
        public static void Initialize(string home)
        {
            try
            {
                var path = DiagnosticsLog.PathFor(home);
                var dir = Path.GetDirectoryName(path);
                if (!string.IsNullOrEmpty(dir))
                {
                    Directory.CreateDirectory(dir!);
                }

                if (File.Exists(path))
                {
                    var size = new FileInfo(path).Length;
                    if (DiagnosticsLog.ShouldTruncate(size))
                    {
                        File.WriteAllText(path, string.Empty);
                    }
                }

                _logPath = path;
            }
            catch
            {
                _logPath = null;
            }
        }

        public static void LogError(string context, string message)
        {
            Write("ERROR", context, message, static m => Microsoft.VisualStudio.Shell.ActivityLog.TryLogError(Source, m));
        }

        public static void LogWarning(string context, string message)
        {
            Write("WARN", context, message, static m => Microsoft.VisualStudio.Shell.ActivityLog.TryLogWarning(Source, m));
        }

        public static void LogInformation(string context, string message)
        {
            Write("INFO", context, message, static m => Microsoft.VisualStudio.Shell.ActivityLog.TryLogInformation(Source, m));
        }

        private static void Write(string level, string context, string message, Func<string, bool> sink)
        {
            try
            {
                // The TryLog* overloads —
                // they report failure via their bool return rather than
                // throwing, so a missing/not-yet-ready Activity Log service
                // cannot itself become an unhandled exception here.
                sink(context + ": " + message);
            }
            catch
            {
                // A logging failure (e.g. the log service unavailable this
                // early/late in the package's lifetime) must never be why
                // something else fails.
            }

            AppendToFile(level, context, message);
        }

        private static void AppendToFile(string level, string context, string message)
        {
            var path = _logPath;
            if (path == null)
            {
                return;
            }

            // Appended from a background thread ONLY,
            // never on the UI thread. This is deliberately fire-and-forget
            // (nothing ever awaits the returned Task, and nothing may —
            // Write/LogError/.. are synchronous, called from anywhere,
            // including the UI thread) — Task.Run queues the write to the
            // thread pool regardless of which thread AppendToFile itself
            // runs on.
            _ = Task.Run(() =>
            {
                try
                {
                    var line = DiagnosticsLog.FormatLine(DateTimeOffset.Now, level, Pid, context, message);
                    File.AppendAllText(path, line + Environment.NewLine);
                }
                catch
                {
                    // Every failure of the log itself is swallowed (host
                    // design §1.3 / this class's own doc comment).
                }
            });
        }
    }
}
