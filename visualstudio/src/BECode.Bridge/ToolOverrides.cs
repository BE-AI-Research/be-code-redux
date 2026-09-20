using System;
using System.Collections.Generic;

namespace BECode.Bridge
{
    /// <summary>
    /// Fix round 1, F6 (Ruling R-9): the shared manifest's
    /// <c>debug_start</c>/<c>debug_configs</c> descriptions tell the model to
    /// use vscode's <c>program</c>+<c>type</c> launch shape, which this
    /// bridge refuses — Visual Studio debugs the startup project instead.
    /// <see cref="ToolRegistry"/> overrides EXACTLY these two names'
    /// descriptions when building <see cref="ToolRegistry.List"/>; schemas
    /// and names are never overridden, and every other tool's description is
    /// the manifest's own, verbatim.
    /// </summary>
    internal static class ToolOverrides
    {
        public static readonly IReadOnlyDictionary<string, string> Descriptions = new Dictionary<string, string>(StringComparer.Ordinal)
        {
            ["debug_configs"] = "List what Visual Studio can debug: the solution's startup projects and their launch profiles. Pass a name to debug_start as config.",
            ["debug_start"] = "Start debugging in Visual Studio. Use config (a startup project or launch profile name from debug_configs), or omit it to debug the current startup project. program, type and args are not supported here: Visual Studio debugs the startup project with its launch profile's arguments. Returns where execution stopped.",
        };
    }
}
