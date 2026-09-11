import { relative, isAbsolute, resolve, join, sep } from "node:path";
import { existsSync } from "node:fs";

export function relPath(folders: string[], fsPath: string): string {
  for (const f of folders) {
    const r = relative(f, fsPath);
    if (r && !r.startsWith("..") && !isAbsolute(r)) return r.split("\\").join("/");
  }
  return fsPath;
}

// inside reports whether abs is the folder itself or a path beneath it.
// The separator matters: "/ws-evil/x" must not count as inside "/ws".
function inside(folder: string, abs: string): boolean {
  const f = resolve(folder);
  return abs === f || abs.startsWith(f.endsWith(sep) ? f : f + sep);
}

// absPath turns a tool argument into an absolute path inside the workspace.
// A relative path is resolved against the first workspace folder that
// actually has such a file (multi-root workspaces would otherwise always
// resolve into the first folder), falling back to the first folder for
// files that do not exist yet. Whatever the input, the result must lie
// inside one of the workspace folders — the editor bridge is confined to
// the workspace exactly as BE-Code's own file tools are.
export function absPath(folders: string[], p: string): string {
  const roots = folders.length ? folders : [process.cwd()];
  let abs: string;
  if (isAbsolute(p)) {
    abs = resolve(p);
  } else {
    const match = roots.find((f) => existsSync(join(f, p)));
    abs = resolve(match ?? roots[0], p);
  }
  if (!roots.some((f) => inside(f, abs))) throw new Error("path is outside the workspace");
  return abs;
}
