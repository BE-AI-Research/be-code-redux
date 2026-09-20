namespace BECode.Bridge.Hosting
{
    /// <summary>
    /// Host design §4, <c>DebugSession</c>'s <c>OnEnterBreakMode</c>/
    /// <c>OnEnterDesignMode</c> handlers: maps an <c>EnvDTE.dbgEventReason</c>
    /// value to the free-text reason word <c>IDebugHost.StartAsync</c>'s doc
    /// comment (Ruling D2) asks for, and decides whether leaving run mode
    /// was a normal process exit. Takes the enum value's OWN
    /// <c>ToString()</c> name rather than the <c>dbgEventReason</c> type
    /// itself: <c>BECode.Bridge</c> must never reference a Visual Studio/
    /// EnvDTE assembly (see <c>BECode.Bridge.csproj</c>'s own comment), and
    /// this mapping needs no more than the name to do its job.
    /// </summary>
    public static class DebugReason
    {
        /// <summary>dbgEventReasonBreakpoint → "breakpoint", *Step → "step", the two exception reasons → "exception", *UserBreak → "pause", anything else → "stopped".</summary>
        public static string ForBreak(string dbgEventReasonName)
        {
            switch (dbgEventReasonName)
            {
                case "dbgEventReasonBreakpoint":
                    return "breakpoint";
                case "dbgEventReasonStep":
                    return "step";
                case "dbgEventReasonExceptionThrown":
                case "dbgEventReasonExceptionNotHandled":
                    return "exception";
                case "dbgEventReasonUserBreak":
                    return "pause";
                default:
                    return "stopped";
            }
        }

        /// <summary>True only for dbgEventReasonEndProgram (StopKind.Exited); anything else leaving run mode is StopKind.Terminated.</summary>
        public static bool IsNormalExit(string dbgEventReasonName)
        {
            return dbgEventReasonName == "dbgEventReasonEndProgram";
        }
    }
}
