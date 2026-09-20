using System;
using System.Diagnostics;
using System.IO;
using System.Linq;
using System.Text;
using BECode.Bridge;
using Xunit;

namespace BECode.Bridge.Tests
{
    public class FramerTests
    {
        [Fact]
        public void MessageSplitAcrossThreePushCalls()
        {
            var framer = new LineFramer();
            var message = "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"ping\"}\n";
            var bytes = Encoding.UTF8.GetBytes(message);

            // Split into three arbitrary chunks, none of which lands on the
            // '\n' by itself.
            var third = bytes.Length / 3;
            var chunk1 = bytes.AsSpan(0, third);
            var chunk2 = bytes.AsSpan(third, third);
            var chunk3 = bytes.AsSpan(third * 2);

            var lines1 = framer.Push(chunk1).ToList();
            var lines2 = framer.Push(chunk2).ToList();
            var lines3 = framer.Push(chunk3).ToList();

            Assert.Empty(lines1);
            Assert.Empty(lines2);
            var produced = lines3.Single();
            Assert.Equal(message.TrimEnd('\n'), produced);
        }

        [Fact]
        public void TwoMessagesInOneChunk()
        {
            var framer = new LineFramer();
            var bytes = Encoding.UTF8.GetBytes("{\"a\":1}\n{\"b\":2}\n");

            var lines = framer.Push(bytes).ToList();

            Assert.Equal(new[] { "{\"a\":1}", "{\"b\":2}" }, lines);
        }

        [Fact]
        public void CarriageReturnLineFeedEndingsAreStripped()
        {
            var framer = new LineFramer();
            var bytes = Encoding.UTF8.GetBytes("{\"a\":1}\r\n{\"b\":2}\r\n");

            var lines = framer.Push(bytes).ToList();

            Assert.Equal(new[] { "{\"a\":1}", "{\"b\":2}" }, lines);
        }

        [Fact]
        public void MultiByteUtf8CharacterSplitAcrossChunks()
        {
            var framer = new LineFramer();
            // U+1F600 GRINNING FACE is a 4-byte UTF-8 sequence.
            var message = "{\"text\":\"\U0001F600\"}\n";
            var bytes = Encoding.UTF8.GetBytes(message);

            // Find the 4-byte emoji sequence and split the chunk in the
            // middle of it.
            var splitIndex = FindEmojiStart(bytes) + 2;

            var lines1 = framer.Push(bytes.AsSpan(0, splitIndex)).ToList();
            var lines2 = framer.Push(bytes.AsSpan(splitIndex)).ToList();

            Assert.Empty(lines1);
            var produced = lines2.Single();
            Assert.Equal(message.TrimEnd('\n'), produced);
        }

        private static int FindEmojiStart(byte[] bytes)
        {
            for (int i = 0; i < bytes.Length; i++)
            {
                // 0xF0 starts a 4-byte UTF-8 sequence.
                if (bytes[i] == 0xF0)
                {
                    return i;
                }
            }

            throw new System.InvalidOperationException("test fixture bug: no 4-byte UTF-8 sequence found");
        }

        [Fact]
        public void EmptyLineYieldsNothing()
        {
            var framer = new LineFramer();
            var bytes = Encoding.UTF8.GetBytes("\n{\"a\":1}\n\n");

            var lines = framer.Push(bytes).ToList();

            Assert.Equal(new[] { "{\"a\":1}" }, lines);
        }

        // A non-empty remainder must carry over
        // correctly into the next Push, not just the "everything up to the
        // last newline" case the other tests exercise.
        [Fact]
        public void NonEmptyRemainderCarriesOverToTheNextPush()
        {
            var framer = new LineFramer();

            var lines1 = framer.Push(Encoding.UTF8.GetBytes("a\nb")).ToList();
            var lines2 = framer.Push(Encoding.UTF8.GetBytes("c\n")).ToList();

            Assert.Equal(new[] { "a" }, lines1);
            Assert.Equal(new[] { "bc" }, lines2);
        }

        // A line whose unterminated byte count exceeds the cap must
        // throw eagerly, from Push itself (Push is not an iterator, so
        // there is nothing to defer to).
        [Fact]
        public void PendingBytesBeyondTheCapThrowInvalidDataException()
        {
            var framer = new LineFramer(maxLineBytes: 16);
            var chunk = Encoding.UTF8.GetBytes(new string('x', 20)); // no '\n', over the 16-byte cap

            var ex = Assert.Throws<InvalidDataException>(() => framer.Push(chunk).ToList());
            Assert.Contains("16", ex.Message);
        }

        // The cap must be enforced cumulatively across pushes too, not
        // just within a single chunk.
        [Fact]
        public void PendingBytesAccumulatedAcrossPushesBeyondTheCapThrow()
        {
            var framer = new LineFramer(maxLineBytes: 16);

            framer.Push(Encoding.UTF8.GetBytes(new string('x', 10))).ToList();
            Assert.Throws<InvalidDataException>(() => framer.Push(Encoding.UTF8.GetBytes(new string('y', 10))).ToList());
        }

        // Perf guard: an implementation that reallocates and copies
        // the *entire* pending buffer on every Push is quadratic in
        // the number of chunks for one long unterminated line. The size is
        // what gives the guard teeth: at 4 MiB the quadratic code took about
        // 2.2 s in a Debug build but only 0.9 s in Release, so a 2 s bound
        // let it through. At 16 MiB it needs about 14 s even in Release,
        // against roughly half a second for the linear code, so a 4 s bound
        // is far from both.
        [Fact]
        public void SixteenMebibyteLineInEightKibibyteChunksIsFast()
        {
            // The cap is not under test here; keep it well clear of the line.
            var framer = new LineFramer(64 * 1024 * 1024);
            var line = new byte[16 * 1024 * 1024];
            new Random(1234).NextBytes(line);
            // Make sure there is no accidental '\n' (0x0A) in the payload,
            // so this really is one long unterminated line until the final
            // chunk.
            for (int i = 0; i < line.Length; i++)
            {
                if (line[i] == (byte)'\n')
                {
                    line[i] = (byte)'x';
                }
            }

            const int chunkSize = 8 * 1024;
            var stopwatch = Stopwatch.StartNew();
            System.Collections.Generic.List<string> lastLines = new System.Collections.Generic.List<string>();
            for (int offset = 0; offset < line.Length; offset += chunkSize)
            {
                var len = Math.Min(chunkSize, line.Length - offset);
                lastLines = framer.Push(line.AsSpan(offset, len)).ToList();
            }
            lastLines = framer.Push(Encoding.UTF8.GetBytes("\n")).ToList();
            stopwatch.Stop();

            Assert.Single(lastLines);
            Assert.True(stopwatch.ElapsedMilliseconds < 4000, $"expected well under 4000ms, took {stopwatch.ElapsedMilliseconds}ms");
        }
    }
}
