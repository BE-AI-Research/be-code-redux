import { createServer, Server, Socket } from "node:net";
import { LineFramer, encode } from "./lib/framing";
import { ToolRegistry } from "./tools/registry";

export class BridgeServer {
  private srv?: Server;
  private socks = new Set<Socket>();
  onConnectionChange?: () => void;

  constructor(private tools: ToolRegistry, private token: string) {}

  get connections(): number { return this.socks.size; }

  listen(port: number): Promise<number> {
    return new Promise((resolve, reject) => {
      this.srv = createServer((sock) => this.handle(sock));
      this.srv.on("error", reject);
      this.srv.listen(port, "127.0.0.1", () => {
        const addr = this.srv!.address();
        resolve(typeof addr === "object" && addr ? addr.port : port);
      });
    });
  }

  async close(): Promise<void> {
    for (const s of this.socks) { this.tools.onConnectionClosed?.(s); s.destroy(); }
    this.socks.clear();
    await new Promise<void>((r) => (this.srv ? this.srv.close(() => r()) : r()));
  }

  private handle(sock: Socket) {
    const framer = new LineFramer();
    const state = { authed: false };
    let queue: Promise<void> = Promise.resolve();
    this.socks.add(sock);
    this.onConnectionChange?.();
    sock.on("close", () => {
      this.socks.delete(sock);
      // Let tools drop anything scoped to this connection (accept-all).
      this.tools.onConnectionClosed?.(sock);
      this.onConnectionChange?.();
    });
    sock.on("error", () => {});
    sock.on("data", (chunk) => {
      for (const line of framer.push(chunk)) {
        queue = queue.then(() => this.handleLine(sock, state, line)).catch(() => {});
      }
    });
  }

  private async handleLine(sock: Socket, state: { authed: boolean }, line: string): Promise<void> {
    let req: any;
    try { req = JSON.parse(line); } catch { return; }
    if (req.id === undefined) return; // notification
    // A tool call can outlive its connection, so a reply may find the socket
    // gone. A client that went away is ordinary: say nothing.
    const send = (msg: object) => { if (!sock.destroyed && sock.writable) sock.write(encode(msg)); };
    const reply = (result: unknown) => send({ jsonrpc: "2.0", id: req.id, result });
    const fail = (code: number, message: string) => send({ jsonrpc: "2.0", id: req.id, error: { code, message } });
    switch (req.method) {
      case "initialize":
        if (req.params?.auth?.token !== this.token) { fail(-32001, "bad token"); sock.end(); return; }
        state.authed = true;
        reply({ protocolVersion: "2024-11-05", capabilities: { tools: {} }, serverInfo: { name: "be-code-vscode", version: "1.0.0" } });
        break;
      case "tools/list":
        if (!state.authed) { fail(-32001, "not authenticated"); break; }
        reply({ tools: this.tools.list() });
        break;
      case "tools/call": {
        if (!state.authed) { fail(-32001, "not authenticated"); break; }
        // Dispatched in order, but not awaited: the call runs on its own and
        // answers by id when it finishes. The harness multiplexes calls on one
        // connection and sends review_cancel on the same connection as the
        // review_diff it withdraws; awaiting here queued the cancel behind the
        // very call it was meant to cancel, and every later tool call behind
        // an open diff. ToolRegistry.call never rejects (a throwing handler is
        // an isError result), so the catch is only a last line of defence.
        void this.tools.call(req.params?.name, req.params?.arguments, sock)
          .then(({ text, isError }) => reply({ content: [{ type: "text", text }], isError }))
          .catch((e: any) => fail(-32603, e?.message ?? String(e)));
        break;
      }
      case "ping":
        reply({});
        break;
      default:
        fail(-32601, `unknown method ${req.method}`);
    }
  }
}
