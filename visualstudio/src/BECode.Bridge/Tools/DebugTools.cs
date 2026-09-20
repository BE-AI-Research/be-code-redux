using System;
using System.Collections.Generic;
using System.Linq;
using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;

namespace BECode.Bridge.Tools
{
    /// <summary>
    /// The ten debug_* tools, against <see cref="IEditorHost.Debug"/>. Ports
    /// vscode/src/tools/debug.ts's <c>DebugManager</c>, with the design
    /// spec's Visual Studio adaptations (section 4): <c>debug_configs</c>
    /// lists startup projects/launch profiles instead of <c>launch.json</c>
    /// entries, and <c>debug_start</c> refuses vscode's ad-hoc
    /// program+type launch shape with a clear error rather than attempting
    /// it — Visual Studio debugs the solution's startup project.
    /// </summary>
    public sealed class DebugTools
    {
        // Text only: the 60s wait itself is IDebugHost's job (vscode's own
        // WAIT_MS), not the tool's — see StartAsync/ContinueAsync/StepAsync's
        // doc comment on IEditorHost.
        private const int WaitSeconds = 60;

        private readonly IEditorHost _host;

        public DebugTools(IEditorHost host)
        {
            _host = host;
        }

        public async Task<ToolResult> Configs(JsonElement args, object connection, CancellationToken ct)
        {
            var configs = await _host.Debug.ConfigsAsync(ct).ConfigureAwait(false);
            if (configs.Count == 0)
            {
                return new ToolResult("no startup projects or launch profiles found", false);
            }

            return new ToolResult(string.Join("\n", configs.Select(c => $"{c.Name} ({c.Kind})")), false);
        }

        public async Task<ToolResult> Start(JsonElement args, object connection, CancellationToken ct)
        {
            if (HasNonEmptyStringProperty(args, "program") || HasNonEmptyStringProperty(args, "type"))
            {
                return new ToolResult(
                    "Visual Studio debugs the startup project; use debug_configs to list configurations and pass config, not program/type.",
                    true);
            }

            var config = ToolArgs.GetString(args, "config");
            var stop = await _host.Debug.StartAsync(config, ct).ConfigureAwait(false);
            return new ToolResult(await Describe(stop, ct).ConfigureAwait(false), false);
        }

        public async Task<ToolResult> Breakpoint(JsonElement args, object connection, CancellationToken ct)
        {
            if (!ToolArgs.TryRequireString(args, "path", out var path, out var errPath))
            {
                return errPath!;
            }

            if (!ToolArgs.TryRequireInt(args, "line", out var line, out var errLine))
            {
                return errLine!;
            }

            var action = ToolArgs.GetString(args, "action") ?? "add";
            var condition = ToolArgs.GetString(args, "condition");

            var (abs, _, resolveErr) = await ToolPaths.ResolveAsync(_host, path, ct).ConfigureAwait(false);
            if (resolveErr != null)
            {
                return resolveErr;
            }

            var breakpoints = await _host.Debug.SetBreakpointAsync(abs!, line, action, condition, ct).ConfigureAwait(false);
            if (breakpoints.Count == 0)
            {
                return new ToolResult($"no breakpoints in {path}", false);
            }

            var lines = breakpoints.Select(b => $"{path}:{b.Line}{(b.Condition != null ? " if " + b.Condition : "")}");
            return new ToolResult(string.Join("\n", lines), false);
        }

        public async Task<ToolResult> Continue(JsonElement args, object connection, CancellationToken ct)
        {
            var stop = await _host.Debug.ContinueAsync(ct).ConfigureAwait(false);
            return new ToolResult(await Describe(stop, ct).ConfigureAwait(false), false);
        }

        public async Task<ToolResult> Step(JsonElement args, object connection, CancellationToken ct)
        {
            if (!ToolArgs.TryRequireString(args, "step", out var step, out var err))
            {
                return err!;
            }

            var stop = await _host.Debug.StepAsync(step, ct).ConfigureAwait(false);
            return new ToolResult(await Describe(stop, ct).ConfigureAwait(false), false);
        }

        public async Task<ToolResult> Stack(JsonElement args, object connection, CancellationToken ct)
        {
            var depth = ToolArgs.GetInt(args, "depth") ?? 10;
            var frames = await _host.Debug.StackAsync(depth, ct).ConfigureAwait(false);
            var folders = await _host.GetWorkspaceFoldersAsync(ct).ConfigureAwait(false);
            return new ToolResult(FormatStack(frames, folders), false);
        }

        public async Task<ToolResult> Variables(JsonElement args, object connection, CancellationToken ct)
        {
            var frame = ToolArgs.GetInt(args, "frame");
            var scope = ToolArgs.GetString(args, "scope") ?? "locals";
            var scopes = await _host.Debug.VariablesAsync(frame, scope, ct).ConfigureAwait(false);
            return new ToolResult(FormatVariables(scopes, scope), false);
        }

