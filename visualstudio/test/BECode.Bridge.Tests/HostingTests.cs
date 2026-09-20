using System;
using System.Threading;
using System.Threading.Tasks;
using BECode.Bridge.Hosting;
using Xunit;

namespace BECode.Bridge.Tests
{
    // Task 6: the pure-logic helpers extracted from the Visual Studio host
    // design because they need no Visual Studio type (the brief's Rules
    // section names each one). net472 xunit tests cannot run on this Linux
    // checkout (no Mono, no .NET Framework runtime installed) so, per the
    // brief's own decision rule, these live in BECode.Bridge/Hosting and are
    // tested here instead, against the existing net8.0 test project.
    public class ErrorSourceTests
    {
        [Theory]
        [InlineData("CS1002: ; expected", "CS1002")]
        [InlineData("C2065: 'foo': undeclared identifier", "C2065")]
        [InlineData("MSB3073: The command exited with code 1", "MSB3073")]
        public void ParsesALeadingErrorCode(string description, string expected)
        {
            Assert.Equal(expected, ErrorSource.ParseCode(description));
        }

        [Theory]
        [InlineData("something went wrong: no code here")]
        [InlineData("no colon at all")]
        [InlineData("")]
        [InlineData(null)]
        [InlineData(" CS1002: leading space before the code")]
        [InlineData("1002: starts with a digit, not a letter")]
        [InlineData("CS: letters with no digits")]
        public void ReturnsNullWhenThereIsNoLeadingCode(string? description)
        {
            Assert.Null(ErrorSource.ParseCode(description));
        }
    }

    public class DebugReasonTests
    {
        [Theory]
        [InlineData("dbgEventReasonBreakpoint", "breakpoint")]
        [InlineData("dbgEventReasonStep", "step")]
        [InlineData("dbgEventReasonExceptionThrown", "exception")]
        [InlineData("dbgEventReasonExceptionNotHandled", "exception")]
        [InlineData("dbgEventReasonUserBreak", "pause")]
        [InlineData("dbgEventReasonNone", "stopped")]
        [InlineData("somethingUnrecognised", "stopped")]
        public void MapsBreakReasons(string dteName, string expected)
        {
            Assert.Equal(expected, DebugReason.ForBreak(dteName));
        }

        [Fact]
        public void OnlyEndProgramIsANormalExit()
        {
            Assert.True(DebugReason.IsNormalExit("dbgEventReasonEndProgram"));
            Assert.False(DebugReason.IsNormalExit("dbgEventReasonStopDebugging"));
            Assert.False(DebugReason.IsNormalExit("dbgEventReasonAttachProgram"));
        }
    }

    public class OutputCursorTests
    {
        [Fact]
        public void ReturnsLinesSinceTheCursorAndTheNewCursor()
        {
            var (lines, cursor) = OutputCursor.Since("a\nb\nc\n", 1);
            Assert.Equal(new[] { "b", "c" }, lines);
            Assert.Equal(3, cursor);
        }

        [Fact]
        public void EmptyTextIsZeroLines()
        {
            var (lines, cursor) = OutputCursor.Since("", 0);
            Assert.Empty(lines);
            Assert.Equal(0, cursor);
        }

        [Fact]
        public void ACursorAtTheEndReturnsNothingButTheSameCursor()
        {
            var (lines, cursor) = OutputCursor.Since("a\nb\n", 2);
            Assert.Empty(lines);
            Assert.Equal(2, cursor);
        }

        [Fact]
        public void ACursorPastAClearedShorterPaneRestartsFromZero()
        {
            // The pane was cleared and only "x" has been printed since:
            // since=10 no longer makes sense against a 1-line pane.
            var (lines, cursor) = OutputCursor.Since("x\n", 10);
            Assert.Equal(new[] { "x" }, lines);
            Assert.Equal(1, cursor);
        }

        [Fact]
        public void CarriageReturnsAreNormalised()
        {
            var (lines, cursor) = OutputCursor.Since("a\r\nb\rc\n", 0);
            Assert.Equal(new[] { "a", "b", "c" }, lines);
            Assert.Equal(3, cursor);
        }
    }

    public class DocCommentSummaryTests
    {
        [Fact]
        public void ExtractsAndNormalisesASummary()
        {
            var xml = "<member name=\"M:Foo.Bar\">\n    <summary>\n    Does the thing.\n    Really.\n    </summary>\n</member>";
            Assert.Equal("Does the thing. Really.", DocCommentSummary.Extract(xml));
        }

        [Fact]
        public void ASummaryElementAtTheRootIsAlsoAccepted()
        {
            Assert.Equal("Hello.", DocCommentSummary.Extract("<summary>\nHello.\n</summary>"));
        }

        [Theory]
        [InlineData(null)]
        [InlineData("")]
        [InlineData("   ")]
        [InlineData("not xml at all <")]
        [InlineData("<member name=\"M:Foo.Bar\"><returns>an int</returns></member>")]
        public void ReturnsNullWhenThereIsNoUsableSummary(string? xml)
        {
            Assert.Null(DocCommentSummary.Extract(xml));
        }
    }

