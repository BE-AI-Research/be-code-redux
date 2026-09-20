using System;
using System.Collections.Generic;
using System.IO;
using System.Linq;
using System.Runtime.InteropServices;

namespace BECode.Bridge
{
    /// <summary>A path argument resolved outside every workspace folder. Never allowed to reach <see cref="IEditorHost"/>.</summary>
    public sealed class PathOutsideWorkspaceException : Exception
    {
        public PathOutsideWorkspaceException(string message) : base(message)
        {
        }
    }

    /// <summary>
    /// Ports vscode/src/lib/paths.ts's <c>absPath</c>/<c>relPath</c>: every
    /// tool argument that names a file is resolved and confined here before
    /// it reaches <see cref="IEditorHost"/>, exactly as BE-Code's own file
    /// tools are confined to their workspace root.
    /// </summary>
    public static class Paths
    {
        private static readonly StringComparison PathComparison =
            RuntimeInformation.IsOSPlatform(OSPlatform.Windows) ? StringComparison.OrdinalIgnoreCase : StringComparison.Ordinal;

        /// <summary>
        /// Turns a tool argument into an absolute path inside the workspace.
        /// A relative path is resolved against the first workspace folder
        /// that actually has such a file (falling back to the first folder
        /// for one that does not exist yet). Throws
        /// <see cref="PathOutsideWorkspaceException"/> for anything that
        /// resolves outside every folder — the caller must never pass that
        /// path to the host.
        /// </summary>
        public static string AbsPath(IReadOnlyList<string> folders, string p)
        {
            var roots = folders.Count > 0 ? folders : new[] { Directory.GetCurrentDirectory() };

            string abs;
            if (Path.IsPathRooted(p))
            {
                abs = Path.GetFullPath(p);
            }
            else
            {
                var match = roots.FirstOrDefault(f => File.Exists(Path.Combine(f, p)) || Directory.Exists(Path.Combine(f, p)));
                abs = Path.GetFullPath(Path.Combine(match ?? roots[0], p));
            }

            if (!roots.Any(f => Inside(f, abs)))
            {
                throw new PathOutsideWorkspaceException("path is outside the workspace");
            }

            return abs;
        }

        /// <summary>
        /// Reports <paramref name="fsPath"/> relative to whichever workspace
        /// folder contains it (forward slashes always, matching vscode's
        /// <c>.split("\\").join("/")</c>), or the path unchanged when it lies
        /// outside every folder.
        /// </summary>
        public static string RelPath(IReadOnlyList<string> folders, string fsPath)
        {
            foreach (var folder in folders)
            {
                var rel = TryRelative(folder, fsPath);
                if (!string.IsNullOrEmpty(rel) && !rel!.StartsWith("..", StringComparison.Ordinal))
                {
                    return rel;
                }
            }

            return fsPath;
        }

        // "Inside" the folder itself, or beneath it. The separator matters:
        // "/ws-evil/x" must not count as inside "/ws".
        private static bool Inside(string folder, string abs)
        {
            return RelativeIfInside(folder, abs) != null;
        }

        // Fix round 1, S3: the original implementation round-tripped
        // relative paths through System.Uri (MakeRelativeUri, then
        // Uri.UnescapeDataString(relUri.ToString())) to get forward
        // slashes without depending on Path.GetRelativePath (not part of
        // the netstandard2.0 surface this project targets). Two latent
        // Windows defects followed from that: (1) Uri.ToString() already
        // unescapes "safe" characters, so unescaping it AGAIN corrupted any
        // file name containing a literal '%' sequence that happens to look
        // like percent-encoding (e.g. "file%41.txt" came back "fileA.txt");
        // (2) MakeRelativeUri across two different Windows drives produces
        // an absolute file:// URI, not a relative one, which the caller's
        // ".." guard was supposed to reject via TS's `!isAbsolute(r)` but
        // never actually ran against, since nothing checked for it here.
        // Ported by hand instead: pure string/segment work on
        // Path.GetFullPath output, no escaping to get wrong, and an
        // explicit root comparison that returns null (no relative path
        // exists) across two different roots instead of ever producing an
        // absolute path as a "relative" answer.
        private static string? RelativeIfInside(string folder, string abs)
        {
            var f = Path.GetFullPath(folder);

            if (!string.Equals(Path.GetPathRoot(f), Path.GetPathRoot(abs), PathComparison))
            {
                // Different roots (e.g. different drives on Windows): there
                // is no relative path between them.
                return null;
            }

            if (string.Equals(abs, f, PathComparison))
            {
                return string.Empty;
            }

            var withSep = f.EndsWith(Path.DirectorySeparatorChar.ToString(), StringComparison.Ordinal)
                ? f
                : f + Path.DirectorySeparatorChar;
            if (!abs.StartsWith(withSep, PathComparison))
            {
                return null;
            }

            return abs.Substring(withSep.Length).Replace('\\', '/');
        }

        private static string? TryRelative(string folder, string fsPath)
        {
            string abs;
            try
            {
                abs = Path.GetFullPath(fsPath);
            }
            catch
            {
                return null;
            }

            return RelativeIfInside(folder, abs);
        }
    }
}
