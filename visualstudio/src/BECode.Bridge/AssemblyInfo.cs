using System.Runtime.CompilerServices;

// Fix round 1: lets BECode.Bridge.Tests reference internal members directly —
// Tools.Format.SeveritiesFor (F3, tested the same direct way vscode's own
// format.test.ts tests severitiesFor), ToolOverrides (F6), and ToolRegistry's
// internal manifest-injection constructor and ToolManifest's internal
// assembly-injection overload (F5, proving the manifest/handler parity guard
// and the missing-resource guard actually throw, in both directions, rather
// than trusting that by accident).
[assembly: InternalsVisibleTo("BECode.Bridge.Tests")]
