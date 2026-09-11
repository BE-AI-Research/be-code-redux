import { describe, it, expect } from "vitest";
import { firstChangedLine } from "../src/lib/firstchange";

describe("firstChangedLine", () => {
  it("finds the first differing line", () => {
    expect(firstChangedLine("a\nb\nc", "a\nB\nc")).toBe(1);
  });
  it("returns 0 for identical texts", () => {
    expect(firstChangedLine("a\nb", "a\nb")).toBe(0);
  });
  it("points at the appended tail", () => {
    expect(firstChangedLine("a\nb", "a\nb\nc")).toBe(2);
  });
  it("points at the removed tail", () => {
    expect(firstChangedLine("a\nb\nc", "a\nb")).toBe(2);
  });
  it("handles an empty original", () => {
    expect(firstChangedLine("", "new\nfile")).toBe(0);
  });
});
