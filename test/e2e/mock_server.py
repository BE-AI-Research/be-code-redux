#!/usr/bin/env python3
"""Scripted OpenAI-compatible mock: plays a local model that first writes a
broken Go file, then fixes it when the repair prompt arrives; also plays a
co-working model consulted mid-task. Both models are served from the same
port and routed by the request's "model" field, so routing must happen
before any per-scenario call counter is consulted.

The same port also plays a fake Ollama on /api/*, for the native scenario:
/api/chat streams NDJSON and reports back the options.num_ctx it was sent,
/api/ps says nothing is resident (so the loader needs no consent to choose
the window) and /api/show offers no Modelfile num_ctx. Requests are routed
by path first, since the native path is a different protocol on the same
server, not another model on the OpenAI one."""
import json, http.server, itertools

counter = itertools.count()

BROKEN = "package main\n\nfunc Add(a, b int) int {\n\treturn a + b\n" # missing }
FIXED  = "package main\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n"
ADVICE = "Add the missing return at the end of add.go."

def tool_call_chunk(name, args):
    return {"choices": [{"delta": {"tool_calls": [{
        "index": 0, "id": f"call_{next(counter)}", "type": "function",
        "function": {"name": name, "arguments": json.dumps(args)}}]}}]}

def text_chunk(t):
    return {"choices": [{"delta": {"content": t}}]}

def first_user_content(body):
    for m in body["messages"]:
        if m.get("role") == "user":
            return m.get("content") or ""
    return ""

state = {"n": 0, "consult_n": 0}

# Third scenario: the engine (working memory) test. The task asks for
# main.go to be read three times; the mock scripts three read_file calls,
# then reports what the fourth request's system/user content looked like so
# the shell test can confirm the Working memory block survived the trimming
# of the transcript and the redundant read carried its "already read"
# footer. TRIM reports whether the transcript really was trimmed: without
# it the WM/FOOTER assertions would pass at any context budget, since
# nothing would have been dropped for working memory to make up for. It
# also notes (informationally only) whether a model-written compaction
# summary was requested, since the harness may compact by cheap collapse
# alone.

# The stub History.trim/CollapseToolResults leave behind (collapsedStub in
# internal/agent/history.go: "[old tool result removed to save context]").
TRIM_STUB = "old tool result removed"
ENG = {"n": 0, "summarized": False}

def engine_chunks(body):
    n = ENG["n"]; ENG["n"] += 1
    sys_prompt = body["messages"][0].get("content") or ""
    if sys_prompt.startswith("Summarize this coding-agent"):
        ENG["summarized"] = True
        return [text_chunk("Task: engine scenario. Read main.go.\n\nfiles:\n- main.go — has main\n")]
    if n < 3:  # three reads: compaction needs six messages in history
        return [tool_call_chunk("read_file", {"path": "main.go"})]
    last = body["messages"][-1].get("content") or ""
    footer = "FOOTER:yes" if "already read at turn" in last else "FOOTER:no"
    wm = "WM:yes" if "Working memory:" in sys_prompt and "main.go (lines" in sys_prompt else "WM:no"
    trimmed = any(TRIM_STUB in (m.get("content") or "") for m in body["messages"])
    trim = "TRIM:yes" if trimmed else "TRIM:no"
    summ = "SUM:yes" if ENG["summarized"] else "SUM:no"
    return [text_chunk(f"{wm} {footer} {trim} {summ}")]

# Fourth scenario: the task record survives a compaction that produces no
# summary — the VM failure this whole design exists to fix. The scripted
# model plans a task, marks its first step doing, and runs one shell command
# whose *output* carries the sentinel. The sentinel is never in a prompt, a
# request, the guidance text or the user's message: the only way it can
# reach a later system prompt is the engine's rendered "raw:" block for the
# node that was doing when the command ran. The budget is small enough that
# every turn is over the limit and collapsing tool traffic cannot reach the
# target, so the fourth call compacts with the model — and the model (this
# mock) returns an empty summary, which is the failure under test. The
# assertions are read off the *system prompt* alone, never the transcript,
# because the transcript's kept tail still holds the shell result.
SENTINEL = "PARSER-SENTINEL-4F2A"
TASK_TEXT = "fix the parser"
NO_SUMMARY = "No summary of the earlier turns was produced"
TASK = {"n": 0}

