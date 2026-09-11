// AcceptAllTracker remembers which connections chose "accept all this
// session" for editor diff review. Keyed by connection identity (the
// socket), never window-globally: one BE-Code session saying "stop asking"
// must not silently auto-accept another session's writes. Entries are held
// weakly, so a closed connection's entry disappears with its socket even if
// nothing clears it explicitly.
export class AcceptAllTracker {
  private conns = new WeakSet<object>();

  // Without a connection identity there is nothing to scope the choice to,
  // so accept-all is never remembered and never reported.
  set(conn?: object): void { if (conn) this.conns.add(conn); }
  has(conn?: object): boolean { return conn ? this.conns.has(conn) : false; }
  clear(conn?: object): void { if (conn) this.conns.delete(conn); }
}
