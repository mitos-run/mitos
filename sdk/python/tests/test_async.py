import base64
import json
import struct

import httpx
import pytest

from mitos.aio import AsyncSandbox
from mitos.errors import AgentRunError

_END = 0b00000010


def _frame(payload: bytes, end: bool = False) -> bytes:
    """One Connect enveloped frame: 1-byte flag (0x02 on end-stream), 4-byte
    big-endian length, then the JSON payload."""
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


async def _app(scope, receive, send):
    """Minimal ASGI app speaking the Connect sandbox.v1.Sandbox runtime RPCs
    (ExecStream, ReadFile, WriteFile, List) plus the REST lifecycle routes.

    Connect runtime RPCs are POST /sandbox.v1.Sandbox/<Method>; the streaming
    ones reply with application/connect+json enveloped frames, the unary ones
    with application/json. The REST lifecycle routes stay /v1/*."""
    assert scope["type"] == "http"
    path = scope["path"]
    headers = {k.decode(): v.decode() for k, v in scope.get("headers", [])}
    body = b""
    while True:
        msg = await receive()
        body += msg.get("body", b"")
        if not msg.get("more_body"):
            break

    async def send_json(status: int, payload: dict) -> None:
        data = json.dumps(payload).encode()
        await send({"type": "http.response.start", "status": status,
                    "headers": [(b"content-type", b"application/json")]})
        await send({"type": "http.response.body", "body": data})

    async def send_stream(*frames: bytes) -> None:
        await send({"type": "http.response.start", "status": 200,
                    "headers": [(b"content-type", b"application/connect+json")]})
        await send({"type": "http.response.body", "body": b"".join(frames)})

    method = path.rsplit("/", 1)[-1]

    if path.startswith("/sandbox.v1.Sandbox/"):
        if method == "ExecStream":
            req = _decode_request_frames(body)[0]
            await send_stream(
                _frame(json.dumps({"stdout": base64.b64encode(
                    f"ran:{req['command']}".encode()).decode()}).encode()),
                _frame(json.dumps({"exit": {"exitCode": 0, "execTimeMs": 1.0}}).encode()),
                _frame(json.dumps({}).encode(), end=True),
            )
            return
        if method == "ReadFile":
            await send_stream(
                _frame(json.dumps({"data": base64.b64encode(b"file-body").decode(),
                                   "eof": True}).encode()),
                _frame(json.dumps({}).encode(), end=True),
            )
            return
        if method == "WriteFile":
            await send_stream(
                _frame(json.dumps({"bytesWritten": 9}).encode()),
                _frame(json.dumps({}).encode(), end=True),
            )
            return
        if method == "List":
            # List is unary application/json: the request body is plain JSON.
            # A not-found probe (routes the structured error test) returns the
            # Connect error envelope; otherwise a one-entry listing.
            req = json.loads(body) if body else {}
            if "nope" in req.get("parent", ""):
                await send_json(404, {"code": "not_found", "message": "no route"})
                return
            await send_json(200, {"entries": [
                {"name": "a.txt", "isDir": False, "size": 9, "mode": 0o644,
                 "modifiedAtUnix": 1700000000},
            ]})
            return
        await send_json(501, {"code": "unimplemented", "message": method})
        return

    # REST lifecycle routes.
    req = json.loads(body or b"{}")
    if path == "/v1/set_timeout":
        ts = int(req.get("timeout_seconds", 0))
        if ts > 10**8:
            await send_json(400, {"error": {"code": "timeout_too_large",
                                            "message": "too large", "remediation": "Lower it."}})
        else:
            await send_json(200, {"status": "ok", "deadline_unix": 1_700_000_000 + ts})
        return
    if path == "/v1/pause":
        await send_json(200, {"status": "paused"})
        return
    if path == "/v1/resume":
        await send_json(200, {"status": "running"})
        return
    await send_json(404, {"error": {"code": "not_found", "message": "no route",
                                    "remediation": "Use a documented endpoint."}})


def _async_sandbox():
    transport = httpx.ASGITransport(app=_app)
    client = httpx.AsyncClient(transport=transport, base_url="http://sb")
    return AsyncSandbox(id="sb-1", endpoint="http://sb", token=None, _http=client)


@pytest.mark.asyncio
async def test_async_exec():
    sb = _async_sandbox()
    res = await sb.exec("pytest -x")
    assert res.exit_code == 0
    assert res.stdout == "ran:pytest -x"
    await sb.aclose()


@pytest.mark.asyncio
async def test_async_exec_targets_connect_with_sandbox_header():
    seen = {}

    async def app(scope, receive, send):
        headers = {k.decode(): v.decode() for k, v in scope.get("headers", [])}
        seen["path"] = scope["path"]
        seen["sandbox_id"] = headers.get("x-sandbox-id")
        seen["auth"] = headers.get("authorization")
        while True:
            msg = await receive()
            if not msg.get("more_body"):
                break
        await send({"type": "http.response.start", "status": 200,
                    "headers": [(b"content-type", b"application/connect+json")]})
        await send({"type": "http.response.body", "body":
                    _frame(json.dumps({"exit": {"exitCode": 0}}).encode())
                    + _frame(json.dumps({}).encode(), end=True)})

    transport = httpx.ASGITransport(app=app)
    client = httpx.AsyncClient(transport=transport, base_url="http://sb")
    sb = AsyncSandbox(id="sb-1", endpoint="http://sb", token="sk-async", _http=client)
    await sb.exec("true")
    assert seen["path"] == "/sandbox.v1.Sandbox/ExecStream"
    assert seen["sandbox_id"] == "sb-1"
    assert seen["auth"] == "Bearer sk-async"
    await sb.aclose()


@pytest.mark.asyncio
async def test_async_exec_streams_callbacks():
    sb = _async_sandbox()
    out: list[bytes] = []
    res = await sb.exec("pytest -x", on_stdout=out.append)
    assert b"".join(out) == b"ran:pytest -x"
    assert res.exit_code == 0
    await sb.aclose()


@pytest.mark.asyncio
async def test_async_files_roundtrip():
    sb = _async_sandbox()
    await sb.files.write("/workspace/notes.md", "# findings")
    content = await sb.files.read("/workspace/notes.md")
    assert content == "file-body"
    await sb.aclose()


@pytest.mark.asyncio
async def test_async_files_list_maps_camelcase():
    sb = _async_sandbox()
    entries = await sb.files.list("/workspace")
    assert entries[0].name == "a.txt"
    assert entries[0].is_dir is False
    assert entries[0].size == 9
    assert entries[0].modified_at == 1700000000
    await sb.aclose()


@pytest.mark.asyncio
async def test_async_error_is_structured():
    sb = _async_sandbox()
    with pytest.raises(AgentRunError) as ei:
        await sb.files.list("/nope-route-trigger")  # routes the Connect 404
    assert ei.value.code == "not_found"
    await sb.aclose()


@pytest.mark.asyncio
async def test_async_set_timeout():
    sb = _async_sandbox()
    deadline = await sb.set_timeout(600)
    assert deadline == 1_700_000_000 + 600
    await sb.aclose()


@pytest.mark.asyncio
async def test_async_set_timeout_over_ceiling_rejected():
    from mitos.errors import TimeoutTooLargeError

    sb = _async_sandbox()
    with pytest.raises(TimeoutTooLargeError):
        await sb.set_timeout(10**9)
    await sb.aclose()


@pytest.mark.asyncio
async def test_async_pause_resume():
    sb = _async_sandbox()
    await sb.pause()
    await sb.resume()
    await sb.aclose()
