#!/bin/sh
# Application API scenario: a plain session serves the API; the client
# exercises every endpoint and the refusals over the unix socket.
set -e
HERE=$(cd "$(dirname "$0")" && pwd)
BIN="$HERE/../../be-code"
WS=$(mktemp -d); SESSION=
# A unix socket path is limited to about 104 bytes; keep HOME short.
OLDHOME=$HOME
HOME=$(mktemp -d /tmp/beapi.XXXXXX); export HOME
mkdir -p "$HOME/.be-code"
cp "$OLDHOME/.be-code/config.json" "$HOME/.be-code/config.json"
trap 'kill $SESSION 2>/dev/null || :; rm -rf "$WS" "$HOME"' EXIT
printf 'module example.com/api\n\ngo 1.22\n' > "$WS/go.mod"

# api.enabled on, everything else as the suite's config.
python3 - "$HOME/.be-code/config.json" <<'PY'
import json, sys
p = sys.argv[1]; c = json.load(open(p)); c["api"] = {"enabled": True, "timeout": "30s"}
json.dump(c, open(p, "w"))
PY

KEY=$("$BIN" apps add e2e-app --tiers query,completions --sessions named | sed -n 's/^ *\(bek_1_[A-Za-z0-9_-]*\)$/\1/p')
[ -n "$KEY" ] || { echo "apps add printed no key"; exit 1; }

# A saved session to serve: headless, so nobody hosts its file afterwards.
"$BIN" run -y -C "$WS" "api scenario: warm up" > /dev/null
CODE=$("$BIN" sessions | awk 'NR==2 {print $1}')
[ -n "$CODE" ] || { echo "no saved session"; exit 1; }
TOKEN=$("$BIN" session token "$CODE" --stdout)
[ -n "$TOKEN" ] || { echo "no session token"; exit 1; }

# The plain session resumes it with stdin held open: the REPL serves while it waits for a line.
FIFO="$WS/in"; mkfifo "$FIFO"
"$BIN" --plain --resume "$CODE" -C "$WS" < "$FIFO" > "$WS/session.log" 2>&1 &
SESSION=$!
exec 3>"$FIFO"

SOCK="$HOME/.be-code/api/api.sock"
for i in $(seq 1 50); do [ -S "$SOCK" ] && break; sleep 0.2; done
[ -S "$SOCK" ] || { echo "the API socket never appeared"; cat "$WS/session.log"; exit 1; }

python3 "$HERE/api_client.py" "$SOCK" "$KEY" "$TOKEN"

printf '/quit\n' >&3
exec 3>&-
wait $SESSION || true
[ ! -S "$SOCK" ] || { echo "the socket outlived the session"; exit 1; }
grep -q "e2e-app" "$HOME/.be-code/api/access.log" || { echo "no access log line"; exit 1; }
! grep -q "$KEY" "$HOME/.be-code/api/access.log" || { echo "the key reached the access log"; exit 1; }
! grep -q "$TOKEN" "$HOME/.be-code/api/access.log" || { echo "the token reached the access log"; exit 1; }
echo "[PASS] api"
