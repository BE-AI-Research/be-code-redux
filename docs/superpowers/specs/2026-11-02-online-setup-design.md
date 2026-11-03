# Online provider setup — design

**Status:** approved in brainstorming, 2026-11-02. Target release: 1.4.0 (not yet released; was planned as 1.3.1).
Branch `feature/online-setup` off `main`.

**Builds on:** `2026-10-04-online-providers-design.md` (the presets, `LocalEndpoint`, the
online-project gate, spend, the helper), `2026-11-02-app-api-design.md` (the 0600 file
discipline and "a secret never reaches a log, notice, error or transcript").

## §1 Goal and scope

A person adds an online provider from inside the harness and uses any text model it serves,
without leaving the terminal to export an environment variable. Three ways in: the env var
the preset already names, a key pasted into a masked field, or, for OpenRouter, a sign-in in
the browser that issues a user-scoped key (OpenRouter's PKCE flow, which needs no client
registration). The same operations the harness supports today, on the same OpenAI-compatible
chat wire; nothing about what a model can be asked changes.

**Owner decisions (brainstorming):** text models only, modalities later; a separate 0600
credential file, not the OS keyring and not env-only; OpenRouter's flow built in, no generic
OAuth description per preset; the TUI flow is assembled from the existing pickers and input
row, not a full-screen wizard and not a web page.

**Out of scope, stated so silence is not read as a promise:** keyring storage; refresh tokens;
any provider's consumer-subscription OAuth (ChatGPT Plus through Codex's client, Claude Pro
through Claude Code's client: forbidden to third-party harnesses by those providers' terms and
actively blocked); image, audio or file inputs; Anthropic's native messages API (its preset
speaks OpenAI-compatible chat and is unchanged); a `--key` flag anywhere.

## §2 The credential store (`internal/credentials`)

A leaf package, stdlib only, that owns `~/.be-code/credentials.json`. The dotdir is passed in,
so `config` may import `credentials` and no cycle forms.

**File.** Written atomically (temp file in the same directory, rename) at mode 0600, created on
first write. Reading and writing are both refused when the file's mode admits group or world
(`mode&0o077 != 0`): the error names the file and the command, `credentials.json is readable
by others; run chmod 600 ~/.be-code/credentials.json`, and the session continues with no
stored keys. The mode check is skipped on Windows, where Go reports every file as 0666 and
the profile directory's ACL is the boundary (the same rule the API socket directory follows). A file that does not parse reads as empty, but a write to it is refused naming
the file, so a hand-edit that broke the JSON is never silently discarded.

**Shape.** Keyed by the provider's name as it appears in `config.json`'s `providers` map, so a
configured provider has at most one credential:

```json
{"version": 1,
 "providers": {
   "openrouter": {"kind": "api_key", "key": "sk-or-…", "source": "oauth",
                  "created": "2026-11-02T20:10:00Z"},
   "groq":       {"kind": "api_key", "key": "gsk_…", "source": "pasted",
                  "created": "2026-11-02T20:12:00Z"}}}
```

`kind` is the extension point: only `api_key` exists in this version, and a later
refresh-token kind adds its own fields under its own kind without touching this one. `source`
is `pasted` or `oauth`. An unknown `kind` is kept verbatim and reported by `doctor` as
"unsupported credential kind; update be-code", never deleted.

**API.**
- `Open(dir string) (*Store, error)`: reads the file (absent is empty); the mode refusal is the
  error.
- `(*Store) Key(provider string) (Credential, bool)`.
- `(*Store) Put(provider string, c Credential) error` and `(*Store) Revoke(provider string)
  error`: read-modify-write under the file, atomic rename, 0600. `Revoke` of a missing entry is
  a no-op.
- `Credential{Kind, Key, Source string; Created time.Time}`.

**Resolution.** `config.Config` gains `ProviderKey(name string) string`: the env var named by
the provider's `api_key_env`, when set and non-empty; otherwise the stored credential for
`name`; otherwise `""`. `provider.FromConfig` and `setup.Probe` call it in place of
`ProviderConfig.APIKey()`, which stays as the env-only accessor. The env var winning keeps every
script, CI run and existing setup behaving as it does now and lets a person override a stored
key for one shell.

**Secrecy.** The key never reaches a log, a notice, an error, the transcript, the input history
file or `run --json`. Where the harness must say where a key came from it says `from
$GROQ_API_KEY` or `stored (pasted 2026-11-02)`.

**README rule.** "The harness never stores a key" becomes: "`config.json` never holds a secret;
stored credentials live in `~/.be-code/credentials.json` at mode 0600, and an env var named by
`api_key_env` always wins."

## §3 Adding a provider (`setup.AddOnline`)

One implementation in `internal/setup`, used by the TUI, plain mode, `be-code online add` and
the wizard; the UIs supply only prompts.

