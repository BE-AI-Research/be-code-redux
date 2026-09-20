using System;
using System.IO;
using System.Linq;
using System.Text.Json;
using System.Text.RegularExpressions;
using System.Xml.Linq;
using Xunit;

namespace BECode.Bridge.Tests
{
    // BE-Code has two editor extensions, and both package to a ".vsix". They
    // keep separate version numbers on purpose, so the only thing telling a
    // person which file is for which editor is its name. These tests read the
    // files people actually see.
    public class ExtensionIdentityTests
    {
        private static string RepoRoot()
        {
            var dir = new DirectoryInfo(AppContext.BaseDirectory);
            while (dir != null && !File.Exists(Path.Combine(dir.FullName, "build.mk")))
            {
                dir = dir.Parent;
            }

            Assert.True(dir != null, "could not find the repository root (build.mk) above " + AppContext.BaseDirectory);
            return dir!.FullName;
        }

        private static string VsDir => Path.Combine(RepoRoot(), "visualstudio", "src", "BECode.VisualStudio");

        private static XElement VsixMetadata()
        {
            var doc = XDocument.Load(Path.Combine(VsDir, "source.extension.vsixmanifest"));
            XNamespace ns = "http://schemas.microsoft.com/developer/vsx-schema/2011";
            return doc.Root!.Element(ns + "Metadata")!;
        }

        // The Visual Studio extension's version is written in three places: the
        // manifest (what the installer and the Extensions dialog show), the
        // package (what the lock file and serverInfo report) and the project
        // (what names the .vsix file). Nothing generates one from another.
        [Fact]
        public void TheVisualStudioExtensionsVersionAgreesEverywhereItIsWritten()
        {
            XNamespace ns = "http://schemas.microsoft.com/developer/vsx-schema/2011";
            var manifest = VsixMetadata().Element(ns + "Identity")!.Attribute("Version")!.Value;

            var package = File.ReadAllText(Path.Combine(VsDir, "BECodePackage.cs"));
            var inPackage = Regex.Match(package, "const string Version = \"([^\"]+)\"");
            Assert.True(inPackage.Success, "BECodePackage.cs no longer declares `const string Version`");

            var project = XDocument.Load(Path.Combine(VsDir, "BECode.VisualStudio.csproj"));
            var inProject = project.Root!.Descendants("BECodeExtensionVersion").FirstOrDefault()?.Value;
            Assert.False(string.IsNullOrEmpty(inProject), "BECode.VisualStudio.csproj no longer declares <BECodeExtensionVersion>");

            Assert.Equal(manifest, inPackage.Groups[1].Value);
            Assert.Equal(manifest, inProject);
        }

        [Fact]
        public void EachExtensionSaysWhichEditorItIsFor()
        {
            XNamespace ns = "http://schemas.microsoft.com/developer/vsx-schema/2011";
            Assert.Equal("BE-Code for Visual Studio", VsixMetadata().Element(ns + "DisplayName")!.Value);

            using var pkg = JsonDocument.Parse(File.ReadAllText(Path.Combine(RepoRoot(), "vscode", "package.json")));
            Assert.Equal("BE-Code for VS Code", pkg.RootElement.GetProperty("displayName").GetString());
            // The id stays: publisher.name is how VS Code recognises an
            // installed extension, and renaming it would orphan every install.
            Assert.Equal("be-code", pkg.RootElement.GetProperty("name").GetString());
        }

        [Fact]
        public void EachPackageFileIsNamedForItsEditor()
        {
            using var pkg = JsonDocument.Parse(File.ReadAllText(Path.Combine(RepoRoot(), "vscode", "package.json")));
            var script = pkg.RootElement.GetProperty("scripts").GetProperty("package").GetString();
            Assert.Contains("be-code-vscode-$npm_package_version.vsix", script, StringComparison.Ordinal);

            var project = File.ReadAllText(Path.Combine(VsDir, "BECode.VisualStudio.csproj"));
            Assert.Contains("be-code-visualstudio-$(BECodeExtensionVersion).vsix", project, StringComparison.Ordinal);

            var build = File.ReadAllText(Path.Combine(RepoRoot(), "visualstudio", "build.ps1"));
            Assert.Contains("be-code-visualstudio-", build, StringComparison.Ordinal);
        }
    }
}
