# Application API — design

**Status:** approved in brainstorming, 2026-11-02. Target release: 1.3.1 (not yet released).
Branch `feature/app-api` off `main`.

**Supersedes** the draft at the workspace root, `docs/2026-11-01-app-api-and-session-tokens-design.md`,
whose §3.4 queue, `run` tier, `be-code serve`, callbacks, TCP listener, token index and
scheduler-as-caller are deferred to a later spec. Its decisions that survive are restated here
so this document stands alone.

**Builds on:** `2026-09-26-schedules-design.md` (a UI starts what `buildAgent` builds, once
approvals are wired; nothing runs while no session runs), `2026-09-12-shared-sessions-design.md`
(one live instance per session; `live.Record` and its pruning), the prompt-cache section of
`CLAUDE.md` (`StartPrefill`).

## Purpose

Let other programs on the same machine ask a running BE-Code session for an answer without a
person relaying it. The two proving callers are a UE5 helper application written for this API
and existing OpenAI-client programs pointed at it with a URL change, but the subsystem is
general purpose: any registered program may ask for a plain model answer or an
OpenAI-compatible completion, and, when it holds a session's token, an answer informed by what
that session is working on.

## Decisions taken in brainstorming

| Question | Decision | Why |
| --- | --- | --- |
| Where does the API run? | **Inside an interactive session**, never a machine-wide service and never a headless run. | Same rule as schedules: nothing runs while no session runs, and a service would be a second owner of the workspace. |
| Scope of this cut | **Two tiers:** `query` (one tool-less answer) and `completions` (OpenAI chat completions), plus read-only session context. No tools, no runs, no turns appended. | Most callers want an answer, not an agent. Everything with tools needs the allowance intersection, asks and run records — a separate spec. |
| Who may call? | **Registered apps only**, each with a key and a profile. One caller kind. | A person is the terminal. Someone scripting their own session registers an app for the script. The key prefix leaves room for another credential kind later. |
| How is a session addressed? | **By a persistent token** minted once and kept with the session across resumes, replaceable with `rotate`. Never by id or resume code. | The id is a filename that sorts by time; the code is six characters. The token is the one thing a caller must know and must not be able to guess. |
| Which session may be named? | **Only the serving session** in this cut. | No other session is running in that process; naming one is the deferred token index's job. |
| Does an app's profile need consent? | **No.** `apps add` is an ordinary command: it prints the profile and the key once. | A profile here grants inference and reading one session's context, not execution. The schedule-style prompt returns with the `run` tier. |
| Transport | **Unix socket only**, mode 0600, the same AF_UNIX call the live host already makes on Windows. No TCP key yet. | Offline-first and same-machine. A config key nothing serves is a lie waiting to be read. |
| Browsers | **Refused** before authentication: any `Origin` or `Sec-Fetch-Mode` header is 403. | A page in the person's browser can reach loopback. The API is for programs, not pages. |
| Package shape | **`internal/api` is a leaf** behind a narrow `Backend` interface implemented in `cmd/root.go`. | Auth, registry, tokens and budgets are testable with `httptest` and a fake backend; the agent side is one adapter. Same injection pattern as `ReviewerFactory`. |

## §1 Transport and lifecycle

### 1.1 Listener

`~/.be-code/api/api.sock`. The directory is created 0700 and the socket 0600. Every platform
uses `net.Listen("unix", …)`, as the live host does; on Windows the socket's mode is not
enforced, so the directory's ACL is the boundary, and the README says so.

At start the serving process checks that `~/.be-code/api` and `~/.be-code/api/apps` are mode
0700 on Unix and refuses to serve otherwise, with `api: ~/.be-code/api is mode 0755; fix with
chmod 700 ~/.be-code/api`. A readable socket is the whole boundary, so a wrong mode is never
silently accepted.

### 1.2 Who serves

Any interactive session with `api.enabled: true`: a hosted session's host process, an
in-process TUI (`--no-host`) or the plain REPL. Headless `run` and `init` never serve. The
server is built in `cmd/root.go:buildAgent` and **started only by the UI** after `Approve` and
`Events` are wired, alongside `StartSchedules` and `StartPacks`, and stopped first thing in
`finishSession`, ahead of its early returns, so a session never leaves a listener behind.

`/api on` starts the server in a session whose config has it off, and `/api off` stops it,
for that session only; neither writes config.

