import * as vscode from "vscode";
import { ToolRegistry } from "./registry";
import { AcceptAllTracker } from "../lib/acceptall";
import { firstChangedLine } from "../lib/firstchange";

const SCHEME = "be-code-review";

let reviewSeq = 0;

export function registerReviewTool(reg: ToolRegistry, ctx: vscode.ExtensionContext) {
  const docs = new Map<string, string>();
  ctx.subscriptions.push(vscode.workspace.registerTextDocumentContentProvider(SCHEME, {
    provideTextDocumentContent: (uri) => docs.get(uri.toString()) ?? "",
  }));
  // "Accept all this session" is the choice of one BE-Code session, not of
  // the window: scope it to the connection that made it, and forget it as
  // soon as that connection closes.
  const acceptAll = new AcceptAllTracker();
  reg.onConnectionClosed((conn) => acceptAll.clear(conn));

  reg.add({
    name: "review_diff",
    // BE-Code calls this one by name; the model must never see it in
    // tools/list, let alone drive the user's review dialog itself.
    hidden: true,
    description: "(used by BE-Code, not by the model) Show a proposed file change as an editor diff and return the user's decision.",
    inputSchema: { type: "object", properties: { path: { type: "string" }, original: { type: "string" }, proposed: { type: "string" }, summary: { type: "string" } }, required: ["path", "proposed"] },
    handler: async (a, conn) => {
      if (typeof a?.path !== "string" || !a.path) throw new Error("review_diff: 'path' is required");
      if (typeof a?.proposed !== "string") throw new Error("review_diff: 'proposed' must be a string");

      if (acceptAll.has(conn)) return JSON.stringify({ decision: "accept" });

      const id = `${reviewSeq++}-${Math.random().toString(36).slice(2, 8)}`;
      const left = vscode.Uri.from({ scheme: SCHEME, path: `/${id}/original/${a.path}` });
      const right = vscode.Uri.from({ scheme: SCHEME, path: `/${id}/proposed/${a.path}` });
      const title = `BE-Code: ${a.path} (proposed change ${id})`;

      docs.set(left.toString(), a.original ?? "");
      docs.set(right.toString(), a.proposed ?? "");

      try {
        // Open scrolled to the first changed line, not the top of the file.
        const line = firstChangedLine(a.original ?? "", a.proposed ?? "");
        await vscode.commands.executeCommand("vscode.diff", left, right, title, { preview: true, selection: new vscode.Range(line, 0, line, 0) });
        const choice = await vscode.window.showInformationMessage(a.summary ?? `Apply change to ${a.path}?`, { modal: true }, "Accept", "Accept all this session", "Reject");

        if (choice === "Accept all this session") {
          acceptAll.set(conn);
          return JSON.stringify({ decision: "accept_all" });
        }
        return JSON.stringify({ decision: choice === "Accept" ? "accept" : "reject" });
      } finally {
        const leftStr = left.toString();
        const rightStr = right.toString();
        const toClose: vscode.Tab[] = [];
        for (const g of vscode.window.tabGroups.all) {
          for (const t of g.tabs) {
            if (t.input instanceof vscode.TabInputTextDiff) {
              if (t.input.original.toString() === leftStr && t.input.modified.toString() === rightStr) {
                toClose.push(t);
              }
            }
          }
        }
        if (toClose.length === 0) {
          for (const g of vscode.window.tabGroups.all) {
            for (const t of g.tabs) {
              if (t.label === title) toClose.push(t);
            }
          }
        }
        if (toClose.length > 0) {
          try {
            await vscode.window.tabGroups.close(toClose);
          } catch {
            // best-effort; nothing more we can do if closing fails
          }
        }
        docs.delete(leftStr);
        docs.delete(rightStr);
      }
    },
  });
}
