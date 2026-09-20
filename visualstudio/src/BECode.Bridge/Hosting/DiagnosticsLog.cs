using System;
using System.IO;

namespace BECode.Bridge.Hosting
{
    /// <summary>
    /// Fix round 1, I-9 (Ruling R-15): pure formatting/rotation logic for
    /// the plain-file mirror of every Activity Log entry BE-Code's Visual
    /// Studio host writes, <c>&lt;home&gt;\.be-code\visualstudio.log</c> —
    /// the owner's only diagnostic surface besides the Activity Log itself
    /// (which may need the UI thread, and whose own failure is silently
    /// swallowed) when nothing attaches. The actual file I/O — which must
    /// run off the UI thread, appended from a background thread only, and
    /// NEVER against the real <c>~/.be-code</c> in a test — is
    /// <c>BECode.VisualStudio</c>'s own <c>ActivityLog.cs</c>; this class
    /// only decides the path, the line text, and whether the file needs
    /// truncating first, so those decisions are testable without a Visual
    /// Studio host.
    /// </summary>
    public static class DiagnosticsLog
    {
        /// <summary>The file is truncated at package load when it is already over this size.</summary>
        public const long MaxBytesBeforeTruncate = 1024 * 1024; // 1 MiB

        public static string PathFor(string home)
        {
            return Path.Combine(home, ".be-code", "visualstudio.log");
        }

        /// <summary>
        /// One tab-separated line: timestamp, level, pid, context, message,
        /// and — when given — the exception text. Embedded newlines and
        /// carriage returns in <paramref name="message"/>/<paramref name="exception"/>
        /// are flattened so one log entry is always exactly one line.
        /// </summary>
        public static string FormatLine(DateTimeOffset timestamp, string level, int pid, string context, string message, string? exception = null)
        {
            if (level == null)
            {
                throw new ArgumentNullException(nameof(level));
            }

            if (context == null)
            {
                throw new ArgumentNullException(nameof(context));
            }

            if (message == null)
            {
                throw new ArgumentNullException(nameof(message));
            }

            var line = timestamp.ToString("O") + "\t" + level + "\t" + pid.ToString() + "\t" + Flatten(context) + "\t" + Flatten(message);
            if (!string.IsNullOrEmpty(exception))
            {
                line += "\t" + Flatten(exception!);
            }

            return line;
        }

        /// <summary>True when the file at <paramref name="currentSizeBytes"/> should be truncated before appending further.</summary>
        public static bool ShouldTruncate(long currentSizeBytes)
        {
            return currentSizeBytes > MaxBytesBeforeTruncate;
        }

        private static string Flatten(string text)
        {
            return text.Replace("\r\n", " ").Replace('\n', ' ').Replace('\r', ' ').Replace('\t', ' ');
        }
    }
}
