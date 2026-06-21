"""Flat one-liner native onboarding (issue #217).

The headline quickstart: an API-key-authed flat create that returns a READY
sandbox handle exposing exec / run_code / files / fork / terminate directly.

Two harnesses:

1. A self-contained in-process fake sandbox-server (http.server) that implements
   the same REST routes the real sandbox-server serves (exec, files, run_code,
   fork, terminate). The real sandbox-server in --mock mode boots no guest agent,
   so it cannot answer exec/files/run_code; the fake stands in for the guest so
   the FULL quickstart (create -> exec/run_code/files/fork/terminate) runs end to
   end, deterministically and cross-platform. This mirrors how the rest of the
   Python suite exercises the exec/files/run_code wire shape against a fake HTTP
   layer (test_sandbox.py, test_run_code.py, test_stream.py).

2. The real mock sandbox-server, reused from test_direct.py's fixture pattern,
   covers create -> fork -> terminate against the actual Go binary.
"""

import base64
import json
import os
import subprocess
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import httpx
import pytest

import mitos
from mitos.direct import DirectSandbox, SandboxServer


# ---------------------------------------------------------------------------
# In-process fake sandbox-server: implements the REST routes the SDK calls.
# ---------------------------------------------------------------------------


def _make_fake_server():
    state = {"templates": set(), "sandboxes": {}, "files": {}, "template_network": {}}

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args):  # silence test server logging
            pass

        def _json(self, code, obj):
            body = json.dumps(obj).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def _err(self, code, msg):
            self._json(code, {"error": {"code": _code_for(code), "message": msg, "cause": msg}})

        def _read(self):
            n = int(self.headers.get("Content-Length", 0))
            return json.loads(self.rfile.read(n)) if n else {}

        def do_GET(self):
            if self.path == "/v1/health":
                self._json(200, {"status": "ok", "mock": True})
            elif self.path == "/v1/templates":
                self._json(200, [{"id": t, "ready": True} for t in state["templates"]])
            elif self.path == "/v1/sandboxes":
                self._json(200, list(state["sandboxes"].values()))
            else:
                self._err(404, "not found")

        def do_POST(self):
            req = self._read()
            if self.path == "/v1/templates":
                tid = req.get("id")
                if tid in state["templates"]:
                    self._err(409, "template exists")
                    return
                state["templates"].add(tid)
                # Record the network posture so tests can assert what the SDK sent
                # (issue #219); echo it back the way the real server does.
                net = req.get("network")
                state["template_network"][tid] = net
                self._json(200, {"id": tid, "ready": True, "network": net})
            elif self.path == "/v1/fork":
                tpl = req.get("template")
                if tpl not in state["templates"]:
                    self._err(404, "template not found")
                    return
                sid = req["id"]
                info = {
                    "id": sid, "template_id": tpl,
                    "endpoint": "http://localhost", "fork_time_ms": 0.8,
                }
                state["sandboxes"][sid] = info
                state["files"].setdefault(sid, {})
                self._json(200, info)
            elif self.path == "/v1/exec":
                cmd = req.get("command", "")
                out = cmd.split("echo ", 1)[1] + "\n" if "echo " in cmd else ""
                self._json(200, {"exit_code": 0, "stdout": out, "stderr": "", "exec_time_ms": 1})
            elif self.path == "/v1/files/write":
                state["files"].setdefault(req["sandbox"], {})[req["path"]] = req.get("content", "")
                self._json(200, {"status": "ok"})
            elif self.path == "/v1/files/read":
                files = state["files"].get(req["sandbox"], {})
                if req["path"] not in files:
                    self._err(404, "no such file")
                    return
                self._json(200, {"content": files[req["path"]]})
            elif self.path == "/v1/files/list":
                files = state["files"].get(req["sandbox"], {})
                base = req["path"].rstrip("/")
                entries = [
                    {"name": p[len(base) + 1:], "is_dir": False, "size": len(c), "mode": 0o644}
                    for p, c in files.items()
                    if p.startswith(base + "/") and "/" not in p[len(base) + 1:]
                ]
                self._json(200, {"entries": entries})
            elif self.path == "/v1/files/remove":
                state["files"].get(req["sandbox"], {}).pop(req["path"], None)
                self._json(200, {"status": "ok"})
            elif self.path == "/v1/run_code/stream":
                self.send_response(200)
                self.send_header("Content-Type", "application/x-ndjson")
                self.end_headers()
                frames = [
                    {"kind": "stdout", "stdout": base64.b64encode(b"ran\n").decode()},
                    {"kind": "result", "result": {"text": "2", "data": {"text/plain": "2"}}},
                    {"kind": "exit", "exit_code": 0},
                ]
                for f in frames:
                    self.wfile.write((json.dumps(f) + "\n").encode())
            else:
                self._err(404, "not found")

        def do_DELETE(self):
            # /v1/sandboxes/{id}
            sid = self.path.rsplit("/", 1)[-1]
            if sid in state["sandboxes"]:
                del state["sandboxes"][sid]
                self._json(200, {"status": "terminated", "id": sid})
            else:
                self._err(404, "not found")

    def _code_for(code):
        return {404: "not_found", 409: "conflict", 400: "bad_request"}.get(code, "internal_error")

    httpd = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    host, port = httpd.server_address
    return httpd, f"http://{host}:{port}", state


