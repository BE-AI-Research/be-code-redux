import { describe, it, expect } from "vitest";
import { createConnection } from "node:net";
import { BridgeServer } from "../src/server";
import { ToolRegistry } from "../src/tools/registry";
import { AcceptAllTracker } from "../src/lib/acceptall";

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

// session opens an initialized connection and hands back replies by id, so a
// test can say which reply it is waiting for instead of assuming an order.
async function session(port: number, waitMs = 2000) {
  const got = new Map<number, any>();
  const waiting = new Map<number, (m: any) => void>();
  const c = createConnection({ host: "127.0.0.1", port });
  await new Promise<void>((resolve, reject) => { c.once("connect", () => resolve()); c.once("error", reject); });
  c.on("error", () => {});
  let buf = "";
  c.on("data", (d) => {
    buf += d.toString();
    let i: number;
    while ((i = buf.indexOf("\n")) >= 0) {
      const m = JSON.parse(buf.slice(0, i));
      buf = buf.slice(i + 1);
      got.set(m.id, m);
      waiting.get(m.id)?.(m);
    }
  });
  const send = (m: object) => { c.write(JSON.stringify(m) + "\n"); };
  const reply = (id: number): Promise<any> => {
    if (got.has(id)) return Promise.resolve(got.get(id));
    return new Promise((resolve, reject) => {
      const t = setTimeout(() => reject(new Error(`no reply to id ${id} within ${waitMs}ms`)), waitMs);
      waiting.set(id, (m) => { clearTimeout(t); resolve(m); });
    });
  };
  send({ jsonrpc: "2.0", id: 1, method: "initialize", params: { auth: { token: "tok" } } });
  await reply(1);
  return {
    send, reply,
    seen: (id: number) => got.has(id),
    end: () => c.end(),
    destroy: () => new Promise<void>((resolve) => { c.once("close", () => resolve()); c.destroy(); }),
  };
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
  // This used to assert the opposite: that a slow call holds back the reply to
  // a later one. That pinned a defect. The harness multiplexes calls on one
  // connection and sends review_cancel on the same connection as the
  // review_diff it withdraws, so a server that finishes one request before it
  // starts the next can never process the cancel.
  it("answers a later call while an earlier one is still pending", async () => {
    const reg = new ToolRegistry();
    let open!: () => void;
    const gate = new Promise<void>((r) => (open = r));
    reg.add({ name: "slow", description: "", inputSchema: { type: "object" }, handler: async () => { await gate; return "slow-done"; } });
    reg.add({ name: "fast", description: "", inputSchema: { type: "object" }, handler: async () => "fast-done" });
    const s = new BridgeServer(reg, "tok");
    const port = await s.listen(0);
    const c = await session(port);
    c.send({ jsonrpc: "2.0", id: 2, method: "tools/call", params: { name: "slow", arguments: {} } });
    c.send({ jsonrpc: "2.0", id: 3, method: "tools/call", params: { name: "fast", arguments: {} } });
    const fast = await c.reply(3);
    expect(fast.result.content[0].text).toBe("fast-done");
    expect(c.seen(2)).toBe(false);
    open();
    expect((await c.reply(2)).result.content[0].text).toBe("slow-done");
    c.end();
    await s.close();
  });
  it("lets one call unblock another on the same connection", async () => {
    const reg = new ToolRegistry();
    let open!: () => void;
    const gate = new Promise<void>((r) => (open = r));
    reg.add({ name: "wait", description: "", inputSchema: { type: "object" }, handler: async () => { await gate; return "released"; } });
    reg.add({ name: "release", description: "", inputSchema: { type: "object" }, handler: async () => { open(); return "ok"; } });
    const s = new BridgeServer(reg, "tok");
    const port = await s.listen(0);
    const c = await session(port);
    c.send({ jsonrpc: "2.0", id: 2, method: "tools/call", params: { name: "wait", arguments: {} } });
    c.send({ jsonrpc: "2.0", id: 3, method: "tools/call", params: { name: "release", arguments: {} } });
    expect((await c.reply(3)).result.content[0].text).toBe("ok");
    expect((await c.reply(2)).result.content[0].text).toBe("released");
    c.end();
    await s.close();
  });
  it("answers tools/list while a call is pending", async () => {
    const reg = new ToolRegistry();
    let open!: () => void;
    const gate = new Promise<void>((r) => (open = r));
    reg.add({ name: "slow", description: "", inputSchema: { type: "object" }, handler: async () => { await gate; return "slow-done"; } });
    const s = new BridgeServer(reg, "tok");
    const port = await s.listen(0);
    const c = await session(port);
    c.send({ jsonrpc: "2.0", id: 2, method: "tools/call", params: { name: "slow", arguments: {} } });
    c.send({ jsonrpc: "2.0", id: 3, method: "tools/list" });
    expect((await c.reply(3)).result.tools[0].name).toBe("slow");
    open();
    await c.reply(2);
    c.end();
    await s.close();
  });
  it("refuses a call that arrives before initialize even when initialize follows", async () => {
    const reg = new ToolRegistry();
    reg.add({ name: "echo", description: "", inputSchema: { type: "object" }, handler: async () => "hi" });
    const s = new BridgeServer(reg, "tok");
    const port = await s.listen(0);
    const res = await rpc(port, [
      { jsonrpc: "2.0", id: 1, method: "tools/call", params: { name: "echo", arguments: {} } },
      { jsonrpc: "2.0", id: 2, method: "initialize", params: { auth: { token: "tok" } } },
    ]);
    const byId = new Map(res.map((m) => [m.id, m]));
    expect(byId.get(1).error.code).toBe(-32001);
    expect(byId.get(2).result.serverInfo.name).toBe("be-code-vscode");
    await s.close();
  });
  it("drops the reply of a call whose connection has gone", async () => {
    const reg = new ToolRegistry();
    let open!: () => void;
    const gate = new Promise<void>((r) => (open = r));
    let finished!: () => void;
    const done = new Promise<void>((r) => (finished = r));
    reg.add({ name: "slow", description: "", inputSchema: { type: "object" }, handler: async () => { await gate; finished(); return "late"; } });
    const s = new BridgeServer(reg, "tok");
    const port = await s.listen(0);
    const c = await session(port);
    c.send({ jsonrpc: "2.0", id: 2, method: "tools/call", params: { name: "slow", arguments: {} } });
    // tools/list is answered inline, so its reply proves the call before it was dispatched.
    c.send({ jsonrpc: "2.0", id: 3, method: "tools/list" });
    await c.reply(3);
    await c.destroy();
    open();
    await done;
    // The server is still serving: a write into the dead socket did not take it down.
    const again = await rpc(port, [{ jsonrpc: "2.0", id: 1, method: "initialize", params: { auth: { token: "tok" } } }]);
    expect(again[0].result.serverInfo.name).toBe("be-code-vscode");
    await s.close();
  });
});

