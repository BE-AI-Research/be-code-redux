// firstChangedLine returns the 0-based index of the first line where two
// texts differ, so the diff opens scrolled to the actual change instead of
// the top of a long file. Returns 0 when the texts are identical (nothing
// to scroll to).
export function firstChangedLine(original: string, proposed: string): number {
  const a = original.split("\n");
  const b = proposed.split("\n");
  const n = Math.min(a.length, b.length);
  for (let i = 0; i < n; i++) if (a[i] !== b[i]) return i;
  // No differing line in the common prefix: the change is the appended or
  // removed tail, which starts at the end of the shorter text.
  return a.length === b.length ? 0 : n;
}
