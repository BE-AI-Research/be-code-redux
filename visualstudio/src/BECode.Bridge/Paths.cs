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
            var f = Path.GetFullPath(folder);
            if (string.Equals(abs, f, PathComparison))
            {
                return true;
            }

            var withSep = f.EndsWith(Path.DirectorySeparatorChar.ToString(), StringComparison.Ordinal)
                ? f
                : f + Path.DirectorySeparatorChar;
            return abs.StartsWith(withSep, PathComparison);
        }

        // Uri.MakeRelativeUri gives a forward-slash-separated relative path
        // without depending on Path.GetRelativePath, which is not part of
        // the netstandard2.0 surface this project targets.
        private static string? TryRelative(string folder, string fsPath)
        {
            try
            {
                var folderFull = Path.GetFullPath(folder);
                var withSep = folderFull.EndsWith(Path.DirectorySeparatorChar.ToString(), StringComparison.Ordinal)
                    ? folderFull
                    : folderFull + Path.DirectorySeparatorChar;
                var folderUri = new Uri(withSep);
                var fileUri = new Uri(Path.GetFullPath(fsPath));
                if (folderUri.Scheme != fileUri.Scheme)
                {
                    return null;
                }

                var relUri = folderUri.MakeRelativeUri(fileUri);
                return Uri.UnescapeDataString(relUri.ToString());
            }
            catch
            {
                return null;
            }
        }
    }
}
