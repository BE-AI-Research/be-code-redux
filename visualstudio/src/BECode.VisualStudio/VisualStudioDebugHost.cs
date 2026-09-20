using System;
using System.Collections.Generic;
using System.IO;
using System.Linq;
using System.Threading;
using System.Threading.Tasks;
using BECode.Bridge;
using BECode.Bridge.Hosting;
using EnvDTE;
using EnvDTE80;
using EnvDTE90a;
using Microsoft.VisualStudio;
using Microsoft.VisualStudio.Shell;
using Microsoft.VisualStudio.Shell.Interop;
using Microsoft.VisualStudio.Threading;
using BridgeBreakpointInfo = BECode.Bridge.BreakpointInfo;

namespace BECode.VisualStudio
{
    /// <summary>
    /// Host design §4: <see cref="IDebugHost"/> over <c>EnvDTE.Debugger</c>
    /// (<c>dte.Debugger</c>) and <c>DebuggerEvents</c>, kept in a field for
    /// the package's lifetime (a local is collected and the events silently
    /// stop — the best-known DTE trap). One <c>_pendingStop</c>
    /// <see cref="TaskCompletionSource{StopResult}"/>, guarded by
    /// <see cref="_gate"/>, is armed before every action that resumes or
    /// starts execution and resolved by <see cref="OnEnterBreakMode"/>/
    /// <see cref="OnEnterDesignMode"/>. Only one debug session is ever
    /// active through this interface at a time.
    /// </summary>
    internal sealed class VisualStudioDebugHost : IDebugHost
    {
        private readonly AsyncPackage _package;
        private readonly DTE2 _dte;
        private readonly Debugger _debugger;
        private readonly DebuggerEvents _debuggerEvents;

        private readonly object _gate = new object();
        private TaskCompletionSource<StopResult>? _pendingStop;

        private VisualStudioDebugHost(AsyncPackage package, DTE2 dte, Debugger debugger, DebuggerEvents debuggerEvents)
        {
            ThreadHelper.ThrowIfNotOnUIThread();

            _package = package;
            _dte = dte;
            _debugger = debugger;
            _debuggerEvents = debuggerEvents;

            _debuggerEvents.OnEnterBreakMode += OnEnterBreakMode;
            _debuggerEvents.OnEnterDesignMode += OnEnterDesignMode;
        }

        public static async Task<VisualStudioDebugHost> CreateAsync(AsyncPackage package, CancellationToken ct)
        {
            await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync(ct);

            var dte = await package.GetServiceAsync(typeof(SDTE)).ConfigureAwait(true) as DTE2;
            if (dte == null)
            {
                throw new InvalidOperationException("DTE is unavailable");
            }

            // dte.Events.DebuggerEvents MUST be kept in a field (host design
            // §4) — a local here is collected and the events silently stop.
            var debuggerEvents = dte.Events.DebuggerEvents;

            return new VisualStudioDebugHost(package, dte, dte.Debugger, debuggerEvents);
        }

        private void OnEnterBreakMode(dbgEventReason reason, ref dbgExecutionAction executionAction)
        {
            ThreadHelper.ThrowIfNotOnUIThread();
            Resolve(new StopResult(StopKind.Stopped, DebugReason.ForBreak(reason.ToString())));
        }

        private void OnEnterDesignMode(dbgEventReason reason)
        {
            ThreadHelper.ThrowIfNotOnUIThread();
            var kind = DebugReason.IsNormalExit(reason.ToString()) ? StopKind.Exited : StopKind.Terminated;
            Resolve(new StopResult(kind));
        }

        private void Resolve(StopResult result)
        {
            TaskCompletionSource<StopResult>? tcs;
            lock (_gate)
            {
                tcs = _pendingStop;
                _pendingStop = null;
            }

            tcs?.TrySetResult(result);
        }

        private TaskCompletionSource<StopResult> Arm()
        {
            var tcs = new TaskCompletionSource<StopResult>();
            lock (_gate)
            {
                _pendingStop = tcs;
            }

            return tcs;
        }