describe("hidden tools", () => {
  it("omits hidden tools from tools/list but still serves tools/call", async () => {
    const reg = new ToolRegistry();
    reg.add({ name: "visible", description: "v", inputSchema: { type: "object" }, handler: async () => "v-ok" });
    reg.add({ name: "secret", description: "s", inputSchema: { type: "object" }, hidden: true, handler: async () => "s-ok" });
    const s = new BridgeServer(reg, "tok");
    const port = await s.listen(0);
    const res = await rpc(port, [
      { jsonrpc: "2.0", id: 1, method: "initialize", params: { auth: { token: "tok" } } },
      { jsonrpc: "2.0", id: 2, method: "tools/list" },
      { jsonrpc: "2.0", id: 3, method: "tools/call", params: { name: "secret", arguments: {} } },
    ]);
    expect(res[1].result.tools.map((t: any) => t.name)).toEqual(["visible"]);
    expect(res[2].result.content[0].text).toBe("s-ok");
    expect(res[2].result.isError).toBe(false);
    await s.close();
  });
});

describe("per-connection state", () => {
  it("keeps one connection's accept-all out of another's", async () => {
    const reg = new ToolRegistry();
    const tracker = new AcceptAllTracker();
    reg.onConnectionClosed((c) => tracker.clear(c));
    reg.add({
      name: "review_like", description: "", inputSchema: { type: "object" }, hidden: true,
      handler: async (_a, conn) => {
        if (tracker.has(conn)) return "accept (remembered)";
        tracker.set(conn);
        return "asked";
      },
    });
    const s = new BridgeServer(reg, "tok");
    const port = await s.listen(0);
    const call = { jsonrpc: "2.0", id: 2, method: "tools/call", params: { name: "review_like", arguments: {} } };
    const init = { jsonrpc: "2.0", id: 1, method: "initialize", params: { auth: { token: "tok" } } };

    const first = await rpc(port, [init, call, { ...call, id: 3 }]);
    expect(first[1].result.content[0].text).toBe("asked");
    expect(first[2].result.content[0].text).toBe("accept (remembered)");

    // A second, independent connection must start from scratch.
    const second = await rpc(port, [init, call]);
    expect(second[1].result.content[0].text).toBe("asked");
    await s.close();
  });
});
