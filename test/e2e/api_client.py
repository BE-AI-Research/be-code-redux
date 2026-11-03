#!/usr/bin/env python3
"""Exercise be-code's application API over its unix socket.

usage: api_client.py SOCKET KEY SESSION_TOKEN
Prints OK lines; exits non-zero on the first failure."""
import http.client, json, socket, sys

SOCK, KEY, TOKEN = sys.argv[1], sys.argv[2], sys.argv[3]


class UnixConn(http.client.HTTPConnection):
    def __init__(self):
        super().__init__("localhost")

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.connect(SOCK)


def call(method, path, body=None, key=KEY, session=None, headers=None):
    c = UnixConn()
    h = {"Content-Type": "application/json"}
    if key:
        h["Authorization"] = "Bearer " + key
    if session:
        h["X-BE-Session"] = session
    h.update(headers or {})
    c.request(method, path, body=json.dumps(body) if body is not None else None, headers=h)
    r = c.getresponse()
    raw = r.read()
    c.close()
    try:
        return r.status, json.loads(raw)
    except ValueError:
        return r.status, raw.decode()


def expect(name, got, want):
    if got != want:
        print(f"FAIL {name}: want {want!r}, got {got!r}")
        sys.exit(1)
    print(f"ok {name}")


st, body = call("GET", "/v1/health")
expect("health", st, 200)
expect("health has a session code", isinstance(body.get("session", {}).get("code"), str), True)

st, body = call("POST", "/v1/query", {"prompt": "api scenario: say hello"})
expect("query", st, 200)
expect("query text", body.get("text"), "the api says hello")

st, body = call("POST", "/v1/chat/completions", {"messages": [{"role": "user", "content": "api scenario: hi"}]})
expect("completions", st, 200)
expect("completions content", body["choices"][0]["message"]["content"], "the api says hello")

st, body = call("GET", "/v1/sessions/" + TOKEN, session=TOKEN)
expect("session info", st, 200)
expect("session info hides the code", "code" in body, False)

st, body = call("POST", "/v1/sessions/" + TOKEN + "/query", {"prompt": "api scenario: what now?"}, session=TOKEN)
expect("session query", st, 200)

st, body = call("POST", "/v1/query", {"prompt": "x"}, key="bek_1_" + "A" * 32)
expect("bad key", st, 401)
st, body = call("POST", "/v1/query", {"prompt": "x"}, headers={"Origin": "http://evil"})
expect("browser refused", st, 403)
st, body = call("POST", "/v1/chat/completions", {"messages": [{"role": "user", "content": "x"}], "tools": [{}]})
expect("tools refused", st, 400)
expect("tools refused code", body["error"]["code"], "not_supported")

# Streaming, raw: the stream must end with [DONE].
c = UnixConn()
c.request("POST", "/v1/chat/completions",
          body=json.dumps({"stream": True, "messages": [{"role": "user", "content": "api scenario: stream"}]}),
          headers={"Authorization": "Bearer " + KEY, "Content-Type": "application/json"})
r = c.getresponse()
expect("stream status", r.status, 200)
text, done = "", False
for line in r.read().decode().splitlines():
    if not line.startswith("data: "):
        continue
    payload = line[6:]
    if payload == "[DONE]":
        done = True
        break
    delta = json.loads(payload)["choices"][0]["delta"]
    text += delta.get("content", "")
expect("stream ends with [DONE]", done, True)
expect("stream text", text, "the api says hello")

# The official client, when present: base_url + api_key and nothing else.
try:
    import httpx, openai
except ImportError:
    print("skip openai client (not installed)")
else:
    transport = httpx.HTTPTransport(uds=SOCK)
    client = openai.OpenAI(base_url="http://localhost/v1", api_key=KEY, http_client=httpx.Client(transport=transport))
    parts = [chunk.choices[0].delta.content or "" for chunk in
             client.chat.completions.create(model="default", stream=True,
                                            messages=[{"role": "user", "content": "api scenario: via openai"}])]
    expect("openai client streamed", "".join(parts), "the api says hello")
print("API CLIENT PASS")
