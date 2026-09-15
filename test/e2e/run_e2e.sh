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
grep -q "mock-cw> Add the missing return" "$OUT"
echo "[PASS] consult"

# Third scenario, same server/workspace: the engine (working memory) test.
# Grow main.go to about 300 lines (~1500 tokens per read) so three reads and
# a small context_tokens budget force the older reads to be collapsed to
# stubs before the fourth call (compact_with_model:false keeps the harness on
# that cheap collapse path instead of falling through to a model-written
# summary, which replaces every tool result — the redundant read's own footer
# included — with a generic stub; collapse-only leaves the newest tool
# result, footer and all, untouched). The mock reports whether the Working
# memory block survived that, whether the redundant read still carried its
# "already read" footer, and — TRIM — whether the transcript was trimmed at
# all: without that last check the first two would pass at any budget, with
# nothing yet dropped for working memory to make up for.
{
	printf 'package main\n\nfunc main() {}\n'
	i=1
	while [ "$i" -le 297 ]; do
		printf '// line %d\n' "$i"
		i=$((i + 1))
	done
} > "$WS/main.go"
cat > "$HOME/.be-code/config.json" <<CFG
{"default_provider":"mock","model":"mock-model",
 "providers":{"mock":{"type":"openai","base_url":"http://127.0.0.1:18111/v1"}},
 "coworkers":[{"name":"mock-cw","provider":"mock","model":"coworker-model"}],
 "max_turns":10,"max_repairs":2,"compat_tool_calls":"auto","verify_on_done":true,
 "context_tokens":2048,"engine":{"enabled":true},"compact_with_model":false}
CFG
OUT3="$DIR/engine.out"
"$(dirname "$0")/../../be-code" run -y -C "$WS" "engine scenario: read main.go three times" > "$OUT3"
cat "$OUT3"
grep -q "WM:yes FOOTER:yes" "$OUT3"
grep -q "TRIM:yes" "$OUT3"
echo "[PASS] engine"

echo "E2E PASS"
