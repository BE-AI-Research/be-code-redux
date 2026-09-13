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

  // pending tracks reviews currently awaiting a decision, keyed by
  // connection then path (never by path alone: the same path can be under
  // review from two BE-Code sessions at once, and each must only be able to
  // cancel its own), so a review_cancel for the same connection+path (the
  // terminal side of a shared review answered first) can resolve
  // review_diff's race instead of leaving it open until the caller's own
  // timeout gives up on it. `resolved` is set the instant a handle's race is
  // decided — by its own cancel() or by review_diff's normal completion —
  // so a cancel that arrives after that (but before the map entry is
  // cleaned up in `finally`) correctly reports failure rather than a
  // no-op success.
  type Handle = { resolved: boolean; cancel: () => void };
  const pending = new Map<object | undefined, Map<string, Handle>>();
  reg.onConnectionClosed((conn) => {
    acceptAll.clear(conn);
    pending.delete(conn);
  });

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

      // Declared outside the try so `finally` (which must remove only this
      // review's own map entry, by identity) can still see it.
      const handle: Handle = { resolved: false, cancel: () => {} };

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
          new Promise<"__cancel__">((resolve) => {
            handle.cancel = () => {
              if (handle.resolved) return;
              handle.resolved = true;
              resolve("__cancel__");
            };
            let byConn = pending.get(conn);
            if (!byConn) {
              byConn = new Map();
              pending.set(conn, byConn);
            }
            byConn.set(a.path, handle);
          }),
        ]);
        // Whichever side won the race, the decision is now final: a cancel
        // arriving after this point must not claim to have changed it.
        handle.resolved = true;

        if (choice === "__cancel__") return JSON.stringify({ decision: "cancelled" });
        if (choice === "Accept all this session") {
          acceptAll.set(conn);
          return JSON.stringify({ decision: "accept_all" });
        }
        if (choice === undefined && a.shared) {
          // A shared prompt is non-modal, so `undefined` is the user clearing
          // the notification away — not a rejection. Answering "reject" here
          // would be taken as the first real answer and would withdraw the
          // prompt from every attached terminal, so the change nobody
          // answered would be refused. "cancelled" instead: BE-Code ignores
          // it, the terminals keep deciding, and if both sides end up
          // cancelled Decide returns ReviewUnavailable and fs.go asks.
          return JSON.stringify({ decision: "cancelled" });
        }
        return JSON.stringify({ decision: choice === "Accept" ? "accept" : "reject" });
      } finally {
        // Only remove this review's own entry — a same-connection re-review
        // of the same path that raced in after this one started must not
        // have its live entry clobbered by this cleanup.
        const byConn = pending.get(conn);
        if (byConn) {
          if (byConn.get(a.path) === handle) byConn.delete(a.path);
          if (byConn.size === 0) pending.delete(conn);
        }
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
    // review_cancel arrives on the same connection as the review_diff it
    // withdraws (the Go side calls both on one mcp.Client), so looking it
    // up under (conn, path) is enough to find the right one without any
    // wire change.
    handler: async (a, conn) => {
      const p = typeof a?.path === "string" ? pending.get(conn)?.get(a.path) : undefined;
      if (!p || p.resolved) return JSON.stringify({ cancelled: false });
      p.cancel();
      return JSON.stringify({ cancelled: true });
    },
  });
}
