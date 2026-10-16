#!/usr/bin/env python3
"""Fix ~/.be-code/config.json so be-code can launch.

Symptom this fixes
------------------
    be-code exits at startup with:
        provider "pocketpal" has unknown type "pocketpal"

Root cause: the provider type resolver (internal/provider/registry.go) only
accepts the types "ollama", "openai" or "" (empty). A config.json that names
any other type — e.g. "pocketpal" — makes FromConfig error and be-code
refuses to start. The script rewrites every provider entry whose type is not
one of the three legal values.

What it does
------------
1. Locates the config: --config FILE, else $BE_CODE_CONFIG, else
   ~/.be-code/config.json (matches internal/config/config.go Load).
2. For each provider entry with an illegal type:
     - the entry named by "default_provider": its type is replaced with the
       legal type given by --type (default: ollama); it is repaired in place,
       never deleted;
     - any other entry: it is deleted (an unused provider with an unknown
       type can only do harm).
   (There is defensive code to re-point "default_provider" at the first
   remaining provider if the default were ever deleted, but the current flag
   parsing never deletes the default, so that branch does not run in
   practice.)
3. Creates a timestamped backup <name>.bak-YYYYmmdd-HHMMSS before writing
   (only when the content actually changes).
4. Prints exactly what changed and re-dumps the file with 2-space indent,
   preserving key insertion order (json with object_pairs_hook). Idempotent:
   a second run reports nothing to do.

Usage
-----
    python3 fix-provider-config.py                 # inspect + fix, type -> ollama
    python3 fix-provider-config.py --type openai   # repair the default as openai
    python3 fix-provider-config.py --dry-run       # show what would change
    python3 fix-provider-config.py --config PATH   # non-default config file

Exit codes: 0 = success (config changed and written), 1 = error (file
missing, bad JSON, bad --type, or write/backup failed), 2 = no changes
needed (config already clean), 3 = dry-run (changes would be made).
"""

import argparse
import json
import os
import shutil
import sys
import time

# Exactly what internal/provider/registry.go accepts.
LEGAL_TYPES = ("ollama", "openai", "")


def default_config_path():
    env = os.environ.get("BE_CODE_CONFIG")
    if env:
        return os.path.expanduser(env)
    return os.path.expanduser(os.path.join("~", ".be-code", "config.json"))


def load(path):
    """Parse the config, preserving key insertion order (py3.7+ dicts)."""
    try:
        with open(path, "r", encoding="utf-8") as fh:
            cfg = json.load(fh, object_pairs_hook=lambda pairs: dict(pairs))
    except json.JSONDecodeError as e:
        print(f"error: {path} is not valid JSON: {e}\n"
              f"the script cannot safely repair it; fix it by hand or restore a backup",
              file=sys.stderr)
        sys.exit(1)
    if not isinstance(cfg, dict):
        print(f"error: {path} does not contain a JSON object", file=sys.stderr)
        sys.exit(1)
    return cfg


def save(path, cfg):
    """Write the config back with 2-space indent, preserving key order."""
    text = json.dumps(cfg, indent=2) + "\n"
    with open(path, "w", encoding="utf-8") as fh:
        fh.write(text)
    return text


def backup(path):
    """Create a timestamped backup; return the backup path."""
    stamp = time.strftime("%Y%m%d-%H%M%S")
    dest = f"{path}.bak-{stamp}"
    # Avoid clobbering a backup made within the same second.
    n = 1
    while os.path.exists(dest):
        dest = f"{path}.bak-{stamp}-{n}"
        n += 1
    shutil.copy2(path, dest)
    return dest


def plan(cfg, repair_type):
    """Mutate cfg in place; return (had_changes: bool, changes: list[str])."""
    providers = cfg.get("providers")
    if not isinstance(providers, dict):
        return False, []
    default_name = cfg.get("default_provider")
    changes = []
    to_delete = []
    for name, entry in list(providers.items()):
        if not isinstance(entry, dict):
            continue  # leave garbage to the user, don't guess
        t = entry.get("type")
        if t is None:
            continue  # empty/absent type is legal
        if t in LEGAL_TYPES:
            continue
        if name == default_name and repair_type is not None:
            entry["type"] = repair_type
            changes.append(f'repair default provider "{name}": type "{t}" -> "{repair_type}"')
        else:
            to_delete.append(name)
            if name == default_name:
                changes.append(f'delete default provider "{name}" (unknown type "{t}"); re-pointing default_provider')
            else:
                changes.append(f'delete provider "{name}" (unknown type "{t}")')
    for name in to_delete:
        del providers[name]
    if default_name in to_delete:
        fallback = next(iter(providers), None)
        if fallback is None:
            cfg.pop("default_provider", None)
            changes.append("WARNING: no providers remain; removed default_provider")
        else:
            cfg["default_provider"] = fallback
            changes.append(f'default_provider: "{default_name}" -> "{fallback}"')
    return bool(changes), changes


def main():
    ap = argparse.ArgumentParser(description="Fix ~/.be-code/config.json so be-code can launch.")
    ap.add_argument("--config", metavar="FILE",
                    help="path to config.json (default: $BE_CODE_CONFIG or ~/.be-code/config.json)")
    ap.add_argument("--type", dest="repair_type", default="ollama",
                    help="legal type to repair the default provider to (default: ollama; 'openai' also works)")
    ap.add_argument("--dry-run", action="store_true",
                    help="report what would change without writing anything")
    args = ap.parse_args()

    if args.repair_type not in LEGAL_TYPES:
        print(f"error: --type must be one of {list(LEGAL_TYPES)}, got {args.repair_type!r}", file=sys.stderr)
        sys.exit(1)

    path = os.path.expanduser(args.config) if args.config else default_config_path()
    if not os.path.isfile(path):
        print(f"error: config file not found: {path}", file=sys.stderr)
        sys.exit(1)

    cfg = load(path)
    had_changes, changes = plan(cfg, args.repair_type)

    if not had_changes:
        print(f"ok: {path} has no provider with an unknown type (legal types: ollama, openai, empty)")
        sys.exit(2)

    print(f"fixing {path}:")
    for line in changes:
        print(f"  {line}")

    if args.dry_run:
        print("dry-run: no changes written (re-run without --dry-run to apply)")
        sys.exit(3)

    # Backup before touching the original.
    try:
        backup_path = backup(path)
    except OSError as e:
        print(f"error: could not back up {path}: {e}", file=sys.stderr)
        sys.exit(1)
    print(f"backup: {backup_path}")

    # Save
    try:
        save(path, cfg)
    except OSError as e:
        print(f"error: could not write {path}: {e}\n"
              f"original config is intact; backup at {backup_path}", file=sys.stderr)
        sys.exit(1)

    print(f"wrote:  {path}")
    print("done. Launch be-code again; if it still fails the cause is elsewhere.")
    sys.exit(0)


if __name__ == "__main__":
    main()
