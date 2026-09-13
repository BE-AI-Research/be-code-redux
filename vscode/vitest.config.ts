import path from "node:path";
import { defineConfig } from "vitest/config";

// review.ts (and its test) import "vscode", which only exists inside a real
// extension host. Alias it to the test double so vitest can resolve it;
// production builds never see this file (esbuild.mjs marks "vscode"
// external instead).
export default defineConfig({
  resolve: {
    alias: {
      vscode: path.resolve(__dirname, "test/__mocks__/vscode.ts"),
    },
  },
});
