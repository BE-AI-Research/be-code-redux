using System.Runtime.CompilerServices;

// Lets BECode.Bridge.Tests reference internal members directly —
// Tools.Format.SeveritiesFor (tested the same direct way vscode's own
// format.test.ts tests severitiesFor), ToolOverrides, and ToolRegistry's
// internal manifest-injection constructor and ToolManifest's internal
// assembly-injection overload, proving the manifest/handler parity guard
// and the missing-resource guard actually throw, in both directions, rather
// than trusting that by accident.
[assembly: InternalsVisibleTo("BECode.Bridge.Tests")]
