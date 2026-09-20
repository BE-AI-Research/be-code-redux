namespace BECode.Bridge.Hosting
{
    /// <summary>
    /// Host design §3.4 (<c>DiagnosticsAsync</c>/<c>ErrorListReader</c>):
    /// "<c>Source</c> = the leading error code parsed from
    /// <c>Description</c> when it looks like <c>CS1002:</c>/<c>C2065</c>,
    /// else the project name, else <c>-</c>." Only the parsing itself is
    /// pure/editor-neutral; the fallback chain (project name, then "-") is
    /// <c>ErrorListReader</c>'s own job since it needs the <c>ErrorItem</c>.
    /// </summary>
    public static class ErrorSource
    {
        /// <summary>
        /// Returns the leading error code (e.g. "CS1002", "C2065",
        /// "MSB3073") when <paramref name="description"/> starts with one
        /// followed by a colon, else null. A code is recognised as one or
        /// more letters immediately followed by one or more digits, with no
        /// embedded whitespace — deliberately conservative: a description
        /// that merely happens to contain a colon later on (ordinary
        /// prose) must not be misread as a code.
        /// </summary>
        public static string? ParseCode(string? description)
        {
            if (string.IsNullOrEmpty(description))
            {
                return null;
            }

            var colon = description!.IndexOf(':');
            if (colon <= 0)
            {
                return null;
            }

            var candidate = description.Substring(0, colon).Trim();
            if (candidate.Length == 0 || candidate.Length > 12 || candidate != description.Substring(0, colon))
            {
                // Leading/trailing whitespace before the colon disqualifies
                // it too (Trim() changed it): a real error code sits flush
                // against the colon ("CS1002:"), not "CS1002 :" or " CS1002:".
                return null;
            }

            var i = 0;
            while (i < candidate.Length && char.IsLetter(candidate[i]))
            {
                i++;
            }

            if (i == 0 || i == candidate.Length)
            {
                return null;
            }

            for (var j = i; j < candidate.Length; j++)
            {
                if (!char.IsDigit(candidate[j]))
                {
                    return null;
                }
            }

            return candidate;
        }
    }
}
