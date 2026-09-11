import * as vscode from "vscode";
import { randomBytes } from "node:crypto";
import { homedir } from "node:os";
import { BridgeServer } from "./server";
import { ToolRegistry } from "./tools/registry";
import { writeLock, removeLock, LockInfo } from "./lib/lock";
import { registerEditorTools } from "./tools/editor";
import { registerDiagnosticsTool } from "./tools/diagnostics";
import { registerDebugTools } from "./tools/debug";
import { registerReviewTool } from "./tools/review";

let server: BridgeServer | undefined;
let status: vscode.StatusBarItem;
// The lock as last written, so a workspace-folder change can be republished
// without re-running the bridge (same pid, port and token).
let lock: LockInfo | undefined;

export async function activate(ctx: vscode.ExtensionContext) {
  status = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left, 50);
  ctx.subscriptions.push(status);
  ctx.subscriptions.push(
    vscode.commands.registerCommand("be-code.openTerminal", () => {
      const cwd = vscode.workspace.workspaceFolders?.[0]?.uri.fsPath;
      const t = vscode.window.createTerminal({ name: "BE-Code", cwd });
      t.sendText("be-code");
      t.show();
    }),
    vscode.commands.registerCommand("be-code.status", () => {
      vscode.window.showInformationMessage(server ? `BE-Code bridge listening; ${server.connections} connection(s)` : "BE-Code bridge is not running");
    }),
  );
  if (vscode.workspace.getConfiguration("be-code").get<boolean>("autoStart", true)) {
    try {
      await start(ctx);
    } catch (err) {
      // start() already reports failure via the status bar and an error
      // message; swallow here so a rejection never escapes activate().
    }
  }
}

async function start(ctx: vscode.ExtensionContext) {
  try {
    const tools = new ToolRegistry();
    registerEditorTools(tools);
    registerDiagnosticsTool(tools);
    registerDebugTools(tools, ctx);
    registerReviewTool(tools, ctx);
    const token = randomBytes(24).toString("hex");
    server = new BridgeServer(tools, token);
    const port = await server.listen(vscode.workspace.getConfiguration("be-code").get<number>("port", 0));
    lock = {
      pid: process.pid, port, token,
      workspaceFolders: (vscode.workspace.workspaceFolders ?? []).map((f) => f.uri.fsPath),
      ideName: "vscode", version: ctx.extension.packageJSON.version,
    };
    await writeLock(homedir(), lock);
    // BE-Code picks the bridge whose folders contain its workspace, so the
    // lock has to follow folders being added to or removed from the window.
    ctx.subscriptions.push(vscode.workspace.onDidChangeWorkspaceFolders(() => {
      if (!lock) return;
      lock = { ...lock, workspaceFolders: (vscode.workspace.workspaceFolders ?? []).map((f) => f.uri.fsPath) };
      void writeLock(homedir(), lock).catch(() => { /* best-effort; the old lock stays valid */ });
    }));
    const refresh = () => { status.text = server && server.connections > 0 ? "$(plug) BE-Code: connected" : "$(radio-tower) BE-Code: listening"; status.show(); };
    server.onConnectionChange = refresh;
    refresh();
  } catch (err) {
    await server?.close();
    server = undefined;
    lock = undefined;
    status.text = "$(warning) BE-Code: bridge failed";
    status.tooltip = String(err);
    status.show();
    vscode.window.showErrorMessage(`BE-Code bridge failed to start: ${err}`);
    throw err;
  }
}

export async function deactivate() {
  await server?.close();
  lock = undefined;
  await removeLock(homedir(), process.pid);
}
