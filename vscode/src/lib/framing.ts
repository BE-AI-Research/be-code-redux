export class LineFramer {
  private buf = "";
  push(chunk: Buffer): string[] {
    this.buf += chunk.toString("utf8");
    const lines: string[] = [];
    let i: number;
    while ((i = this.buf.indexOf("\n")) >= 0) {
      lines.push(this.buf.slice(0, i).replace(/\r$/, ""));
      this.buf = this.buf.slice(i + 1);
    }
    return lines;
  }
}

export function encode(msg: unknown): string {
  return JSON.stringify(msg) + "\n";
}
