import { describe, it, expect } from "vitest";
import { AcceptAllTracker } from "../src/lib/acceptall";

describe("AcceptAllTracker", () => {
  it("scopes accept-all to the connection that chose it", () => {
    const t = new AcceptAllTracker();
    const a = {}, b = {};
    expect(t.has(a)).toBe(false);
    t.set(a);
    expect(t.has(a)).toBe(true);
    expect(t.has(b)).toBe(false);
  });
  it("forgets a connection when it closes", () => {
    const t = new AcceptAllTracker();
    const a = {};
    t.set(a);
    t.clear(a);
    expect(t.has(a)).toBe(false);
  });
  it("never accepts all without a connection identity", () => {
    const t = new AcceptAllTracker();
    t.set(undefined);
    expect(t.has(undefined)).toBe(false);
  });
});
