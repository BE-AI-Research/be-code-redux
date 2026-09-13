import { describe, it, expect, vi, beforeEach } from "vitest";
import * as vscode from "vscode";
import { ToolRegistry } from "../src/tools/registry";
import { registerReviewTool } from "../src/tools/review";

// Flushes every already-queued microtask (promise reactions) without
// depending on real timers, so the test can observe the state review.ts
// reaches synchronously between two `await`s (e.g. the pending-review map
// populated inside the Promise.race, before showInformationMessage settles).
function tick(): Promise<void> {
  return new Promise((resolve) => setImmediate(resolve));
}

function fakeCtx() {
  return { subscriptions: { push: vi.fn() } } as unknown as vscode.ExtensionContext;
}

describe("review_diff / review_cancel", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    // The real vscode.d.ts marks `all` readonly; the mock module keeps it a
    // plain mutable array so tests can seed it, hence the cast.
    (vscode.window.tabGroups as any).all = [];
  });

  it("a shared review is cancelled by review_cancel (same connection) and closes its diff tab", async () => {
    const reg = new ToolRegistry();
    registerReviewTool(reg, fakeCtx());
    // review_cancel must arrive on the same connection as its review_diff
    // (as the Go side does, over one mcp.Client) to find it.
    const conn = {};

    // showInformationMessage never resolves on its own for this test — the
    // decision must come from the cancel race, not from a click.
    (vscode.window.showInformationMessage as ReturnType<typeof vi.fn>).mockImplementation(
      () => new Promise(() => {}),
    );

    const diffPromise = reg.call(
      "review_diff",
      { path: "a.txt", original: "old", proposed: "new", summary: "s", shared: true },
      conn,
    );
    await tick();

    expect(vscode.window.showInformationMessage).toHaveBeenCalledWith(
      "s",
      { modal: false },
      "Accept",
      "Accept all this session",
      "Reject",
    );

    // Mirror what the tab would look like so the finally block's close
    // logic (matched by TabInputTextDiff uris) finds it.
    const [, left, right, title] = (vscode.commands.executeCommand as ReturnType<typeof vi.fn>).mock.calls[0];
    (vscode.window.tabGroups as any).all = [
      { tabs: [{ input: new vscode.TabInputTextDiff(left, right), label: title }] },
    ];

    const cancelRes = await reg.call("review_cancel", { path: "a.txt" }, conn);
    expect(JSON.parse(cancelRes.text)).toEqual({ cancelled: true });

    const diffRes = await diffPromise;
    expect(JSON.parse(diffRes.text)).toEqual({ decision: "cancelled" });
    expect(vscode.window.tabGroups.close).toHaveBeenCalledTimes(1);
  });

  it("review_cancel for an unknown path returns cancelled: false", async () => {
    const reg = new ToolRegistry();
    registerReviewTool(reg, fakeCtx());

    const res = await reg.call("review_cancel", { path: "nope.txt" }, {});
    expect(JSON.parse(res.text)).toEqual({ cancelled: false });
  });

  it("two connections reviewing the same path are isolated from each other's cancel", async () => {
    const reg = new ToolRegistry();
    registerReviewTool(reg, fakeCtx());
    const connA = {};
    const connB = {};

    const resolvers: Array<(v: string | undefined) => void> = [];
    (vscode.window.showInformationMessage as ReturnType<typeof vi.fn>).mockImplementation(
      () => new Promise((res) => { resolvers.push(res); }),
    );

    // A starts first, B starts second, for the SAME path — with a
    // path-only pending map, B's pending.set would overwrite A's cancel
    // handle, so a later "cancel A" would silently cancel B instead (and A
    // would never be cancellable again). Keying by (connection, path) must
    // keep them independent regardless of arrival order.
    const diffA = reg.call("review_diff", { path: "shared.txt", proposed: "new", shared: true }, connA);
    await tick();
    const diffB = reg.call("review_diff", { path: "shared.txt", proposed: "new", shared: true }, connB);
    await tick();
    expect(resolvers).toHaveLength(2);

    // Cancel A specifically. It must resolve exactly diffA, not diffB.
    const cancelA = await reg.call("review_cancel", { path: "shared.txt" }, connA);
    expect(JSON.parse(cancelA.text)).toEqual({ cancelled: true });
    expect(JSON.parse((await diffA).text)).toEqual({ decision: "cancelled" });

    // A repeat cancel for the same, now-resolved connection+path reports
    // failure rather than a no-op success.
    const cancelAAgain = await reg.call("review_cancel", { path: "shared.txt" }, connA);
    expect(JSON.parse(cancelAAgain.text)).toEqual({ cancelled: false });

    // B was never touched by any of the above — A's cancellation and its
    // own cleanup (a different connection, same path) must not remove B's
    // still-pending entry. It resolves normally once its own prompt is
    // answered.
    resolvers[1]("Accept");
    expect(JSON.parse((await diffB).text)).toEqual({ decision: "accept" });

    // B's bookkeeping is intact afterwards: a fresh review on B for the
    // same path still gets its own, independently cancellable entry.
    (vscode.window.showInformationMessage as ReturnType<typeof vi.fn>).mockImplementation(
      () => new Promise(() => {}),
    );
    const diffB2 = reg.call("review_diff", { path: "shared.txt", proposed: "new2", shared: true }, connB);
    await tick();
    const cancelB = await reg.call("review_cancel", { path: "shared.txt" }, connB);
    expect(JSON.parse(cancelB.text)).toEqual({ cancelled: true });
    expect(JSON.parse((await diffB2).text)).toEqual({ decision: "cancelled" });
  });

  it("a non-shared review shows a modal prompt and Accept resolves normally", async () => {
    const reg = new ToolRegistry();
    registerReviewTool(reg, fakeCtx());
    (vscode.window.showInformationMessage as ReturnType<typeof vi.fn>).mockResolvedValue("Accept");

    const res = await reg.call("review_diff", { path: "b.txt", proposed: "new" }, {});

    expect(vscode.window.showInformationMessage).toHaveBeenCalledWith(
      expect.any(String),
      { modal: true },
      "Accept",
      "Accept all this session",
      "Reject",
    );
    expect(JSON.parse(res.text)).toEqual({ decision: "accept" });
  });

  it("both hidden tools are omitted from tools/list", () => {
    const reg = new ToolRegistry();
    registerReviewTool(reg, fakeCtx());
    const names = reg.list().map((t) => t.name);
    expect(names).not.toContain("review_diff");
    expect(names).not.toContain("review_cancel");
  });
});