@pytest.fixture
def fake_server():
    httpd, url, state = _make_fake_server()
    yield url, state
    httpd.shutdown()


# ---------------------------------------------------------------------------
# Flat quickstart end to end against the in-process fake server.
# ---------------------------------------------------------------------------


def test_quickstart_four_lines(fake_server):
    """The headline quickstart runs unchanged: create -> exec -> terminate."""
    url, _ = fake_server
    sb = mitos.create("python", api_key="sk-test", base_url=url)
    result = sb.exec("echo hello")
    assert result.exit_code == 0
    assert "hello" in result.stdout
    sb.terminate()


def test_create_returns_ready_direct_handle(fake_server):
    url, _ = fake_server
    sb = mitos.create("python", api_key="sk-test", base_url=url)
    assert isinstance(sb, DirectSandbox)
    assert sb.id
    assert sb.template == "python"
    sb.terminate()


def test_create_via_env(monkeypatch, fake_server):
    url, _ = fake_server
    monkeypatch.setenv("MITOS_API_KEY", "sk-env")
    monkeypatch.setenv("MITOS_BASE_URL", url)
    sb = mitos.create("python")
    assert sb.exec("echo x").exit_code == 0
    sb.terminate()


def test_explicit_args_override_env(monkeypatch, fake_server):
    url, _ = fake_server
    monkeypatch.setenv("MITOS_BASE_URL", "http://wrong.invalid:1")
    sb = mitos.create("python", api_key="sk-test", base_url=url)
    assert sb.id
    sb.terminate()


def test_missing_base_url_raises(monkeypatch):
    monkeypatch.delenv("MITOS_BASE_URL", raising=False)
    monkeypatch.delenv("MITOS_API_KEY", raising=False)
    from mitos.errors import AgentRunError

    with pytest.raises(AgentRunError) as ei:
        mitos.create("python")
    assert ei.value.code == "missing_base_url"
    # The remediation must not leak any key value.
    assert "sk-" not in (ei.value.remediation or "")
    assert "sk-" not in (ei.value.cause or "")


def test_api_key_never_in_repr_or_error(fake_server):
    url, _ = fake_server
    sb = mitos.create("python", api_key="sk-secret-value", base_url=url)
    assert "sk-secret-value" not in repr(sb)
    sb.terminate()


def test_sandbox_create_classmethod(fake_server):
    """Sandbox.create is the documented alias to the same flat path."""
    from mitos import Sandbox

    url, _ = fake_server
    sb = Sandbox.create("python", api_key="sk-test", base_url=url)
    assert isinstance(sb, DirectSandbox)
    sb.terminate()


def test_files_roundtrip(fake_server):
    url, _ = fake_server
    sb = mitos.create("python", api_key="sk-test", base_url=url)
    sb.files.write("/workspace/note.txt", "hello world")
    assert sb.files.read("/workspace/note.txt") == "hello world"
    names = [e.name for e in sb.files.list("/workspace")]
    assert "note.txt" in names
    sb.files.remove("/workspace/note.txt")
    names = [e.name for e in sb.files.list("/workspace")]
    assert "note.txt" not in names
    sb.terminate()


def test_run_code(fake_server):
    url, _ = fake_server
    sb = mitos.create("python", api_key="sk-test", base_url=url)
    ex = sb.run_code("print(1 + 1)\n2")
    assert ex.text == "2"
    assert ex.logs["stdout"] == ["ran\n"]
    sb.terminate()


def test_fork_creates_independent_siblings(fake_server):
    url, state = fake_server
    sb = mitos.create("python", api_key="sk-test", base_url=url)
    children = sb.fork(2)
    assert len(children) == 2
    ids = {c.id for c in children}
    assert len(ids) == 2
    assert sb.id not in ids
    for child in children:
        assert isinstance(child, DirectSandbox)
        assert child.exec("echo ok").exit_code == 0
        child.terminate()
    sb.terminate()


def test_context_manager(fake_server):
    url, state = fake_server
    with mitos.create("python", api_key="sk-test", base_url=url) as sb:
        sid = sb.id
        assert sb.exec("echo x").exit_code == 0
    assert sid not in state["sandboxes"]


