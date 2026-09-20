using System;
using System.Collections.Generic;
using System.Text.Json;

namespace BECode.Bridge.Hosting
{
    /// <summary>
    /// Host design §4, <c>ConfigsAsync</c>: "launchSettings.json profiles
    /// appear as '&lt;project&gt; › &lt;profile&gt;' ('launch profile') so
    /// the model can see them." Parsing the profile NAMES out of a
    /// launchSettings.json document needs no Visual Studio type —
    /// <c>VisualStudioDebugHost</c> reads the file (a VS-specific concern:
    /// which project's <c>Properties/launchSettings.json</c>) and hands the
    /// raw text here.
    /// </summary>
    public static class LaunchProfiles
    {
        /// <summary>The top-level "profiles" object's own key names, in document order. Empty (never throws) for anything that is not a well-formed launchSettings.json.</summary>
        public static IReadOnlyList<string> ParseNames(string? launchSettingsJson)
        {
            if (string.IsNullOrWhiteSpace(launchSettingsJson))
            {
                return Array.Empty<string>();
            }

            try
            {
                using var doc = JsonDocument.Parse(launchSettingsJson!);
                if (!doc.RootElement.TryGetProperty("profiles", out var profiles) || profiles.ValueKind != JsonValueKind.Object)
                {
                    return Array.Empty<string>();
                }

                var names = new List<string>();
                foreach (var prop in profiles.EnumerateObject())
                {
                    names.Add(prop.Name);
                }

                return names;
            }
            catch (JsonException)
            {
                return Array.Empty<string>();
            }
        }
    }
}
