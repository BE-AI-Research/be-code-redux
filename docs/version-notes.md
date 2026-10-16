# Version Notes (internal)

Purpose: record what actually happened during each version — operational
reality, failure modes, decisions, constraints — that the user-facing
changelog and the release procedure intentionally do not carry.

New versions go above the oldest entry (newest first), matching
CHANGELOG.md. Every section is a record, not a template: write what you
actually did, not what you planned to do.

---

## v1.3.0 — "Packs, online models, a browser, schedules" (2026-10-10)

**Tag:** `v1.3.0` → commit `50c7a18`
**Release:** GitHub release `v1.3.0`, 5 assets
**Branch history:** `rel/v1.3.0` (feature branch, merged via PR #4) → `main`

### Release asset failure mode (SHA256SUMS naming)

**Symptom reported by user:** arm64 binary "could not be verified; not installed."

**Actual root cause:** The checksums asset was initially published as
`SHA256SUMS.txt`. The self-updater (`update.go:205`) and `install.ps1:54`
both look up the exact name `SHA256SUMS` (no `.txt` suffix). The lookup
missed, the updater treated the release as unverifiable and refused to
install it. The arm64 binary itself was a valid AArch64 ELF — nothing
wrong with the binary.

**Fix applied during the release session:**
- Fetched the published `SHA256SUMS.txt` content.
- Rewrote it with bare filenames (no `dist/` prefix), because
  `parseSums` in `update.go` matches on `$2 == filename` (the second
  whitespace-delimited field), which only works when the field is just
  the filename.
- Uploaded the corrected content as a new asset named exactly
  `SHA256SUMS` (no extension).
- Deleted the stray `SHA256SUMS.txt` asset.
- Verified live: fetched `SHA256SUMS`, parsed the `be-code-linux-amd64`
  line, compared its hash to the hash of the actual downloaded binary —
  matched. Updater now works.

**Lesson:** this is a one-time release-publish error, not a code bug.
The contract between the checksums file and the parser is:
- Asset name must be exactly `SHA256SUMS` (no extension).
- Each line: `<sha256hex>  <filename>` — two spaces between the hash and
  the bare filename. No directory prefix.
- `build.mk` builds exactly four binaries; all four must appear in the
  file: `be-code-linux-amd64`, `be-code-linux-arm64`,
  `be-code-darwin-arm64`, `be-code-windows-amd64.exe`.
  There is no `darwin-amd64` build.

This contract is now documented in `docs/RELEASE.md` (steps 4 and 7)
and in the "Gotchas" section of that file.

### GitHub ruleset / GPG signing

**Initial situation:** The `main` branch was protected by GitHub
ruleset `24674182` with a `required_signatures` condition. Local commits
were signed with GPG key `47F03AF7…F09BE328` (subkey
`860E37185BC7522C`), and `git verify-commit` confirmed the signatures
were cryptographically valid ("Good signature from Shayne G. Brown").
However, GitHub's commit-verification API returned `verified: null,
signer: null` for every signed commit. This was a GitHub server-side
attribution/propagation issue — not fixable client-side. The ruleset
enforced its condition based on GitHub's own verification, so all pushes
to `main` were rejected regardless of the local signature quality.

**Resolution (user-approved):**
1. User approved a one-time change to the ruleset policy (the standing
   constraint is "do not unilaterally change the GitHub ruleset"; this
   change was explicitly approved).
2. Ruleset `24674182` was deleted (`DELETE`, returned 204).
3. `main` was pushed: `d536ee9..50c7a18` (59 commits, fast-forward).
4. A replacement ruleset `24979264` was created **without**
   `required_signatures`. Retained conditions: `deletion`, `update`,
   `creation`, `required_linear_history`, `copilot_code_review`.
   **Caveat:** the new ruleset has empty `conditions` — the old one had
   `~DEFAULT_BRANCH` — so it now applies to **all refs**, not just
   `main`. This is broader than before and should be reviewed if ref
   policy ever matters.

**Subsequent commits (post-release hotfix and docs):** The GPG secret
key for `47F03AF7…F09BE328` was not on the machine
(`~/.gnupg` had an empty keyring). The user decided: "if we did push
the other update with no signing key, then let's go with option 3 as we
don't need one" — i.e., commit and push without GPG signing. All
subsequent commits were made with `git commit --no-gpg-sign`.

### Post-release commits on `main` (in order)

| Commit  | Description |
|---------|-------------|
| `b4e5375` | hotfix: `fix-provider-config.py` repair script + doc |
| `1a9a7cc` | docs: document `fix-provider-config.py` repair script in README |
| `9ca5131` | docs: regenerate `BECODE.md` with current workspace facts |
| `754aa56` | docs: add `RELEASE.md` — canonical release procedure |

### Known issues at release time

- **Provider config bug:** sessions started with an unknown provider
  type in `config.json` failed at startup with a "pocketpal"
  (or similar) unknown-provider-type error. Fixed by the
  `scripts/fix-provider-config.py` repair script shipped in `b4e5375`.
  See `docs/fix-provider-config.md` for details and `README.md` for
  user-facing instructions.
- **No `darwin-amd64` binary:** `build.mk` does not build one. This is
  intentional, not an oversight — but it should be noted if a user
  reports a missing platform.

### What worked well

- PR #4 (`rel/v1.3.0` → `main`) was mergeable throughout; it auto-merged
  when the push to `main` succeeded after the ruleset change. No force
  push was needed.
- The 5-asset contract (4 binaries + `SHA256SUMS`) was verified
  programmatically with `gh release view` immediately after upload.
- The `build.mk` target set was confirmed to match the platform list in
  `RELEASE.md` before the release was published.

### Open items / follow-ups

- [ ] Ruleset `24979264` applies to all refs (conditions empty). If
  branch-specific policy is needed again, add a condition back.
- [ ] The GPG signing key is not on this machine. If signing is wanted
  again for future releases, the key must be restored before the next
  release.
- [ ] `docs/RELEASE.md` step 7 (verification) could be scripted as a
  `make check-release` target to reduce manual steps on the next release.
