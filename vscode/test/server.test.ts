import { describe, it, expect } from "vitest";
import { createConnection } from "node:net";
import { BridgeServer } from "../src/server";
import { ToolRegistry } from "../src/tools/registry";

function rpc(port: number, msgs: object[]): Promise<any[]> {
  return new Promise((resolve, reject) => {
    const out: any[] = [];
    const c = createConnection({ host: "127.0.0.1", port }, () => {
      for (const m of msgs) c.write(JSON.stringify(m) + "\n");
    });
    let buf = "";
    c.on("data", (d) => {
      buf += d.toString();
      let i: number;
      while ((i = buf.indexOf("\n")) >= 0) { out.push(JSON.parse(buf.slice(0, i))); buf = buf.slice(i + 1); }
      if (out.length >= msgs.filter((m: any) => m.id !== undefined).length) { c.end(); resolve(out); }
    });
    c.on("error", reject);
    c.on("close", () => resolve(out));
  });
}

describe("BridgeServer", () => {
  it("handshakes with the right token, lists and calls tools", async () => {
    const reg = new ToolRegistry();
    reg.add({ name: "echo", description: "echo", inputSchema: { type: "object" }, handler: async (a) => `hi ${a.who}` });
    const s = new BridgeServer(reg, "tok");
    const port = await s.listen(0);
    const res = await rpc(port, [
      { jsonrpc: "2.0", id: 1, method: "initialize", params: { auth: { token: "tok" } } },
      { jsonrpc: "2.0", method: "notifications/initialized" },
      { jsonrpc: "2.0", id: 2, method: "tools/list" },
      { jsonrpc: "2.0", id: 3, method: "tools/call", params: { name: "echo", arguments: { who: "bob" } } },
    ]);
    expect(res[0].result.serverInfo.name).toBe("be-code-vscode");
    expect(res[1].result.tools[0].name).toBe("echo");
    expect(res[2].result.content[0].text).toBe("hi bob");
    await s.close();
  });
  it("rejects a bad token with -32001 and closes", async () => {
    const s = new BridgeServer(new ToolRegistry(), "tok");
    const port = await s.listen(0);
    const res = await rpc(port, [{ jsonrpc: "2.0", id: 1, method: "initialize", params: { auth: { token: "nope" } } }]);
    expect(res[0].error.code).toBe(-32001);
    await s.close();
  });
  it("reports tool exceptions as isError results", async () => {
    const reg = new ToolRegistry();
    reg.add({ name: "boom", description: "", inputSchema: { type: "object" }, handler: async () => { throw new Error("bad"); } });
    const s = new BridgeServer(reg, "tok");
    const port = await s.listen(0);
    const res = await rpc(port, [
      { jsonrpc: "2.0", id: 1, method: "initialize", params: { auth: { token: "tok" } } },
      { jsonrpc: "2.0", id: 2, method: "tools/call", params: { name: "boom", arguments: {} } },
    ]);
    expect(res[1].result.isError).toBe(true);
    expect(res[1].result.content[0].text).toContain("bad");
    await s.close();
  });
  it("replies in request order even when an earlier call is slower than a later one", async () => {
    const reg = new ToolRegistry();
    reg.add({ name: "slow", description: "", inputSchema: { type: "object" }, handler: async () => new Promise((r) => setTimeout(() => r("slow-done"), 150)) });
    reg.add({ name: "fast", description: "", inputSchema: { type: "object" }, handler: async () => "fast-done" });
    const s = new BridgeServer(reg, "tok");
    const port = await s.listen(0);
    const res: any[] = await new Promise((resolve, reject) => {
      const out: any[] = [];
      const c = createConnection({ host: "127.0.0.1", port }, () => {
        c.write(JSON.stringify({ jsonrpc: "2.0", id: 1, method: "initialize", params: { auth: { token: "tok" } } }) + "\n");
      });
      let buf = "";
      let sentCalls = false;
      c.on("data", (d) => {
        buf += d.toString();
        let i: number;
        while ((i = buf.indexOf("\n")) >= 0) { out.push(JSON.parse(buf.slice(0, i))); buf = buf.slice(i + 1); }
        if (!sentCalls && out.some((m) => m.id === 1)) {
          sentCalls = true;
          c.write(JSON.stringify({ jsonrpc: "2.0", id: 2, method: "tools/call", params: { name: "slow", arguments: {} } }) + "\n");
          setImmediate(() => {
            c.write(JSON.stringify({ jsonrpc: "2.0", id: 3, method: "tools/call", params: { name: "fast", arguments: {} } }) + "\n");
          });
        }
        if (out.length >= 3) { c.end(); resolve(out); }
      });
      c.on("error", reject);
      c.on("close", () => resolve(out));
    });
    expect(res[1].id).toBe(2);
    expect(res[2].id).toBe(3);
    expect(res[1].result.content[0].text).toBe("slow-done");
    expect(res[2].result.content[0].text).toBe("fast-done");
    await s.close();
  });
});
