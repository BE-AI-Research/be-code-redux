# fix-provider-config.py — repair a config that blocks launch

Location: `scripts/fix-provider-config.py`. Pure python3 stdlib, no Go
toolchain required — for running on boxes where be-code itself cannot build.

## The error it fixes

    be-code exits at startup with:
        provider "pocketpal" has unknown type "pocketpal"

The provider type resolver (`internal/provider/registry.go`) only accepts
`"ollama"`, `"openai"` or `""` (absent/empty). Any other value in a provider
entry's `"type"` field makes `FromConfig` fail and be-code refuses to start.
`"pocketpal"` is not defined anywhere in the codebase — the value came from a
bad config state, not a typo of a real provider. The script rewrites every
provider entry whose type is not one of the three legal values.

## What it does

1. Locates the config: `--config FILE`, else `$BE_CODE_CONFIG`, else
   `~/.be-code/config.json` (same resolution as `internal/config/config.go`
   `Load`).
2. For each provider entry with an illegal type:
   - the entry named by `"default_provider"`: its `type` is replaced with the
     legal type given by `--type` (default `ollama`); it is repaired in place,
     never deleted;
   - any other entry: it is deleted (an unused provider with an unknown type
     can only do harm).
   There is also defensive code to re-point `"default_provider"` at the first
   remaining provider if the default were ever deleted, but the current flag
   parsing never deletes the default, so that branch does not run in practice.
3. Writes a timestamped backup `<name>.bak-YYYYmmdd-HHMMSS` (suffix `-N` if
   one already exists in the same second) before touching the original.
4. Re-dumps the file with 2-space indent, preserving key insertion order.
   Prints exactly what changed. Idempotent: a second run prints nothing to do
   and exits 2.

Entries that are not JSON objects, or whose type is absent/empty/already
legal, are left untouched — the script never guesses at malformed entries.

## Usage

    python3 scripts/fix-provider-config.py                # inspect + fix, default -> ollama
    python3 scripts/fix-provider-config.py --type openai  # repair the default as openai
    python3 scripts/fix-provider-config.py --dry-run      # report, write nothing
    python3 scripts/fix-provider-config.py --config PATH  # non-default config file

Flags:

- `--config FILE` — path to the config (default `$BE_CODE_CONFIG` or
  `~/.be-code/config.json`; `~` is expanded).
- `--type T` — legal type to repair the default provider to; `ollama`
  (default), `openai` or empty string. Anything else is rejected, exit 1.
- `--dry-run` — report the planned changes without writing; exit 3.

## Exit codes

| code | meaning                                                        |
|------|----------------------------------------------------------------|
| 0    | success: config changed and written                            |
| 1    | error: file missing, bad JSON, bad `--type`, or write/backup failed |
| 2    | no changes needed (config already clean)                       |
| 3    | dry-run: changes would be made, nothing written                |

## Safety

- Read-only unless a change is actually needed; a clean config is never
  rewritten, reformatted or backed up.
- Backup is created before the original is modified; if the write then fails,
  the original is intact and the message says where the backup is.
- Invalid JSON is not guessed at — it aborts (exit 1) so the user fixes or
  restores it by hand.
- Re-running after a fix exits 2, so it is safe in scripts:

    python3 scripts/fix-provider-config.py || [ $? -eq 2 ] && be-code

## Example

Given a config whose default provider has `"type": "pocketpal"`:

    $ python3 scripts/fix-provider-config.py --dry-run
    fixing /home/user/.be-code/config.json:
      repair default provider "pocketpal": type "pocketpal" -> "ollama"
    dry-run: no changes written (re-run without --dry-run to apply)
    $ echo $?
    3
    $ python3 scripts/fix-provider-config.py
    fixing /home/user/.be-code/config.json:
      repair default provider "pocketpal": type "pocketpal" -> "ollama"
    backup: /home/user/.be-code/config.json.bak-20261015-004201
    wrote:  /home/user/.be-code/config.json
    done. Launch be-code again; if it still fails the cause is elsewhere.

If the repair makes sense as a permanent behaviour (accepting `pocketpal` or
another vendor as a real provider type), that is a change to
`internal/provider/registry.go` plus `presets.go` and the `ProviderConfig`
docs in `internal/config/config.go` — the script is a config repair, not a
code fix.
