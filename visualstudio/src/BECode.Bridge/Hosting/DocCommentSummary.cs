using System;
using System.Linq;
using System.Xml;
using System.Xml.Linq;

namespace BECode.Bridge.Hosting
{
    /// <summary>
    /// Host design §3.3, <c>HoverAsync</c>: "then the &lt;summary&gt; text of
    /// <c>symbol.GetDocumentationCommentXml()</c> when there is one."
    /// Parsing that XML fragment needs no Visual Studio or Roslyn type —
    /// <c>RoslynNavigator</c> passes in the raw XML string it already has.
    /// </summary>
    public static class DocCommentSummary
    {
        /// <summary>
        /// The &lt;summary&gt; element's text, whitespace-normalised (each
        /// line trimmed, blank lines dropped, the rest joined with single
        /// spaces — XML doc comments are indented to match the source, which
        /// is not part of the text itself). Null when there is no XML, it
        /// does not parse, or there is no &lt;summary&gt; element.
        /// </summary>
        public static string? Extract(string? xml)
        {
            if (string.IsNullOrWhiteSpace(xml))
            {
                return null;
            }

            XElement root;
            try
            {
                root = XElement.Parse(xml!);
            }
            catch (XmlException)
            {
                return null;
            }

            var summary = root.Name.LocalName == "summary" ? root : root.Element("summary");
            if (summary == null)
            {
                return null;
            }

            var lines = summary.Value
                .Split('\n')
                .Select(l => l.Trim())
                .Where(l => l.Length > 0);

            var text = string.Join(" ", lines);
            return text.Length == 0 ? null : text;
        }
    }
}
