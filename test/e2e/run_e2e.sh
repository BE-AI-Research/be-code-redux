#!/bin/sh
# End-to-end test: real be-code binary vs a scripted mock backend that writes
# broken Go and repairs it when verification feedback arrives.
set -e
DIR=$(mktemp -d); trap 'rm -rf "$DIR"; kill $SRV 2>/dev/null' EXIT
# (Also inherited by online_e2e.sh's mock, which logs to it too; harmless.)
export BE_E2E_LOG="$DIR/mock.log"
cp /dev/null "$BE_E2E_LOG"
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
# The exact double space before "@sub" and before "scope:", and the two-space
# indent on the child line, are internal/engine/markdown.go's own rendering
# (renderFields, indentStep) — a change to that rendering should break this
# assertion deliberately rather than have it silently stop checking anything.
grep -q "@sub  scope: internal/scan" "$SUBWS"/.be-code/tasks/*.md
grep -q "^  - \[x\] .*port internal/scan" "$SUBWS"/.be-code/tasks/*.md
echo "[PASS] sub-agent"

# Seventh scenario: dispatch. The primary hands a scoped job to co-worker
# mock-cw (no sub_agent flag needed); the job writes pkg/x.go; the primary's
# DISPATCHED:done is gated in the mock on the hand-back line naming the
# co-worker and the file, so it cannot appear unless the hand-back reached
# the primary. Then the stall branch: the job's first reply is held until
# the primary has called jobs/extend, which it only does on the supervisor's
# "has been quiet for" line, so the mock log holding an extend call proves
# the whole stall -> notice -> extend path. The co-worker is on a different
# provider spelling (localhost vs 127.0.0.1) of the same mock, so it has its
# own lane: sharing the primary's lane, a held sub-agent request would block
# the primary's extend call behind it (one request in flight per server).
cat > "$HOME/.be-code/config.json" <<CFG
{"default_provider":"mock","model":"boss-model",
 "providers":{"mock":{"type":"openai","base_url":"http://127.0.0.1:18111/v1"},
              "mockjob":{"type":"openai","base_url":"http://localhost:18111/v1"}},
 "coworkers":[{"name":"mock-cw","provider":"mockjob","model":"job-model"}],
 "sub_agents":{"max_concurrent":1,"max_turns":6,"ask_timeout":30,"stall_after":2},
 "max_turns":8,"max_repairs":0,"compat_tool_calls":"auto","verify_on_done":false,
 "engine":{"enabled":true}}
CFG
DISPWS="$DIR/dispws"; mkdir -p "$DISPWS/pkg"
OUT7="$DIR/dispatch.out"
"$(dirname "$0")/../../be-code" run -y -C "$DISPWS" "dispatch scenario: port the scanner" > "$OUT7"
cat "$OUT7"
# DISPATCHED:done cannot pass vacuously: the mock emits it only once a user
# message holds "sub-agent mock-cw finished" and "wrote pkg/x.go", i.e. the
# hand-back text reached the primary; otherwise it says "dispatched; waiting".
grep -q "DISPATCHED:done" "$OUT7" || { echo "FAIL dispatch: primary never saw the hand-back (no DISPATCHED:done)"; exit 1; }
test -f "$DISPWS/pkg/x.go" || { echo "FAIL dispatch: job did not write pkg/x.go"; exit 1; }
# The job field renders as trailing "  job" (internal/engine/markdown.go). Not
# vacuous: the tasks README has no line ending in "job" and no step text here
# does, so only the rendered field matches.
grep -q "job$" "$DISPWS"/.be-code/tasks/*.md || { echo "FAIL dispatch: task document has no job field"; exit 1; }
DISPWS2="$DIR/dispws2"; mkdir -p "$DISPWS2/pkg"
OUT8="$DIR/dispatch-stall.out"
"$(dirname "$0")/../../be-code" run -y -C "$DISPWS2" "dispatch scenario stall: port the scanner" > "$OUT8"
cat "$OUT8"
grep -q "DISPATCHED:done" "$OUT8" || { echo "FAIL dispatch(stall): no DISPATCHED:done after the stall"; exit 1; }
test -f "$DISPWS2/pkg/x.go" || { echo "FAIL dispatch(stall): job did not write pkg/x.go"; exit 1; }
# The supervisor announces the stall on the run's output as "job <id> stalled".
grep -q "job .* stalled" "$OUT8" || { echo "FAIL dispatch(stall): supervisor never reported a stall"; exit 1; }
# The mock log holds every request body; a request whose history carries an
# assistant tool call named jobs with action extend proves the primary acted
# on the stall line (the tool schema text alone cannot satisfy this). The
# mock's 75 s release of the held job reply does not make this vacuous: the
# release writes the file but never records an extend call, so a run that
# merely waited the hold out fails here.
python3 -I -c '
import json, sys
for l in open(sys.argv[1]):
    try:
        body = json.loads(l)
    except ValueError:
        continue
    for m in body["messages"]:
        for tc in m.get("tool_calls") or []:
            f = tc["function"]
            if f["name"] == "jobs" and json.loads(f["arguments"]).get("action") == "extend":
                sys.exit(0)
sys.exit(1)' "$DIR/mock.log" || { echo "FAIL dispatch(stall): primary never issued jobs extend after the stall line"; exit 1; }
echo "[PASS] dispatch"

sh "$(dirname "$0")/online_e2e.sh"
sh "$(dirname "$0")/api_e2e.sh"
echo "E2E PASS"
