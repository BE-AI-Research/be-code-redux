using System;

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
    /// </summary>
    internal static class ActivityLog
    {
        private const string Source = "BE-Code";

        public static void LogError(string context, string message)
        {
            Write(context, message, static m => Microsoft.VisualStudio.Shell.ActivityLog.LogError(Source, m));
        }

        public static void LogWarning(string context, string message)
        {
            Write(context, message, static m => Microsoft.VisualStudio.Shell.ActivityLog.LogWarning(Source, m));
        }

        public static void LogInformation(string context, string message)
        {
            Write(context, message, static m => Microsoft.VisualStudio.Shell.ActivityLog.LogInformation(Source, m));
        }

        private static void Write(string context, string message, Action<string> sink)
        {
            try
            {
                sink(context + ": " + message);
            }
            catch
            {
                // A logging failure (e.g. the log service unavailable this
                // early/late in the package's lifetime) must never be why
                // something else fails.
            }
        }
    }
}
