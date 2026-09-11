import * as vscode from "vscode";
import { ToolRegistry } from "./registry";
import { formatDiagnostics, DiagItem } from "../lib/format";
import { relPath } from "../lib/paths";

const sev = (s: vscode.DiagnosticSeverity): DiagItem["severity"] =>
  s === vscode.DiagnosticSeverity.Error ? "error" : s === vscode.DiagnosticSeverity.Warning ? "warning" : s === vscode.DiagnosticSeverity.Information ? "info" : "hint";

export function registerDiagnosticsTool(reg: ToolRegistry) {
  reg.add({
    name: "diagnostics",
    description: "Errors and warnings from the editor's language servers, as path:line:col severity source: message. Optional path filters to one file; severity: error, warning or all (default: errors and warnings).",
    inputSchema: { type: "object", properties: { path: { type: "string" }, severity: { type: "string", enum: ["error", "warning", "all"] } } },
    handler: async (args) => {
      const folders = (vscode.workspace.workspaceFolders ?? []).map((f) => f.uri.fsPath);
      const want = args.severity === "all" ? ["error", "warning", "info", "hint"] : args.severity === "error" ? ["error"] : ["error", "warning"];
      const items: DiagItem[] = [];
      for (const [uri, diags] of vscode.languages.getDiagnostics()) {
        const p = relPath(folders, uri.fsPath);
        if (args.path && p !== args.path) continue;
        for (const d of diags) {
          const s = sev(d.severity);
          if (!want.includes(s)) continue;
          items.push({ path: p, line: d.range.start.line + 1, col: d.range.start.character + 1, severity: s, source: d.source ?? "", message: d.message });
        }
      }
      return formatDiagnostics(items);
    },
  });
}
