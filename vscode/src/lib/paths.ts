import { relative, isAbsolute, resolve } from "node:path";

export function relPath(folders: string[], fsPath: string): string {
  for (const f of folders) {
    const r = relative(f, fsPath);
    if (r && !r.startsWith("..") && !isAbsolute(r)) return r.split("\\").join("/");
  }
  return fsPath;
}

export function absPath(folders: string[], p: string): string {
  if (isAbsolute(p)) return p;
  return resolve(folders[0] ?? process.cwd(), p);
}