```go
type Choice struct {
    Name    string // preset name, or the custom provider's name
    BaseURL string // the preset's URL, or the custom endpoint
    KeyEnv  string // the preset's env var; "" for custom
    Preset  bool
}
type Added struct {
    Models  []provider.ModelInfo // the listing, possibly empty
    Model   string               // chosen model
    Default bool                 // default_provider was set to Name
}
// Validate lists models with the credential; the error classifies the failure.
func Validate(ctx context.Context, ch Choice, key string) ([]provider.ModelInfo, error)
// AddOnline writes the credential, then the config block, then (if asked) the default.
func AddOnline(ctx context.Context, cfg *config.Config, store *credentials.Store, ch Choice,
    key, source, model string, makeDefault bool) (Added, error)
```

**The choice.** The seven presets from `provider.Presets()`, each giving name, base URL and env
var; or a custom OpenAI-compatible endpoint with a name and a base URL. A preset's name always
maps to the preset's URL: when a config block of that name was hand-edited to another URL the
flow says `openrouter is a preset; its URL is …` and uses the preset's. A custom name must be
new or an existing custom entry; a custom name equal to a preset's is refused.

**Validation.** One models listing against the base URL with the key, bounded by 15 s. Three
classified errors, each its own type so a UI can branch: `ErrKeyRejected` (401 or 403: the UI
returns to the key step), `ErrUnreachable` (transport failure or a non-JSON reply: the UI
names the URL and the error and offers retry or cancel), and success with the listing. A
provider whose listing route is refused or empty is accepted with a typed model name instead
of the picker; no chat probe is attempted, since the first real request is the test and the
existing outage and key-rejected paths explain its failure.

**Write order.** Credential first (`store.Put` with `source`), then the provider block through
`cfg.Save`: the preset's `ProviderConfig` (`type: openai`, base URL, `api_key_env`, `online:
true`) or, for a custom endpoint, the same with `online` decided by `config.LocalEndpoint`
exactly as validation decides it for hand-written config; with the chosen model as the
block's `default_model`. `default_provider` is changed only when `makeDefault` is true. Nothing is written
before validation succeeds, so a cancel at any earlier step leaves both files untouched.

**Replacing a key.** Running the flow for a provider already configured overwrites the
credential, keeps the config block, and offers the model picker again.

## §4 The OpenRouter sign-in (`internal/signin`)

A leaf package, stdlib plus `procattr`, implementing OpenRouter's documented PKCE flow. The
authorize URL, the exchange URL and the browser launcher are fields of `Options`, so tests run
the whole flow against an in-process fake and never open a browser.

`SignIn(ctx context.Context, o Options) (key string, err error)`:

1. A 32-byte random verifier, base64url without padding, and its S256 challenge.
2. A listener on `127.0.0.1:0` with one handler at a random path `/cb/<16 hex>`. The random
   path is what stops another local process from racing the callback; a guessed code fails the
   exchange anyway because the exchange needs the verifier.
3. The URL `https://openrouter.ai/auth?callback_url=http://127.0.0.1:<port>/cb/<id>&code_challenge=<c>&code_challenge_method=S256`,
   opened with `xdg-open` (Linux), `open` (macOS) or `rundll32 url.dll,FileProtocolHandler`
   (Windows) through `procattr.Hide`. A launch failure, or Linux with neither `DISPLAY` nor
   `WAYLAND_DISPLAY`, is not an error: `o.Show(url)` prints the URL and the flow continues.
4. A wait, bounded by 5 minutes and by `ctx`, for whichever comes first: the callback with
   `?code=`, which replies with the one-line page "You can return to BE-Code", or
   `o.Pasted`, a channel on which the UI delivers what the person typed: the full redirect URL
   (the code is read from its query) or the bare code. The pasted path is the SSH story: the
   browser runs on the laptop, its redirect to `127.0.0.1` fails there, and the person copies
   the address bar into the terminal.
5. The exchange: `POST https://openrouter.ai/api/v1/auth/keys` with `{"code": …,
   "code_verifier": …, "code_challenge_method": "S256"}`, bounded by 15 s, expecting
   `{"key": "sk-or-…"}`. A non-200 or a reply with no key is an error carrying the body's
   `error.message` when present, never the code or the verifier.

