import { promises as fs } from "node:fs";
import { join } from "node:path";

export interface LockInfo {
  pid: number; port: number; token: string; workspaceFolders: string[]; ideName: string; version: string;
}

export function lockDir(home: string): string {
  return join(home, ".be-code", "ide");
}

export async function writeLock(home: string, info: LockInfo): Promise<string> {
  const dir = lockDir(home);
  await fs.mkdir(dir, { recursive: true, mode: 0o700 });
  const p = join(dir, `${info.pid}.json`);
  await fs.writeFile(p, JSON.stringify(info, null, 1), { mode: 0o600 });
  return p;
}

export async function removeLock(home: string, pid: number): Promise<void> {
  await fs.rm(join(lockDir(home), `${pid}.json`), { force: true });
}
