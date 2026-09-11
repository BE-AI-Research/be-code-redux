import * as vscode from "vscode";
import { ToolRegistry } from "./registry";

const SCHEME = "be-code-review";

export function registerReviewTool(reg: ToolRegistry, ctx: vscode.ExtensionContext) {
  const docs = new Map<string, string>();
  ctx.subscriptions.push(vscode.workspace.registerTextDocumentContentProvider(SCHEME, {
    provideTextDocumentContent: (uri) => docs.get(uri.toString()) ?? "",
  }));
  let acceptAll = false;

  reg.add({
    name: "review_diff",
    description: "(used by BE-Code, not by the model) Show a proposed file change as an editor diff and return the user's decision.",
    inputSchema: { type: "object", properties: { path: { type: "string" }, original: { type: "string" }, proposed: { type: "string" }, summary: { type: "string" } }, required: ["path", "proposed"] },
    handler: async (a) => {
      if (typeof a?.path !== "string" || !a.path) throw new Error("review_diff: 'path' is required");
      if (typeof a?.proposed !== "string") throw new Error("review_diff: 'proposed' must be a string");

      if (acceptAll) return JSON.stringify({ decision: "accept" });

      const id = Date.now().toString(36);
      const left = vscode.Uri.parse(`${SCHEME}:/${id}/original/${a.path}`);
      const right = vscode.Uri.parse(`${SCHEME}:/${id}/proposed/${a.path}`);
      const title = `BE-Code: ${a.path} (proposed change)`;

      docs.set(left.toString(), a.original ?? "");
      docs.set(right.toString(), a.proposed ?? "");

      try {
        await vscode.commands.executeCommand("vscode.diff", left, right, title, { preview: true });
        const choice = await vscode.window.showInformationMessage(a.summary ?? `Apply change to ${a.path}?`, { modal: true }, "Accept", "Accept all this session", "Reject");

        if (choice === "Accept all this session") {
          acceptAll = true;
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