        public Task<IReadOnlyList<DebugConfigInfo>> ConfigsAsync(CancellationToken ct)
        {
            return Guard.RunAsync(nameof(ConfigsAsync), ct, async () =>
            {
                await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync(ct);

                string? startupUniqueName = null;
                try
                {
                    if (_dte.Solution?.SolutionBuild?.StartupProjects is object[] startupProjects && startupProjects.Length > 0)
                    {
                        startupUniqueName = startupProjects[0] as string;
                    }
                }
                catch (Exception ex)
                {
                    ActivityLog.LogWarning(nameof(ConfigsAsync), ex.ToString());
                }

                var startable = new List<DebugConfigInfo>();
                var launchProfiles = new List<DebugConfigInfo>();
                DebugConfigInfo? startupFirst = null;

                var solution = _dte.Solution;
                if (solution != null)
                {
                    foreach (Project project in solution.Projects)
                    {
                        if (!IsStartable(project))
                        {
                            continue;
                        }

                        var info = new DebugConfigInfo(project.Name, "startup project");
                        bool isStartup;
                        try
                        {
                            isStartup = startupUniqueName != null && string.Equals(project.UniqueName, startupUniqueName, StringComparison.OrdinalIgnoreCase);
                        }
                        catch
                        {
                            isStartup = false;
                        }

                        if (isStartup && startupFirst == null)
                        {
                            startupFirst = info;
                        }
                        else
                        {
                            startable.Add(info);
                        }

                        launchProfiles.AddRange(LaunchProfilesFor(project));
                    }
                }

                var result = new List<DebugConfigInfo>();
                if (startupFirst != null)
                {
                    result.Add(startupFirst);
                }

                result.AddRange(startable);
                result.AddRange(launchProfiles);

                return (IReadOnlyList<DebugConfigInfo>)result;
            });
        }

        public Task<StopResult> StartAsync(string? config, CancellationToken ct)
        {
            return Guard.RunAsync(nameof(StartAsync), ct, async () =>
            {
                await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync(ct);

                if (_debugger.CurrentMode != dbgDebugMode.dbgDesignMode)
                {
                    var stopTcs = Arm();
                    _debugger.Stop(false);
                    await TaskScheduler.Default;
                    await WaitBoundedAsync(stopTcs.Task, TimeSpan.FromSeconds(10), ct).ConfigureAwait(false);
                    await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync(ct);
                }

                if (config != null)
                {
                    if (config.Contains(LaunchProfileSeparator))
                    {
                        throw new InvalidOperationException(
                            "\"" + config + "\" is a launch profile; select it in Visual Studio's own toolbar first — switching it programmatically is not supported (host design §4)");
                    }

                    var project = FindStartableProjectByName(config);
                    if (project == null)
                    {
                        throw new InvalidOperationException("no startup project named \"" + config + "\"");
                    }

                    _dte.Solution.SolutionBuild.StartupProjects = project.UniqueName;
                }

                var tcs = Arm();
                _dte.ExecuteCommand("Debug.Start");
                await TaskScheduler.Default;

                // Alongside the 60s stop wait, a 5s "did we leave design
                // mode" check (host design §4): ExecuteCommand returns at
                // once, and a build failure never enters run mode.
                using var buildCheckCts = CancellationTokenSource.CreateLinkedTokenSource(ct);
                var buildCheck = CheckBuildFailureAsync(buildCheckCts.Token);

                var result = await WaitBoundedAsync(tcs.Task, TimeSpan.FromSeconds(60), ct).ConfigureAwait(false);
                buildCheckCts.Cancel();
                try
                {
                    await buildCheck.ConfigureAwait(false);
                }
                catch (OperationCanceledException)
                {
                }

                return result ?? new StopResult(StopKind.Timeout);
            });
        }

        private async Task CheckBuildFailureAsync(CancellationToken ct)
        {
            try
            {
                await Task.Delay(TimeSpan.FromSeconds(5), ct).ConfigureAwait(false);
            }
            catch (OperationCanceledException)
            {
                return;
            }

            await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync(ct);

            if (_debugger.CurrentMode != dbgDebugMode.dbgDesignMode)
            {
                return;
            }

            var lastBuildInfo = 0;
            try
            {
                lastBuildInfo = _dte.Solution?.SolutionBuild?.LastBuildInfo ?? 0;
            }
            catch
            {
            }

            if (lastBuildInfo > 0)
            {
                Resolve(new StopResult(StopKind.Terminated, "build failed (" + lastBuildInfo + " projects)"));
            }
        }

        public Task<StopResult> ContinueAsync(CancellationToken ct)
        {
            return Guard.RunAsync(nameof(ContinueAsync), ct, async () =>
            {
                await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync(ct);

                if (_debugger.CurrentMode != dbgDebugMode.dbgBreakMode)
                {
                    throw new InvalidOperationException("not stopped at a breakpoint");
                }

                var tcs = Arm();
                _debugger.Go(false);
                await TaskScheduler.Default;

                var result = await WaitBoundedAsync(tcs.Task, TimeSpan.FromSeconds(60), ct).ConfigureAwait(false);
                return result ?? new StopResult(StopKind.Timeout);
            });
        }

