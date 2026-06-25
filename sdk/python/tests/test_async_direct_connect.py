"""Async direct-mode runtime transport over the Connect sandbox.v1.Sandbox
service (issue #24).

The async ``AsyncDirectSandbox`` (and the async k8s ``AsyncSandbox``) runtime
calls speak the Connect ``sandbox.v1.Sandbox`` service at
``/sandbox.v1.Sandbox/<Method>`` over the AsyncConnectClient, mirroring the sync
DirectSandbox:

  - exec     -> ExecStream     (server-streaming over HTTP/1.1)
  - run_code -> RunCodeStream  (server-streaming over HTTP/1.1)
  - files.*  -> ReadFile / WriteFile / List / Mkdir / Remove

These tests stand up a fake Connect server as an httpx ASGI app (httpx
ASGITransport buffers the response, so the enveloped frames all arrive and the
incremental decode still drives the fold). They assert the wire the SDK sends
(the Connect envelope, the X-Sandbox-Id routing header, proto-JSON camelCase),
and that the public async return types are unchanged.
"""
from __future__ import annotations

import base64
import json
import struct

import httpx
import pytest

from mitos.aio import AsyncDirectSandbox
from mitos.errors import AgentRunError, ExecutionDeadlineError

_END = 0b00000010


def _frame(payload: bytes, end: bool = False) -> bytes:
    flag = _END if end else 0
    return bytes([flag]) + struct.pack(">I", len(payload)) + payload


def _decode_request_frames(body: bytes) -> list[dict]:
    out, i = [], 0
    while i + 5 <= len(body):
        length = struct.unpack(">I", body[i + 1 : i + 5])[0]
        payload = body[i + 5 : i + 5 + length]
        i += 5 + length
        if payload:
            out.append(json.loads(payload))
    return out


def _make_app(script):
    """script(method, request_msgs, captured) -> (status, content_type, body).

    A streaming reply is bytes of enveloped frames with content_type
    application/connect+json; a unary reply is JSON bytes with application/json.
    """
    captured = {"headers": {}, "requests": []}

    async def app(scope, receive, send):
        assert scope["type"] == "http"
        headers = {k.decode(): v.decode() for k, v in scope.get("headers", [])}
        captured["headers"] = headers
        body = b""
        while True:
            msg = await receive()
            body += msg.get("body", b"")
            if not msg.get("more_body"):
                break
        method = scope["path"].rsplit("/", 1)[-1]
        ct = headers.get("content-type", "")
        if ct.startswith("application/connect"):
            msgs = _decode_request_frames(body)
        else:
            msgs = [json.loads(body)] if body else [{}]
        captured["requests"].append((method, msgs))
        status, content_type, out = script(method, msgs, captured)
        await send({"type": "http.response.start", "status": status,
                    "headers": [(b"content-type", content_type.encode())]})
        await send({"type": "http.response.body", "body": out})

    return app, captured


def _stream(*frames: bytes) -> tuple[int, str, bytes]:
    return 200, "application/connect+json", b"".join(frames)


def _unary(obj: dict, status: int = 200) -> tuple[int, str, bytes]:
    return status, "application/json", json.dumps(obj).encode()


def _direct(app, token=None) -> AsyncDirectSandbox:
    client = httpx.AsyncClient(transport=httpx.ASGITransport(app=app),
                              base_url="http://sb")
    return AsyncDirectSandbox(
        id="sb-async", template="python", endpoint="http://sb",
        server_url="http://sb", fork_time_ms=0.5, api_key=token, _http=client,
    )


@pytest.mark.asyncio
async def test_async_exec_folds_exit_and_sends_header():
    def script(method, msgs, cap):
        assert method == "ExecStream"
        return _stream(
            _frame(json.dumps({"stdout": base64.b64encode(b"first ").decode()}).encode()),
            _frame(json.dumps({"stdout": base64.b64encode(b"second\n").decode()}).encode()),
            _frame(json.dumps({"stderr": base64.b64encode(b"warn\n").decode()}).encode()),
            _frame(json.dumps({"exit": {"exitCode": 3, "execTimeMs": 4.0}}).encode()),
            _frame(json.dumps({}).encode(), end=True),
        )

    app, cap = _make_app(script)
    sb = _direct(app, token="sk-x")
    r = await sb.exec("echo first second")
    assert r.exit_code == 3
    assert r.stdout == "first second\n"
    assert r.stderr == "warn\n"
    assert r.exec_time_ms == 4.0
    assert cap["headers"].get("x-sandbox-id") == "sb-async"
    assert cap["headers"].get("authorization") == "Bearer sk-x"
    _, msgs = cap["requests"][0]
    assert msgs[0]["command"] == "echo first second"
    assert "timeoutSeconds" in msgs[0]
    await sb._http.aclose()


@pytest.mark.asyncio
async def test_async_exec_deadline_maps_to_typed_error():
    def script(method, msgs, cap):
        return _stream(
            _frame(json.dumps({"exit": {"exitCode": 124}}).encode()),
            _frame(json.dumps({}).encode(), end=True),
        )

    app, _ = _make_app(script)
    sb = _direct(app)
    with pytest.raises(ExecutionDeadlineError):
        await sb.exec("sleep 999", timeout=1)
    await sb._http.aclose()


