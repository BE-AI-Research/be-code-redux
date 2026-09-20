using System.Collections.Generic;
using System.Text;
using System.Text.Json;
using System.Text.Json.Serialization;
using System.Threading;
using System.Threading.Tasks;

namespace BECode.Bridge
{
    /// <summary>
    /// A tool as advertised by <c>tools/list</c>. Property names carry
    /// <see cref="JsonPropertyNameAttribute"/> because the wire shape (and
    /// the vscode/tools.manifest.json this is compared against in a later
    /// task) is camelCase, not the C# record's PascalCase.
    /// </summary>
    public sealed record ToolInfo(
        [property: JsonPropertyName("name")] string Name,
        [property: JsonPropertyName("description")] string Description,
        [property: JsonPropertyName("inputSchema")] JsonElement InputSchema);

    /// <summary>The result of a tool call: text plus whether it is an error.</summary>
    public sealed record ToolResult(string Text, bool IsError);

    /// <summary>
    /// Implemented once, in a later task, by the tool registry the bridge
    /// dispatches <c>tools/call</c> to. <see cref="BridgeServer"/> depends
    /// only on this seam so the wire layer can be built and tested before
    /// any concrete tool exists.
    /// </summary>
    public interface IToolDispatcher
    {
        /// <summary>Never includes hidden tools.</summary>
        IReadOnlyList<ToolInfo> List();

        Task<ToolResult> CallAsync(string name, JsonElement args, object connection, CancellationToken ct);

        void ConnectionClosed(object connection);
    }

    /// <summary>The JSON-RPC error codes this wire contract fixes.</summary>
    internal static class JsonRpcCodes
    {
        public const int BadToken = -32001;
        public const int NotInitialized = -32002;
        public const int MethodNotFound = -32601;
    }

    /// <summary>
    /// Encodes JSON-RPC 2.0 reply frames. A success and an error reply are
    /// built as separate shapes (never one object carrying a null
    /// "result"/"error" alongside the other), matching the wire the Go
    /// client and the vscode extension already speak.
    /// </summary>
    internal static class JsonRpcWriter
    {
        private static readonly JsonSerializerOptions Options = new JsonSerializerOptions();

        public static byte[] EncodeResult(JsonElement id, object? result)
        {
            return Encode(new { jsonrpc = "2.0", id, result });
        }

        public static byte[] EncodeError(JsonElement id, int code, string message)
        {
            return Encode(new { jsonrpc = "2.0", id, error = new { code, message } });
        }

        private static byte[] Encode(object frame)
        {
            var json = JsonSerializer.Serialize(frame, Options);
            return Encoding.UTF8.GetBytes(json + "\n");
        }
    }
}
