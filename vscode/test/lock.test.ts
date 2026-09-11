import { describe, it, expect } from "vitest";
import { mkdtempSync, readFileSync, existsSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { lockDir, writeLock, removeLock } from "../src/lib/lock";

describe("lock file", () => {
  it("writes and removes ~/.be-code/ide/<pid>.json", async () => {
    const home = mkdtempSync(join(tmpdir(), "bec-"));
    const p = await writeLock(home, { pid: 4242, port: 5000, token: "t", workspaceFolders: ["/w"], ideName: "vscode", version: "1.0.0" });
    expect(p).toBe(join(lockDir(home), "4242.json"));
    expect(JSON.parse(readFileSync(p, "utf8")).port).toBe(5000);
    await removeLock(home, 4242);
    expect(existsSync(p)).toBe(false);
  });
});
