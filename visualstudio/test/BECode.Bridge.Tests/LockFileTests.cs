using System;
using System.Collections.Generic;
using System.IO;
using System.Linq;
using System.Runtime.InteropServices;
using System.Text.Json;
using System.Threading.Tasks;
using BECode.Bridge;
using Xunit;

namespace BECode.Bridge.Tests
{
    // Every test here uses a fresh temp directory as "home" — LockFile must
    // never touch the real ~/.be-code, which is exactly why LockFile takes
    // home as a parameter.
    public class LockFileTests : IDisposable
    {
        private readonly string _home;

        public LockFileTests()
        {
            _home = Directory.CreateTempSubdirectory("becode-bridge-lockfile-tests-").FullName;
        }

        public void Dispose()
        {
            try
            {
                Directory.Delete(_home, recursive: true);
            }
            catch
            {
                // best-effort cleanup
            }
        }

        private static LockInfo SampleInfo(int pid) => new LockInfo(
            Pid: pid,
            Port: 54321,
            Token: LockFile.NewToken(),
            WorkspaceFolders: new List<string> { "/workspace/one", "/workspace/two" },
            IdeName: "visualstudio",
            Version: "1.0.0");

        [Fact]
        public async Task WrittenFileHasExactlyTheSixExpectedKeys()
        {
            var info = SampleInfo(4242);

            await LockFile.WriteAsync(_home, info);

            var path = LockFile.PathFor(_home, info.Pid);
            var json = await File.ReadAllTextAsync(path);
            using var doc = JsonDocument.Parse(json);

            var keys = doc.RootElement.EnumerateObject().Select(p => p.Name).OrderBy(n => n, StringComparer.Ordinal).ToArray();
            var expected = new[] { "ideName", "pid", "port", "token", "version", "workspaceFolders" };
            Assert.Equal(expected, keys);

            Assert.Equal(info.Pid, doc.RootElement.GetProperty("pid").GetInt32());
            Assert.Equal(info.Port, doc.RootElement.GetProperty("port").GetInt32());
            Assert.Equal(info.Token, doc.RootElement.GetProperty("token").GetString());
            Assert.Equal(info.IdeName, doc.RootElement.GetProperty("ideName").GetString());
            Assert.Equal(info.Version, doc.RootElement.GetProperty("version").GetString());
            var folders = doc.RootElement.GetProperty("workspaceFolders").EnumerateArray().Select(e => e.GetString()!).ToArray();
            Assert.Equal(info.WorkspaceFolders, folders);
        }

        // M7 (review round 1): this assertion is synchronous; there was no
        // reason for the test method itself to be async.
        [Fact]
        public void PathForMatchesGoSideLayout()
        {
            var expected = Path.Combine(_home, ".be-code", "ide", "777.json");
            Assert.Equal(expected, LockFile.PathFor(_home, 777));
        }

        [Fact]
        public async Task SecondWriteReplacesAtomicallyAndLeavesNoTempFile()
        {
            var pid = 9001;
            var first = new LockInfo(pid, 1111, LockFile.NewToken(), new List<string> { "/a" }, "visualstudio", "1.0.0");
            var second = new LockInfo(pid, 2222, LockFile.NewToken(), new List<string> { "/b" }, "visualstudio", "1.0.1");

            await LockFile.WriteAsync(_home, first);
            await LockFile.WriteAsync(_home, second);

            var path = LockFile.PathFor(_home, pid);
            var json = await File.ReadAllTextAsync(path);
            using var doc = JsonDocument.Parse(json);
            Assert.Equal(second.Port, doc.RootElement.GetProperty("port").GetInt32());
            Assert.Equal(second.Token, doc.RootElement.GetProperty("token").GetString());

            var dir = Path.GetDirectoryName(path)!;
            var allFiles = Directory.GetFiles(dir);
            Assert.Single(allFiles);
            Assert.Equal(path, allFiles[0]);
        }

        [Fact]
        public void RemoveOfAMissingFileDoesNotThrow()
        {
            // No lock was ever written for this pid, and the ide directory
            // itself does not exist yet.
            var exception = Record.Exception(() => LockFile.Remove(_home, 123456));
            Assert.Null(exception);
        }

        [Fact]
        public async Task RemoveDeletesAnExistingLock()
        {
            var info = SampleInfo(555);
            await LockFile.WriteAsync(_home, info);
            var path = LockFile.PathFor(_home, info.Pid);
            Assert.True(File.Exists(path));

            LockFile.Remove(_home, info.Pid);

            Assert.False(File.Exists(path));
        }

        [Fact]
        public void NewTokenIsSixtyFourLowerCaseHexCharacters()
        {
            var token = LockFile.NewToken();

            Assert.Equal(64, token.Length);
            Assert.Matches("^[0-9a-f]{64}$", token);
        }

        [Fact]
        public void TwoNewTokenCallsDiffer()
        {
            var a = LockFile.NewToken();
            var b = LockFile.NewToken();

            Assert.NotEqual(a, b);
        }

        [Fact]
        public async Task WrittenFileModeIsOwnerReadWriteOnly()
        {
            if (RuntimeInformation.IsOSPlatform(OSPlatform.Windows))
            {
                // File modes are not meaningful on Windows; there is
                // nothing to assert there.
                return;
            }

            var info = SampleInfo(6006);
            await LockFile.WriteAsync(_home, info);
            var path = LockFile.PathFor(_home, info.Pid);

            var mode = File.GetUnixFileMode(path);
            Assert.Equal(UnixFileMode.UserRead | UnixFileMode.UserWrite, mode);
        }