### 1.3 One server per machine

The reclaim decision is made under `api.lock` from `server.json`'s pid, not from errno: a failed
probe plus an absent or dead-pid record is stale, a live-pid record is busy — so Windows
reclaims a crashed socket too. Bind. On address-in-use, connect: a refusal means a stale socket left by a crash, which is
removed and re-bound; a successful connect means another session serves, and this one notices
once — `API is served by session CODE (pid N); this session is not serving` — and does not
bind. `~/.be-code/api/server.json` (0600: pid, socket, session code, started) is what that
notice, `/api` and `doctor` read; a record whose pid is gone (`procattr.Alive`) is pruned like a
live record. A session that stops serving removes its record and its socket.

### 1.4 Every request

- Body capped at 1 MiB; `ReadHeaderTimeout` 30 s; the request context is cancelled when the
  client disconnects.
- `Origin` or `Sec-Fetch-Mode` present means 403 `browser_refused` before the body is read.
- Responses are JSON. A failure is `{"error": {"code": "…", "message": "…"}}` with the status
  codes named below. Streaming responses are `text/event-stream`.

## §2 Authentication and the app registry

### 2.1 App files

`~/.be-code/api/apps/<name>.json`, mode 0600, one per app:

```json
{
  "name": "ue5-helper",
  "key_hash": "sha256:3f1a…",
  "created": "2026-11-02T14:02:00-07:00",
  "profile": {
    "tiers":    ["query", "completions"],
    "models":   ["default", "qwen2.5-coder:14b"],
    "packs":    ["ue5"],
    "sessions": "named",
    "budget":   {"tokens": 4096, "per_hour": 120}
  }
}
```

- `name` matches `^[a-z][a-z0-9-]{1,31}$`, the pack-name rule.
- `key_hash` is the SHA-256 of the key, hex, prefixed `sha256:`. The key itself is never
  stored. `revoked` is a time, present only once `apps revoke` has run.
- `tiers`: any of `query`, `completions`. Empty means the app may call only `GET /v1/health`.
- `models`: names the app may ask for. `default` means the serving session's current model.
  Empty means `default` only.
- `packs`: packs whose core rules a `query` may ask to have in its system prompt. The pack must
  also be active in the serving session; the profile narrows, never widens.
- `sessions`: `none` (stateless endpoints only) or `named` (may present a session token).
- `budget.tokens` caps one reply's `max_tokens` and is the most a request may ask for;
  `budget.per_hour` is the number of requests allowed in any sliding hour. Absent or zero
  takes the default; a budget is always set — unbounded is deliberately not expressible through
  `apps add`. Defaults when `apps add` is not told otherwise: 4096 and 120.

### 2.2 Keys

`bek_1_` plus 24 bytes from `crypto/rand`, base64url unpadded. `apps add` prints it once and
stores only the hash. `apps rotate <name>` mints a new key and the old one stops at once — an app
is a program and restarts with the new key. `apps revoke <name>` stamps `revoked`, after which
the key is 401 (`apps rotate` on a revoked app re-activates it, and `apps revoke` says so);
`apps remove <name>` deletes the file. The `1` is a format tag, so a second
credential kind (or a second key format) can be told apart by prefix without a redesign.

### 2.3 Lookup

The registry is read from disk on each request, cached by the directory's and each file's
modification time, so `rotate`, `revoke` and `remove` take effect without restarting the serving
session. A file that cannot be read or parsed makes that app absent (401) and is reported once
per file per session on stderr, so a corrupt file can never widen access. A presented key is
hashed and compared in constant time against every app's hash; the registry is small.

### 2.4 Check order

Each step fails closed; the first failure answers.

1. `Origin` or `Sec-Fetch-Mode` present → 403 `browser_refused`.
2. `Authorization: Bearer <key>` missing, wrong prefix, no hash match, or revoked → 401
   `unauthorized`.
3. Endpoint's tier not in the profile → 403 `tier_not_allowed`.
4. `X-BE-Session` present but the profile's `sessions` is `none` → 403 `sessions_not_allowed`.
4b. Path token and `X-BE-Session` differ → 400 `session_mismatch` (a malformed request, judged
   without consulting any token).
5. `X-BE-Session` present but `Accepts` is false → 404 `session_not_found`. Not 403: existence
   is not confirmed to a caller holding the wrong token.
