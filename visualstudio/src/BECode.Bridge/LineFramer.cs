using System;
using System.Collections.Generic;
using System.IO;
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
    ///
    /// The pending buffer is a single growable array (doubled on demand,
    /// like <c>List&lt;T&gt;</c>'s own growth) rather than a full
    /// reallocate-and-copy on every <see cref="Push"/>, and each call only
    /// scans the bytes appended since the last call for '\n' — a byte is
    /// scanned once and copied (during compaction) at most once, so total
    /// work across the life of a connection is O(total bytes), not
    /// quadratic in the number of chunks one line arrives in. This runs
    /// before authentication, on attacker-controlled input, so it is also
    /// capped: see <see cref="LineFramer(int)"/>.
    /// </summary>
    public sealed class LineFramer
    {
        private const int InitialCapacity = 8 * 1024;

        private readonly int _maxLineBytes;
        private byte[] _buf;
        private int _length;   // valid bytes are _buf[0.._length)
        private int _scanned;  // _buf[0.._scanned) has already been scanned for '\n' and has none

        /// <param name="maxLineBytes">
        /// The most pending (unterminated) bytes this framer will hold
        /// before <see cref="Push"/> throws <see cref="InvalidDataException"/>.
        /// Defaults to 16 MiB.
        /// </param>
        public LineFramer(int maxLineBytes = 16 * 1024 * 1024)
        {
            if (maxLineBytes <= 0)
            {
                throw new ArgumentOutOfRangeException(nameof(maxLineBytes));
            }

            _maxLineBytes = maxLineBytes;
            _buf = new byte[Math.Min(InitialCapacity, maxLineBytes)];
            _length = 0;
            _scanned = 0;
        }

        /// <remarks>
        /// Not an iterator method: <paramref name="chunk"/> is a
        /// <see cref="ReadOnlySpan{T}"/>, which cannot be captured across a
        /// <c>yield return</c> boundary, and the cap must throw eagerly
        /// (from this call), not lazily when the result is enumerated.
        /// </remarks>
        public IEnumerable<string> Push(ReadOnlySpan<byte> chunk)
        {
            if (!chunk.IsEmpty)
            {
                Append(chunk);
            }

            var lines = new List<string>();
            int start = 0; // _buf[0..start) has already been extracted as complete lines this call
            int i = _scanned;
            for (; i < _length; i++)
            {
                if (_buf[i] != (byte)'\n')
                {
                    continue;
                }

                int len = i - start;
                if (len > 0 && _buf[start + len - 1] == (byte)'\r')
                {
                    len--;
                }

                if (len > 0)
                {
                    lines.Add(Encoding.UTF8.GetString(_buf, start, len));
                }

                start = i + 1;
            }

            if (start > 0)
            {
                // Compact: drop the consumed prefix. Everything left in
                // [0, remaining) was already scanned above (as part of this
                // very call) with no '\n' found in it, so scanning can
                // resume from the end next time rather than from 0.
                int remaining = _length - start;
                if (remaining > 0)
                {
                    Buffer.BlockCopy(_buf, start, _buf, 0, remaining);
                }

                _length = remaining;
                _scanned = remaining;
            }
            else
            {
                // No line completed this call: everything has been scanned
                // up to the current length, and nothing needs to move.
                _scanned = _length;
            }

            if (_length > _maxLineBytes)
            {
                // Reset so a discarded-but-still-referenced instance does
                // not keep holding the oversized buffer.
                _length = 0;
                _scanned = 0;
                _buf = new byte[Math.Min(InitialCapacity, _maxLineBytes)];
                throw new InvalidDataException($"line exceeds {_maxLineBytes} bytes");
            }

            return lines;
        }

        private void Append(ReadOnlySpan<byte> chunk)
        {
            int needed = _length + chunk.Length;
            if (needed > _buf.Length)
            {
                int newCapacity = _buf.Length > 0 ? _buf.Length : InitialCapacity;
                while (newCapacity < needed)
                {
                    // Guard against overflow on a pathological single chunk;
                    // the cap check right after Push's scan loop is what
                    // actually rejects an oversized line.
                    newCapacity = newCapacity > int.MaxValue / 2 ? needed : newCapacity * 2;
                }

                Array.Resize(ref _buf, newCapacity);
            }

            chunk.CopyTo(_buf.AsSpan(_length, chunk.Length));
            _length += chunk.Length;
        }
    }
}
