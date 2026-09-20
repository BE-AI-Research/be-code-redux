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

        // Fix round 1, I-6: held across Start/Continue/Step/Stop — the seam's
        // own doc comment (IDebugHost.StartAsync's Ruling D2 paragraph)
        // requires debug-session mutation to be serialised, and StartAsync's
        // "already debugging" branch awaits up to 10s, during which a second
        // concurrent StartAsync could otherwise arm ITS OWN waiter over the
        // first one's.
        private readonly SemaphoreSlim _debugGate = new SemaphoreSlim(1, 1);

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

            // Fix round 1, I-8: Visual Studio invokes DebuggerEvents
            // handlers directly as part of its own debugger dispatch while
            // entering break mode — an unhandled exception here is not just
            // a lost stop result, it runs inside that dispatch.
            try
            {
                Resolve(new StopResult(StopKind.Stopped, DebugReason.ForBreak(reason.ToString())));
            }
            catch (Exception ex)
            {
                ActivityLog.LogError(nameof(OnEnterBreakMode), ex.ToString());
            }
        }

        private void OnEnterDesignMode(dbgEventReason reason)
        {
            ThreadHelper.ThrowIfNotOnUIThread();

            try
            {
                var kind = DebugReason.IsNormalExit(reason.ToString()) ? StopKind.Exited : StopKind.Terminated;
                Resolve(new StopResult(kind));
            }
            catch (Exception ex)
            {
                ActivityLog.LogError(nameof(OnEnterDesignMode), ex.ToString());
            }
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

        /// <summary>
        /// Fix round 1, I-5: resolves <paramref name="expected"/> only if it
        /// is STILL the currently-armed waiter — used by
        /// <see cref="CheckBuildFailureAsync"/>, whose 5s-delayed check can
        /// otherwise fire after a NEWER call (e.g. a second StartAsync that
        /// stopped-and-rearmed) has already replaced <see cref="_pendingStop"/>
        /// with its own waiter; resolving that unconditionally, the way
        /// <see cref="Resolve"/> does for a genuine debugger-event
        /// transition, would complete the wrong request.
        /// </summary>
        private bool ResolveIfCurrent(TaskCompletionSource<StopResult> expected, StopResult result)
        {
            lock (_gate)
            {
                if (!ReferenceEquals(_pendingStop, expected))
                {
                    return false;
                }

                _pendingStop = null;
            }

            return expected.TrySetResult(result);
        }

        private TaskCompletionSource<StopResult> Arm()
        {
            // Fix round 1, C-3: same reasoning as DiffReview's tcs — resolved
            // by OnEnterBreakMode/OnEnterDesignMode on the UI thread inside
            // Visual Studio's own debugger-event dispatch; without
            // RunContinuationsAsynchronously, every awaiter of this Task
            // (WaitBoundedAsync and everything after it) would resume INLINE
            // on that call stack, running while Visual Studio is itself
            // entering break/design mode.
            var tcs = new TaskCompletionSource<StopResult>(TaskCreationOptions.RunContinuationsAsynchronously);
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

                // Fix round 1, I-4 (Ruling R-14): list EVERY loaded project
                // that is not a solution folder, via the SAME enumeration
                // WorkspaceFolders already uses (IVsSolution.GetProjectEnum),
                // not solution.Projects — which the design forbids AND
                // cannot see projects inside solution folders. I-3: the old
                // OutputType/"IsStartable" heuristic is deleted; over-listing
                // is harmless (debug_start on a class library fails with
                // Visual Studio's own clear message), under-listing hides a
                // project the model then never tries.
                var solutionService = await _package.GetServiceAsync(typeof(SVsSolution)).ConfigureAwait(true) as IVsSolution;
                var projects = WorkspaceFolders.EnumerateLoadedProjects(solutionService);

                var startupUniqueNames = new HashSet<string>(StringComparer.OrdinalIgnoreCase);
                try
                {
                    if (_dte.Solution?.SolutionBuild?.StartupProjects is object[] startupProjects)
                    {
                        foreach (var startupProject in startupProjects)
                        {
                            if (startupProject is string uniqueName)
                            {
                                startupUniqueNames.Add(uniqueName);
                            }
                        }
                    }
                }
                catch (Exception ex)
                {
                    ActivityLog.LogWarning(nameof(ConfigsAsync), ex.ToString());
                }

                // Fix round 1, I-11: project names/dirs are already collected
                // (on the UI thread, above); hop off before reading each
                // project's launchSettings.json — ordinary file I/O that
                // does not need the UI thread.
                await TaskScheduler.Default;

                var startupFirst = new List<DebugConfigInfo>();
                var rest = new List<DebugConfigInfo>();
                var launchProfiles = new List<DebugConfigInfo>();

                foreach (var project in projects)
                {
                    var info = new DebugConfigInfo(project.Name, "startup project");
                    if (startupUniqueNames.Contains(project.UniqueName))
                    {
                        startupFirst.Add(info);
                    }
                    else
                    {
                        rest.Add(info);
                    }

                    launchProfiles.AddRange(LaunchProfilesFor(project));
                }

                var result = new List<DebugConfigInfo>(startupFirst.Count + rest.Count + launchProfiles.Count);
                result.AddRange(startupFirst);
                result.AddRange(rest);
                result.AddRange(launchProfiles);

                return (IReadOnlyList<DebugConfigInfo>)result;
            });
        }

        public Task<StopResult> StartAsync(string? config, CancellationToken ct)
        {
            return Guard.RunAsync(nameof(StartAsync), ct, async () =>
            {
                // Fix round 1, I-6: held for the whole method — the
                // "already debugging" branch below awaits up to 10s, during
                // which a second concurrent StartAsync must not be able to
                // arm its own waiter over this one's.
                await _debugGate.WaitAsync(ct).ConfigureAwait(false);
                try
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

                        // Fix round 1, I-4: resolved through the shared
                        // enumeration, no longer filtered by the deleted
                        // IsStartable heuristic (I-3) — a name ConfigsAsync
                        // just listed is always resolvable here.
                        var solutionService = await _package.GetServiceAsync(typeof(SVsSolution)).ConfigureAwait(true) as IVsSolution;
                        var projects = WorkspaceFolders.EnumerateLoadedProjects(solutionService);

                        ProjectEntry match = default;
                        var found = false;
                        foreach (var project in projects)
                        {
                            if (string.Equals(project.Name, config, StringComparison.OrdinalIgnoreCase))
                            {
                                match = project;
                                found = true;
                                break;
                            }
                        }

                        if (!found)
                        {
                            throw new InvalidOperationException("no project named \"" + config + "\"");
                        }

                        _dte.Solution.SolutionBuild.StartupProjects = match.UniqueName;
                    }

                    var tcs = Arm();
                    _dte.ExecuteCommand("Debug.Start");
                    await TaskScheduler.Default;

                    // Alongside the 60s stop wait, a 5s "did we leave design
                    // mode" check (host design §4): ExecuteCommand returns at
                    // once, and a build failure never enters run mode.
                    using var buildCheckCts = CancellationTokenSource.CreateLinkedTokenSource(ct);
                    var buildCheck = CheckBuildFailureAsync(tcs, buildCheckCts.Token);

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
                }
                finally
                {
                    _debugGate.Release();
                }
            });
        }

        /// <summary>
        /// Fix round 1, I-5: previously fired ONCE at a fixed 5s and read
        /// <c>LastBuildInfo</c>, which is the PREVIOUS build's failure count
        /// — the normal state of an agent fixing compile errors, so this
        /// could report "build failed" for a build that was in fact still
        /// running, then (via the old unconditional <c>Resolve</c>) clear
        /// the pending stop and lose the breakpoint hit that followed. Now
        /// polls <c>SolutionBuild.BuildState</c> until it leaves
        /// <c>vsBuildStateInProgress</c> (bounded by the caller's own 60s
        /// wait/<paramref name="ct"/>, off the UI thread between polls),
        /// decides only once the build has actually finished, and resolves
        /// only <paramref name="tcs"/> — the SPECIFIC waiter this call was
        /// armed for (<see cref="ResolveIfCurrent"/>) — so a late check can
        /// never complete a newer request's waiter.
        /// </summary>
        private async Task CheckBuildFailureAsync(TaskCompletionSource<StopResult> tcs, CancellationToken ct)
        {
            try
            {
                await Task.Delay(TimeSpan.FromSeconds(5), ct).ConfigureAwait(false);
            }
            catch (OperationCanceledException)
            {
                return;
            }

            while (true)
            {
                ct.ThrowIfCancellationRequested();

                await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync(ct);

                var buildState = vsBuildState.vsBuildStateDone;
                try
                {
                    buildState = _dte.Solution?.SolutionBuild?.BuildState ?? vsBuildState.vsBuildStateDone;
                }
                catch
                {
                }

                if (buildState != vsBuildState.vsBuildStateInProgress)
                {
                    break;
                }

                await TaskScheduler.Default;

                try
                {
                    await Task.Delay(TimeSpan.FromMilliseconds(500), ct).ConfigureAwait(false);
                }
                catch (OperationCanceledException)
                {
                    return;
                }
            }

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
                ResolveIfCurrent(tcs, new StopResult(StopKind.Terminated, "build failed (" + lastBuildInfo + " projects)"));
            }
        }

        public Task<StopResult> ContinueAsync(CancellationToken ct)
        {
            return Guard.RunAsync(nameof(ContinueAsync), ct, async () =>
            {
                await _debugGate.WaitAsync(ct).ConfigureAwait(false);
                try
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
                }
                finally
                {
                    _debugGate.Release();
                }
            });
        }

        public Task<StopResult> StepAsync(DebugStepKind step, CancellationToken ct)
        {
            return Guard.RunAsync(nameof(StepAsync), ct, async () =>
            {
                await _debugGate.WaitAsync(ct).ConfigureAwait(false);
                try
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
                }
                finally
                {
                    _debugGate.Release();
                }
            });
        }

        public Task<IReadOnlyList<BridgeBreakpointInfo>> SetBreakpointAsync(string path, int line, BreakpointAction action, string? condition, CancellationToken ct)
        {
            return Guard.RunAsync(nameof(SetBreakpointAsync), ct, async () =>
            {
                await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync(ct);

                if (action == BreakpointAction.Add)
                {
                    // Fix round 1, M-3: both ternary arms were the same
                    // value — dropped.
                    _debugger.Breakpoints.Add(
                        File: path,
                        Line: line,
                        Column: 1,
                        Condition: condition ?? string.Empty,
                        ConditionType: dbgBreakpointConditionType.dbgBreakpointConditionTypeWhenTrue);
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

                // Fix round 1, M-5: a bound breakpoint can appear more than
                // once in _debugger.Breakpoints for the same file/line
                // (Visual Studio's own binding mechanics) — dedupe by line
                // so the reported list matches what the user would see.
                var remaining = new List<BridgeBreakpointInfo>();
                var seenLines = new HashSet<int>();
                foreach (Breakpoint bp in _debugger.Breakpoints)
                {
                    if (string.Equals(bp.File, path, StringComparison.OrdinalIgnoreCase) && seenLines.Add(bp.FileLine))
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
                    // Fix round 1, M-4: fenced PER FRAME — one bad frame
                    // (a COM call that throws for it specifically, e.g. a
                    // frame with no symbols) degrades to Path=null, Line=0
                    // rather than losing every frame after it.
                    var name = "?";
                    string? path = null;
                    var lineNumber = 0;
                    try
                    {
                        var frame = frames.Item(i);
                        name = frame.FunctionName;

                        if (frame is StackFrame2 frame2)
                        {
                            path = string.IsNullOrEmpty(frame2.FileName) ? null : frame2.FileName;
                            lineNumber = (int)frame2.LineNumber;
                        }
                    }
                    catch (Exception ex)
                    {
                        ActivityLog.LogWarning(nameof(StackAsync), ex.ToString());
                    }

                    result.Add(new StackFrameInfo(name, path, lineNumber, i));
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
                await _debugGate.WaitAsync(ct).ConfigureAwait(false);
                try
                {
                    await ThreadHelper.JoinableTaskFactory.SwitchToMainThreadAsync(ct);

                    if (_debugger.CurrentMode == dbgDebugMode.dbgDesignMode)
                    {
                        return;
                    }

                    _debugger.Stop(false);
                }
                finally
                {
                    _debugGate.Release();
                }
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
                // Fix round 1, M-4: fenced PER VARIABLE — one bad variable
                // (evaluating it, or a child's DataMembers, throws) degrades
                // to Value="<unavailable>" rather than losing every
                // variable after it in the scope.
                try
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
                                try
                                {
                                    var child = members.Item(i);
                                    childList.Add(new VariableInfo(child.Name, child.Value, child.Type));
                                }
                                catch (Exception ex)
                                {
                                    ActivityLog.LogWarning(nameof(CollectScope), ex.ToString());
                                    childList.Add(new VariableInfo("?", "<unavailable>", null));
                                }
                            }

                            children = childList;
                        }
                    }

                    variables.Add(new VariableInfo(expr.Name, expr.Value, expr.Type, children));
                }
                catch (Exception ex)
                {
                    ActivityLog.LogWarning(nameof(CollectScope), ex.ToString());
                    variables.Add(new VariableInfo("?", "<unavailable>", null));
                }
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

        // Fix round 1, I-3: the OutputType-based IsStartable heuristic (and
        // FindStartableProjectByName, which filtered through it) are
        // deleted — superseded by I-4's rule (ConfigsAsync/StartAsync now
        // list/resolve EVERY loaded project via WorkspaceFolders.EnumerateLoadedProjects).
        // The heuristic also read the numeric OutputType backwards
        // (VSLangProj.prjOutputType has WinExe=0, Exe=1, Library=2 — the old
        // code accepted "0" and "2", so it listed class libraries and hid
        // console apps).

        /// <summary>
        /// Fix round 1, I-11: no longer asserts the UI thread — the caller
        /// (<see cref="ConfigsAsync"/>) now hops OFF it (<c>await
        /// TaskScheduler.Default</c>) before calling this for each project,
        /// since launchSettings.json is ordinary file I/O that does not
        /// need it. <paramref name="project"/>'s <see cref="ProjectEntry.Dir"/>
        /// was collected on the UI thread by the shared enumeration.
        /// </summary>
        private static List<DebugConfigInfo> LaunchProfilesFor(ProjectEntry project)
        {
            var result = new List<DebugConfigInfo>();

            if (string.IsNullOrEmpty(project.Dir))
            {
                return result;
            }

            var launchSettingsPath = Path.Combine(project.Dir, "Properties", "launchSettings.json");
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
