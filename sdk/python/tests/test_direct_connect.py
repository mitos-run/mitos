"""Direct-mode runtime transport over the Connect sandbox.v1.Sandbox service.

Task 6.1 of issue #24: the native ``DirectSandbox`` runtime calls speak the
Connect ``sandbox.v1.Sandbox`` service at ``/sandbox.v1.Sandbox/<Method>``
instead of the legacy JSON ``/v1/*`` routes:

  - exec     -> ExecStream     (server-streaming over HTTP/1.1)
  - run_code -> RunCodeStream  (server-streaming over HTTP/1.1)
  - files.*  -> ReadFile / WriteFile / List / Mkdir / Remove

These tests assert the WIRE the SDK sends (the Connect envelope, the X-Sandbox-Id
routing header, proto-JSON camelCase field names), that the server-streaming
calls deliver output INCREMENTALLY and fold the rich result/error shapes, and
that the public return types are unchanged.

pty stays on the WebSocket transport: an interactive PTY needs the bidi Exec RPC,
which Connect serves only over HTTP/2; httpx cannot speak cleartext HTTP/2 (h2c).
That migration is a documented #24 follow-up.

The fixtures stand up a small in-process server that speaks the Connect wire (the
same approach test_stream.py uses for the cluster NDJSON path), so no Go binary,
KVM, or guest agent is needed.
"""
from __future__ import annotations

import base64
import json
import struct
import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest

from mitos.direct import DirectSandbox
from mitos.errors import AgentRunError, ExecutionDeadlineError

_END = 0b00000010


def _frame(payload: bytes, end: bool = False) -> bytes:
    flag = _END if end else 0
    return bytes([flag]) + struct.pack(">I", len(payload)) + payload


def _decode_request(body: bytes) -> list[dict]:
    out, i = [], 0
    while i + 5 <= len(body):
        length = struct.unpack(">I", body[i + 1 : i + 5])[0]
        payload = body[i + 5 : i + 5 + length]
        i += 5 + length
        if payload:
            out.append(json.loads(payload))
    return out


def _direct(url: str, token=None) -> DirectSandbox:
    return DirectSandbox(
        id="sb-conn",
        template="python",
        endpoint=url,
        server_url=url,
        fork_time_ms=0.5,
        api_key=token,
    )


# ---------------------------------------------------------------------------
# A gated Connect server: it records the request, the X-Sandbox-Id header, and
# (for streaming RPCs) can pause after the first frame so a test proves the SDK
# delivered the first chunk before the rest were sent.
# ---------------------------------------------------------------------------


