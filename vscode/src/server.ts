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
    const reply = (result: unknown) => sock.write(encode({ jsonrpc: "2.0", id: req.id, result }));
    const fail = (code: number, message: string) => sock.write(encode({ jsonrpc: "2.0", id: req.id, error: { code, message } }));
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
        const { text, isError } = await this.tools.call(req.params?.name, req.params?.arguments, sock);
        reply({ content: [{ type: "text", text }], isError });
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