    public class DiffTempFilesTests
    {
        [Theory]
        [InlineData("Foo.cs", "Foo.proposed.cs")]
        [InlineData("/repo/src/Foo.cs", "Foo.proposed.cs")]
        [InlineData("Makefile", "Makefile.proposed")]
        [InlineData("archive.tar.gz", "archive.tar.proposed.gz")]
        public void NamesTheProposedSideAfterTheOriginalWithAMarkerBeforeTheExtension(string path, string expected)
        {
            // Path.GetFileName only recognises '/' as a separator on Linux
            // (this test project's own runtime); DiffReview.cs runs on
            // Windows, where '\' is recognised too — the transform itself
            // (strip directory, insert ".proposed" before the extension) is
            // identical either way, this just avoids a platform-dependent
            // assertion in a test that runs on Linux.
            Assert.Equal(expected, DiffTempFiles.ProposedFileName(path));
        }
    }

    public class LaunchProfilesTests
    {
        [Fact]
        public void ParsesTheProfileNamesInDocumentOrder()
        {
            var json = "{\"profiles\":{\"IIS Express\":{\"commandName\":\"IISExpress\"},\"MyApp\":{\"commandName\":\"Project\"}}}";
            Assert.Equal(new[] { "IIS Express", "MyApp" }, LaunchProfiles.ParseNames(json));
        }

        [Theory]
        [InlineData(null)]
        [InlineData("")]
        [InlineData("not json")]
        [InlineData("{}")]
        [InlineData("{\"profiles\": []}")]
        public void ReturnsEmptyForAnythingUnusable(string? json)
        {
            Assert.Empty(LaunchProfiles.ParseNames(json));
        }
    }

    public class DebouncerTests
    {
        [Fact]
        public async Task ABurstOfTriggersFiresTheActionOnce()
        {
            var fired = 0;
            var gate = new SemaphoreSlim(0);
            using var debouncer = new Debouncer(
                TimeSpan.FromMilliseconds(20),
                () =>
                {
                    Interlocked.Increment(ref fired);
                    gate.Release();
                    return Task.CompletedTask;
                });

            debouncer.Trigger();
            debouncer.Trigger();
            debouncer.Trigger();

            var signalled = await gate.WaitAsync(TimeSpan.FromSeconds(5));
            Assert.True(signalled, "the debounced action never fired");

            // Give any (incorrect) extra firing a moment to show up.
            await Task.Delay(100);
            Assert.Equal(1, fired);
        }

        [Fact]
        public async Task TwoWellSeparatedTriggersFireTwice()
        {
            var fired = 0;
            using var debouncer = new Debouncer(
                TimeSpan.FromMilliseconds(10),
                () =>
                {
                    Interlocked.Increment(ref fired);
                    return Task.CompletedTask;
                });

            debouncer.Trigger();
            await Task.Delay(100);
            debouncer.Trigger();
            await Task.Delay(100);

            Assert.Equal(2, fired);
        }

        [Fact]
        public async Task DisposeCancelsAPendingFiring()
        {
            var fired = 0;
            var debouncer = new Debouncer(
                TimeSpan.FromMilliseconds(50),
                () =>
                {
                    Interlocked.Increment(ref fired);
                    return Task.CompletedTask;
                });

            debouncer.Trigger();
            debouncer.Dispose();

            await Task.Delay(150);
            Assert.Equal(0, fired);
        }

        [Fact]
        public async Task AThrowingActionDoesNotBreakLaterTriggers()
        {
            var attempts = 0;
            using var debouncer = new Debouncer(
                TimeSpan.FromMilliseconds(10),
                () =>
                {
                    Interlocked.Increment(ref attempts);
                    throw new InvalidOperationException("boom");
                });

            debouncer.Trigger();
            await Task.Delay(100);
            debouncer.Trigger();
            await Task.Delay(100);

            Assert.Equal(2, attempts);
        }
    }

    // Task 6, fix round 1, I-2: the lock-republish ordering guard.
    public class GenerationGateTests
    {
        [Fact]
        public void NextIsStrictlyIncreasing()
        {
            var gate = new GenerationGate();
            var a = gate.Next();
            var b = gate.Next();
            var c = gate.Next();

            Assert.True(a < b);
            Assert.True(b < c);
        }

        [Fact]
        public void InOrderGenerationsAllCommit()
        {
            var gate = new GenerationGate();
            var g1 = gate.Next();
            var g2 = gate.Next();

            Assert.True(gate.TryCommit(g1));
            Assert.True(gate.TryCommit(g2));
        }

        [Fact]
        public void AnOlderGenerationArrivingAfterANewerOneIsRejected()
        {
            var gate = new GenerationGate();
            var older = gate.Next();
            var newer = gate.Next();

            // The newer write finishes first (e.g. a faster disk write for
            // a later-started recompute)...
            Assert.True(gate.TryCommit(newer));

            // ...and the older one, finishing late, must never win.
            Assert.False(gate.TryCommit(older));
        }

        [Fact]
        public void TheSameGenerationCommittedTwiceOnlySucceedsOnce()
        {
            var gate = new GenerationGate();
            var g = gate.Next();

            Assert.True(gate.TryCommit(g));
            Assert.False(gate.TryCommit(g));
        }

        [Fact]
        public void ANeverIssuedGenerationOfZeroIsRejectedOnceAnythingHasCommitted()
        {
            var gate = new GenerationGate();
            var g1 = gate.Next();
            Assert.True(gate.TryCommit(g1));

            // Generation 0 (or any value <= the committed high-water mark)
            // must never be accepted, even though Next() itself never
            // returns 0 (Interlocked.Increment starts at 1) — defends
            // against a caller passing an uninitialised default(long).
            Assert.False(gate.TryCommit(0));
        }
    }
}
