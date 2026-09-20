using System.Text.Json;

namespace BECode.Bridge.Tools
{
    /// <summary>
    /// Argument parsing shared by every tool family. Unlike vscode's TS
    /// handlers, which trust the caller's JSON.stringify-typed values at
    /// runtime, these validate a required argument's presence and type
    /// explicitly, naming it in the error text — the design brief's own
    /// required test case ("required arguments missing is isError:true naming
    /// the argument"), and a deliberate strictness the TS side gets for free
    /// only from the model happening to have filled the schema in correctly.
    /// </summary>
    internal static class ToolArgs
    {
        public static bool TryRequireString(JsonElement args, string name, out string value, out ToolResult? error)
        {
            if (args.ValueKind == JsonValueKind.Object
                && args.TryGetProperty(name, out var el)
                && el.ValueKind == JsonValueKind.String)
            {
                var s = el.GetString();
                if (!string.IsNullOrEmpty(s))
                {
                    value = s!;
                    error = null;
                    return true;
                }
            }

            value = "";
            error = new ToolResult($"'{name}' is required", true);
            return false;
        }

        /// <summary>
        /// Like <see cref="TryRequireString"/>, but an empty string is a
        /// legal value (fix round 1, F4: <c>review_diff</c>'s <c>proposed</c>
        /// argument — proposing an emptied file is legal, "the argument is
        /// missing" and "the argument is the empty string" are different
        /// facts). Only the argument's presence and type are required.
        /// </summary>
        public static bool TryRequireAnyString(JsonElement args, string name, out string value, out ToolResult? error)
        {
            if (args.ValueKind == JsonValueKind.Object
                && args.TryGetProperty(name, out var el)
                && el.ValueKind == JsonValueKind.String)
            {
                value = el.GetString() ?? "";
                error = null;
                return true;
            }

            value = "";
            error = new ToolResult($"'{name}' is required", true);
            return false;
        }

        public static bool TryRequireInt(JsonElement args, string name, out int value, out ToolResult? error)
        {
            if (args.ValueKind == JsonValueKind.Object
                && args.TryGetProperty(name, out var el)
                && el.ValueKind == JsonValueKind.Number
                && el.TryGetInt32(out var n))
            {
                value = n;
                error = null;
                return true;
            }

            value = 0;
            error = new ToolResult($"'{name}' is required", true);
            return false;
        }

        public static string? GetString(JsonElement args, string name)
        {
            if (args.ValueKind == JsonValueKind.Object
                && args.TryGetProperty(name, out var el)
                && el.ValueKind == JsonValueKind.String)
            {
                return el.GetString();
            }

            return null;
        }

        public static int? GetInt(JsonElement args, string name)
        {
            if (args.ValueKind == JsonValueKind.Object
                && args.TryGetProperty(name, out var el)
                && el.ValueKind == JsonValueKind.Number
                && el.TryGetInt32(out var n))
            {
                return n;
            }

            return null;
        }

        public static bool GetBool(JsonElement args, string name, bool defaultValue = false)
        {
            if (args.ValueKind == JsonValueKind.Object && args.TryGetProperty(name, out var el))
            {
                if (el.ValueKind == JsonValueKind.True)
                {
                    return true;
                }

                if (el.ValueKind == JsonValueKind.False)
                {
                    return false;
                }
            }

            return defaultValue;
        }
    }
}