@pytest.mark.asyncio
async def test_async_exec_spawn_error_raises():
    def script(method, msgs, cap):
        return _stream(
            _frame(json.dumps({"exit": {"exitCode": 1, "error": "no such command"}}).encode()),
            _frame(json.dumps({}).encode(), end=True),
        )

    app, _ = _make_app(script)
    sb = _direct(app)
    with pytest.raises(AgentRunError) as ei:
        await sb.exec("bogus")
    assert ei.value.code == "exec_failed"
    await sb._http.aclose()


@pytest.mark.asyncio
async def test_async_run_code_folds_rich_result_and_callbacks():
    def script(method, msgs, cap):
        assert method == "RunCodeStream"
        assert msgs[0]["code"] == "print(1+1)\n2"
        return _stream(
            _frame(json.dumps({"stdout": base64.b64encode(b"out\n").decode()}).encode()),
            _frame(json.dumps({"result": {"text": "42", "data": {
                "text/plain": base64.b64encode(b"42").decode(),
                "image/png": base64.b64encode(b"aGVsbG8=").decode(),
            }}}).encode()),
            _frame(json.dumps({"exitCode": 0}).encode()),
            _frame(json.dumps({}).encode(), end=True),
        )

    app, _ = _make_app(script)
    sb = _direct(app)
    seen_out, seen_res = [], []
    ex = await sb.run_code("print(1+1)\n2", on_stdout=seen_out.append,
                           on_result=seen_res.append)
    assert ex.text == "42"
    assert ex.logs["stdout"] == ["out\n"]
    assert ex.results[0].png == "aGVsbG8="
    assert ex.results[0].text == "42"
    assert seen_out == ["out\n"]
    assert len(seen_res) == 1
    await sb._http.aclose()


@pytest.mark.asyncio
async def test_async_run_code_structured_error():
    def script(method, msgs, cap):
        return _stream(
            _frame(json.dumps({"error": {"name": "ValueError", "value": "bad",
                                         "traceback": ["ValueError: bad"]}}).encode()),
            _frame(json.dumps({"exitCode": 1}).encode()),
            _frame(json.dumps({}).encode(), end=True),
        )

    app, _ = _make_app(script)
    sb = _direct(app)
    ex = await sb.run_code("raise ValueError('bad')")
    assert ex.error is not None
    assert ex.error.name == "ValueError"
    assert ex.error.value == "bad"
    assert ex.error.traceback == ["ValueError: bad"]
    await sb._http.aclose()


@pytest.mark.asyncio
async def test_async_files_write_then_read_roundtrip():
    store: dict[str, bytes] = {}

    def script(method, msgs, cap):
        if method == "WriteFile":
            open_msg = next(m["open"] for m in msgs if "open" in m)
            data = b"".join(base64.b64decode(m["data"]) for m in msgs if "data" in m)
            store[open_msg["path"]] = data
            return _stream(
                _frame(json.dumps({"bytesWritten": len(data)}).encode()),
                _frame(json.dumps({}).encode(), end=True),
            )
        if method == "ReadFile":
            path = msgs[0]["path"]
            return _stream(
                _frame(json.dumps({"data": base64.b64encode(store[path]).decode(),
                                   "eof": True}).encode()),
                _frame(json.dumps({}).encode(), end=True),
            )
        return _unary({})

    app, cap = _make_app(script)
    sb = _direct(app)
    await sb.files.write("/workspace/a.txt", "hello")
    assert await sb.files.read("/workspace/a.txt") == "hello"
    await sb.files.write("/workspace/b.bin", b"\x00\x01\x02")
    assert await sb.files.read_bytes("/workspace/b.bin") == b"\x00\x01\x02"
    write_req = [r for r in cap["requests"] if r[0] == "WriteFile"][0][1]
    assert "open" in write_req[0]
    assert any("data" in m for m in write_req)
    await sb._http.aclose()


@pytest.mark.asyncio
async def test_async_files_list_maps_camelcase():
    def script(method, msgs, cap):
        assert method == "List"
        assert msgs[0]["parent"] == "/workspace"
        return _unary({"entries": [
            {"name": "a.txt", "isDir": False, "size": 5, "mode": 0o644,
             "modifiedAtUnix": 1700000000},
            {"name": "sub", "isDir": True, "size": 0, "mode": 0o755},
        ]})

    app, _ = _make_app(script)
    sb = _direct(app)
    entries = await sb.files.list("/workspace")
    assert [e.name for e in entries] == ["a.txt", "sub"]
    assert entries[0].is_dir is False and entries[0].size == 5
    assert entries[1].is_dir is True
    assert entries[0].modified_at == 1700000000
    await sb._http.aclose()


@pytest.mark.asyncio
async def test_async_files_mkdir_and_remove_are_unary():
    seen = []

    def script(method, msgs, cap):
        seen.append((method, msgs[0]))
        return _unary({})

    app, _ = _make_app(script)
    sb = _direct(app)
    await sb.files.mkdir("/workspace/d")
    await sb.files.remove("/workspace/d")
    assert [m for m, _ in seen] == ["Mkdir", "Remove"]
    assert seen[1][1]["path"] == "/workspace/d"
    await sb._http.aclose()


@pytest.mark.asyncio
async def test_async_token_never_appears_in_error_message():
    def script(method, msgs, cap):
        return _unary({"code": "internal",
                       "message": "failed with Bearer sk-secret-value"}, status=500)

    app, _ = _make_app(script)
    sb = _direct(app, token="sk-secret-value")
    with pytest.raises(AgentRunError) as ei:
        await sb.files.list("/x")
    assert "sk-secret-value" not in str(ei.value)
    assert "sk-secret-value" not in (ei.value.cause or "")
    await sb._http.aclose()
