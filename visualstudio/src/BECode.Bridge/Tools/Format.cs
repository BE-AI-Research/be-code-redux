using System.Collections.Generic;
using System.Linq;

namespace BECode.Bridge.Tools
{
    /// <summary>Ports vscode/src/lib/format.ts's <c>formatDiagnostics</c> and <c>severitiesFor</c> (used by <c>diagnostics</c>).</summary>
    internal static class Format
    {
        /// <summary>
        /// The default (null or unrecognised)
        /// is errors and warnings — the manifest's own description says so
        /// ("severity: error, warning or all (default: errors and
        /// warnings)"), and this is vscode/src/lib/format.ts's
        /// <c>severitiesFor</c> verbatim; do not change the default to "all".
        /// </summary>
        public static IReadOnlyList<string> SeveritiesFor(string? severity)
        {
            if (severity == "all")
            {
                return new[] { "error", "warning", "info", "hint" };
            }

            if (severity == "error")
            {
                return new[] { "error" };
            }

            if (severity == "warning")
            {
                return new[] { "warning" };
            }

            return new[] { "error", "warning" };
        }

        public static string Diagnostics(IReadOnlyList<Diagnostic> items)
        {
            if (items.Count == 0)
            {
                return "no diagnostics";
            }

            var byFile = new Dictionary<string, List<Diagnostic>>(System.StringComparer.Ordinal);
            foreach (var d in items)
            {
                if (!byFile.TryGetValue(d.Path, out var list))
                {
                    list = new List<Diagnostic>();
                    byFile[d.Path] = list;
                }

                list.Add(d);
            }

            var errors = items.Count(d => d.Severity == "error");
            var warnings = items.Count(d => d.Severity == "warning");

            string Plural(int n, string word) => $"{n} {word}{(n == 1 ? "" : "s")}";

            var lines = new List<string>
            {
                $"{Plural(errors, "error")}, {Plural(warnings, "warning")} in {byFile.Count} file{(byFile.Count == 1 ? "" : "s")}",
            };

            foreach (var file in byFile.Keys.OrderBy(f => f, System.StringComparer.Ordinal))
            {
                foreach (var d in byFile[file].OrderBy(d => d.Line))
                {
                    var firstLine = d.Message.Split('\n')[0];
                    var source = string.IsNullOrEmpty(d.Source) ? "-" : d.Source;
                    lines.Add($"{d.Path}:{d.Line}:{d.Col} {d.Severity} {source}: {firstLine}");
                }
            }

            return string.Join("\n", lines);
        }
    }
}