        public async Task<ToolResult> Evaluate(JsonElement args, object connection, CancellationToken ct)
        {
            if (!ToolArgs.TryRequireString(args, "expression", out var expression, out var err))
            {
                return err!;
            }

            var frame = ToolArgs.GetInt(args, "frame");
            var result = await _host.Debug.EvaluateAsync(expression, frame, ct).ConfigureAwait(false);
            var suffix = string.IsNullOrEmpty(result.Type) ? "" : " (" + result.Type + ")";
            return new ToolResult($"{result.Result}{suffix}", false);
        }

        public async Task<ToolResult> Output(JsonElement args, object connection, CancellationToken ct)
        {
            var since = ToolArgs.GetInt(args, "since") ?? 0;
            var result = await _host.Debug.OutputAsync(since, ct).ConfigureAwait(false);
            var body = result.Lines.Count > 0 ? string.Join("\n", result.Lines) : "(no new output)";
            return new ToolResult($"{body}\n[cursor {result.Cursor}]", false);
        }

        public async Task<ToolResult> Stop(JsonElement args, object connection, CancellationToken ct)
        {
            await _host.Debug.StopAsync(ct).ConfigureAwait(false);
            return new ToolResult("stopped", false);
        }

        private static bool HasNonEmptyStringProperty(JsonElement args, string name)
        {
            return args.ValueKind == JsonValueKind.Object
                && args.TryGetProperty(name, out var el)
                && el.ValueKind == JsonValueKind.String
                && !string.IsNullOrEmpty(el.GetString());
        }

        // Mirrors vscode's DebugManager.describe(): a stop reads its own top
        // of stack, a timeout/exit/terminate is a fixed message.
        private async Task<string> Describe(StopResult r, CancellationToken ct)
        {
            switch (r.Kind)
            {
                case StopKind.Stopped:
                    try
                    {
                        var frames = await _host.Debug.StackAsync(3, ct).ConfigureAwait(false);
                        var folders = await _host.GetWorkspaceFoldersAsync(ct).ConfigureAwait(false);
                        return $"stopped ({r.Reason ?? "unknown"})\n{FormatStack(frames, folders)}";
                    }
                    // Fix round 1, F8: a bare `catch` here swallowed
                    // OperationCanceledException along with a genuinely dead
                    // session, turning a cancelled request (the connection
                    // going away while this read was in flight) into ordinary
                    // success text — BridgeServer's own "a cancelled in-
                    // flight call gets no reply" handling never sees it,
                    // because CallAsync returns normally instead of
                    // propagating the cancellation. Let cancellation through.
                    catch (Exception ex) when (!(ex is OperationCanceledException))
                    {
                        return $"stopped ({r.Reason ?? "unknown"})\n(session ended before the stack could be read)";
                    }

                case StopKind.Timeout:
                    return $"still running after {WaitSeconds}s (no breakpoint hit); use debug_output or debug_stop";

                case StopKind.Exited:
                    return $"program exited with code {(r.ExitCode.HasValue ? r.ExitCode.Value.ToString() : "?")}";

                default:
                    return "debug session terminated";
            }
        }

        // Ruling S3: StackFrameInfo.Path crosses the seam absolute;
        // relativise here, exactly where vscode's own debug_stack handler
        // does.
        private static string FormatStack(IReadOnlyList<StackFrameInfo> frames, IReadOnlyList<string> folders)
        {
            if (frames.Count == 0)
            {
                return "no frames";
            }

            var lines = new List<string>(frames.Count);
            for (var i = 0; i < frames.Count; i++)
            {
                var f = frames[i];
                var path = f.Path != null ? Paths.RelPath(folders, f.Path) : "?";
                lines.Add($"#{i} {f.Name} {path}:{f.Line}  [frame {f.FrameId}]");
            }

            return string.Join("\n", lines);
        }

        // Mirrors vscode's DebugManager.variables(): filters scopes by name
        // (locals/args/all), shows at most 60 variables per scope, and
        // expands a variable's children only when the scope has ten or fewer
        // variables in total, capped at 20 children.
        private static string FormatVariables(IReadOnlyList<VariableScope> scopes, string scopeName)
        {
            var lines = new List<string>();
            foreach (var sc in scopes)
            {
                if (scopeName != "all")
                {
                    var needle = scopeName == "args" ? "arg" : "local";
                    if (!sc.Name.ToLowerInvariant().Contains(needle))
                    {
                        continue;
                    }
                }

                lines.Add($"{sc.Name}:");
                var vars = sc.Variables;
                foreach (var v in vars.Take(60))
                {
                    var suffix = string.IsNullOrEmpty(v.Type) ? "" : " (" + v.Type + ")";
                    lines.Add($"  {v.Name} = {v.Value}{suffix}");

                    if (v.Children != null && v.Children.Count > 0 && vars.Count <= 10)
                    {
                        foreach (var k in v.Children.Take(20))
                        {
                            lines.Add($"    {k.Name} = {k.Value}");
                        }
                    }
                }
            }

            return lines.Count > 0 ? string.Join("\n", lines) : "no variables in scope";
        }
    }
}
