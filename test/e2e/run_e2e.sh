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

# Fourth scenario: a compaction whose summary comes back empty — the failure
# reported from the validation VM — and the task record carrying the work
# across it. The sentinel lives in one workspace file and reaches the model
# only as the output of a shell command, so it can enter a later system
# prompt by exactly one route: the engine's verbatim block for the node that
# was doing when the command ran. TREE:yes is read off the system prompt
# alone (the kept tail still holds the shell result, so the transcript would
# be no proof at all), and COMPACT:yes confirms the empty-summary branch was
# the one taken.
#
# The proof that none of this passes vacuously is one command:
#
#   E2E_TASK_ENGINE=false sh test/e2e/run_e2e.sh
#
# which runs the identical script with the engine off and stops here with
# TREE:no, since with no task record there is nothing but a trimmed
# transcript on the far side of the compaction.
TASKWS="$DIR/taskws"; mkdir -p "$TASKWS"
printf 'PARSER-SENTINEL-4F2A\n' > "$TASKWS/parser-check.txt"
task_config() { # $1: the engine.enabled value
	cat > "$HOME/.be-code/config.json" <<CFG
{"default_provider":"mock","model":"task-model",
 "providers":{"mock":{"type":"openai","base_url":"http://127.0.0.1:18111/v1"}},
 "max_turns":10,"max_repairs":0,"compat_tool_calls":"auto","verify_on_done":false,
 "context_tokens":1200,"engine":{"enabled":$1},"compact_with_model":true}
CFG
}
task_config "${E2E_TASK_ENGINE:-true}"
OUT4="$DIR/task.out"
"$(dirname "$0")/../../be-code" run -y -C "$TASKWS" "task scenario: check the parser" > "$OUT4"
cat "$OUT4"
grep -q "TREE:yes COMPACT:yes" "$OUT4"
echo "[PASS] task"

# Fifth scenario: the native Ollama path. The provider is type ollama, the
# model has a configured context_window, and the fake /api/ps reports it as
# not resident — so the loader picks that window without needing consent,
# which a scripted run could never give. The fake /api/chat answers with the
# options.num_ctx it actually received, so the assertion is on the wire and
# not on anything the harness says about itself.
cat > "$HOME/.be-code/config.json" <<CFG
{"default_provider":"fake-ollama","model":"native-model",
 "providers":{"fake-ollama":{"type":"ollama","base_url":"http://127.0.0.1:18111",
   "context_window":32768}},
 "max_turns":4,"max_repairs":0,"compat_tool_calls":"never","verify_on_done":false,
 "engine":{"enabled":false},"keep_alive":"0"}
CFG
OUT5="$DIR/native.out"
"$(dirname "$0")/../../be-code" run -y -C "$WS" "native scenario: report the window" > "$OUT5"
cat "$OUT5"
grep -q "NUMCTX:32768" "$OUT5"
echo "[PASS] native"

# Sixth scenario: a sub-agent owns one step. The lead assigns it, the
# sub-agent writes inside its scope, is refused outside, asks, and the
# lead's final reply confirms the hand-back named the file.
SUBWS="$DIR/subws"; mkdir -p "$SUBWS/internal/scan" "$SUBWS/cmd"
cat > "$HOME/.be-code/config.json" <<CFG
{"default_provider":"mock","model":"lead-model",
 "providers":{"mock":{"type":"openai","base_url":"http://127.0.0.1:18111/v1"}},
 "coworkers":[{"name":"sub","provider":"mock","model":"sub-model","sub_agent":true}],
 "sub_agents":{"max_concurrent":1,"max_turns":6,"ask_timeout":30},
 "max_turns":8,"max_repairs":0,"compat_tool_calls":"auto","verify_on_done":false,
 "engine":{"enabled":true}}
CFG
OUT6="$DIR/subagent.out"
"$(dirname "$0")/../../be-code" run -y -C "$SUBWS" "sub-agent scenario: port the scanner" > "$OUT6"
cat "$OUT6"
grep -q "HANDBACK:yes" "$OUT6"
test -f "$SUBWS/internal/scan/token.go"
test ! -f "$SUBWS/cmd/x.go"
grep -q "@sub  scope: internal/scan" "$SUBWS"/.be-code/tasks/*.md
grep -q "^  - \[x\] .*port internal/scan" "$SUBWS"/.be-code/tasks/*.md
echo "[PASS] sub-agent"

echo "E2E PASS"
