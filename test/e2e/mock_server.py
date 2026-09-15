#!/usr/bin/env python3
"""Scripted OpenAI-compatible mock: plays a local model that first writes a
broken Go file, then fixes it when the repair prompt arrives; also plays a
co-working model consulted mid-task. Both models are served from the same
port and routed by the request's "model" field, so routing must happen
before any per-scenario call counter is consulted."""
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
# the shell test can confirm the Working memory block survived compaction
# and the redundant read carried its "already read" footer. It also notes
# (informationally only) whether a model-written compaction summary was
# requested, since the harness may compact by cheap collapse alone.
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
    summ = "SUM:yes" if ENG["summarized"] else "SUM:no"
    return [text_chunk(f"{wm} {footer} {summ}")]

class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        model = body.get("model", "")
        last = body["messages"][-1]["content"] or ""

        if model == "coworker-model":
            # The co-worker's scratch agent gets one scripted reply: plain
            # text (no tool call), so its own run ends after one turn and
            # never issues read_file/list_dir/search here.
            chunks = [text_chunk(ADVICE)]
        elif any("engine scenario" in (m.get("content") or "") for m in body["messages"] if m["role"] == "user"):
            chunks = engine_chunks(body)
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
    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps({"data": [{"id": "mock-model"}, {"id": "coworker-model"}]}).encode())

http.server.HTTPServer(("127.0.0.1", 18111), H).serve_forever()