6. Session named but it is not the serving session → 409 `session_not_running`.
7. `model` not in the profile → 403 `model_not_allowed`; `pack` not in the profile or not active
   → 403 `pack_not_allowed`.
8. Over `budget.per_hour`, or a second request from the same app while one is in flight →
   429 `rate_limited` with `Retry-After`. `max_tokens` above `budget.tokens` → 400
   `over_budget` naming the cap. An in-flight refusal is not charged against `per_hour`;
   handler-level refusals (model, pack, `over_budget`, `bad_request`) are.

Unmatched paths and wrong methods answer JSON 404 `not_found` after authentication (405 is
collapsed into 404 so route existence is not confirmed); a non-canonical path (`//`, `.`, `..`
segments) is JSON 404 before routing, never a redirect. `browser_refused` and `shutting_down`
answers also produce an access-log line, with app `-`.

### 2.5 Abuse and logging

A 401 or 404 is answered after a 250 ms delay and the connection is closed. More than 20 such
failures in one minute, across all connections, make every authentication fail 429
`rate_limited` until the minute ends. The access log (`~/.be-code/api/access.log`, 0600, one
line per request: time, app name when known, session code when named, path, status, duration,
tokens) never contains a key or a session token; the golden test in §8 pins that.

## §3 Session tokens

### 3.1 Fields on `store.Session`

```go
Token          string    `json:"token,omitempty"`
PrevToken      string    `json:"prev_token,omitempty"`
PrevUntil      time.Time `json:"prev_until,omitempty"`
TokenRotatedAt time.Time `json:"token_rotated_at,omitempty"`
```

- `store.NewToken()`: `sess_1_` plus 16 bytes from `crypto/rand`, base64url unpadded. Panics on
  a failed read, as `live.NewToken` does: a token that cannot be trusted is not used.
- `(s *Session) AccessToken() string` returns `Token`, minting it and marking the session dirty
  when empty. Nothing else reads the field.
- `(s *Session) Rotate(grace time.Duration) error`: moves `Token` to `PrevToken`, sets
  `PrevUntil` to now plus grace (zero clears both), mints a new `Token`, stamps
  `TokenRotatedAt`. A grace over 24 h is refused.
- `(s *Session) Accepts(tok string) bool`: constant-time compare against `Token`, then against
  `PrevToken` while `PrevUntil` is in the future; an expired `PrevToken` is cleared on the way.

### 3.2 Where it is checked

The serving host compares a presented token against its own in-memory session only. This cut
can name no other session, so the draft's hashed token index (`~/.be-code/api/tokens.json`) is
not built; it exists to resolve a token to a session file when a non-serving session can be
addressed, which is the deferred `run` tier's business.

### 3.3 What the token is not

Not a filename, a log line, a title or a status-bar item. `--resume`, the picker, `sessions
list` and the header show `Code` and `ID` exactly as today. Not `live.Record.Token`, which stays
the per-process hello secret for the live socket. Not required: a session never addressed
over the API never gets one.

### 3.4 Compatibility

A session file without the fields loads, and the first `AccessToken()` mints one that the next
autosave persists. An older build that resaves the session drops the unknown fields, so the
next access mints a new token and the app must fetch it again. That is the one compatibility
cost and the README states it.

### 3.5 Showing it

