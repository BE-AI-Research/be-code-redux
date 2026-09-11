import * as vscode from "vscode";
import { ToolRegistry } from "./registry";
import { StopWaiter, RingLog, StopResult } from "../lib/stopwaiter";
import { relPath, absPath } from "../lib/paths";

const WAIT_MS = 60_000;
const folders = () => (vscode.workspace.workspaceFolders ?? []).map((f) => f.uri.fsPath);

class DebugManager {
  session?: vscode.DebugSession;
  // The session actually carrying DAP traffic right now: usually the root
  // session, but some debuggers (e.g. debugpy) launch a parent session plus a
  // child session that does the real work, so `active` tracks whichever of
  // the two (root or child) most recently reported a stop.
  active?: vscode.DebugSession;
  waiter = new StopWaiter();
  log = new RingLog(2000);
  threadId?: number;

  constructor(ctx: vscode.ExtensionContext) {
    ctx.subscriptions.push(
      vscode.debug.registerDebugAdapterTrackerFactory("*", {
        createDebugAdapterTracker: (session) => ({
          onDidSendMessage: (m: any) => {
            if (!(session === this.session || session.parentSession === this.session) || m.type !== "event") return;
            if (m.event === "output" && m.body?.output) for (const l of String(m.body.output).split("\n")) if (l) this.log.push(l);
            if (m.event === "stopped") {
              this.active = session;
              if (m.body?.threadId) this.threadId = m.body.threadId;
            }
            this.waiter.onEvent(m);
          },
        }),
      }),
      vscode.debug.onDidStartDebugSession((s) => { if (s.parentSession === this.session) this.active = s; }),
      vscode.debug.onDidTerminateDebugSession((s) => {
        if (s === this.session) this.session = undefined;
        if (s === this.active) this.active = undefined;
      }),
    );
  }

  async start(args: any): Promise<string> {
    if (this.session) await vscode.debug.stopDebugging(this.session);
    this.waiter = new StopWaiter(); this.log = new RingLog(2000); this.threadId = undefined; this.active = undefined;
    const folder = vscode.workspace.workspaceFolders?.[0];
    let config: string | vscode.DebugConfiguration;
    if (args.config) config = args.config;
    else if (args.program) {
      const type = args.type === "python" ? "python" : "go";
      config = type === "python"
        ? { type: "python", request: "launch", name: "be-code", program: absPath(folders(), args.program), args: args.args ?? [], console: "internalConsole", justMyCode: false }
        : { type: "go", request: "launch", name: "be-code", mode: "debug", program: absPath(folders(), args.program), args: args.args ?? [] };
    } else throw new Error("give config (a launch.json name) or program (+ type go|python)");
    let d: vscode.Disposable | undefined;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const cleanup = () => { d?.dispose(); if (timer) clearTimeout(timer); };
    const started = new Promise<vscode.DebugSession>((resolve, reject) => {
      d = vscode.debug.onDidStartDebugSession((s) => {
        if (s.parentSession !== undefined) return; // wait for the root session; children are picked up separately
        cleanup();
        resolve(s);
      });
      timer = setTimeout(() => { cleanup(); reject(new Error("debug session did not report starting")); }, 15_000);
    });
    if (!(await vscode.debug.startDebugging(folder, config))) {
      cleanup();
      throw new Error("debug session failed to start (check the launch configuration and that the debugger extension is installed)");
    }
    this.session = await started;
    return this.describe(await this.waiter.wait(WAIT_MS));
  }

  need(): vscode.DebugSession { if (!this.session) throw new Error("no debug session; use debug_start first"); return this.active ?? this.session; }

  async describe(r: StopResult): Promise<string> {
    if (r.kind === "stopped") {
      try {
        const top = await this.stack(3);
        return `stopped (${r.reason ?? "unknown"})\n${top}`;
      } catch {
        return `stopped (${r.reason ?? "unknown"})\n(session ended before the stack could be read)`;
      }
    }
    if (r.kind === "timeout") return `still running after ${WAIT_MS / 1000}s (no breakpoint hit); use debug_output or debug_stop`;
    return r.kind === "exited" ? `program exited with code ${r.exitCode ?? "?"}` : "debug session terminated";
  }

  async stack(depth: number): Promise<string> {
    const s = this.need();
    const tid = this.threadId ?? (await s.customRequest("threads")).threads?.[0]?.id;
    const st = await s.customRequest("stackTrace", { threadId: tid, startFrame: 0, levels: depth });
    return (st.stackFrames ?? []).map((f: any, i: number) => `#${i} ${f.name} ${f.source?.path ? relPath(folders(), f.source.path) : "?"}:${f.line}  [frame ${f.id}]`).join("\n") || "no frames";
  }

  async variables(frameArg: number | undefined, scopeName: string): Promise<string> {
    const s = this.need();
    const frameId = frameArg ?? (await this.topFrameId());
    const scopes = (await s.customRequest("scopes", { frameId })).scopes ?? [];
    const out: string[] = [];
    for (const sc of scopes) {
      if (scopeName !== "all" && !sc.name.toLowerCase().includes(scopeName === "args" ? "arg" : "local")) continue;
      out.push(`${sc.name}:`);
      const vars = (await s.customRequest("variables", { variablesReference: sc.variablesReference })).variables ?? [];
      for (const v of vars.slice(0, 60)) {
        out.push(`  ${v.name} = ${v.value}${v.type ? " (" + v.type + ")" : ""}`);
        if (v.variablesReference > 0 && vars.length <= 10) {
          const kids = (await s.customRequest("variables", { variablesReference: v.variablesReference })).variables ?? [];
          for (const k of kids.slice(0, 20)) out.push(`    ${k.name} = ${k.value}`);
        }
      }
    }
    return out.join("\n") || "no variables in scope";
  }

