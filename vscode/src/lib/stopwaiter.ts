export type StopResult = { kind: "stopped" | "terminated" | "exited" | "timeout"; reason?: string; threadId?: number; exitCode?: number };

// StopWaiter turns DAP events into "wait for the next stop" promises; an
// event that arrives before anyone waits is kept for the next wait.
export class StopWaiter {
  private pending?: (r: StopResult) => void;
  private queued?: StopResult;

  onEvent(ev: { event: string; body?: any }) {
    let r: StopResult | undefined;
    if (ev.event === "stopped") r = { kind: "stopped", reason: ev.body?.reason, threadId: ev.body?.threadId };
    else if (ev.event === "terminated") r = { kind: "terminated" };
    else if (ev.event === "exited") r = { kind: "exited", exitCode: ev.body?.exitCode };
    if (!r) return;
    if (this.pending) { const p = this.pending; this.pending = undefined; p(r); } else this.queued = r;
  }

  wait(timeoutMs: number): Promise<StopResult> {
    if (this.queued) { const q = this.queued; this.queued = undefined; return Promise.resolve(q); }
    return new Promise((resolve) => {
      const t = setTimeout(() => { this.pending = undefined; resolve({ kind: "timeout" }); }, timeoutMs);
      this.pending = (r) => { clearTimeout(t); resolve(r); };
    });
  }
}

// RingLog keeps the newest N lines with a monotonic cursor for "since".
export class RingLog {
  private lines: string[] = [];
  private base = 0; // cursor of lines[0]
  constructor(private cap: number) {}
  push(line: string) {
    this.lines.push(line);
    if (this.lines.length > this.cap) { this.lines.shift(); this.base++; }
  }
  since(cursor: number): { lines: string[]; cursor: number } {
    const start = Math.max(0, cursor - this.base);
    return { lines: this.lines.slice(start), cursor: this.base + this.lines.length };
  }
}
