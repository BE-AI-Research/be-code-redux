import * as vscode from "vscode";
import { ToolRegistry } from "./registry";
import { relPath, absPath } from "../lib/paths";

const folders = () => (vscode.workspace.workspaceFolders ?? []).map((f) => f.uri.fsPath);
const pos = (args: any) => new vscode.Position(Math.max(0, (args.line ?? 1) - 1), Math.max(0, (args.col ?? 1) - 1));
const uriOf = (p: string) => vscode.Uri.file(absPath(folders(), p));
const locLine = (l: vscode.Location | vscode.LocationLink) => {
  const uri = "targetUri" in l ? l.targetUri : l.uri;
  const range = "targetRange" in l ? l.targetRange : l.range;
  return `${relPath(folders(), uri.fsPath)}:${range.start.line + 1}:${range.start.character + 1}`;
};

export function registerEditorTools(reg: ToolRegistry) {
  reg.add({
    name: "context",
    description: "What the user is looking at: active file, cursor line, selection, open files, workspace folders (JSON).",
    inputSchema: { type: "object", properties: {} },
    handler: async () => {
      const ed = vscode.window.activeTextEditor;
      const open = vscode.workspace.textDocuments.filter((d) => d.uri.scheme === "file").map((d) => relPath(folders(), d.uri.fsPath));
      if (!ed || ed.document.uri.scheme !== "file") return JSON.stringify({ file: "", line: 0, selStart: 0, selEnd: 0, selection: "", open, workspaceFolders: folders() });
      const sel = ed.selection;
      const selection = sel.isEmpty ? "" : ed.document.getText(sel).slice(0, 2048);
      return JSON.stringify({
        file: relPath(folders(), ed.document.uri.fsPath), line: sel.active.line + 1,
        selStart: sel.isEmpty ? 0 : sel.start.line + 1, selEnd: sel.isEmpty ? 0 : sel.end.line + 1,
        selection, open, workspaceFolders: folders(),
      });
    },
  });
  reg.add({
    name: "open",
    description: "Reveal a file in the editor, optionally at a line.",
    inputSchema: { type: "object", properties: { path: { type: "string" }, line: { type: "integer" } }, required: ["path"] },
    handler: async (args) => {
      const doc = await vscode.workspace.openTextDocument(uriOf(args.path));
      const ed = await vscode.window.showTextDocument(doc, { preserveFocus: true });
      if (args.line) { const p = new vscode.Position(args.line - 1, 0); ed.revealRange(new vscode.Range(p, p), vscode.TextEditorRevealType.InCenter); ed.selection = new vscode.Selection(p, p); }
      return `opened ${args.path}${args.line ? ":" + args.line : ""}`;
    },
  });
  reg.add({
    name: "definition",
    description: "Where the symbol at path:line:col is defined (language server).",
    inputSchema: { type: "object", properties: { path: { type: "string" }, line: { type: "integer" }, col: { type: "integer" } }, required: ["path", "line", "col"] },
    handler: async (args) => {
      const res = (await vscode.commands.executeCommand<(vscode.Location | vscode.LocationLink)[]>("vscode.executeDefinitionProvider", uriOf(args.path), pos(args))) ?? [];
      return res.length ? res.map(locLine).join("\n") : "no definition found (is the language server running and the file saved?)";
    },
  });
  reg.add({
    name: "references",
    description: "References to the symbol at path:line:col, as path:line: text.",
    inputSchema: { type: "object", properties: { path: { type: "string" }, line: { type: "integer" }, col: { type: "integer" }, max: { type: "integer" } }, required: ["path", "line", "col"] },
    handler: async (args) => {
      const res = (await vscode.commands.executeCommand<vscode.Location[]>("vscode.executeReferenceProvider", uriOf(args.path), pos(args))) ?? [];
      const max = args.max ?? 50;
      const lines: string[] = [];
      for (const l of res.slice(0, max)) {
        const doc = await vscode.workspace.openTextDocument(l.uri);
        lines.push(`${relPath(folders(), l.uri.fsPath)}:${l.range.start.line + 1}: ${doc.lineAt(l.range.start.line).text.trim()}`);
      }
      return lines.length ? `${res.length} reference(s)\n` + lines.join("\n") : "no references found";
    },
  });
  reg.add({
    name: "hover",
    description: "Type or signature information for the symbol at path:line:col (language server hover).",
    inputSchema: { type: "object", properties: { path: { type: "string" }, line: { type: "integer" }, col: { type: "integer" } }, required: ["path", "line", "col"] },
    handler: async (args) => {
      const res = (await vscode.commands.executeCommand<vscode.Hover[]>("vscode.executeHoverProvider", uriOf(args.path), pos(args))) ?? [];
      const text = res.flatMap((h) => h.contents.map((c) => (typeof c === "string" ? c : "value" in c ? c.value : String(c)))).join("\n\n").trim();
      return text || "no hover information";
    },
  });
}
