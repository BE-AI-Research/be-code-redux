export interface DiagItem { path: string; line: number; col: number; severity: "error" | "warning" | "info" | "hint"; source: string; message: string }

export function formatDiagnostics(items: DiagItem[]): string {
  if (items.length === 0) return "no diagnostics";
  const byFile = new Map<string, DiagItem[]>();
  for (const d of items) byFile.set(d.path, [...(byFile.get(d.path) ?? []), d]);
  const errors = items.filter((d) => d.severity === "error").length;
  const warnings = items.filter((d) => d.severity === "warning").length;
  const plural = (n: number, w: string) => `${n} ${w}${n === 1 ? "" : "s"}`;
  const lines = [`${plural(errors, "error")}, ${plural(warnings, "warning")} in ${byFile.size} file${byFile.size === 1 ? "" : "s"}`];
  for (const file of [...byFile.keys()].sort()) {
    for (const d of byFile.get(file)!.sort((a, b) => a.line - b.line)) {
      lines.push(`${d.path}:${d.line}:${d.col} ${d.severity} ${d.source || "-"}: ${d.message.split("\n")[0]}`);
    }
  }
  return lines.join("\n");
}
