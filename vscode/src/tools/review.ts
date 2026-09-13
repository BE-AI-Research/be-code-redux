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

  // pending tracks reviews currently awaiting a decision, keyed by path, so
  // a review_cancel for the same path (the terminal side of a shared review
  // answered first) can resolve review_diff's race instead of leaving it
  // open until the caller's own timeout gives up on it.
  const pending = new Map<string, { cancel: () => void }>();

  reg.add({
    name: "review_diff",
    // BE-Code calls this one by name; the model must never see it in
    // tools/list, let alone drive the user's review dialog itself.
    hidden: true,
    description: "(used by BE-Code, not by the model) Show a proposed file change as an editor diff and return the user's decision.",
    inputSchema: { type: "object", properties: { path: { type: "string" }, original: { type: "string" }, proposed: { type: "string" }, summary: { type: "string" }, shared: { type: "boolean" } }, required: ["path", "proposed"] },
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
        // A shared review is also being asked as a terminal prompt; VS Code
        // cannot dismiss a modal message from the API, so this one must be
        // non-modal (it stays visible, but is otherwise resolved via the
        // race below the moment either side answers).
        const choice = await Promise.race([
          vscode.window.showInformationMessage(a.summary ?? `Apply change to ${a.path}?`, { modal: !a.shared }, "Accept", "Accept all this session", "Reject"),
          new Promise<"__cancel__">((resolve) => pending.set(a.path, { cancel: () => resolve("__cancel__") })),
        ]);

        if (choice === "__cancel__") return JSON.stringify({ decision: "cancelled" });
        if (choice === "Accept all this session") {
          acceptAll.set(conn);
          return JSON.stringify({ decision: "accept_all" });
        }
        return JSON.stringify({ decision: choice === "Accept" ? "accept" : "reject" });
      } finally {
        pending.delete(a.path);
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

  reg.add({
    name: "review_cancel",
    // Driven by BE-Code alone, same as review_diff: it withdraws a review
    // the model never chose to raise, so the model must never see or call
    // it directly either.
    hidden: true,
    description: "(used by BE-Code, not by the model) Withdraw a pending review for a path.",
    inputSchema: { type: "object", properties: { path: { type: "string" } }, required: ["path"] },
    handler: async (a) => {
      const p = typeof a?.path === "string" ? pending.get(a.path) : undefined;
      if (!p) return JSON.stringify({ cancelled: false });
      p.cancel();
      return JSON.stringify({ cancelled: true });
    },
  });
}
