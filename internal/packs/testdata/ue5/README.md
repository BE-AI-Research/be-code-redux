# ue5 pack

Unreal Engine 5 expertise for BE-Code. Version 0.1.0 (seed).

## Install

```sh
cp -r packs/ue5 ~/.be-code/packs/ue5
chmod +x ~/.be-code/packs/ue5/tools/ue-build
export UE_ROOT=/path/to/UnrealEngine   # folder containing Engine/, if not auto-found
```

The pack activates in any workspace with a `.uproject` at its root.

## What it contains

| Part | State |
|---|---|
| `core.md` | Done. ~700 tokens of always-on rules (budget 1500) |
| `chunks/cpp-reflection.md` | Done |
| `chunks/build-system.md` | Done |
| `playbooks/errors.md` | 16 entries, routing tested in `reference/playbook` |
| `commands/` | `ue-actor`, `ue-component`, `ue-diagnose`, `ue-dep` |
| `templates/` | Actor, ActorComponent |
| `tools/ue-build` | Linux/macOS. Tested with `--dry-run` against a fake engine tree; not yet against a real engine |
| `tools/ue-build.ps1` | Windows. **Untested** — written without PowerShell available |

## Chunk roadmap

Write in this order; each 1.5–3k tokens, same shape as the existing two
(how it works, correct minimal example, tables of the parts you need,
common mistakes). Re-run the evals after each.

1. `gameplay-framework` — GameMode/GameState/PlayerState, Pawn/Character, PlayerController, HUD, subsystems (Game Instance / World / Local Player), where each kind of logic belongs, authority on each.
2. `replication` — actor and property replication, OnRep, RPC types and reliability, ownership and relevancy, `HasAuthority` vs `IsLocallyControlled`, dormancy, common desync bugs.
3. `input` — Enhanced Input: actions, mapping contexts, triggers/modifiers, binding in `SetupPlayerInputComponent`, adding contexts via the local player subsystem.
4. `gas` — Ability System Component placement, attribute sets and `ATTRIBUTE_ACCESSORS`, abilities, effects, tags, prediction keys; the module/plugin setup.
5. `editor-scripting` — what can be done from Python and commandlets, how to hand Blueprint/asset work to the editor bridge, what to tell the user when no bridge exists.
6. `performance` — Unreal Insights, `stat` commands, tick cost, profiling workflow.
7. `animation`, `niagara`, `world` (World Partition, PCG, streaming), `ui` (UMG/CommonUI), `rendering` (Nanite, Lumen, materials) — as evals show demand.

Every chunk should mark anything version-specific with the 5.x release where
it changed.

## Not in this pack

The `ue-editor` MCP server (Blueprint and asset operations through a running
editor) is declared as optional and is a separate project.