- `/session token` and `/session rotate` are refused while a turn is running ("try again after
  it") rather than waiting.
- `/session token` and `/session rotate [grace]` in the TUI render the token on the terminal
  that asked only — a local line, never a transcript entry, never saved, never shown to another
  attached terminal; in the REPL they print it, refusing when stdout is not a terminal.
- `be-code session token <code>` and `be-code session rotate <code> [--grace 10m]` act on a
  session file that is not live, saving it. For a live session they refuse with `CODE is live;
  join it and use /session token`, because the host owns the file (the save guard in
  `internal/agent/loop.go`).

## §4 Endpoints

All need a valid app key (§2.4). `model` is resolved through the profile: `default` is the
serving session's current model; any other listed name is loaded through
`cmd/root.go:secondaryLoad` as a reviewer is, which never changes the primary's window.

| Method and path | Tier | What |
| --- | --- | --- |
| `GET /v1/health` | any key | `{version, session: {code, model}, serving_since, apps_seen}`. The code is fine here: the caller already holds a registered key. |
| `POST /v1/query` | `query` | `{prompt, system?, model?, pack?, json_schema?, max_tokens?}` → `{text, model, pack, usage}`. One tool-less chat, no history. |
| `POST /v1/chat/completions` | `completions` | The OpenAI chat-completions shape. |
| `GET /v1/sessions/{token}` | `named` | `{title, workspace, model, live: true}`. Never id or code. |
| `POST /v1/sessions/{token}/query` | `query` + `named` | As `/v1/query`, with the session's conversation as context. Never appends. |

The token travels in the path for routing and in `X-BE-Session` for the check; the two must
agree (400 `session_mismatch`). The access log records the session code in its place.

### 4.1 `/v1/query`

- `system` is the caller's system text; the harness adds nothing of its own except the pack
  rules below. The reply is `NoThink` unless the profile's model is a thinking profile that the
  provider cannot silence, in which case reasoning is stripped (`profiles.StripThink`) and
  counted in `usage`.
- `pack`: its core rules (what `packBlock` renders for the system prompt) are prepended to
  `system`, framed as instructions, exactly as the serving session sees them. The response's
  `pack` is the name, or `null` when the pack went inactive between the check and the call (the
  call runs without it rather than failing).
- `json_schema`: passed as the provider's structured-output format where the backend has one
  (native Ollama `format`, OpenAI `response_format`). The reply is checked against the schema;
  a miss is 422 `schema_mismatch` with the raw text in `error.text`.
- `max_tokens` defaults to `budget.tokens` and may not exceed it; negative is 400 `bad_request`.
- A `json_schema` outside the supported subset (non-object top level, unknown or non-string
  `type`, non-array `required`/`enum`, `false` or tuple-form subschemas) is 400 `bad_request`
  before any model call; `"json_schema": null` means no schema; a boolean `true` subschema is
  unconstrained.

### 4.2 `/v1/chat/completions`

Request fields honoured: `messages` (system, user, assistant), `model`, `stream`,
`temperature`, `max_tokens`, `response_format` (`json_object` and `json_schema`). `stream:
true` answers as SSE in the OpenAI delta shape, ending with `data: [DONE]`. The response carries
`id` (`chatcmpl-` plus 16 random bytes in hex), `object`, `created`, `model` (the resolved
name; streamed chunks carry the requested name instead), `choices` and `usage`. A request carrying `tools`, `functions`, `tool_choice`, or `n`
above 1 is 400 `not_supported` naming the field, so a client learns at once rather than
getting a silent non-answer. Unknown fields are ignored, as the OpenAI API ignores them.

The reference `openai` Python client completes a streamed chat against it with only
`base_url` (through a unix-socket transport) and `api_key` set — the §8 e2e scenario.

### 4.3 `/v1/sessions/{token}/query`

The context is a **snapshot** of the serving session's system prompt and messages, taken only
when no turn is running (the turn lock is tried, never waited for: a turn can hold it for
minutes, so a request that finds it held is answered 409 `session_busy` at once and the caller
retries) and so never mid-rewrite, with harness state stripped (`StripHarnessState`) and
browser, `web_fetch` and `web_search` results withheld as `(not shown)`, the way the model-written
handoff withholds them. The caller's `system` is appended after the session's system prompt;
the caller's `prompt` is the final user message. Nothing is appended to the session: the
snapshot is a copy, and the call is not a typed request (it never clears `UntrustedWeb`, never
touches the inbox, the engine or the work ledger, and never writes).

If snapshot plus prompt plus `max_tokens` does not fit the window, the answer is 413
`context_too_large` with the sizes. Nothing is trimmed or compacted for the caller: a silently
shortened context is a different question.

`GET /v1/sessions/{token}` reports `workspace` as the absolute path, since the caller is on the
same machine and may well be the program that opened it.

## §5 Inference and the lane

- Every API model call goes through `chatInLane` on the serving session's server, so it waits
  for the person's call in flight and never interleaves with it. One request in flight per app
  (§2.4 step 8); across apps the lane serialises anyway.
- **Prompt cache.** An API call on the primary model is a different prompt, so the person's next
  turn would pay a cold read (measured at 43 s on the owner's server for a 19k-token prompt).
  When an API call ends and the primary is idle, the host calls `Agent.StartPrefill`, which
  re-reads the person's prompt in the background before they type. The cost that remains is the
  API call's own read, which the app's budget bounds.
- The adapter also consults the online-project gate without asking: an unapproved online
  provider is 403 `online_not_approved` for every model, named ones included, because named
  models are built from the default provider.
- `api.timeout` (120 s) bounds a request end to end; a client that disconnects cancels the
  model call through the request context, and the lane is released.
- `budget.per_hour` is a sliding window in the serving process's memory, per app, reset by a
  restart; a rate limit does not need to survive one.
- **Online primaries.** A request is counted in the session's spend (`internal/agent/online.go`)
  like any other call. A `max_spend_usd` breach is 429 `spend_cap`, never a prompt to the
  person: a program cannot be the one that raises a cost question. The online-project gate must
  already have been answered by the person for the session to be serving at all; an app can
  never answer it. `ShareGate` does not apply: nothing an app sends is page text, and page text
  in a session snapshot is withheld (§4.3).
- Events: an API call does not go through the session's own streaming path — that path writes
  into the person's transcript — so no transcript or thinking status reflects it; the bottom
  line reads `api: <app>` while one runs (§6.3), and `/stats` counts the request.

## §6 Config, commands, UI

### 6.1 Config

```json
"api": {"enabled": false, "timeout": "120s"}
```

Both in `config.Default` and in the README config reference. Everything per-app lives in the
app file.

### 6.2 Commands

- `be-code apps add <name> [--tiers query,completions] [--models a,b] [--packs p] [--sessions
  none|named] [--per-hour N] [--max-tokens N]` or `--profile file.json`. Prints the stored
  profile and the key once, then nothing again. No confirmation step.
- `be-code apps list` (name, tiers, sessions, created, revoked), `apps show <name>` (the file
  minus the hash), `apps rotate <name>` (prints the new key once), `apps revoke <name>`, `apps
  remove <name>`.
- `be-code session token <code>` and `be-code session rotate <code> [--grace 10m]` (§3.5), with
  `--stdout` required when stdout is not a terminal.
- `be-code doctor`: socket path and mode, who serves (from `server.json`), the number of
  registered apps, one line per revoked app that still has a file, and the directory-mode check
  of §1.1.

### 6.3 Slash commands and UI

Added to `ui.SlashCommandTable` so the palette, the menu and both UIs agree:

- `/api`: socket path, whether this session serves or which one does, requests served this
  session, last app and when. `/api on` and `/api off` for this session only (§1.2).
- `/apps`: registered apps with tiers and last call time.
- `/session token`, `/session rotate [grace]` (§3.5).

Header: an `api` marker beside the attached-terminals count while this session serves,
ASCII-safe like the rest of the header on Windows. The compact bottom line (under 70x20)
carries no `api:` marker. Bottom line: `api: <app>` while a request is
in flight. No transcript entries, so the person's conversation is never interleaved with
machine traffic; a session-context query leaves no trace in the session at all.

## §7 Failure handling

- **Stale socket after a crash:** connect probe refused means remove and bind (§1.3); a
  `server.json` with a dead pid is pruned.
- **Serving session ends mid-request:** `finishSession` stops the server first; in-flight
  requests get 503 `shutting_down` and their model calls are cancelled. Nothing is retried on
  the host side; the app retries.
- **Model switch mid-request** (`/model` by the person): the request finishes on the model it
  started with, as a review does; the next request sees the new default.
- **Pack goes inactive between check and call:** the call runs without it and the response says
  `pack: null` (§4.1).
- **Registry file unreadable or malformed:** that app is absent, reported once per file per
  session (§2.3).
- **Directory or socket mode too open:** refused at start with the `chmod` line (§1.1).
- **Session file without the fields, or resaved by an older build:** lazy minting (§3.4).
- **Clock moves during a grace window:** `Accepts` compares wall time; a backwards jump
  lengthens the grace and a forward one shortens it, bounded by the 24 h cap. Stated, not
  engineered around.
- **Backend down:** the request gets the same `chatWithRetry` behaviour the session has, within
  `api.timeout`; the final error is 502 `backend_error` with the provider's message, never the
  request body.

## §8 Testing

- `store`: token format and prefix; lazy mint marks the session dirty; `Accepts` with and
  without grace; expiry clears `PrevToken`; a legacy file without the fields loads and persists
  a token after save; grace over 24 h is refused.
- `api` with `httptest` and a fake `Backend`: the §2.4 order as a table test; browser headers
  refused before the body is read (a body reader that fails the test if touched); the 250 ms
  delay and the 20-per-minute lockout with a fake clock; the access log golden file containing
  no key or token; registry reload on mtime change; `revoke` effective without restart; per-app
  concurrency (second request 429); `budget.per_hour` sliding window; `max_tokens` over
  `budget.tokens` is 400.
- Endpoints: `/v1/query` prepends only a listed, active pack and reports `pack: null` when it
  went inactive; `json_schema` miss is 422 with the text; `tools` and `n` are 400; SSE ends with
  `[DONE]` and a mid-stream disconnect cancels the backend call; `/v1/sessions/{token}/query`
  uses a snapshot with harness state stripped and web text withheld and leaves the session's
  messages byte-identical; 413 when it does not fit; path and header token disagreement is 400.
- OpenAI compatibility: an e2e scenario in `test/e2e/` behind `python3`, where the official
  `openai` client (installed in a scratch venv, skipped when unavailable) completes a streamed
  chat with only `base_url` and `api_key` set, against the scripted mock backend.
- Lifecycle: two sessions, one socket, the second notices and does not bind; a stale socket is
  reclaimed; `finishSession` closes the listener and removes `server.json`; `run -y` never
  serves; `/api off` then `/api on` serves again.
- `cmd`: `apps add` prints the key once and stores only its hash; `session token` refuses a
  non-TTY without `--stdout` and a live session's code; the mode check refuses a 0755 directory;
  `doctor` lines.

## §9 Out of scope

Deferred to a later spec, in the order they are likely to be wanted: the `run` tier (agentic
turns with the allowance intersection, asks with `ask_timeout`, run records and SSE events);
`be-code serve` (a hosted session with no terminal); session turns appended to a transcript;
the priority queue and `max_concurrent`; callbacks; the scheduler as a caller; a TCP listener;
the token index that lets a non-serving session be named; a human credential kind. None of
them changes a file format or an endpoint defined here: the `bek_1_` and `sess_1_` prefixes,
the app file's `profile` object and the error shape are the extension points.

## Amendments from implementation (2026-11-02)

Rulings made while building, applied in place above: check step 4b (`session_mismatch`);
JSON 404 `not_found` for unmatched routes and non-canonical paths; refusals not charged to
`per_hour` when in flight; budgets always set; `apps rotate` re-activates a revoked app;
`max_tokens` and `json_schema` validation; the online-project gate (`online_not_approved`);
lock-based reclaim; token commands refused mid-turn; no `api:` marker on the compact bottom line;
streamed chunks name the requested model. Error codes added to the list the spec implies:
`bad_request`, `timeout`, `not_found`, `online_not_approved`. `session_busy` (409) stands as
planned.

Final-review fixes (2026-11-02): a named model is built on the session's *current* provider (not
`default_provider`) and refused with 403 `model_not_allowed` while that provider is online, since
spend is counted and capped only at the primary's pricing; the API path takes the primary
server's lane (created by `Agent.EnsurePrimaryLane` when no sub-agent runner exists) at
background priority, waits for a window resolution (`awaitWindow`) and stops a prefill before
its call; structured output reaches the provider (`json_schema` as Ollama's `format` / OpenAI's
`response_format` json_schema, a completion's `response_format` unchanged, JSON `null` meaning
none); app budgets are defaulted on every read, so a hand-edited 0 is never unbounded; and
`session token|rotate` refuse while any live process owns the file — a live host by the live
registry, a plain or `--no-host` session by its `host_pid`.

Post-merge fix (2026-11-02): the API path's gate check and its provider snapshot are paired
through `Agent.switchGen`, a count of every `SetProvider` and `applySwitch` under `modelMu`,
read before the checks and again with the snapshot. The gap between them is `awaitWindow`,
which waits out a whole model resolution, and a `/provider` switch landing in it (the session
is idle, so the UI allows it) sent an app's request to an unapproved online provider. A
changed count re-runs the checks, three times at most, then 409 `session_busy`; a request
between `SetProvider` and its paired `SetModel` is `session_busy` too, not
`online_not_approved`. The tool loop never had the exposure: `/provider` and `/model` are not
busy-safe, and the agent's own mid-run switches run on the loop goroutine.