def task_chunks(body):
    sys_prompt = body["messages"][0].get("content") or ""
    if sys_prompt.startswith("Summarize this coding-agent"):
        # The empty summary. Compaction must then continue from the tree.
        return [text_chunk("")]
    n = TASK["n"]; TASK["n"] += 1
    if n == 0:
        return [tool_call_chunk("task", {
            "action": "plan", "text": TASK_TEXT,
            "steps": ["find the bug", "fix it"]})]
    if n == 1:
        # The plan's own result names the task's id ("task 2"), so the step
        # is addressed by what the tree actually assigned rather than by a
        # number guessed here.
        last = body["messages"][-1].get("content") or ""
        root = last.split()[-1] if last.startswith("task ") else "2"
        return [tool_call_chunk("task", {"action": "status", "id": root + ".1", "status": "doing"})]
    if n == 2:
        # The sentinel is in the file this reads, never in the command.
        return [tool_call_chunk("shell", {"command": "cat parser-check.txt"})]
    # Anchored on the engine's own block, not merely on the system prompt:
    # the sentinel and the task line must be *inside* "Working memory:",
    # which is the one section no request, guidance paragraph or project
    # note can write into.
    wm = sys_prompt.split("Working memory:", 1)
    tree = "yes" if len(wm) == 2 and SENTINEL in wm[1] and TASK_TEXT in wm[1] else "no"
    compacted = any(NO_SUMMARY in (m.get("content") or "") for m in body["messages"])
    return [text_chunk("TREE:%s COMPACT:%s" % (tree, "yes" if compacted else "no"))]

# The native (fake Ollama) scenario. /api/chat records the num_ctx it was
# sent and answers with it, so the assertion is on what actually reached the
# wire rather than on anything the harness reports about itself.
def native_chunks(body):
    num_ctx = (body.get("options") or {}).get("num_ctx", 0)
    return [
        {"message": {"role": "assistant", "content": "NUMCTX:%s" % num_ctx}, "done": False},
        {"message": {"role": "assistant", "content": ""}, "done": True,
         "done_reason": "stop", "prompt_eval_count": 11, "eval_count": 5},
    ]

class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass

    def _json(self, obj):
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps(obj).encode())

    def do_POST(self):
        if self.path.startswith("/api/"):
            return self.do_native()
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        model = body.get("model", "")
        last = body["messages"][-1]["content"] or ""

        if model == "native-model":
            # The native scenario's model, arriving on the OpenAI endpoint:
            # the provider gave up on /api/chat. Say so rather than letting
            # it fall through to another scenario's script, so a broken
            # native path fails its assertion loudly.
            chunks = [text_chunk("NUMCTX:openai-fallback")]
        elif model == "coworker-model":
            # The co-worker's scratch agent gets one scripted reply: plain
            # text (no tool call), so its own run ends after one turn and
            # never issues read_file/list_dir/search here.
            chunks = [text_chunk(ADVICE)]
        elif any("engine scenario" in (m.get("content") or "") for m in body["messages"] if m["role"] == "user"):
            chunks = engine_chunks(body)
        elif model == "task-model":
            # Routed by model, not by the user's words: the scenario's point
            # is that the compaction replaces the first user message, so
            # after it there is nothing in the transcript left to route on.
            chunks = task_chunks(body)
        elif "consult scenario" in first_user_content(body):
            n = state["consult_n"]; state["consult_n"] += 1
            if n == 0:
                chunks = [tool_call_chunk("consult", {"question": "why does add.go not compile?"})]
            else:
                chunks = [text_chunk(f"The co-worker says: {ADVICE}")]
        else:
            n = state["n"]; state["n"] += 1
            if n == 0:
                chunks = [tool_call_chunk("write_file", {"path": "add.go", "content": BROKEN})]
            elif n == 1:
                chunks = [text_chunk("Added add.go with an Add function.")]
            elif "Verification failed" in last or n == 2:
                chunks = [tool_call_chunk("write_file", {"path": "add.go", "content": FIXED})]
            else:
                chunks = [text_chunk("Fixed the missing brace in add.go; build should pass now.")]

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        for c in chunks:
            self.wfile.write(f"data: {json.dumps(c)}\n\n".encode())
        self.wfile.write(b"data: [DONE]\n\n")
    def do_native(self):
        n = int(self.headers.get("Content-Length") or 0)
        body = json.loads(self.rfile.read(n)) if n else {}
        if self.path == "/api/chat":
            self.send_response(200)
            self.send_header("Content-Type", "application/x-ndjson")
            self.end_headers()
            for c in native_chunks(body):
                self.wfile.write((json.dumps(c) + "\n").encode())
            return
        if self.path == "/api/show":
            # No Modelfile num_ctx: nothing here may supply a window, so the
            # only window that can reach the wire is the configured one.
            return self._json({"parameters": ""})
        # /api/generate (the keep-alive touch) and anything else.
        self._json({"done": True})

    def do_GET(self):
        if self.path.startswith("/api/ps"):
            # Nothing resident: the loader may choose the configured window
            # without evicting anyone, so it needs no consent — which a
            # scripted, non-interactive run could never give.
            return self._json({"models": []})
        if self.path.startswith("/api/tags"):
            return self._json({"models": [{"name": "native-model", "size": 1,
                                           "details": {"family": "mock", "quantization_level": "Q4"}}]})
        self._json({"data": [{"id": "mock-model"}, {"id": "coworker-model"}, {"id": "task-model"}]})

http.server.HTTPServer(("127.0.0.1", 18111), H).serve_forever()
