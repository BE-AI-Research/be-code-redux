#!/usr/bin/env python3
"""Scripted OpenAI-compatible mock: plays a local model that first writes a
broken Go file, then fixes it when the repair prompt arrives."""
import json, http.server, itertools

counter = itertools.count()

BROKEN = "package main\n\nfunc Add(a, b int) int {\n\treturn a + b\n" # missing }
FIXED  = "package main\n\nfunc Add(a, b int) int {\n\treturn a + b\n}\n"

def tool_call_chunk(name, args):
    return {"choices": [{"delta": {"tool_calls": [{
        "index": 0, "id": f"call_{next(counter)}", "type": "function",
        "function": {"name": name, "arguments": json.dumps(args)}}]}}]}

def text_chunk(t):
    return {"choices": [{"delta": {"content": t}}]}

state = {"n": 0}

class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        n = state["n"]; state["n"] += 1
        last = body["messages"][-1]["content"] or ""
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
        self.wfile.write(json.dumps({"data": [{"id": "mock-model"}]}).encode())

http.server.HTTPServer(("127.0.0.1", 18111), H).serve_forever()
