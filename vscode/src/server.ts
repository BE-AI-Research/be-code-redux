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
    for (const s of this.socks) s.destroy();
    this.socks.clear();
    await new Promise<void>((r) => (this.srv ? this.srv.close(() => r()) : r()));
  }

  private handle(sock: Socket) {
    const framer = new LineFramer();
    let authed = false;
    this.socks.add(sock);
    this.onConnectionChange?.();
    sock.on("close", () => { this.socks.delete(sock); this.onConnectionChange?.(); });
    sock.on("error", () => {});
    sock.on("data", async (chunk) => {
      for (const line of framer.push(chunk)) {
        let req: any;
        try { req = JSON.parse(line); } catch { continue; }
        if (req.id === undefined) continue; // notification
        const reply = (result: unknown) => sock.write(encode({ jsonrpc: "2.0", id: req.id, result }));
        const fail = (code: number, message: string) => sock.write(encode({ jsonrpc: "2.0", id: req.id, error: { code, message } }));
        switch (req.method) {
          case "initialize":
            if (req.params?.auth?.token !== this.token) { fail(-32001, "bad token"); sock.end(); return; }
            authed = true;
            reply({ protocolVersion: "2024-11-05", capabilities: { tools: {} }, serverInfo: { name: "be-code-vscode", version: "1.0.0" } });
            break;
          case "tools/list":
            if (!authed) { fail(-32001, "not authenticated"); break; }
            reply({ tools: this.tools.list() });
            break;
          case "tools/call": {
            if (!authed) { fail(-32001, "not authenticated"); break; }
            const { text, isError } = await this.tools.call(req.params?.name, req.params?.arguments);
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
    });
  }
}
