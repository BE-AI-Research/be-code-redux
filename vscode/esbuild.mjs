import { build, context } from "esbuild";
const opts = { entryPoints: ["src/extension.ts"], bundle: true, outfile: "dist/extension.js", external: ["vscode"], format: "cjs", platform: "node", target: "node20", sourcemap: true };
if (process.argv.includes("--watch")) { (await context(opts)).watch(); } else { await build(opts); }
