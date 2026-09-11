import { describe, it, expect } from "vitest";
import { LineFramer, encode } from "../src/lib/framing";

describe("LineFramer", () => {
  it("splits complete lines and keeps the partial tail", () => {
    const f = new LineFramer();
    expect(f.push(Buffer.from('{"a":1}\n{"b":'))).toEqual(['{"a":1}']);
    expect(f.push(Buffer.from('2}\n'))).toEqual(['{"b":2}']);
  });
  it("handles multiple lines in one chunk and CRLF", () => {
    const f = new LineFramer();
    expect(f.push(Buffer.from("x\r\ny\n"))).toEqual(["x", "y"]);
  });
});

describe("encode", () => {
  it("appends a newline", () => {
    expect(encode({ jsonrpc: "2.0", id: 1, result: {} })).toBe('{"jsonrpc":"2.0","id":1,"result":{}}\n');
  });
});
