# be-code release procedure

Run this top to bottom for every tagged release. The steps that have bitten
us in the past are marked **HARD** — do not skip their check.

The whole flow assumes a clean, up-to-date `main`. Work from the repo root
(`be-code/be-code/`).

## 1. Decide the version

Pick the semver bump for the change (patch / minor / major). Record it as
`X.Y.Z` below and in the shell as:

```sh
export V=1.3.0   # the version you are about to ship
```

## 2. Bump the version stamp

`build.mk` is the single source of truth for the version that gets baked in
via `-ldflags` at link time. Update `VERSION` to `X.Y.Z`.

**HARD — keep it in lockstep** with:
- `README.md` (the version badge and any pinned `BE_CODE_VERSION`).
- `CHANGELOG.md` — move the `Unreleased` block under a new `vX.Y.Z` heading and
  give it a date. If you added contributor credits to a PR, add them here.

The TUI header shows `vX.Y.Z` for stamped builds and `dev` for plain
`go build` runs, so an un-stamped build will read `dev` — that is expected and
not a bug.

## 3. Build the release binaries

```sh
make release
```

`build.mk` produces the **four** platform binaries in `dist/`:

| File                          | Target            |
|-------------------------------|-------------------|
| `dist/be-code-linux-amd64`    | linux / amd64     |
| `dist/be-code-linux-arm64`    | linux / arm64     |
| `dist/be-code-darwin-arm64`   | darwin / arm64    |
| `dist/be-code-windows-amd64.exe` | windows / amd64 |

Confirm all four exist and are non-trivial in size:

```sh
ls -la dist/
```

## 4. Generate the checksums file

This is the step that caused the v1.3.0 failure, so get it exactly right.

```sh
( cd dist && sha256sum \
    be-code-linux-amd64 \
    be-code-linux-arm64 \
    be-code-darwin-arm64 \
    be-code-windows-amd64.exe \
  > SHA256SUMS )
cat dist/SHA256SUMS
```

**HARD — the content rules** (the updater is unforgiving about both):

1. **Bare filenames only.** Each line must be `  <hash>  <name>` where
   `<name>` is exactly the asset name (`be-code-linux-amd64`, ...
   `be-code-windows-amd64.exe`). **No `dist/` prefix.** `parseSums`
   (`internal/update/update.go`) matches the second field against the asset
   name; a `dist/` prefix makes every lookup miss.
2. **One line per binary**, in the same set you upload.

**HARD — the asset name** is **`SHA256SUMS`** — no `.txt`, no extension. The
updater (`update.go:205`) and `install.ps1:54` look up the asset by that
exact string. Uploading it as `SHA256SUMS.txt` is what made v1.3.0 fail with
`could not be verified; not installed` even though the hashes were correct.

Sanity check the hashes against the actual files before moving on:

```sh
( cd dist && sha256sum -c SHA256SUMS )
```

## 5. Tag the commit

```sh
git tag v$V
```

The tag should point at the commit that carries the `build.mk` version bump and
the changelog update (i.e. `main`, after steps 2–4 are committed).

## 6. Create the GitHub release + upload assets

Upload the **four** binaries and the **checksums file named `SHA256SUMS`**:

```sh
gh release create v$V \
  dist/be-code-linux-amd64 \
  dist/be-code-linux-arm64 \
  dist/be-code-darwin-arm64 \
  dist/be-code-windows-amd64.exe \
  dist/SHA256SUMS \
  --title "v$V" \
  --notes "$(sed -n "/## v$V/,/^## /p" CHANGELOG.md)"
```

If you already created the release, add/replace assets with
`gh release upload v$V <files> --clobber`.

## 7. Verify the published release (do not skip)

This is the check that would have caught v1.3.0. Confirm the remote release
exposes the assets the updater needs, and that one of them actually matches:

```sh
# 7a. Asset list — must include SHA256SUMS (exactly) plus the 4 binaries.
#     Exactly five assets total, nothing else.
gh release view v$V --json assets -q '.assets[].name'

# 7b. Fetch the live checksums and one live binary; confirm the hash matches.
tmp=$(mktemp -d)
curl -fsSL -o "$tmp/SHA256SUMS" \
  "https://github.com/BE-AI-Research/be-code-redux/releases/download/v$V/SHA256SUMS"
curl -fsSL -o "$tmp/be-code-linux-amd64" \
  "https://github.com/BE-AI-Research/be-code-redux/releases/download/v$V/be-code-linux-amd64"
want=$(awk '$2=="be-code-linux-amd64" {print $1}' "$tmp/SHA256SUMS")
got=$(sha256sum "$tmp/be-code-linux-amd64" | awk '{print $1}')
echo "want=$want"; echo "got =$got"; [ "$want" = "$got" ] && echo "OK" || echo "MISMATCH"
rm -rf "$tmp"
```

Both `7a` and `7b` must be green before you declare the release done.

## 8. Smoke-test the self-update path

The in-app self-update is the TUI **`/update`** command (it prompts
`Update BE-Code X → Y?` and runs `update.Install`). There is no
`be-code update` CLI subcommand — do not confuse it with the *pack* updater
(`be-code pack update <name>`).

To exercise the real verifier against the live release, either:

- launch an older installed build and run `/update` inside it, or
- from a scratch copy of the updater, point it at the published release and
  call `update.Install(ctx, http.DefaultClient, "linux/amd64", ...)` from a
  throwaway `go run` / test.

It must report a verified install. If it says `could not be verified;
not installed`, re-run step 7 — it is almost always the asset name or the
bare-filename content.

## 9. Housekeeping

- Confirm `main` on the remote matches the tagged commit
  (`git log --oneline -1 origin/main` vs `git rev-parse v$V^{}`).
- Close/merge any release branch (e.g. `rel/v$V`) and its PR.
- If you touched the ruleset to get a push through, note the ruleset id and
  what changed, and whether `required_signatures` is on or off.

## Gotchas (what has actually broken a release)

- **Wrong checksums asset name.** Uploader named it `SHA256SUMS.txt`; the
  updater only knows `SHA256SUMS`. Fix: re-upload as `SHA256SUMS`, delete the
  stray, re-run step 7. No code or tag change needed.
- **`dist/`-prefixed lines in SHA256SUMS.** `parseSums` misses them. Fix:
  regenerate with bare filenames (step 4).
- **Version out of sync** between `build.mk`, `README.md` and `CHANGELOG.md`.
  The binary self-updater and MCP client report the stamped version, so a
  mismatch shows up as "still on the old version".
- **Pushing `main` blocked by a ruleset** that requires commit signatures.
  Do not change the ruleset unilaterally — confirm with the repo owner first.
