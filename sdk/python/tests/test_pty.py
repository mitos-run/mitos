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
    length = struct.unpack(">I", message[1:5])[0]
    return message[0], message[5 : 5 + length]


class _EchoServer:
    """A local ws server speaking the Connect-enveloped Exec protocol: it echoes
    a stdin frame's bytes back as a stdout ExecResponse and exits on stdin
    'exit\\n' with a terminal exit frame, mimicking forkd's ws Exec transport."""

    def __init__(self):
        self.port = None
        self._thread = None
        self._loop = None
        self._stop = None

    def start(self):
        ready = threading.Event()

        def run():
            self._loop = asyncio.new_event_loop()
            asyncio.set_event_loop(self._loop)
            self._stop = self._loop.create_future()

            async def handler(ws):
                first = True
                async for raw in ws:
                    _flag, payload = _decode_frame(bytes(raw))
                    req = json.loads(payload)
                    if first:
                        # The first frame is always the open.
                        assert "open" in req
                        first = False
                        continue
                    if "stdin" in req:
                        data = req["stdin"]
                        decoded = base64.b64decode(data) if data else b""
                        if decoded == b"exit\n":
                            await ws.send(
                                _encode_frame(
                                    json.dumps({"exit": {"exitCode": 0}}).encode(),
                                    end_stream=True,
                                )
                            )
                            return
                        await ws.send(
                            _encode_frame(json.dumps({"stdout": data}).encode())
                        )

            async def main():
                # Negotiate the same subprotocol the ws Exec transport advertises
                # so the websocket-client handshake matches the real server.
                server = await websockets.serve(
                    handler, "127.0.0.1", 0, subprotocols=["connect.sandbox.v1"]
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

    def url(self):
        return f"ws://127.0.0.1:{self.port}/sandbox.v1.Sandbox/Exec?sandbox=sb1"


def test_pty_echo_and_exit():
    srv = _EchoServer()
    srv.start()
    received = []
    handle = PtyHandle(
        url=srv.url(),
        token=None,
        on_data=lambda b: received.append(b),
        cols=80,
        rows=24,
    )
    handle.send_input(b"hi-from-test\n")
    deadline = time.time() + 3
    while time.time() < deadline and b"".join(received) != b"hi-from-test\n":
        time.sleep(0.02)
    assert b"".join(received) == b"hi-from-test\n"

    handle.send_input(b"exit\n")
    assert handle.wait(timeout=3) == 0
    srv.stop()


def test_pty_resize_sends_frame():
    srv = _EchoServer()
    srv.start()
    handle = PtyHandle(
        url=srv.url(),
        token=None,
        on_data=lambda b: None,
        cols=80,
        rows=24,
    )
    # Resize should not raise; the echo server ignores it.
    handle.resize(120, 40)
    handle.send_input(b"exit\n")
    assert handle.wait(timeout=3) == 0
    srv.stop()


@pytest.mark.asyncio
async def test_async_pty_echo_and_exit():
    from mitos.pty import AsyncPtyHandle

    srv = _EchoServer()
    srv.start()
    received = []
    handle = await AsyncPtyHandle.connect(
        url=srv.url(),
        token=None,
        on_data=lambda b: received.append(b),
        cols=80,
        rows=24,
    )
    await handle.send_input(b"async-hi\n")
    for _ in range(150):
        if b"".join(received) == b"async-hi\n":
            break
        await asyncio.sleep(0.02)
    assert b"".join(received) == b"async-hi\n"
    await handle.send_input(b"exit\n")
    assert await handle.wait() == 0
    srv.stop()