        public Task<StopResult> StepAsync(DebugStepKind step, CancellationToken ct)
        {
            return Guard.RunAsync(nameof(StepAsync), ct, async () =>
            {
                await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync(ct);

                if (_debugger.CurrentMode != dbgDebugMode.dbgBreakMode)
                {
                    throw new InvalidOperationException("not stopped at a breakpoint");
                }

                var tcs = Arm();
                switch (step)
                {
                    case DebugStepKind.Over:
                        _debugger.StepOver(false);
                        break;
                    case DebugStepKind.Into:
                        _debugger.StepInto(false);
                        break;
                    case DebugStepKind.Out:
                        _debugger.StepOut(false);
                        break;
                }

                await TaskScheduler.Default;

                var result = await WaitBoundedAsync(tcs.Task, TimeSpan.FromSeconds(60), ct).ConfigureAwait(false);
                return result ?? new StopResult(StopKind.Timeout);
            });
        }

        public Task<IReadOnlyList<BridgeBreakpointInfo>> SetBreakpointAsync(string path, int line, BreakpointAction action, string? condition, CancellationToken ct)
        {
            return Guard.RunAsync(nameof(SetBreakpointAsync), ct, async () =>
            {
                await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync(ct);

                if (action == BreakpointAction.Add)
                {
                    _debugger.Breakpoints.Add(
                        File: path,
                        Line: line,
                        Column: 1,
                        Condition: condition ?? string.Empty,
                        ConditionType: string.IsNullOrEmpty(condition) ? dbgBreakpointConditionType.dbgBreakpointConditionTypeWhenTrue : dbgBreakpointConditionType.dbgBreakpointConditionTypeWhenTrue);
                }
                else
                {
                    var toDelete = new List<Breakpoint>();
                    foreach (Breakpoint bp in _debugger.Breakpoints)
                    {
                        if (string.Equals(bp.File, path, StringComparison.OrdinalIgnoreCase) && bp.FileLine == line)
                        {
                            toDelete.Add(bp);
                        }
                    }

                    foreach (var bp in toDelete)
                    {
                        bp.Delete();
                    }
                }

                var remaining = new List<BridgeBreakpointInfo>();
                foreach (Breakpoint bp in _debugger.Breakpoints)
                {
                    if (string.Equals(bp.File, path, StringComparison.OrdinalIgnoreCase))
                    {
                        remaining.Add(new BridgeBreakpointInfo(bp.FileLine, string.IsNullOrEmpty(bp.Condition) ? null : bp.Condition));
                    }
                }

                return (IReadOnlyList<BridgeBreakpointInfo>)remaining;
            });
        }

        public Task<IReadOnlyList<StackFrameInfo>> StackAsync(int depth, CancellationToken ct)
        {
            return Guard.RunAsync(nameof(StackAsync), ct, async () =>
            {
                await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync(ct);

                var result = new List<StackFrameInfo>();
                var thread = _debugger.CurrentThread;
                if (thread == null)
                {
                    return (IReadOnlyList<StackFrameInfo>)result;
                }

                var frames = thread.StackFrames;
                var count = Math.Min(depth, frames.Count);
                for (var i = 1; i <= count; i++)
                {
                    var frame = frames.Item(i);
                    string? path = null;
                    var lineNumber = 0;

                    if (frame is StackFrame2 frame2)
                    {
                        path = string.IsNullOrEmpty(frame2.FileName) ? null : frame2.FileName;
                        lineNumber = (int)frame2.LineNumber;
                    }

                    result.Add(new StackFrameInfo(frame.FunctionName, path, lineNumber, i));
                }

                return (IReadOnlyList<StackFrameInfo>)result;
            });
        }

        public Task<IReadOnlyList<VariableScope>> VariablesAsync(int? frame, CancellationToken ct)
        {
            return Guard.RunAsync(nameof(VariablesAsync), ct, async () =>
            {
                await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync(ct);

                var stackFrame = ResolveFrame(frame);
                if (stackFrame == null)
                {
                    return (IReadOnlyList<VariableScope>)Array.Empty<VariableScope>();
                }

                var locals = CollectScope(stackFrame.Locals);
                var arguments = CollectScope(stackFrame.Arguments);

                return (IReadOnlyList<VariableScope>)new[]
                {
                    new VariableScope("Locals", locals),
                    new VariableScope("Arguments", arguments),
                };
            });
        }

