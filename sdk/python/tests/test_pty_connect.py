"""Tests for the interactive PTY carried over the Connect ws transport.

The PTY no longer rides the legacy /v1/pty JSON WebSocket; it speaks the
sandbox.v1.Sandbox.Exec bidi schema over a WebSocket, where every binary ws
message is exactly one Connect-enveloped frame (a 5-byte header then the
protojson payload). The client sends ExecRequest frames (open first, then
stdin/resize), the server sends ExecResponse frames (stdout/stderr/exit).

These tests stand up an in-process fake ws server that speaks the enveloped
protocol and drive both PtyHandle (sync) and AsyncPtyHandle (async) against it.
"""
import asyncio
import base64
import json
import struct
import threading
import time

import pytest

from mitos.pty import PtyHandle

websockets = pytest.importorskip("websockets")

_FLAG_END_STREAM = 0b00000010


def _encode_frame(payload: bytes, end_stream: bool = False) -> bytes:
    flag = _FLAG_END_STREAM if end_stream else 0
    return bytes([flag]) + struct.pack(">I", len(payload)) + payload


def _decode_frame(message: bytes):
    """Split one binary ws message (exactly one enveloped frame) into
    (flag, payload). The host sends one frame per message, so no reassembly is
    needed here."""
    flag = message[0]
    length = struct.unpack(">I", message[1:5])[0]
    payload = message[5 : 5 + length]
    return flag, payload


class _ConnectExecServer:
    """A local ws server that speaks the enveloped ExecRequest/ExecResponse
    protocol: it asserts the first frame is open{pty{size}}, echoes a stdin
    frame's bytes back as a stdout ExecResponse, and on stdin == b"exit\\n"
    sends a terminal exit ExecResponse with the end-stream flag.

    It records every decoded client request so a test can assert the stdin
    frame arrived as a proper enveloped ExecRequest{stdin}."""

    def __init__(self):
        self.port = None
        self._thread = None
        self._loop = None
        self._stop = None
        self.requests = []  # decoded client ExecRequest dicts
        self.open_seen = threading.Event()
        self.subprotocol = None

    def start(self):
        ready = threading.Event()

        def run():
            self._loop = asyncio.new_event_loop()
            asyncio.set_event_loop(self._loop)
            self._stop = self._loop.create_future()

            async def handler(ws):
                self.subprotocol = ws.subprotocol
                first = True
                async for raw in ws:
                    # Every client message is binary; reject str defensively.
                    assert isinstance(raw, (bytes, bytearray)), "expected binary frame"
                    _flag, payload = _decode_frame(bytes(raw))
                    req = json.loads(payload)
                    self.requests.append(req)
                    if first:
                        assert "open" in req, "first frame must be the open"
                        assert req["open"]["pty"]["size"]["cols"] >= 1
                        assert req["open"]["pty"]["size"]["rows"] >= 1
                        self.open_seen.set()
                        first = False
                        continue
                    if "stdin" in req:
                        decoded = base64.b64decode(req["stdin"])
                        if decoded == b"exit\n":
                            exit_msg = {"exit": {"exitCode": 0}}
                            await ws.send(
                                _encode_frame(
                                    json.dumps(exit_msg).encode(), end_stream=True
                                )
                            )
                            return
                        out = {"stdout": req["stdin"]}
                        await ws.send(_encode_frame(json.dumps(out).encode()))

            async def main():
                server = await websockets.serve(
                    handler,
                    "127.0.0.1",
                    0,
                    subprotocols=["connect.sandbox.v1"],
                )
                self.port = server.sockets[0].getsockname()[1]
                ready.set()
                await self._stop

            self._loop.run_until_complete(main())

        self._thread = threading.Thread(target=run, daemon=True)
        self._thread.start()
        ready.wait(5)

    def stop(self):
        if self._loop and self._stop and not self._stop.done():
            self._loop.call_soon_threadsafe(self._stop.set_result, None)

    def url(self, sandbox="sb1"):
        return f"ws://127.0.0.1:{self.port}/sandbox.v1.Sandbox/Exec?sandbox={sandbox}"


def test_pty_connect_echo_and_exit():
    srv = _ConnectExecServer()
    srv.start()
    received = []
    handle = PtyHandle(
        url=srv.url(),
        token=None,
        on_data=lambda b: received.append(b),
        cols=80,
        rows=24,
    )
    # The open frame must reach the server before any input.
    assert srv.open_seen.wait(3), "server never saw the open frame"

    handle.send_input(b"hi-from-test\n")
    deadline = time.time() + 3
    while time.time() < deadline and b"".join(received) != b"hi-from-test\n":
        time.sleep(0.02)
    assert b"".join(received) == b"hi-from-test\n"

    handle.send_input(b"exit\n")
    assert handle.wait(timeout=3) == 0

    # The open frame carried open.pty.size; the stdin frame carried stdin.
    assert "open" in srv.requests[0]
    assert srv.requests[0]["open"]["pty"]["size"] == {"cols": 80, "rows": 24}
    stdin_reqs = [r for r in srv.requests if "stdin" in r]
    assert any(base64.b64decode(r["stdin"]) == b"hi-from-test\n" for r in stdin_reqs)
    assert srv.subprotocol == "connect.sandbox.v1"
    srv.stop()


def test_pty_connect_resize_sends_frame():
    srv = _ConnectExecServer()
    srv.start()
    handle = PtyHandle(
        url=srv.url(),
        token=None,
        on_data=lambda b: None,
        cols=80,
        rows=24,
    )
    assert srv.open_seen.wait(3)
    handle.resize(120, 40)
    # Give the resize frame a moment to arrive before exit.
    deadline = time.time() + 2
    while time.time() < deadline and not any("resize" in r for r in srv.requests):
        time.sleep(0.02)
    handle.send_input(b"exit\n")
    assert handle.wait(timeout=3) == 0
    resize_reqs = [r for r in srv.requests if "resize" in r]
    assert resize_reqs and resize_reqs[0]["resize"] == {"cols": 120, "rows": 40}
    srv.stop()


@pytest.mark.asyncio
async def test_async_pty_connect_echo_and_exit():
    from mitos.pty import AsyncPtyHandle

    srv = _ConnectExecServer()
    srv.start()
    received = []
    handle = await AsyncPtyHandle.connect(
        url=srv.url(),
        token=None,
        on_data=lambda b: received.append(b),
        cols=100,
        rows=30,
    )
    assert srv.open_seen.wait(3)
    await handle.send_input(b"async-hi\n")
    for _ in range(150):
        if b"".join(received) == b"async-hi\n":
            break
        await asyncio.sleep(0.02)
    assert b"".join(received) == b"async-hi\n"
    await handle.send_input(b"exit\n")
    assert await handle.wait() == 0

    assert srv.requests[0]["open"]["pty"]["size"] == {"cols": 100, "rows": 30}
    stdin_reqs = [r for r in srv.requests if "stdin" in r]
    assert any(base64.b64decode(r["stdin"]) == b"async-hi\n" for r in stdin_reqs)
    srv.stop()
