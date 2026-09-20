import { describe, it, expect } from "vitest";
import { readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import * as vscode from "vscode";
import { ToolRegistry } from "../src/tools/registry";
import { registerEditorTools } from "../src/tools/editor";
import { registerDiagnosticsTool } from "../src/tools/diagnostics";
import { registerDebugTools } from "../src/tools/debug";
import { registerReviewTool } from "../src/tools/review";

// tools.manifest.json is the single source of truth the VS Code and Visual
// Studio editor extensions are both tested against (see the Visual Studio
// extension design doc, task 1). This test builds the real registry exactly
// as extension.ts does and fails the moment it drifts from the committed
// manifest — in shape, order, count or which tools are hidden.
const manifestPath = path.resolve(__dirname, "../tools.manifest.json");

function fakeCtx() {
  return { subscriptions: { push: () => {} } } as unknown as vscode.ExtensionContext;
}

function buildManifest() {
  const reg = new ToolRegistry();
  const ctx = fakeCtx();
  registerEditorTools(reg);
  registerDiagnosticsTool(reg);
  registerDebugTools(reg, ctx);
  registerReviewTool(reg, ctx);
  // ToolDef.hidden is optional (only review.ts sets it, to true); the
  // manifest carries it explicitly on every entry so consumers (the C#
  // project, the Go contract test) never have to treat a missing key as
  // "not hidden".
  return reg.all().map(({ name, description, inputSchema, hidden }) => ({
    name,
    description,
    inputSchema,
    hidden: hidden ?? false,
  }));
}

describe("tools.manifest.json", () => {
  it("matches the live tool registry: eighteen entries, registration order, hidden set explicit", () => {
    const built = buildManifest();

    // Guardrails from the task brief: if either of these ever fails, stop
    // and report it rather than adjusting the manifest or the assertion to
    // fit whatever the registry now produces.
    expect(built).toHaveLength(18);
    expect(built.filter((t) => t.hidden).map((t) => t.name)).toEqual(["review_diff", "review_cancel"]);
    expect(built.every((t) => typeof t.hidden === "boolean")).toBe(true);

    // GENERATE_MANIFEST=1 regenerates tools.manifest.json from this same,
    // live registry instead of comparing against the committed one. This is
    // the only path that writes the file, and it never runs on a plain
    // `npm test` / `vitest run` — only when explicitly requested, e.g.:
    //   GENERATE_MANIFEST=1 npx vitest run test/manifest.test.ts
    if (process.env.GENERATE_MANIFEST) {
      writeFileSync(manifestPath, JSON.stringify(built, null, 2) + "\n");
      return;
    }

    const manifest = JSON.parse(readFileSync(manifestPath, "utf8"));
    expect(built).toEqual(manifest);
    expect(manifest.map((t: { name: string }) => t.name)).toEqual(built.map((t) => t.name));
  });
});