        public Task<EvaluateResult> EvaluateAsync(string expression, int? frame, CancellationToken ct)
        {
            return Guard.RunAsync(nameof(EvaluateAsync), ct, async () =>
            {
                await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync(ct);

                if (frame.HasValue)
                {
                    var stackFrame = ResolveFrame(frame);
                    if (stackFrame != null)
                    {
                        _debugger.CurrentStackFrame = stackFrame;
                    }
                }

                var expr = _debugger.GetExpression(expression, false, 5000);
                if (!expr.IsValidValue)
                {
                    throw new InvalidOperationException(expr.Value);
                }

                return new EvaluateResult(expr.Value, expr.Type);
            });
        }

        public Task<DebugOutputResult> OutputAsync(int since, CancellationToken ct)
        {
            return Guard.RunAsync(nameof(OutputAsync), ct, async () =>
            {
                await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync(ct);

                var text = ReadDebugPaneText();
                var (lines, cursor) = OutputCursor.Since(text, since);
                return new DebugOutputResult(lines, cursor);
            });
        }

        public Task StopAsync(CancellationToken ct)
        {
            return Guard.RunAsync(nameof(StopAsync), ct, async () =>
            {
                await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync(ct);

                if (_debugger.CurrentMode == dbgDebugMode.dbgDesignMode)
                {
                    return;
                }

                _debugger.Stop(false);
            });
        }

        private const string LaunchProfileSeparator = " › ";

        private EnvDTE.StackFrame? ResolveFrame(int? frame)
        {
            ThreadHelper.ThrowIfNotOnUIThread();

            if (frame == null)
            {
                return _debugger.CurrentStackFrame;
            }

            var thread = _debugger.CurrentThread;
            if (thread == null)
            {
                return null;
            }

            var frames = thread.StackFrames;
            if (frame.Value < 1 || frame.Value > frames.Count)
            {
                return null;
            }

            return frames.Item(frame.Value);
        }

        /// <summary>
        /// Children (<see cref="VariableInfo.Children"/>) resolved at most
        /// ONE level deep, and only for a scope with ten or fewer variables
        /// in total (Ruling S7) — reading <c>DataMembers</c> evaluates
        /// properties in the debuggee, which can be slow or have side
        /// effects.
        /// </summary>
        private static IReadOnlyList<VariableInfo> CollectScope(Expressions expressions)
        {
            ThreadHelper.ThrowIfNotOnUIThread();

            var variables = new List<VariableInfo>();
            var count = expressions.Count;
            var resolveChildren = count > 0 && count <= 10;

            foreach (Expression expr in expressions)
            {
                IReadOnlyList<VariableInfo>? children = null;

                if (resolveChildren)
                {
                    Expressions? members = null;
                    try
                    {
                        members = expr.DataMembers;
                    }
                    catch
                    {
                        members = null;
                    }

                    if (members != null && members.Count > 0)
                    {
                        var childList = new List<VariableInfo>();
                        var childCount = Math.Min(20, members.Count);
                        for (var i = 1; i <= childCount; i++)
                        {
                            var child = members.Item(i);
                            childList.Add(new VariableInfo(child.Name, child.Value, child.Type));
                        }

                        children = childList;
                    }
                }

                variables.Add(new VariableInfo(expr.Name, expr.Value, expr.Type, children));
            }

            return variables;
        }

        private string ReadDebugPaneText()
        {
            ThreadHelper.ThrowIfNotOnUIThread();

            var pane = TryGetPaneByName("Debug") ?? TryGetPaneByGuid(VSConstants.OutputWindowPaneGuid.DebugPane_guid);
            if (pane == null)
            {
                return string.Empty;
            }

            try
            {
                var doc = pane.TextDocument;
                return doc.StartPoint.CreateEditPoint().GetText(doc.EndPoint);
            }
            catch (Exception ex)
            {
                ActivityLog.LogWarning(nameof(ReadDebugPaneText), ex.ToString());
                return string.Empty;
            }
        }

        private OutputWindowPane? TryGetPaneByName(string name)
        {
            ThreadHelper.ThrowIfNotOnUIThread();

            try
            {
                return _dte.ToolWindows.OutputWindow.OutputWindowPanes.Item(name);
            }
            catch
            {
                return null;
            }
        }

