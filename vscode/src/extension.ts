import * as vscode from "vscode";
import { randomBytes } from "node:crypto";
import { homedir } from "node:os";
import { BridgeServer } from "./server";
import { ToolRegistry } from "./tools/registry";
import { writeLock, removeLock } from "./lib/lock";
import { registerEditorTools } from "./tools/editor";
import { registerDiagnosticsTool } from "./tools/diagnostics";
import { registerDebugTools } from "./tools/debug";
import { registerReviewTool } from "./tools/review";

let server: BridgeServer | undefined;
let status: vscode.StatusBarItem;

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
    await writeLock(homedir(), {
      pid: process.pid, port, token,
      workspaceFolders: (vscode.workspace.workspaceFolders ?? []).map((f) => f.uri.fsPath),
      ideName: "vscode", version: ctx.extension.packageJSON.version,
    });
    const refresh = () => { status.text = server && server.connections > 0 ? "$(plug) BE-Code: connected" : "$(radio-tower) BE-Code: listening"; status.show(); };
    server.onConnectionChange = refresh;
    refresh();
  } catch (err) {
    await server?.close();
    server = undefined;
    status.text = "$(warning) BE-Code: bridge failed";
    status.tooltip = String(err);
    status.show();
    vscode.window.showErrorMessage(`BE-Code bridge failed to start: ${err}`);
    throw err;
  }
}

export async function deactivate() {
  await server?.close();
  await removeLock(homedir(), process.pid);
}