  async topFrameId(): Promise<number> {
    const s = this.need();
    const tid = this.threadId ?? (await s.customRequest("threads")).threads?.[0]?.id;
    const st = await s.customRequest("stackTrace", { threadId: tid, startFrame: 0, levels: 1 });
    return st.stackFrames?.[0]?.id;
  }

  async resume(cmd: "continue" | "next" | "stepIn" | "stepOut"): Promise<string> {
    const s = this.need();
    const tid = this.threadId ?? (await s.customRequest("threads")).threads?.[0]?.id;
    await s.customRequest(cmd, { threadId: tid });
    return this.describe(await this.waiter.wait(WAIT_MS));
  }
}

export function registerDebugTools(reg: ToolRegistry, ctx: vscode.ExtensionContext) {
  const dm = new DebugManager(ctx);
  reg.add({ name: "debug_configs", description: "Launch configurations available (launch.json). debug_start also accepts program + type directly.", inputSchema: { type: "object", properties: {} },
    handler: async () => {
      const cfgs = vscode.workspace.getConfiguration("launch").get<any[]>("configurations") ?? [];
      return cfgs.length ? cfgs.map((c) => `${c.name} (${c.type}, ${c.request})`).join("\n") : "no launch configurations; pass program and type (go|python) to debug_start";
    } });
  reg.add({ name: "debug_start", description: "Start debugging and wait until it stops at a breakpoint or exits (60s max). Use config (launch.json name) or program + type (go|python) + args.",
    inputSchema: { type: "object", properties: { config: { type: "string" }, program: { type: "string" }, type: { type: "string", enum: ["go", "python"] }, args: { type: "array", items: { type: "string" } } } },
    handler: (a) => dm.start(a) });
  reg.add({ name: "debug_breakpoint", description: "Add or remove a breakpoint at path:line, optionally with a condition. Returns the breakpoints in that file.",
    inputSchema: { type: "object", properties: { path: { type: "string" }, line: { type: "integer" }, action: { type: "string", enum: ["add", "remove"] }, condition: { type: "string" } }, required: ["path", "line"] },
    handler: async (a) => {
      const uri = vscode.Uri.file(absPath(folders(), a.path));
      const existing = vscode.debug.breakpoints.filter((b) => b instanceof vscode.SourceBreakpoint && b.location.uri.fsPath === uri.fsPath) as vscode.SourceBreakpoint[];
      if (a.action === "remove") vscode.debug.removeBreakpoints(existing.filter((b) => b.location.range.start.line === a.line - 1));
      else vscode.debug.addBreakpoints([new vscode.SourceBreakpoint(new vscode.Location(uri, new vscode.Position(a.line - 1, 0)), true, a.condition)]);
      const now = vscode.debug.breakpoints.filter((b) => b instanceof vscode.SourceBreakpoint && b.location.uri.fsPath === uri.fsPath) as vscode.SourceBreakpoint[];
      return now.length ? now.map((b) => `${a.path}:${b.location.range.start.line + 1}${b.condition ? " if " + b.condition : ""}`).join("\n") : `no breakpoints in ${a.path}`;
    } });
  reg.add({ name: "debug_continue", description: "Continue and wait for the next stop.", inputSchema: { type: "object", properties: {} }, handler: () => dm.resume("continue") });
  reg.add({ name: "debug_step", description: "Step over, into or out, and wait for the stop.", inputSchema: { type: "object", properties: { step: { type: "string", enum: ["over", "into", "out"] } }, required: ["step"] },
    handler: (a) => dm.resume(a.step === "into" ? "stepIn" : a.step === "out" ? "stepOut" : "next") });
  reg.add({ name: "debug_stack", description: "Current call stack.", inputSchema: { type: "object", properties: { depth: { type: "integer" } } }, handler: (a) => dm.stack(a.depth ?? 10) });
  reg.add({ name: "debug_variables", description: "Variables in a frame (default: top). scope: locals, args or all.", inputSchema: { type: "object", properties: { frame: { type: "integer" }, scope: { type: "string" } } },
    handler: (a) => dm.variables(a.frame, a.scope ?? "locals") });
  reg.add({ name: "debug_evaluate", description: "Evaluate an expression in a frame (default: top).", inputSchema: { type: "object", properties: { expression: { type: "string" }, frame: { type: "integer" } }, required: ["expression"] },
    handler: async (a) => { const r = await dm.need().customRequest("evaluate", { expression: a.expression, frameId: a.frame ?? (await dm.topFrameId()), context: "repl" }); return `${r.result}${r.type ? " (" + r.type + ")" : ""}`; } });
  reg.add({ name: "debug_output", description: "Debug console output since the last read (pass the returned cursor next time).", inputSchema: { type: "object", properties: { since: { type: "integer" } } },
    handler: async (a) => { const r = dm.log.since(a.since ?? 0); return (r.lines.join("\n") || "(no new output)") + `\n[cursor ${r.cursor}]`; } });
  reg.add({ name: "debug_stop", description: "Stop the debug session.", inputSchema: { type: "object", properties: {} },
    handler: async () => { if (dm.session) await vscode.debug.stopDebugging(dm.session); dm.session = undefined; dm.active = undefined; return "stopped"; } });
}