def test_network_knobs_sent_on_create(fake_server):
    """A Network(...) passed to create reaches the server's template-create body
    with all egress/ingress knobs (issue #219)."""
    import mitos

    url, state = fake_server
    net = mitos.Network(
        block=False,
        egress="deny",
        allow_domains=["api.example.com:443"],
        allow_cidrs=["10.0.0.0/8"],
        inbound="allow",
        inbound_cidrs=["203.0.113.0/24"],
    )
    sb = mitos.create("python", api_key="sk-test", base_url=url, network=net)
    sent = state["template_network"]["python"]
    assert sent["allow_domains"] == ["api.example.com:443"]
    assert sent["allow_cidrs"] == ["10.0.0.0/8"]
    assert sent["inbound"] == "allow"
    assert sent["inbound_cidrs"] == ["203.0.113.0/24"]
    sb.terminate()


def test_network_block_total_deny(fake_server):
    """Network(block=True) is the total-deny knob (Modal block_network=True)."""
    import mitos

    url, state = fake_server
    sb = mitos.create("python", api_key="sk-test", base_url=url, network=mitos.Network(block=True))
    assert state["template_network"]["python"]["block"] is True
    sb.terminate()


def test_no_network_omits_field_secure_default(fake_server):
    """Omitting network sends no network field; the server applies the secure
    deny-by-default in both directions (issue #219)."""
    import mitos

    url, state = fake_server
    sb = mitos.create("python", api_key="sk-test", base_url=url)
    assert state["template_network"]["python"] is None
    sb.terminate()


def test_network_to_dict_omits_secure_defaults():
    """Network.to_dict omits empty/false defaults so the secure default is
    server-applied and the request stays minimal."""
    import mitos

    assert mitos.Network().to_dict() == {}
    assert mitos.Network(egress="allow").to_dict() == {"egress": "allow"}
    assert mitos.Network(inbound="allow").to_dict() == {"inbound": "allow"}


def test_pty_url_carries_no_key(fake_server):
    url, _ = fake_server
    sb = mitos.create("python", api_key="sk-secret", base_url=url)
    pu = sb.pty_url()
    assert pu.startswith("ws://")
    assert "sk-secret" not in pu
    sb.terminate()


# ---------------------------------------------------------------------------
# Real mock sandbox-server (Go binary): create -> fork -> terminate.
# Reuses test_direct.py's build-and-run fixture pattern.
# ---------------------------------------------------------------------------


REAL_SERVER_URL = "http://localhost:18082"
_real_proc = None


@pytest.fixture(scope="module")
def real_server():
    global _real_proc
    repo_root = os.path.join(os.path.dirname(__file__), "..", "..", "..")
    build = subprocess.run(
        ["go", "build", "-o", "/tmp/sandbox-server-flat-test", "./cmd/sandbox-server/"],
        cwd=repo_root, capture_output=True,
    )
    if build.returncode != 0:
        pytest.skip(f"Could not build sandbox-server: {build.stderr.decode()}")
    _real_proc = subprocess.Popen(
        ["/tmp/sandbox-server-flat-test", "--mock", "--addr", ":18082"],
        stdout=subprocess.PIPE, stderr=subprocess.PIPE,
    )
    time.sleep(1)
    yield REAL_SERVER_URL
    _real_proc.terminate()
    _real_proc.wait(timeout=5)


def test_real_server_create_fork_terminate(real_server):
    """Against the actual Go mock server: the flat create gets-or-creates the
    template, forks a READY sandbox, and a fork() produces an independent
    sibling. (The mock server boots no guest, so exec is not exercised here; the
    in-process fake covers exec/files/run_code.)"""
    sb = mitos.create("python", api_key="sk-test", base_url=real_server)
    assert sb.id
    assert sb.fork_time_ms >= 0
    child = sb.fork(1)[0]
    assert child.id != sb.id
    child.terminate()
    sb.terminate()

    # A second create is idempotent on the template (409 swallowed).
    sb2 = mitos.create("python", api_key="sk-test", base_url=real_server)
    assert sb2.id != sb.id
    sb2.terminate()


# ---------------------------------------------------------------------------
# Async parity (mitos.aio): the flat handle over an ASGI transport.
# ---------------------------------------------------------------------------


