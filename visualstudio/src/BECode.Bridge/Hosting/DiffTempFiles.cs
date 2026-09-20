using System.IO;

namespace BECode.Bridge.Hosting
{
    /// <summary>
    /// Host design §3.5, <c>ReviewDiffAsync</c>: "Right side:
    /// <c>request.Proposed</c> in a temp file named
    /// <c>&lt;name&gt;.proposed&lt;ext&gt;</c> so the language service
    /// colours it." Only the NAME transform is pure/editor-neutral;
    /// <c>DiffReview</c> decides the temp directory and uniqueness itself
    /// (it needs a real, VS-specific place to write to).
    /// </summary>
    public static class DiffTempFiles
    {
        /// <summary>"Foo.cs" → "Foo.proposed.cs"; "Makefile" (no extension) → "Makefile.proposed". <paramref name="path"/>'s directory is ignored — callers pass just the file name, or the full path; only the name is used.</summary>
        public static string ProposedFileName(string path)
        {
            var name = Path.GetFileNameWithoutExtension(path);
            var ext = Path.GetExtension(path);
            return string.IsNullOrEmpty(ext) ? name + ".proposed" : name + ".proposed" + ext;
        }
    }
}
