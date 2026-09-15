#!/bin/sh
# End-to-end test: real be-code binary vs a scripted mock backend that writes
# broken Go and repairs it when verification feedback arrives.
set -e
DIR=$(mktemp -d); trap 'rm -rf "$DIR"; kill $SRV 2>/dev/null' EXIT
python3 "$(dirname "$0")/mock_server.py" & SRV=$!
sleep 1
WS="$DIR/ws"; mkdir -p "$WS"
printf 'module example.com/e2e\n\ngo 1.22\n' > "$WS/go.mod"
printf 'package main\n\nfunc main() {}\n' > "$WS/main.go"
export HOME="$DIR/home"; mkdir -p "$HOME/.be-code"
cat > "$HOME/.be-code/config.json" <<CFG
{"default_provider":"mock","model":"mock-model",
 "providers":{"mock":{"type":"openai","base_url":"http://127.0.0.1:18111/v1"}},
 "coworkers":[{"name":"mock-cw","provider":"mock","model":"coworker-model"}],
 "max_turns":10,"max_repairs":2,"compat_tool_calls":"auto","verify_on_done":true}
CFG
"$(dirname "$0")/../../be-code" run -y -C "$WS" "add an Add function in add.go"
grep -q '^}' "$WS/add.go"

# Second scenario, same server/workspace/config: the primary calls the
# consult tool once for a co-working model, then quotes its advice.
OUT="$DIR/consult.out"
"$(dirname "$0")/../../be-code" run -y -C "$WS" "consult scenario: why does add.go not compile?" > "$OUT"
cat "$OUT"
grep -q "mock-cw> Add the missing return" "$OUT" && echo "[PASS] consult"

echo "E2E PASS"