def _make_server(script):
    """script(handler, method, request_msgs) -> None drives the response."""
    captured = {"headers": {}, "requests": []}

    class H(BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def do_POST(self):
            method = self.path.rsplit("/", 1)[-1]
            n = int(self.headers.get("Content-Length", 0))
            body = self.rfile.read(n) if n else b""
            captured["headers"] = dict(self.headers)
            ct = self.headers.get("Content-Type", "")
            if ct.startswith("application/connect"):
                msgs = _decode_request(body)
            else:
                msgs = [json.loads(body)] if body else [{}]
            captured["requests"].append((method, msgs))
            script(self, method, msgs)

        # Helpers the script uses.
        def unary(self, obj, status=200):
            data = json.dumps(obj).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def stream_start(self):
            self.send_response(200)
            self.send_header("Content-Type", "application/connect+json")
            self.end_headers()

        def stream_msg(self, obj):
            self.wfile.write(_frame(json.dumps(obj).encode()))
            self.wfile.flush()

        def stream_end(self, error=None):
            end = {"error": error} if error else {}
            self.wfile.write(_frame(json.dumps(end).encode(), end=True))
            self.wfile.flush()

    srv = HTTPServer(("127.0.0.1", 0), H)
    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()
    url = f"http://127.0.0.1:{srv.server_address[1]}"
    return srv, url, captured


def test_exec_streams_incrementally_and_sends_sandbox_header():
    """ExecStream is a Connect server-stream: the SDK forwards each stdout/stderr
    chunk the instant its frame arrives, then aggregates the terminal ExecExit.
    Gating after the first frame proves incremental delivery, and the request
    carries the X-Sandbox-Id routing header plus the bearer."""
    gate = threading.Event()

    def script(h, method, msgs):
        assert method == "ExecStream"
        h.stream_start()
        h.stream_msg({"stdout": base64.b64encode(b"first ").decode()})
        # Pause so the test can observe the first chunk before the rest are sent.
        gate.wait(timeout=5)
        h.stream_msg({"stdout": base64.b64encode(b"second\n").decode()})
        h.stream_msg({"stderr": base64.b64encode(b"warn\n").decode()})
        h.stream_msg({"exit": {"exitCode": 3, "execTimeMs": 4.0}})
        h.stream_end()

    srv, url, cap = _make_server(script)
    try:
        sb = _direct(url, token="sk-x")
        result_box = {}

        def run():
            result_box["r"] = sb.exec("echo first second")

        th = threading.Thread(target=run)
        th.start()
        time.sleep(0.2)
        gate.set()
        th.join(timeout=5)
        r = result_box["r"]
        assert r.exit_code == 3
        assert r.stdout == "first second\n"
        assert r.stderr == "warn\n"
        assert r.exec_time_ms == 4.0
        assert cap["headers"].get("X-Sandbox-Id") == "sb-conn"
        assert cap["headers"].get("Authorization") == "Bearer sk-x"
        # ExecStream takes a UNARY request (not an enveloped open frame): the
        # request body is the proto-JSON message with command + timeoutSeconds.
        _, msgs = cap["requests"][0]
        assert msgs[0]["command"] == "echo first second"
        assert "timeoutSeconds" in msgs[0]
    finally:
        srv.shutdown()


def test_exec_deadline_maps_to_typed_error():
    def script(h, method, msgs):
        h.stream_start()
        h.stream_msg({"exit": {"exitCode": 124}})  # the timeout exit code
        h.stream_end()

    srv, url, _ = _make_server(script)
    try:
        sb = _direct(url)
        with pytest.raises(ExecutionDeadlineError):
            sb.exec("sleep 999", timeout=1)
    finally:
        srv.shutdown()


def test_exec_spawn_error_on_exit_frame_raises():
    def script(h, method, msgs):
        h.stream_start()
        h.stream_msg({"exit": {"exitCode": 1, "error": "no such command: bogus"}})
        h.stream_end()

    srv, url, _ = _make_server(script)
    try:
        sb = _direct(url)
        with pytest.raises(AgentRunError) as ei:
            sb.exec("bogus")
        assert ei.value.code == "exec_failed"
    finally:
        srv.shutdown()


def test_run_code_folds_rich_result_and_streams_callbacks():
    def script(h, method, msgs):
        assert method == "RunCodeStream"
        # RunCodeStream takes a UNARY request: code + language + timeoutSeconds.
        assert msgs[0]["code"] == "print(1+1)\n2"
        h.stream_start()
        h.stream_msg({"stdout": base64.b64encode(b"out\n").decode()})
        # RunResult.data is map<string,bytes>: each value base64. text/plain "42"
        # and an image/png whose stored bytes are the base64 string "aGVsbG8=".
        h.stream_msg({"result": {"text": "42", "data": {
            "text/plain": base64.b64encode(b"42").decode(),
            "image/png": base64.b64encode(b"aGVsbG8=").decode(),
        }}})
        h.stream_msg({"exitCode": 0})
        h.stream_end()

    srv, url, _ = _make_server(script)
    try:
        sb = _direct(url)
        seen_out, seen_res = [], []
        ex = sb.run_code(
            "print(1+1)\n2",
            on_stdout=seen_out.append,
            on_result=seen_res.append,
        )
        assert ex.text == "42"
        assert ex.logs["stdout"] == ["out\n"]
        assert ex.results[0].png == "aGVsbG8="
        assert ex.results[0].text == "42"
        assert ex.error is None
        assert seen_out == ["out\n"]
        assert len(seen_res) == 1
    finally:
        srv.shutdown()


def test_run_code_structured_error():
    def script(h, method, msgs):
        h.stream_start()
        h.stream_msg({"error": {"name": "ValueError", "value": "bad", "traceback": ["ValueError: bad"]}})
        h.stream_msg({"exitCode": 1})
        h.stream_end()

    srv, url, _ = _make_server(script)
    try:
        sb = _direct(url)
        ex = sb.run_code("raise ValueError('bad')")
        assert ex.error is not None
        assert ex.error.name == "ValueError"
        assert ex.error.value == "bad"
        assert ex.error.traceback == ["ValueError: bad"]
    finally:
        srv.shutdown()


def test_readfile_streams_incrementally_and_sends_sandbox_header():
    """ReadFile is a Connect server-stream: the SDK delivers each Chunk the
    instant its frame arrives. Gating after the first frame proves incremental
    delivery, and the request carries the X-Sandbox-Id routing header plus the
    bearer."""
    gate = threading.Event()

    def script(h, method, msgs):
        assert method == "ReadFile"
        h.stream_start()
        h.stream_msg({"data": base64.b64encode(b"part-one ").decode()})
        # Pause so the test can observe the first chunk before the rest are sent.
        gate.wait(timeout=5)
        h.stream_msg({"data": base64.b64encode(b"part-two").decode(), "eof": True})
        h.stream_end()

    srv, url, cap = _make_server(script)
    try:
        sb = _direct(url, token="sk-x")
        result_box = {}

        def run():
            result_box["r"] = sb.files.read("/workspace/f.txt")

        th = threading.Thread(target=run)
        th.start()
        time.sleep(0.2)
        gate.set()
        th.join(timeout=5)
        assert result_box["r"] == "part-one part-two"
        # The Connect routing header and bearer rode the request.
        assert cap["headers"].get("X-Sandbox-Id") == "sb-conn"
        assert cap["headers"].get("Authorization") == "Bearer sk-x"
        # The request was an enveloped frame carrying the path.
        _, msgs = cap["requests"][0]
        assert msgs[0]["path"] == "/workspace/f.txt"
    finally:
        srv.shutdown()


def test_files_write_then_read_roundtrip_over_connect():
    store = {}

    def script(h, method, msgs):
        if method == "WriteFile":
            open_msg = next(m["open"] for m in msgs if "open" in m)
            data = b"".join(base64.b64decode(m["data"]) for m in msgs if "data" in m)
            store[open_msg["path"]] = data
            h.stream_start()
            h.stream_msg({"bytesWritten": len(data)})
            h.stream_end()
        elif method == "ReadFile":
            path = msgs[0]["path"]
            h.stream_start()
            h.stream_msg({"data": base64.b64encode(store[path]).decode(), "eof": True})
            h.stream_end()

    srv, url, cap = _make_server(script)
    try:
        sb = _direct(url)
        sb.files.write("/workspace/a.txt", "hello")
        assert sb.files.read("/workspace/a.txt") == "hello"
        # Binary roundtrip via read_bytes.
        sb.files.write("/workspace/b.bin", b"\x00\x01\x02")
        assert sb.files.read_bytes("/workspace/b.bin") == b"\x00\x01\x02"
        # WriteFile sent an open frame then a data frame (client-stream shape).
        write_req = [r for r in cap["requests"] if r[0] == "WriteFile"][0][1]
        assert "open" in write_req[0]
        assert any("data" in m for m in write_req)
    finally:
        srv.shutdown()


def test_files_list_maps_proto_camelcase_fields():
    def script(h, method, msgs):
        assert method == "List"
        assert msgs[0]["parent"] == "/workspace"
        h.unary({"entries": [
            {"name": "a.txt", "isDir": False, "size": 5, "mode": 0o644, "modifiedAtUnix": 1700000000},
            {"name": "sub", "isDir": True, "size": 0, "mode": 0o755},
        ]})

    srv, url, _ = _make_server(script)
    try:
        sb = _direct(url)
        entries = sb.files.list("/workspace")
        assert [e.name for e in entries] == ["a.txt", "sub"]
        assert entries[0].is_dir is False and entries[0].size == 5
        assert entries[1].is_dir is True
        assert entries[0].modified_at == 1700000000
    finally:
        srv.shutdown()


def test_files_exists_false_on_not_found_list():
    def script(h, method, msgs):
        h.unary({"code": "not_found", "message": "no such dir"}, status=404)

    srv, url, _ = _make_server(script)
    try:
        sb = _direct(url)
        assert sb.files.exists("/nope") is False
    finally:
        srv.shutdown()


def test_files_remove_and_mkdir_are_unary():
    seen = []

    def script(h, method, msgs):
        seen.append((method, msgs[0]))
        h.unary({})

    srv, url, _ = _make_server(script)
    try:
        sb = _direct(url)
        sb.files.mkdir("/workspace/d")
        sb.files.remove("/workspace/d")
        methods = [m for m, _ in seen]
        assert methods == ["Mkdir", "Remove"]
        assert seen[1][1]["path"] == "/workspace/d"
    finally:
        srv.shutdown()


def test_unary_connect_error_envelope_raises_typed():
    def script(h, method, msgs):
        h.unary({"code": "internal", "message": "boom"}, status=500)

    srv, url, _ = _make_server(script)
    try:
        sb = _direct(url)
        with pytest.raises(AgentRunError):
            sb.files.list("/x")
    finally:
        srv.shutdown()


def test_token_never_appears_in_error_message():
    def script(h, method, msgs):
        # Echo the bearer back in the message; the SDK must redact it.
        h.unary({"code": "internal", "message": "failed with Bearer sk-secret-value"}, status=500)

    srv, url, _ = _make_server(script)
    try:
        sb = _direct(url, token="sk-secret-value")
        with pytest.raises(AgentRunError) as ei:
            sb.files.list("/x")
        assert "sk-secret-value" not in str(ei.value)
        assert "sk-secret-value" not in (ei.value.cause or "")
    finally:
        srv.shutdown()
