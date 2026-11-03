#!/bin/sh
# Online setup scenario: `be-code online add` on a pty stores a key for a
# custom endpoint (the mock, which demands it), config.json never holds it,
# doctor reports the source, and a real run uses it.
set -e
HERE=$(cd "$(dirname "$0")" && pwd)
BIN="$HERE/../../be-code"
if ! command -v script >/dev/null 2>&1; then
  echo "[SKIP] online (no script(1))"; exit 0
fi
WS=$(mktemp -d); HOME=$(mktemp -d); export HOME
SRV=
trap 'rm -rf "$WS" "$HOME"; [ -z "$SRV" ] || kill $SRV 2>/dev/null || :' EXIT
mkdir -p "$HOME/.be-code"
printf 'module example.com/online\n\ngo 1.22\n' > "$WS/go.mod"
printf 'package main\n\nfunc main() {}\n' > "$WS/main.go"
cat > "$HOME/.be-code/config.json" <<CFG
{"default_provider":"mock","model":"mock-model",
 "providers":{"mock":{"type":"openai","base_url":"http://127.0.0.1:18112/v1"}},
 "max_turns":10,"max_repairs":2,"verify_on_done":true,"compat_tool_calls":"auto"}
CFG
BE_E2E_KEY=e2e-secret BE_E2E_PORT=18112 python3 "$HERE/mock_server.py" & SRV=$!
sleep 1

# Prompts: provider (8 = custom), name, URL, key (wrong, then right), model 1,
# default yes. The pause before each answer lets the program switch the pty's
# echo off before a key is typed, so the key never reaches the log.
# TERM=dumb stops the UI library probing the terminal and eating the answers.
{
  for a in 8 mockkey http://127.0.0.1:18112/v1 wrong e2e-secret 1 y; do
    sleep 1; echo "$a"
  done
  sleep 1
} | TERM=dumb script -qec "$BIN online add" /dev/null > "$WS/add.log" 2>&1 || :
grep -q "key rejected by mockkey" "$WS/add.log" || { cat "$WS/add.log"; exit 1; }
grep -q "mockkey added (model mock-model)" "$WS/add.log" || { cat "$WS/add.log"; exit 1; }
! grep -q "e2e-secret" "$WS/add.log"
! grep -q "e2e-secret" "$HOME/.be-code/config.json"
grep -q '"e2e-secret"' "$HOME/.be-code/credentials.json"
MODE=$(stat -c %a "$HOME/.be-code/credentials.json")
[ "$MODE" = "600" ]
# A loopback endpoint is local, so doctor lists its key source only once the
# provider is flagged online; flag it the way a person editing config would.
python3 - "$HOME/.be-code/config.json" <<'PY2'
import json, sys
p = sys.argv[1]; c = json.load(open(p)); c["providers"]["mockkey"]["online"] = True
json.dump(c, open(p, "w"))
PY2
"$BIN" doctor > "$WS/doctor.log" 2>&1 || :
grep -q "mockkey: key stored (pasted" "$WS/doctor.log" || { cat "$WS/doctor.log"; exit 1; }
! grep -q "e2e-secret" "$WS/doctor.log"
# A real run through the stored key: the mock refuses without it.
"$BIN" run -y -C "$WS" "add an Add function in add.go" > "$WS/run.log" 2>&1 || { cat "$WS/run.log"; exit 1; }
grep -q '^}' "$WS/add.go"
"$BIN" online revoke mockkey | grep -q "revoked locally"
! grep -q mockkey "$HOME/.be-code/credentials.json"
echo "[PASS] online"
