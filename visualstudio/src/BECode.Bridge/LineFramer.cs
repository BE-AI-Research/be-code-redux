using System;
using System.Collections.Generic;
using System.Text;

namespace BECode.Bridge
{
    /// <summary>
    /// Turns a stream of raw byte chunks into complete newline-delimited
    /// lines. Bytes that arrive without a trailing '\n' are held until the
    /// rest of the line shows up in a later <see cref="Push"/> call.
    ///
    /// Lines are only decoded as UTF-8 once a full line (up to '\n') has
    /// been assembled from raw bytes, so a multi-byte UTF-8 character split
    /// across two chunks is never decoded from a partial byte sequence:
    /// '\n' (0x0A) can never occur as a UTF-8 continuation byte (those are
    /// 0x80-0xBF), so splitting on the raw byte is always safe.
    /// </summary>
    public sealed class LineFramer
    {
        private byte[] _pending = new byte[0];

        public IEnumerable<string> Push(ReadOnlySpan<byte> chunk)
        {
            if (!chunk.IsEmpty)
            {
                var combined = new byte[_pending.Length + chunk.Length];
                Buffer.BlockCopy(_pending, 0, combined, 0, _pending.Length);
                chunk.CopyTo(combined.AsSpan(_pending.Length));
                _pending = combined;
            }

            var lines = new List<string>();
            int start = 0;
            for (int i = 0; i < _pending.Length; i++)
            {
                if (_pending[i] != (byte)'\n')
                {
                    continue;
                }

                int len = i - start;
                if (len > 0 && _pending[start + len - 1] == (byte)'\r')
                {
                    len--;
                }

                if (len > 0)
                {
                    lines.Add(Encoding.UTF8.GetString(_pending, start, len));
                }

                start = i + 1;
            }

            if (start > 0)
            {
                var remainder = new byte[_pending.Length - start];
                Buffer.BlockCopy(_pending, start, remainder, 0, remainder.Length);
                _pending = remainder;
            }

            return lines;
        }
    }
}
