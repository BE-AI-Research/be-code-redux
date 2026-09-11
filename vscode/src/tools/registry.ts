export interface ToolDef {
  name: string;
  description: string;
  inputSchema: object;
  // hidden tools are callable but never advertised by tools/list. Used for
  // tools BE-Code drives itself (review_diff): the model must not see them,
  // let alone call them.
  hidden?: boolean;
  // conn identifies the connection the call arrived on, for per-connection
  // state (see AcceptAllTracker); it is undefined for direct calls.
  handler: (args: any, conn?: object) => Promise<string>;
}

export class ToolRegistry {
  private tools = new Map<string, ToolDef>();
  private closeHandlers: ((conn: object) => void)[] = [];

  add(t: ToolDef): void { this.tools.set(t.name, t); }

  // onConnectionClosed registers a callback, and — called with a
  // connection — runs them all, so tools can drop per-connection state as
  // soon as the client goes away rather than waiting for the garbage
  // collector to reclaim the socket.
  onConnectionClosed(fn: (conn: object) => void): void;
  onConnectionClosed(conn: object): void;
  onConnectionClosed(arg: ((conn: object) => void) | object): void {
    if (typeof arg === "function") { this.closeHandlers.push(arg as (conn: object) => void); return; }
    for (const fn of this.closeHandlers) { try { fn(arg); } catch { /* a tool's cleanup must not break the server */ } }
  }

  list() {
    return [...this.tools.values()].filter((t) => !t.hidden).map(({ name, description, inputSchema }) => ({ name, description, inputSchema }));
  }

  async call(name: string, args: any, conn?: object): Promise<{ text: string; isError: boolean }> {
    const t = this.tools.get(name);
    if (!t) return { text: `unknown tool ${name}`, isError: true };
    try {
      return { text: await t.handler(args ?? {}, conn), isError: false };
    } catch (e: any) {
      return { text: `${name}: ${e?.message ?? String(e)}`, isError: true };
    }
  }
}
