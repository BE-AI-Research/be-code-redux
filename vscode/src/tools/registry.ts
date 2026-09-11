export interface ToolDef {
  name: string;
  description: string;
  inputSchema: object;
  handler: (args: any) => Promise<string>;
}

export class ToolRegistry {
  private tools = new Map<string, ToolDef>();
  add(t: ToolDef): void { this.tools.set(t.name, t); }
  list() { return [...this.tools.values()].map(({ name, description, inputSchema }) => ({ name, description, inputSchema })); }
  async call(name: string, args: any): Promise<{ text: string; isError: boolean }> {
    const t = this.tools.get(name);
    if (!t) return { text: `unknown tool ${name}`, isError: true };
    try {
      return { text: await t.handler(args ?? {}), isError: false };
    } catch (e: any) {
      return { text: `${name}: ${e?.message ?? String(e)}`, isError: true };
    }
  }
}
