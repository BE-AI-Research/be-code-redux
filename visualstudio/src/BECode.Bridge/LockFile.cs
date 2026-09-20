using System;
using System.Collections.Generic;
using System.IO;
using System.Runtime.InteropServices;
using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using System.Threading.Tasks;

namespace BECode.Bridge
{
    /// <summary>
    /// The lock file a running bridge advertises itself with: the same
    /// shape and location (&lt;home&gt;/.be-code/ide/&lt;pid&gt;.json) the Go
    /// client (internal/ide) and the vscode extension already write.
    /// </summary>
    public sealed record LockInfo(
        int Pid,
        int Port,
        string Token,
        IReadOnlyList<string> WorkspaceFolders,
        string IdeName,
        string Version);

    public static class LockFile
    {
        /// <summary>32 random bytes, rendered as 64 lower-case hex characters.</summary>
        public static string NewToken()
        {
            var bytes = new byte[32];
            using (var rng = RandomNumberGenerator.Create())
            {
                // RandomNumberGenerator.Fill does not exist on netstandard2.0.
                rng.GetBytes(bytes);
            }

            var sb = new StringBuilder(bytes.Length * 2);
            foreach (var b in bytes)
            {
                sb.Append(b.ToString("x2"));
            }

            return sb.ToString();
        }

        public static string PathFor(string home, int pid)
        {
            return Path.Combine(home, ".be-code", "ide", pid.ToString() + ".json");
        }

        private static string LockDir(string home)
        {
            return Path.Combine(home, ".be-code", "ide");
        }

        /// <summary>
        /// Writes the lock file atomically: an empty temp file in the same
        /// directory, restricted to 0600 (checked — a chmod failure aborts
        /// the write and removes the temp file) BEFORE any content
        /// (including the token) is written into it, then an atomic
        /// move/replace over any prior lock for this pid.
        /// </summary>
        public static async Task WriteAsync(string home, LockInfo info)
        {
            var dotdir = Path.Combine(home, ".be-code");
            var dir = LockDir(home);
            var dotdirExisted = Directory.Exists(dotdir);
            var dirExisted = Directory.Exists(dir);
            Directory.CreateDirectory(dir);
            // The Go side creates this path 0700 (os.MkdirAll(d, 0o700));
            // match it, but only for a directory this call itself created —
            // a pre-existing directory's mode is the user's own choice. Both
            // levels matter: whoever can write .be-code can swap the ide
            // directory for their own and plant a lock the client would dial.
            if (!dotdirExisted)
            {
                TrySetMode(dotdir, Mode0700);
            }

            if (!dirExisted)
            {
                TrySetMode(dir, Mode0700);
            }

            var finalPath = PathFor(home, info.Pid);
            var tempPath = Path.Combine(dir, info.Pid.ToString() + "." + Guid.NewGuid().ToString("N") + ".tmp");

            // Create the temp file EMPTY first...
            using (new FileStream(tempPath, FileMode.CreateNew, FileAccess.Write, FileShare.None))
            {
            }

            try
            {
                // ...restrict it to 0600 while it is still empty...
                SetModeOrThrow(tempPath, Mode0600);

                // ...and only then write the token into it. A chmod
                // failure, or a write failure, both abort with the temp
                // file removed rather than leaving a secret-bearing file
                // at whatever mode the umask happened to give it.
                var payload = new
                {
                    pid = info.Pid,
                    port = info.Port,
                    token = info.Token,
                    workspaceFolders = info.WorkspaceFolders,
                    ideName = info.IdeName,
                    version = info.Version,
                };
                var bytes = Encoding.UTF8.GetBytes(JsonSerializer.Serialize(payload));

                using (var fs = new FileStream(tempPath, FileMode.Open, FileAccess.Write, FileShare.None))
                {
                    await fs.WriteAsync(bytes, 0, bytes.Length).ConfigureAwait(false);
                    await fs.FlushAsync().ConfigureAwait(false);
                }

                // The rename sits inside the same fence: a temp file that
                // carries the token must not outlive a failed write, and
                // nothing would ever sweep it.
                if (File.Exists(finalPath))
                {
                    // File.Replace is the atomic swap; it requires the
                    // destination to already exist, unlike File.Move.
                    File.Replace(tempPath, finalPath, null);
                }
                else
                {
                    File.Move(tempPath, finalPath);
                }
            }
            catch
            {
                TryDeleteQuietly(tempPath);
                throw;
            }
        }

        /// <summary>
        /// Removing a lock that is not there is not an error: the Go side
        /// prunes locks whose pid is dead without first checking they
        /// exist, so a missing file (or even a missing ide directory) here
        /// is the ordinary case, not a failure.
        /// </summary>
        public static void Remove(string home, int pid)
        {
            try
            {
                File.Delete(PathFor(home, pid));
            }
            catch (DirectoryNotFoundException)
            {
            }
            catch (IOException)
            {
            }
            catch (UnauthorizedAccessException)
            {
            }
        }

        private const int Mode0600 = 384; // 0600 in octal
        private const int Mode0700 = 448; // 0700 in octal

        [DllImport("libc", SetLastError = true)]
        private static extern int chmod(string pathname, int mode);

        /// <summary>Chmod, checked: a non-zero result throws with the errno.</summary>
        private static void SetModeOrThrow(string path, int mode)
        {
            if (RuntimeInformation.IsOSPlatform(OSPlatform.Windows))
            {
                return;
            }

            var result = chmod(path, mode);
            if (result != 0)
            {
                var errno = Marshal.GetLastWin32Error();
                throw new IOException($"chmod {path} to {Convert.ToString(mode, 8)} failed (errno {errno})");
            }
        }

        /// <summary>Best-effort chmod for cases where a failure is not fatal to the write.</summary>
        private static void TrySetMode(string path, int mode)
        {
            if (RuntimeInformation.IsOSPlatform(OSPlatform.Windows))
            {
                return;
            }

            chmod(path, mode);
        }

        private static void TryDeleteQuietly(string path)
        {
            try
            {
                File.Delete(path);
            }
            catch
            {
                // best-effort cleanup of a temp file we are already
                // abandoning because something else went wrong
            }
        }
    }
}
