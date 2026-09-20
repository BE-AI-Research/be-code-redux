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
            // Do not implement this by round-tripping through
            // System.Uri (folderUri.MakeRelativeUri(fileUri) then
            // Uri.UnescapeDataString(relUri.ToString())): Uri.ToString()
            // already unescapes "safe" characters, so unescaping it again
            // would corrupt any file name that happens to contain a literal
            // '%' sequence that looks like percent-encoding, decoding
            // "file%41.txt" to "fileA.txt". A real file's name is just
            // bytes; it must round-trip unchanged.
            var literalName = "file%41.txt";
            var path = Path.Combine(_rootA, literalName);
            File.WriteAllText(path, "x");

            Assert.Equal(literalName, Paths.RelPath(_folders, path));
        }

        [Fact]
        public void RelPathPrefersTheFirstFolderThatContainsThePathAmongNestedRoots()
        {
            // Folders are tried in order; "/ws" contains "/ws/sub/a.go"
            // and comes first, so the answer is relative to "/ws" ("sub/a.go"),
            // not to the more specific "/ws/sub" ("a.go").
            Assert.Equal("sub/a.go", Paths.RelPath(new[] { "/ws", "/ws/sub" }, "/ws/sub/a.go"));
        }

        // ---- Paths.RelativeIfInsideCore is the pure, injectable core of
        // RelPath's "inside" check (separator, comparison and root-extraction
        // all passed in) — this is what makes the port's Windows-specific
        // behaviour (drive letters, case-insensitivity, backslash separators)
        // testable on this Linux machine at all, since System.IO.Path's own
        // drive-letter handling only activates on a real Windows OS.

        // A minimal stand-in for Path.GetPathRoot on Windows: "C:\" is the
        // root of "C:\ws\sub", not a substring match — good enough for these
        // tests without depending on any real OS behaviour.
        private static string WindowsDriveRootOf(string p) => p.Length >= 3 && p[1] == ':' ? p.Substring(0, 3) : "";

        [Fact]
        public void RelativeIfInsideCoreTreatsDifferentDrivesAsHavingNoRelativePath()
        {
            var result = Paths.RelativeIfInsideCore(@"C:\ws", @"D:\x\y.go", '\\', StringComparison.OrdinalIgnoreCase, WindowsDriveRootOf);

            Assert.Null(result);
        }

        [Fact]
        public void RelativeIfInsideCoreTreatsDriveLettersCaseInsensitively()
        {
            var result = Paths.RelativeIfInsideCore(@"C:\ws", @"c:\ws\a.go", '\\', StringComparison.OrdinalIgnoreCase, WindowsDriveRootOf);

            Assert.Equal("a.go", result);
        }

        [Fact]
        public void RelativeIfInsideCoreTurnsBackslashesIntoForwardSlashesInTheOutput()
        {
            var result = Paths.RelativeIfInsideCore(@"C:\ws", @"C:\ws\sub\a.go", '\\', StringComparison.OrdinalIgnoreCase, WindowsDriveRootOf);

            Assert.Equal("sub/a.go", result);
        }

        [Fact]
        public void RelativeIfInsideCoreReturnsEmptyForTheFolderItself()
        {
            var result = Paths.RelativeIfInsideCore(@"C:\ws", @"C:\ws", '\\', StringComparison.OrdinalIgnoreCase, WindowsDriveRootOf);

            Assert.Equal(string.Empty, result);
        }

        [Fact]
        public void RelativeIfInsideCoreHandlesATrailingSeparatorOnTheFolder()
        {
            var withTrailingSep = Paths.RelativeIfInsideCore(@"C:\ws\", @"C:\ws\a.go", '\\', StringComparison.OrdinalIgnoreCase, WindowsDriveRootOf);
            var withoutTrailingSep = Paths.RelativeIfInsideCore(@"C:\ws", @"C:\ws\a.go", '\\', StringComparison.OrdinalIgnoreCase, WindowsDriveRootOf);

            Assert.Equal("a.go", withTrailingSep);
            Assert.Equal("a.go", withoutTrailingSep);
        }

        [Fact]
        public void RelativeIfInsideCoreRejectsAPathNotBeneathTheFolder()
        {
            var result = Paths.RelativeIfInsideCore(@"C:\ws", @"C:\ws-evil\a.go", '\\', StringComparison.OrdinalIgnoreCase, WindowsDriveRootOf);

            Assert.Null(result);
        }
    }
}