        // I2 (review round 1): the final file after an atomic replace (not
        // just a first-ever write) must still be 0600 — a regression guard
        // on the create-empty / chmod / write / rename ordering surviving a
        // second write to the same pid.
        [Fact]
        public async Task SecondWriteFileModeIsStillOwnerReadWriteOnly()
        {
            if (RuntimeInformation.IsOSPlatform(OSPlatform.Windows))
            {
                return;
            }

            var pid = 7007;
            await LockFile.WriteAsync(_home, new LockInfo(pid, 1, LockFile.NewToken(), new List<string> { "/a" }, "visualstudio", "1.0.0"));
            await LockFile.WriteAsync(_home, new LockInfo(pid, 2, LockFile.NewToken(), new List<string> { "/b" }, "visualstudio", "1.0.1"));

            var path = LockFile.PathFor(_home, pid);
            var mode = File.GetUnixFileMode(path);
            Assert.Equal(UnixFileMode.UserRead | UnixFileMode.UserWrite, mode);
        }

        // I2: the ide directory this call creates must be 0700 on Unix,
        // matching the Go side's os.MkdirAll(d, 0o700).
        [Fact]
        public async Task FreshlyCreatedIdeDirectoryIsOwnerOnly()
        {
            if (RuntimeInformation.IsOSPlatform(OSPlatform.Windows))
            {
                return;
            }

            var info = SampleInfo(8008);
            var dir = Path.Combine(_home, ".be-code", "ide");
            Assert.False(Directory.Exists(dir));

            await LockFile.WriteAsync(_home, info);

            var mode = File.GetUnixFileMode(dir);
            Assert.Equal(UnixFileMode.UserRead | UnixFileMode.UserWrite | UnixFileMode.UserExecute, mode);
        }

        // I2: a pre-existing ide directory's mode is the user's own choice
        // and must not be touched by WriteAsync.
        [Fact]
        public async Task PreexistingIdeDirectoryModeIsNotChanged()
        {
            if (RuntimeInformation.IsOSPlatform(OSPlatform.Windows))
            {
                return;
            }

            var dir = Path.Combine(_home, ".be-code", "ide");
            Directory.CreateDirectory(dir);
            File.SetUnixFileMode(dir, UnixFileMode.UserRead | UnixFileMode.UserWrite | UnixFileMode.UserExecute
                | UnixFileMode.GroupRead | UnixFileMode.GroupExecute
                | UnixFileMode.OtherRead | UnixFileMode.OtherExecute); // 0755

            await LockFile.WriteAsync(_home, SampleInfo(9009));

            var mode = File.GetUnixFileMode(dir);
            Assert.Equal(
                UnixFileMode.UserRead | UnixFileMode.UserWrite | UnixFileMode.UserExecute
                | UnixFileMode.GroupRead | UnixFileMode.GroupExecute
                | UnixFileMode.OtherRead | UnixFileMode.OtherExecute,
                mode);
        }

        // The Go side makes the whole path 0700 (os.MkdirAll(d, 0o700)). A
        // .be-code left at the umask default can be group-writable, and
        // whoever can write it can swap the ide directory for their own and
        // plant a lock the client would dial.
        [Fact]
        public async Task FreshlyCreatedDotdirIsOwnerOnly()
        {
            if (RuntimeInformation.IsOSPlatform(OSPlatform.Windows))
            {
                return;
            }

            var dotdir = Path.Combine(_home, ".be-code");
            Assert.False(Directory.Exists(dotdir));

            await LockFile.WriteAsync(_home, SampleInfo(8118));

            Assert.Equal(
                UnixFileMode.UserRead | UnixFileMode.UserWrite | UnixFileMode.UserExecute,
                File.GetUnixFileMode(dotdir));
        }

        // ...and a .be-code that was already there keeps the mode its owner
        // gave it, while the ide directory this call creates is still 0700.
        [Fact]
        public async Task PreexistingDotdirModeIsNotChanged()
        {
            if (RuntimeInformation.IsOSPlatform(OSPlatform.Windows))
            {
                return;
            }

            var dotdir = Path.Combine(_home, ".be-code");
            Directory.CreateDirectory(dotdir);
            var mode0755 = UnixFileMode.UserRead | UnixFileMode.UserWrite | UnixFileMode.UserExecute
                | UnixFileMode.GroupRead | UnixFileMode.GroupExecute
                | UnixFileMode.OtherRead | UnixFileMode.OtherExecute;
            File.SetUnixFileMode(dotdir, mode0755);

            await LockFile.WriteAsync(_home, SampleInfo(8228));

            Assert.Equal(mode0755, File.GetUnixFileMode(dotdir));
            Assert.Equal(
                UnixFileMode.UserRead | UnixFileMode.UserWrite | UnixFileMode.UserExecute,
                File.GetUnixFileMode(Path.Combine(dotdir, "ide")));
        }

        // A rename that fails must not strand the temp file: it is 0600, but
        // it carries the token and nothing would ever sweep it.
        [Fact]
        public async Task AFailedRenameLeavesNoTempFile()
        {
            var pid = 3333;
            var final = LockFile.PathFor(_home, pid);
            // A directory where the lock file should go makes the rename fail.
            Directory.CreateDirectory(final);

            await Assert.ThrowsAnyAsync<Exception>(() => LockFile.WriteAsync(_home, SampleInfo(pid)));

            Assert.Empty(Directory.GetFiles(Path.GetDirectoryName(final)!, "*.tmp"));
        }
    }
}
