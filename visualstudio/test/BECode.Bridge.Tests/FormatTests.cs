using BECode.Bridge.Tools;
using Xunit;

namespace BECode.Bridge.Tests
{
    // Ports vscode/test/format.test.ts's severitiesFor cases directly against
    // BECode.Bridge.Tools.Format.SeveritiesFor (F3).
    public class FormatTests
    {
        [Fact]
        public void SeveritiesForDefaultsToErrorsAndWarnings()
        {
            Assert.Equal(new[] { "error", "warning" }, Format.SeveritiesFor(null));
        }

        [Fact]
        public void SeveritiesForNarrowsToErrorOnly()
        {
            Assert.Equal(new[] { "error" }, Format.SeveritiesFor("error"));
        }

        [Fact]
        public void SeveritiesForNarrowsToWarningOnly()
        {
            Assert.Equal(new[] { "warning" }, Format.SeveritiesFor("warning"));
        }

        [Fact]
        public void SeveritiesForExpandsToEverythingForAll()
        {
            Assert.Equal(new[] { "error", "warning", "info", "hint" }, Format.SeveritiesFor("all"));
        }

        [Fact]
        public void SeveritiesForFallsBackToErrorsAndWarningsForAnUnrecognisedValue()
        {
            // Ruling R-6: the manifest's own description ("severity: error,
            // warning or all (default: errors and warnings)") and vscode's
            // severitiesFor both treat anything else as the default.
            Assert.Equal(new[] { "error", "warning" }, Format.SeveritiesFor("bogus"));
        }
    }
}
