import { describe, it, expect } from "vitest";
import { formatDiagnostics, severitiesFor } from "../src/lib/format";
import { relPath } from "../src/lib/paths";

describe("formatDiagnostics", () => {
  it("groups by file with a count summary first", () => {
    const out = formatDiagnostics([
      { path: "b.go", line: 3, col: 1, severity: "error", source: "go", message: "undefined: x" },
      { path: "a.py", line: 10, col: 5, severity: "warning", source: "Pylance", message: "unused" },
      { path: "b.go", line: 1, col: 1, severity: "error", source: "go", message: "missing import" },
    ]);
    expect(out.split("\n")[0]).toBe("2 errors, 1 warning in 2 files");
    expect(out).toContain("b.go:1:1 error go: missing import");
    expect(out.indexOf("a.py")).toBeLessThan(out.indexOf("b.go")); // files sorted
  });
  it("says so when clean", () => {
    expect(formatDiagnostics([])).toBe("no diagnostics");
  });
});

describe("severitiesFor", () => {
  it("defaults to errors and warnings", () => {
    expect(severitiesFor(undefined)).toEqual(["error", "warning"]);
  });
  it("narrows to error only", () => {
    expect(severitiesFor("error")).toEqual(["error"]);
  });
  it("narrows to warning only", () => {
    expect(severitiesFor("warning")).toEqual(["warning"]);
  });
  it("expands to everything for all", () => {
    expect(severitiesFor("all")).toEqual(["error", "warning", "info", "hint"]);
  });
});

describe("relPath", () => {
  it("makes paths workspace-relative", () => {
    expect(relPath(["/w/proj"], "/w/proj/internal/x.go")).toBe("internal/x.go");
    expect(relPath(["/w/proj"], "/elsewhere/y.go")).toBe("/elsewhere/y.go");
  });
});
