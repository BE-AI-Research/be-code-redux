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
        /// Writes the lock file atomically: a temp file in the same
        /// directory, mode 0600 applied before it is visible under its
        /// final name, then an atomic move/replace over any prior lock for
        /// this pid.
        /// </summary>
        public static async Task WriteAsync(string home, LockInfo info)
        {
            var dir = LockDir(home);
            Directory.CreateDirectory(dir);

            var finalPath = PathFor(home, info.Pid);
            var tempPath = Path.Combine(dir, info.Pid.ToString() + "." + Guid.NewGuid().ToString("N") + ".tmp");

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

            using (var fs = new FileStream(tempPath, FileMode.CreateNew, FileAccess.Write, FileShare.None))
            {
                await fs.WriteAsync(bytes, 0, bytes.Length).ConfigureAwait(false);
                await fs.FlushAsync().ConfigureAwait(false);
            }

            TrySetOwnerReadWrite(tempPath);

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

        /// <summary>Removing a lock that is not there is not an error.</summary>
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

        [DllImport("libc", SetLastError = true)]
        private static extern int chmod(string pathname, int mode);

        private static void TrySetOwnerReadWrite(string path)
        {
            if (RuntimeInformation.IsOSPlatform(OSPlatform.Windows))
            {
                return;
            }

            const int mode0600 = 384; // 0600 in octal
            chmod(path, mode0600);
        }
    }
}