async def _async_app(scope, receive, send):
    assert scope["type"] == "http"
    path = scope["path"]
    body = b""
    while True:
        msg = await receive()
        body += msg.get("body", b"")
        if not msg.get("more_body"):
            break
    req = json.loads(body or b"{}")
    method = scope["method"]

    status, payload, ndjson = 200, {}, None
    if path == "/v1/templates" and method == "POST":
        payload = {"id": req["id"], "ready": True}
    elif path == "/v1/fork":
        payload = {"id": req["id"], "template_id": req["template"],
                   "endpoint": "http://sb", "fork_time_ms": 0.8}
    elif path == "/v1/exec":
        payload = {"exit_code": 0, "stdout": "ok\n", "stderr": "", "exec_time_ms": 1.0}
    elif path == "/v1/files/write":
        payload = {"status": "ok"}
    elif path == "/v1/files/read":
        payload = {"content": "async-body"}
    elif path == "/v1/run_code/stream":
        ndjson = [
            {"kind": "stdout", "stdout": base64.b64encode(b"r\n").decode()},
            {"kind": "result", "result": {"text": "2", "data": {"text/plain": "2"}}},
            {"kind": "exit", "exit_code": 0},
        ]
    elif path.startswith("/v1/sandboxes/") and method == "DELETE":
        payload = {"status": "terminated"}
    else:
        status, payload = 404, {"error": {"code": "not_found", "message": "no route"}}

    if ndjson is not None:
        await send({"type": "http.response.start", "status": 200,
                    "headers": [(b"content-type", b"application/x-ndjson")]})
        data = b"".join((json.dumps(f) + "\n").encode() for f in ndjson)
        await send({"type": "http.response.body", "body": data})
        return
    data = json.dumps(payload).encode()
    await send({"type": "http.response.start", "status": status,
                "headers": [(b"content-type", b"application/json")]})
    await send({"type": "http.response.body", "body": data})


def _async_direct():
    from mitos.aio import AsyncDirectSandbox

    transport = httpx.ASGITransport(app=_async_app)
    client = httpx.AsyncClient(transport=transport, base_url="http://sb")
    return AsyncDirectSandbox(
        id="sb-1", template="python", endpoint="http://sb",
        server_url="http://sb", fork_time_ms=0.8, api_key="sk-test", _http=client,
    )


@pytest.mark.asyncio
async def test_async_create_resolves_auth(monkeypatch):
    """mitos.aio.create resolves the base URL from MITOS_BASE_URL and raises a
    typed error when absent (no key leak)."""
    import mitos.aio as aio
    from mitos.errors import AgentRunError

    monkeypatch.delenv("MITOS_BASE_URL", raising=False)
    monkeypatch.delenv("MITOS_API_KEY", raising=False)
    with pytest.raises(AgentRunError) as ei:
        await aio.create("python")
    assert ei.value.code == "missing_base_url"
    assert "sk-" not in (ei.value.remediation or "")


@pytest.mark.asyncio
async def test_async_exec():
    sb = _async_direct()
    res = await sb.exec("echo hi")
    assert res.exit_code == 0
    assert "ok" in res.stdout
    await sb._http.aclose()


@pytest.mark.asyncio
async def test_async_files_roundtrip():
    sb = _async_direct()
    await sb.files.write("/workspace/n.txt", "x")
    assert await sb.files.read("/workspace/n.txt") == "async-body"
    await sb._http.aclose()


@pytest.mark.asyncio
async def test_async_run_code():
    sb = _async_direct()
    ex = await sb.run_code("print(1+1)\n2")
    assert ex.text == "2"
    await sb._http.aclose()


@pytest.mark.asyncio
async def test_async_fork():
    sb = _async_direct()
    children = await sb.fork(2)
    assert len(children) == 2
    assert len({c.id for c in children}) == 2
    for c in children:
        await c._http.aclose()
    await sb._http.aclose()


@pytest.mark.asyncio
async def test_async_create_sends_network():
    """Async parity: mitos.aio.create threads a Network(...) into the template
    create body (issue #219)."""
    import mitos
    import mitos.aio as aio

    captured = {}

    async def app(scope, receive, send):
        body = b""
        while True:
            msg = await receive()
            body += msg.get("body", b"")
            if not msg.get("more_body"):
                break
        req = json.loads(body or b"{}")
        path = scope["path"]
        if path == "/v1/templates":
            captured["network"] = req.get("network")
            payload = {"id": req["id"], "ready": True}
        elif path == "/v1/fork":
            payload = {"id": req["id"], "template_id": req["template"],
                       "endpoint": "http://sb", "fork_time_ms": 0.8}
        else:
            payload = {}
        data = json.dumps(payload).encode()
        await send({"type": "http.response.start", "status": 200,
                    "headers": [(b"content-type", b"application/json")]})
        await send({"type": "http.response.body", "body": data})

    transport = httpx.ASGITransport(app=app)
    # Patch the module's AsyncClient so create uses the ASGI transport.
    orig = httpx.AsyncClient
    httpx.AsyncClient = lambda *a, **k: orig(transport=transport, base_url="http://sb")
    try:
        sb = await aio.create(
            "python", api_key="sk-test", base_url="http://sb",
            network=mitos.Network(block=True, allow_cidrs=["10.0.0.0/8"]),
        )
    finally:
        httpx.AsyncClient = orig
    assert captured["network"]["block"] is True
    assert captured["network"]["allow_cidrs"] == ["10.0.0.0/8"]
    await sb._http.aclose()
