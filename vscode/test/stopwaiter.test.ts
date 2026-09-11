import { describe, it, expect } from "vitest";
import { StopWaiter, RingLog } from "../src/lib/stopwaiter";

describe("StopWaiter", () => {
  it("resolves the pending wait on a stopped event", async () => {
    const w = new StopWaiter();
    const p = w.wait(1000);
    w.onEvent({ event: "stopped", body: { reason: "breakpoint", threadId: 1 } });
    expect(await p).toEqual({ kind: "stopped", reason: "breakpoint", threadId: 1 });
  });
  it("resolves on terminated and times out otherwise", async () => {
    const w = new StopWaiter();
    const p = w.wait(1000);
    w.onEvent({ event: "terminated" });
    expect((await p).kind).toBe("terminated");
    expect((await w.wait(10)).kind).toBe("timeout");
  });
  it("delivers an event that arrived before wait was called", async () => {
    const w = new StopWaiter();
    w.onEvent({ event: "stopped", body: { reason: "step", threadId: 2 } });
    expect((await w.wait(10)).reason).toBe("step");
  });
  it("rejects a second concurrent wait instead of overwriting the first", async () => {
    const w = new StopWaiter();
    const p1 = w.wait(1000);
    await expect(w.wait(1000)).rejects.toThrow("a debug operation is already waiting for the program to stop");
    w.onEvent({ event: "terminated" });
    expect((await p1).kind).toBe("terminated");
  });
  it("a stale timer from an already-resolved wait does not corrupt a later wait", async () => {
    const w = new StopWaiter();
    const p = w.wait(10);
    w.onEvent({ event: "stopped", body: { reason: "breakpoint", threadId: 1 } });
    expect((await p).kind).toBe("stopped");
    await new Promise((r) => setTimeout(r, 30));
    const p2 = w.wait(1000);
    w.onEvent({ event: "terminated" });
    expect((await p2).kind).toBe("terminated");
  });
});

describe("RingLog", () => {
  it("keeps the newest lines and reads since a cursor", () => {
    const r = new RingLog(3);
    for (const l of ["a", "b", "c", "d"]) r.push(l);
    const first = r.since(0);
    expect(first.lines).toEqual(["b", "c", "d"]);
    r.push("e");
    expect(r.since(first.cursor).lines).toEqual(["e"]);
  });
});
