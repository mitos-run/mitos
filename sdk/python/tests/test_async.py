import json

import httpx
import pytest

from mitos.aio import AsyncSandbox
from mitos.errors import AgentRunError


async def _app(scope, receive, send):
    # Minimal ASGI app reproducing POST /v1/exec and /v1/files/*.
    assert scope["type"] == "http"
    path = scope["path"]
    body = b""
    while True:
        msg = await receive()
        body += msg.get("body", b"")
        if not msg.get("more_body"):
            break
    req = json.loads(body or b"{}")

    if path == "/v1/exec":
        payload = {"exit_code": 0, "stdout": f"ran:{req['command']}", "stderr": "", "exec_time_ms": 1.0}
        status = 200
    elif path == "/v1/files/read":
        payload = {"content": "file-body", "size": 9}
        status = 200
    elif path == "/v1/files/write":
        payload = {"status": "ok"}
        status = 200
    elif path == "/v1/set_timeout":
        ts = int(req.get("timeout_seconds", 0))
        if ts > 10**8:
            payload = {"error": {"code": "timeout_too_large", "message": "too large",
                                 "remediation": "Lower it."}}
            status = 400
        else:
            payload = {"status": "ok", "deadline_unix": 1_700_000_000 + ts,
                       "timeout_seconds": ts}
            status = 200
    elif path == "/v1/pause":
        payload = {"status": "paused"}
        status = 200
    elif path == "/v1/resume":
        payload = {"status": "running"}
        status = 200
    else:
        payload = {"error": {"code": "not_found", "message": "no route",
                             "remediation": "Use a documented endpoint."}}
        status = 404

    data = json.dumps(payload).encode()
    await send({"type": "http.response.start", "status": status,
                "headers": [(b"content-type", b"application/json")]})
    await send({"type": "http.response.body", "body": data})


def _async_sandbox():
    transport = httpx.ASGITransport(app=_app)
    client = httpx.AsyncClient(transport=transport, base_url="http://sb")
    return AsyncSandbox(id="sb-1", endpoint="sb", token=None, _http=client)


@pytest.mark.asyncio
async def test_async_exec():
    sb = _async_sandbox()
    res = await sb.exec("pytest -x")
    assert res.exit_code == 0
    assert res.stdout == "ran:pytest -x"
    await sb.aclose()


@pytest.mark.asyncio
async def test_async_files_roundtrip():
    sb = _async_sandbox()
    await sb.files.write("/workspace/notes.md", "# findings")
    content = await sb.files.read("/workspace/notes.md")
    assert content == "file-body"
    await sb.aclose()


@pytest.mark.asyncio
async def test_async_error_is_structured():
    sb = _async_sandbox()
    with pytest.raises(AgentRunError) as ei:
        await sb.files.list("/nope-route-trigger")  # hits the 404 default route
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
