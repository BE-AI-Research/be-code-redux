using System.Threading;

namespace BECode.Bridge.Hosting
{
    /// <summary>
    /// <c>WorkspaceFolders</c>' republish of the lock file
    /// is debounced and runs its own file I/O, so two recomputes that both
    /// pass the debounce (a rapid solution-then-project-load burst wider
    /// than the 250ms window, say) can have their WRITES complete out of
    /// order — the harness must never see an OLDER workspace-folder list
    /// overwrite a NEWER one just because its write happened to finish
    /// first. Each recompute claims a generation with <see cref="Next"/>
    /// (in start order, before doing any work); <see cref="TryCommit"/>
    /// then accepts only a generation strictly greater than the highest one
    /// already committed, so a late-finishing write for a generation that a
    /// later-started recompute has already superseded is rejected rather
    /// than applied. Pure counter/comparison logic — no Visual Studio type,
    /// no file I/O — so it is tested here rather than against a running
    /// host.
    /// </summary>
    public sealed class GenerationGate
    {
        private readonly object _gate = new object();
        private long _next;
        private long _committed = -1;

        /// <summary>The next generation number, strictly increasing across calls.</summary>
        public long Next()
        {
            return Interlocked.Increment(ref _next);
        }

        /// <summary>
        /// True when <paramref name="generation"/> is newer than every
        /// generation already committed — the caller should perform its
        /// write. False means a newer generation already committed and this
        /// one must be discarded, whatever order the two writes actually
        /// finished in.
        /// </summary>
        public bool TryCommit(long generation)
        {
            lock (_gate)
            {
                if (generation <= _committed)
                {
                    return false;
                }

                _committed = generation;
                return true;
            }
        }
    }
}