        /// <summary>
        /// Host design §4's *Check*: the "Debug" pane's <c>Item()</c> name
        /// is localised on a non-English Visual Studio — this falls back to
        /// matching <see cref="OutputWindowPane.Guid"/> against the pane's
        /// well-known GUID instead of its display name.
        /// </summary>
        private OutputWindowPane? TryGetPaneByGuid(Guid guid)
        {
            ThreadHelper.ThrowIfNotOnUIThread();

            try
            {
                var target = guid.ToString("B");
                foreach (OutputWindowPane pane in _dte.ToolWindows.OutputWindow.OutputWindowPanes)
                {
                    if (string.Equals(pane.Guid, target, StringComparison.OrdinalIgnoreCase))
                    {
                        return pane;
                    }
                }
            }
            catch (Exception ex)
            {
                ActivityLog.LogWarning(nameof(TryGetPaneByGuid), ex.ToString());
            }

            return null;
        }

        /// <summary>
        /// Host design §4, <c>ConfigsAsync</c>: "simpler and what v1 does" —
        /// every project whose <c>OutputType</c> is Exe/WinExe, or an
        /// ASP.NET web site project (which does not carry that property the
        /// same way). This is a best-effort classification, not a claim
        /// that every project system's own notion of "startable" is
        /// captured exactly.
        /// </summary>
        private static bool IsStartable(Project project)
        {
            ThreadHelper.ThrowIfNotOnUIThread();

            try
            {
                if (string.Equals(project.Kind, WebSiteProjectKind, StringComparison.OrdinalIgnoreCase))
                {
                    return true;
                }
            }
            catch
            {
            }

            try
            {
                var props = project.Properties;
                var value = props?.Item("OutputType")?.Value;
                if (value == null)
                {
                    return false;
                }

                var text = value.ToString();
                return text == "0" || text == "2"
                    || string.Equals(text, "Exe", StringComparison.OrdinalIgnoreCase)
                    || string.Equals(text, "WinExe", StringComparison.OrdinalIgnoreCase);
            }
            catch
            {
                return false;
            }
        }

        private const string WebSiteProjectKind = "{E24C65DC-7377-472B-9ABA-BC803B73C61A}";

        private Project? FindStartableProjectByName(string name)
        {
            ThreadHelper.ThrowIfNotOnUIThread();

            var solution = _dte.Solution;
            if (solution == null)
            {
                return null;
            }

            foreach (Project project in solution.Projects)
            {
                if (IsStartable(project) && string.Equals(project.Name, name, StringComparison.OrdinalIgnoreCase))
                {
                    return project;
                }
            }

            return null;
        }

        private static List<DebugConfigInfo> LaunchProfilesFor(Project project)
        {
            ThreadHelper.ThrowIfNotOnUIThread();

            var result = new List<DebugConfigInfo>();
            string? projectDir = null;
            try
            {
                projectDir = Path.GetDirectoryName(project.FullName);
            }
            catch
            {
            }

            if (string.IsNullOrEmpty(projectDir))
            {
                return result;
            }

            var launchSettingsPath = Path.Combine(projectDir!, "Properties", "launchSettings.json");
            if (!File.Exists(launchSettingsPath))
            {
                return result;
            }

            string json;
            try
            {
                json = File.ReadAllText(launchSettingsPath);
            }
            catch
            {
                return result;
            }

            foreach (var name in LaunchProfiles.ParseNames(json))
            {
                result.Add(new DebugConfigInfo(project.Name + LaunchProfileSeparator + name, "launch profile"));
            }

            return result;
        }

        private static async Task<StopResult?> WaitBoundedAsync(Task<StopResult> task, TimeSpan timeout, CancellationToken ct)
        {
            using var cts = CancellationTokenSource.CreateLinkedTokenSource(ct);
            var delay = Task.Delay(timeout, cts.Token);
            var completed = await Task.WhenAny(task, delay).ConfigureAwait(false);

            if (completed == task)
            {
                cts.Cancel();

                // VSTHRD003 assumes any foreign Task might represent work
                // that still needs the main thread, which awaiting here
                // (off the main thread — every caller left it before
                // calling this) could deadlock on. `task` is always our own
                // TaskCompletionSource<StopResult>, resolved by
                // DebuggerEvents' OnEnterBreakMode/OnEnterDesignMode firing
                // on the main thread — a plain signal, not a "started
                // elsewhere" operation this rule is meant to catch.
#pragma warning disable VSTHRD003
                return await task.ConfigureAwait(false);
#pragma warning restore VSTHRD003
            }

            ct.ThrowIfCancellationRequested();
            return null;
        }
    }
}
