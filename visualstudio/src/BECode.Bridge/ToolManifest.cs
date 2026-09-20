using System;
using System.Collections.Generic;
using System.Text.Json;

namespace BECode.Bridge
{
    /// <summary>
    /// One entry from vscode/tools.manifest.json (Task 1): name, description
    /// and input schema come from there verbatim, never retyped here.
    /// <see cref="Hidden"/> marks a tool callable through <c>tools/call</c>
    /// but omitted from <c>tools/list</c> (<c>review_diff</c>/<c>review_cancel</c>).
    /// </summary>
    public sealed class ManifestEntry
    {
        public string Name { get; }
        public string Description { get; }
        public JsonElement InputSchema { get; }
        public bool Hidden { get; }

        public ManifestEntry(string name, string description, JsonElement inputSchema, bool hidden)
        {
            Name = name;
            Description = description;
            InputSchema = inputSchema;
            Hidden = hidden;
        }
    }

    /// <summary>
    /// Reads the embedded copy of vscode/tools.manifest.json back out of this
    /// assembly. Embedded (rather than read from disk) so the manifest ships
    /// inside BECode.Bridge.dll exactly like every other Visual Studio
    /// extension asset, with no runtime dependency on the vscode/ checkout
    /// being present next to it.
    /// </summary>
    public static class ToolManifest
    {
        private const string ResourceName = "BECode.Bridge.tools.manifest.json";

        public static IReadOnlyList<ManifestEntry> Load()
        {
            var assembly = typeof(ToolManifest).Assembly;
            using var stream = assembly.GetManifestResourceStream(ResourceName)
                ?? throw new InvalidOperationException($"embedded resource '{ResourceName}' not found in {assembly.FullName}");

            using var doc = JsonDocument.Parse(stream);
            var entries = new List<ManifestEntry>();
            foreach (var el in doc.RootElement.EnumerateArray())
            {
                var name = el.GetProperty("name").GetString()
                    ?? throw new InvalidOperationException("manifest entry has no 'name'");
                var description = el.TryGetProperty("description", out var d) ? d.GetString() ?? "" : "";
                var schema = el.GetProperty("inputSchema").Clone();
                var hidden = el.TryGetProperty("hidden", out var h) && h.ValueKind == JsonValueKind.True;
                entries.Add(new ManifestEntry(name, description, schema, hidden));
            }

            return entries;
        }
    }
}
