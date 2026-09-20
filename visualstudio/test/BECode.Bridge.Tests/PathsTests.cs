using System;
using System.IO;
using BECode.Bridge;
using Xunit;

namespace BECode.Bridge.Tests
{
    // Ports vscode/test/paths.test.ts's cases against BECode.Bridge.Paths,
    // the C# port of vscode/src/lib/paths.ts (absPath/relPath) used to confine
    // every path argument to the host's workspace folders before it ever
    // reaches IEditorHost.
    public class PathsTests : IDisposable
    {
        private readonly string _base;
        private readonly string _rootA;
        private readonly string _rootB;
        private readonly string[] _folders;

        public PathsTests()
        {
            _base = Directory.CreateTempSubdirectory("be-code-paths-").FullName;
            _rootA = Path.Combine(_base, "a");
            _rootB = Path.Combine(_base, "b");
            Directory.CreateDirectory(Path.Combine(_rootA, "src"));
            Directory.CreateDirectory(Path.Combine(_rootB, "src"));
            File.WriteAllText(Path.Combine(_rootA, "only-in-a.txt"), "a");
            File.WriteAllText(Path.Combine(_rootB, "src", "only-in-b.ts"), "b");
            _folders = new[] { _rootA, _rootB };
        }

        public void Dispose()
        {
            try { Directory.Delete(_base, recursive: true); } catch { /* best effort */ }
        }

        [Fact]
        public void AbsPathResolvesARelativePathAgainstTheFolderThatActuallyHasTheFile()
        {
            Assert.Equal(Path.Combine(_rootB, "src", "only-in-b.ts"), Paths.AbsPath(_folders, Path.Combine("src", "only-in-b.ts")));
            Assert.Equal(Path.Combine(_rootA, "only-in-a.txt"), Paths.AbsPath(_folders, "only-in-a.txt"));
        }

        [Fact]
        public void AbsPathFallsBackToTheFirstFolderForAFileThatDoesNotExistYet()
        {
            Assert.Equal(Path.Combine(_rootA, "brand", "new.ts"), Paths.AbsPath(_folders, Path.Combine("brand", "new.ts")));
        }

        [Fact]
        public void AbsPathAcceptsAnAbsolutePathInsideAWorkspaceFolder()
        {
            var abs = Path.Combine(_rootB, "src", "only-in-b.ts");
            Assert.Equal(abs, Paths.AbsPath(_folders, abs));
        }

        [Fact]
        public void AbsPathRejectsAnEscapeViaDotDot()
        {
            var ex = Assert.Throws<PathOutsideWorkspaceException>(
                () => Paths.AbsPath(_folders, Path.Combine("..", "..", "etc", "passwd")));
            Assert.Contains("outside the workspace", ex.Message);
        }

        [Fact]
        public void AbsPathRejectsAnAbsolutePathOutsideEveryFolder()
        {
            var outside = Path.Combine(Path.GetPathRoot(_base) ?? "/", "etc", "passwd");
            Assert.Throws<PathOutsideWorkspaceException>(() => Paths.AbsPath(_folders, outside));
        }

        [Fact]
        public void AbsPathDoesNotTreatASiblingFolderWithASharedPrefixAsInside()
        {
            var evil = _rootA + "-evil" + Path.DirectorySeparatorChar + "x.txt";
            Assert.Throws<PathOutsideWorkspaceException>(() => Paths.AbsPath(_folders, evil));
        }

        [Fact]
        public void AbsPathAllowsTheFolderItself()
        {
            Assert.Equal(_rootA, Paths.AbsPath(_folders, _rootA));
        }

        [Fact]
        public void RelPathStillReportsAPathRelativeToItsFolder()
        {
            Assert.Equal("src/only-in-b.ts", Paths.RelPath(_folders, Path.Combine(_rootB, "src", "only-in-b.ts")));
        }

        [Fact]
        public void RelPathMakesPathsWorkspaceRelative()
        {
            Assert.Equal("internal/x.go", Paths.RelPath(new[] { "/w/proj" }, "/w/proj/internal/x.go"));
        }

        [Fact]
        public void RelPathReturnsTheOriginalPathWhenOutsideEveryFolder()
        {
            Assert.Equal("/elsewhere/y.go", Paths.RelPath(new[] { "/w/proj" }, "/elsewhere/y.go"));
        }

        [Fact]
        public void RelPathDoesNotDecodeAPercentEncodedLookingLiteralFileName()
        {
            // S3: the original implementation round-tripped through
            // System.Uri (folderUri.MakeRelativeUri(fileUri) then
            // Uri.UnescapeDataString(relUri.ToString())) — but Uri.ToString()
            // already unescapes "safe" characters, so unescaping it again
            // corrupted any file name that happens to contain a literal '%'
            // sequence that looks like percent-encoding, decoding
            // "file%41.txt" to "fileA.txt". A real file's name is just
            // bytes; it must round-trip unchanged.
            var literalName = "file%41.txt";
            var path = Path.Combine(_rootA, literalName);
            File.WriteAllText(path, "x");

            Assert.Equal(literalName, Paths.RelPath(_folders, path));
        }
    }
}
