using System;
using System.Collections.Generic;
using System.IO;

namespace BECode.Bridge.Hosting
{
    /// <summary>
    /// Fix round 1, I-10: roots a bare or relative file name — an MSBuild
    /// <c>ErrorItem.FileName</c> is often project-relative or bare, not
    /// absolute — against a list of candidate directories, tried in order,
    /// the first one where the combined path actually EXISTS wins. When
    /// <paramref name="fileName"/> is already rooted it is returned as-is
    /// (never re-combined with a candidate). When none of the candidates'
    /// combined paths exist, the first non-empty candidate's combined path
    /// is still returned (never null for a non-empty input) — the seam
    /// (<c>IEditorHost.DiagnosticsAsync</c>) requires an absolute path out,
    /// even for a diagnostic whose exact file cannot be found on disk.
    /// </summary>
    public static class PathRooting
    {
        public static string? Root(string? fileName, IEnumerable<string?> candidateDirectories, Func<string, bool> fileExists)
        {
            if (fileExists == null)
            {
                throw new ArgumentNullException(nameof(fileExists));
            }

            if (string.IsNullOrEmpty(fileName))
            {
                return fileName;
            }

            if (Path.IsPathRooted(fileName))
            {
                return fileName;
            }

            string? firstCandidate = null;

            foreach (var dir in candidateDirectories)
            {
                if (string.IsNullOrEmpty(dir))
                {
                    continue;
                }

                string combined;
                try
                {
                    combined = Path.Combine(dir!, fileName!);
                }
                catch (ArgumentException)
                {
                    // A candidate directory or the file name itself carries
                    // an invalid path character — skip it rather than fail
                    // the whole lookup.
                    continue;
                }

                if (firstCandidate == null)
                {
                    firstCandidate = combined;
                }

                if (fileExists(combined))
                {
                    return combined;
                }
            }

            // None of the candidates' combined paths existed — the best
            // still-absolute guess is the first one tried.
            return firstCandidate ?? fileName;
        }
    }
}
