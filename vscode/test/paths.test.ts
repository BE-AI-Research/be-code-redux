import { describe, it, expect, beforeAll, afterAll } from "vitest";
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, sep } from "node:path";
import { relPath, absPath } from "../src/lib/paths";

let rootA: string, rootB: string, folders: string[];

beforeAll(() => {
  const base = mkdtempSync(join(tmpdir(), "be-code-paths-"));
  rootA = join(base, "a");
  rootB = join(base, "b");
  mkdirSync(join(rootA, "src"), { recursive: true });
  mkdirSync(join(rootB, "src"), { recursive: true });
  writeFileSync(join(rootA, "only-in-a.txt"), "a");
  writeFileSync(join(rootB, "src", "only-in-b.ts"), "b");
  folders = [rootA, rootB];
});

afterAll(() => { try { rmSync(join(rootA, ".."), { recursive: true, force: true }); } catch { /* best effort */ } });

describe("absPath", () => {
  it("resolves a relative path against the folder that actually has the file", () => {
    expect(absPath(folders, "src/only-in-b.ts")).toBe(join(rootB, "src", "only-in-b.ts"));
    expect(absPath(folders, "only-in-a.txt")).toBe(join(rootA, "only-in-a.txt"));
  });
  it("falls back to the first folder for a file that does not exist yet", () => {
    expect(absPath(folders, "brand/new.ts")).toBe(join(rootA, "brand", "new.ts"));
  });
  it("accepts an absolute path inside a workspace folder", () => {
    expect(absPath(folders, join(rootB, "src", "only-in-b.ts"))).toBe(join(rootB, "src", "only-in-b.ts"));
  });
  it("rejects an escape via ..", () => {
    expect(() => absPath(folders, "../../etc/passwd")).toThrow(/outside the workspace/);
  });
  it("rejects an absolute path outside every folder", () => {
    expect(() => absPath(folders, `${sep}etc${sep}passwd`)).toThrow(/outside the workspace/);
  });
  it("does not treat a sibling folder with a shared prefix as inside", () => {
    expect(() => absPath(folders, `${rootA}-evil${sep}x.txt`)).toThrow(/outside the workspace/);
  });
  it("allows the folder itself", () => {
    expect(absPath(folders, rootA)).toBe(rootA);
  });
});

describe("relPath", () => {
  it("still reports a path relative to its folder", () => {
    expect(relPath(folders, join(rootB, "src", "only-in-b.ts"))).toBe("src/only-in-b.ts");
  });
});
