// Minimal stand-in for the "vscode" module, aliased in for tests
// (vitest.config.ts) so review.ts can be exercised without the real
// extension host. Only the surface review.ts touches is implemented; add to
// this as other modules under test start importing "vscode".
import { vi } from "vitest";

export class Range {
  constructor(
    public startLine: number,
    public startCharacter: number,
    public endLine: number,
    public endCharacter: number,
  ) {}
}

export class TabInputTextDiff {
  constructor(
    public original: { toString(): string },
    public modified: { toString(): string },
  ) {}
}

export interface MockUriInit {
  scheme: string;
  path: string;
}

export class Uri {
  private constructor(
    public readonly scheme: string,
    public readonly path: string,
  ) {}
  toString(): string {
    return `${this.scheme}:${this.path}`;
  }
  static from(init: MockUriInit): Uri {
    return new Uri(init.scheme, init.path);
  }
}

export const commands = {
  executeCommand: vi.fn(async (..._args: unknown[]) => undefined),
};

export const window = {
  showInformationMessage: vi.fn(async (..._args: unknown[]) => undefined as string | undefined),
  tabGroups: {
    all: [] as Array<{ tabs: Array<{ input?: unknown; label?: string }> }>,
    close: vi.fn(async (..._args: unknown[]) => undefined),
  },
};

export const workspace = {
  registerTextDocumentContentProvider: vi.fn(() => ({ dispose() {} })),
};
