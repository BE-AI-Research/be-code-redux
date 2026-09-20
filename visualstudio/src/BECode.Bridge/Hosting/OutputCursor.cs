using System;
using System.Collections.Generic;
using System.Linq;

namespace BECode.Bridge.Hosting
{
    /// <summary>
    /// Host design §4, <c>OutputAsync</c>: "return lines from
    /// <c>since</c>, new cursor = line count. If the pane was cleared
    /// (count &lt; <c>since</c>), start again from 0." Mirrors vscode's
    /// <c>RingLog.since</c>. Splitting the WHOLE pane's text into lines and
    /// slicing from a cursor needs no Visual Studio type, so it lives here,
    /// pure and testable; <c>ErrorListReader</c>'s caller (<c>DebugHost</c>'s
    /// <c>OutputAsync</c>) does only the VS-specific part: reading the whole
    /// pane's text.
    /// </summary>
    public static class OutputCursor
    {
        public static (IReadOnlyList<string> Lines, int Cursor) Since(string? wholeText, int since)
        {
            var lines = SplitLines(wholeText);

            // Pane cleared since the caller's last cursor: start over from 0
            // rather than returning nothing (or throwing) for a cursor that
            // no longer makes sense against a shorter pane.
            if (lines.Count < since)
            {
                since = 0;
            }

            if (since < 0)
            {
                since = 0;
            }

            IReadOnlyList<string> result = since >= lines.Count
                ? Array.Empty<string>()
                : lines.Skip(since).ToArray();

            return (result, lines.Count);
        }

        private static List<string> SplitLines(string? text)
        {
            if (string.IsNullOrEmpty(text))
            {
                return new List<string>();
            }

            var normalized = text!.Replace("\r\n", "\n").Replace("\r", "\n");
            var parts = normalized.Split('\n');
            var list = new List<string>(parts);

            // A trailing newline produces one empty trailing element that
            // does not correspond to a real line the pane is showing; drop
            // it so the line count matches what the user sees.
            if (list.Count > 0 && list[list.Count - 1].Length == 0)
            {
                list.RemoveAt(list.Count - 1);
            }

            return list;
        }
    }
}