The returned key goes through §3's validation like a pasted one and is stored with `source:
"oauth"`. The listener closes on every way out. No persistent listener, no fixed port (two
sessions may sign in at once), no refresh handling, and the harness never fetches the sign-in
page itself. The documented parameter names are what the fake implements; the plan's last task
is a manual sign-in against the live service from this machine.

## §5 The TUI flow

`/online add [name]` is a sub-command of the existing `/online`, with a menu row "Add an online
provider" under Models and a palette entry from `ui.SlashCommandTable`. It is **not**
busy-safe: it ends in a provider switch, so while a run is in flight it gets the same
"commands wait until the agent is done" line `/provider` gets. One terminal of a served
session may be in the flow at a time (`Session.onlineAdding`, under `mu`); a second gets
`another terminal is adding a provider`.

The steps are a small state on the `View`, since each is one terminal's own modal:

1. The preset picker (`picker` with the seven names and their docs URL, plus "custom
   OpenAI-compatible endpoint"); `/online add <name>` skips it.
2. For a custom endpoint, a name and then a base URL on the ordinary input line.
3. For OpenRouter, a two-row picker: "Sign in with browser" / "Paste a key".
4. The key: the textarea is replaced by a single-line masked field (bubbles `textinput`, echo
   mode password) so a pasted key shows as dots. Nothing from this step reaches the history
   file; the transcript line reads `key entered (hidden)`.
5. Validation, or the browser wait, on a goroutine under a context the step owns: the status
   line reads `checking openrouter…` or `waiting for the browser; or paste the redirect URL`,
   and the input line accepts the pasted redirect during the wait. The result arrives as a
   message; Esc cancels through the context.
6. The model picker over the listing, or a typed model when the listing is empty.
7. "make <name> the default provider?" through the existing ask modal.
8. One `entryOK` line, `<name> added (model <m>); switching to it`, then the `Session`
   method that does the `SetProvider`/`SetModel` pair `/provider` does, so every attached
   terminal sees the new provider line and the online-project gate asks at the next request.

Esc at any step before 5 completes leaves both files untouched (§3's write order).
`/online revoke <name>` deletes the stored credential, with the line `revoked locally; revoke
it at <docs URL> too`, since the harness cannot. `/online` with no argument gains
`credential: stored (oauth, 2026-11-02)` or `credential: from $OPENROUTER_API_KEY`, and its
help lists `add` and `revoke`.

## §6 Plain mode, the wizard, the CLI, `doctor`

**Plain mode** runs the same steps as numbered prompts through the REPL's prompt machinery; the
key is read with terminal echo off (`charmbracelet/x/term.ReadPassword`, already in the module
graph). On a non-TTY stdin `/online add` is refused with `needs a terminal`. The browser wait
prints the URL and reads the pasted redirect from the same prompt.

**`be-code setup`.** The wizard's online branch, which today stops at `MissingKeyError`, offers
the three ways in (env var if set, paste, OpenRouter sign-in) and calls `setup.AddOnline`. The
first-launch message no longer calls a missing key a dead end.

**CLI.** `be-code online add [name]` runs the plain-mode steps outside a session and refuses a
non-TTY stdin; `be-code online revoke <name>` deletes the stored credential and needs no
terminal. No `--key` flag: a key on a command line lands in shell history and `ps`.

**`doctor`.** Two new lines, never containing a key: the credential file (absent, or present
with its mode, with the `chmod` warning when loose) and, per online provider, where its key
would come from: `$GROQ_API_KEY (set)`, `stored (pasted 2026-11-02)`, or `none: run /online add
groq`. A stored key makes a previously failing online probe pass with no other change.

**The key-rejected message** at a real request gains `; or run /online add <name>`.

## §7 Errors, stated once

| Condition | Behaviour |
|---|---|
| `credentials.json` mode admits group/world | read and write refused; `chmod 600` named; session continues without stored keys |
| file does not parse | reads as empty; write refused naming the file |
| key rejected at validation (401/403) | back to the key step |
| endpoint unreachable or not OpenAI-compatible | URL and error shown; retry or cancel |
| listing refused or empty | typed model name; no chat probe |
| sign-in timed out (5 min) or cancelled | `sign-in cancelled; nothing was stored` |
| exchange refused | provider's `error.message` if any; paste-a-key offered |
| second terminal in the flow | refused: `another terminal is adding a provider` |
| preset name with a hand-edited URL | preset URL wins; said so |
| custom name equal to a preset | refused |
| `/online add` while a run is in flight | the busy line |
| non-TTY stdin | `needs a terminal` |

## §8 Testing

- `internal/credentials`: round trip; 0600 on create; loose-mode refusal both ways;
  corrupt-file read-as-empty and write-refused; unknown kind kept; `Revoke` of a missing entry;
  `Config.ProviderKey` precedence (env set, env empty, stored, neither).
- `internal/signin` against an in-process fake of both endpoints that checks the challenge
  against the verifier: browser path through the loopback callback; pasted full URL; pasted
  bare code; a callback on the wrong path ignored; exchange refusal with and without
  `error.message`; timeout; context cancel; the launcher never run (a swappable function that
  fails the test if called).
- `internal/setup`: `Validate`'s three outcomes against a scripted listing server;
  `AddOnline` writes credential then config, leaves `default_provider` alone unless asked,
  replaces a key for an existing provider, custom endpoint with and without a listing, preset
  URL over a hand-edited one, custom name equal to a preset refused.
- TUI (model-level, the style of the picker and ask tests): the step sequence; the masked
  field never reaching the history file; refusal while busy; second-terminal refusal; Esc at
  each step leaving no files; the transcript never containing the key.
- Plain mode: the prompts against a scripted reader; non-TTY refusal.
- `doctor` and `/online`: the new lines, and the output grepped for the key under test.
- e2e: the mock backend as a custom endpoint that lists models, added through `be-code online
  add` with a scripted stdin under a throwaway HOME, then a real `run` through the stored key.
- Manual, last: the live OpenRouter sign-in from this machine.
