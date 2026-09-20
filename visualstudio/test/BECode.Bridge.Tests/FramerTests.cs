using System;
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
    }
}
