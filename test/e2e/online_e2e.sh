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
# default yes. Each answer is fed only after its prompt appears in add.log
# (bounded wait), so the reads never coalesce and the key is typed once the
# program has switched the pty's echo off.
await_prompt() { # await_prompt <text> <nth occurrence>
  i=0
  while [ "$(grep -c -F "$1" "$WS/add.log" 2>/dev/null)" -lt "$2" ]; do
    i=$((i+1))
    if [ $i -gt 100 ]; then echo "FAIL: no prompt '$1' (#$2) within 10s"; cat "$WS/add.log"; exit 1; fi
    sleep 0.1
  done
}
FIFO="$WS/in"; mkfifo "$FIFO"
: > "$WS/add.log"
script -qec "$BIN online add" /dev/null < "$FIFO" > "$WS/add.log" 2>&1 &
SCR=$!
exec 3>"$FIFO"
await_prompt "Choose [1-8]:" 1;                 echo 8 >&3
await_prompt "name for the new provider:" 1;    echo mockkey >&3
await_prompt "base URL" 1;                      echo http://127.0.0.1:18112/v1 >&3
await_prompt "key for mockkey:" 1; sleep 0.3;   echo wrong >&3
await_prompt "key rejected by mockkey" 1
await_prompt "key for mockkey:" 2; sleep 0.3;   echo e2e-secret >&3
await_prompt "Choose a model" 1;                echo 1 >&3
await_prompt "make mockkey the default" 1;      echo y >&3
await_prompt "mockkey added" 1
exec 3>&-
wait $SCR || :
grep -q "key rejected by mockkey" "$WS/add.log" || { cat "$WS/add.log"; exit 1; }
grep -q "mockkey added (model mock-model)" "$WS/add.log" || { cat "$WS/add.log"; exit 1; }
if grep -q "e2e-secret" "$WS/add.log"; then echo "FAIL: key leaked into add.log"; exit 1; fi
if grep -q "e2e-secret" "$HOME/.be-code/config.json"; then echo "FAIL: key leaked into config.json"; exit 1; fi
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
if grep -q "e2e-secret" "$WS/doctor.log"; then echo "FAIL: key leaked into doctor.log"; exit 1; fi
# A real run through the stored key: the mock refuses without it.
"$BIN" run -y -C "$WS" "add an Add function in add.go" > "$WS/run.log" 2>&1 || { cat "$WS/run.log"; exit 1; }
grep -q '^}' "$WS/add.go"
"$BIN" online revoke mockkey | grep -q "revoked locally"
if grep -q mockkey "$HOME/.be-code/credentials.json"; then echo "FAIL: mockkey still in credentials.json after revoke"; exit 1; fi
echo "[PASS] online"
