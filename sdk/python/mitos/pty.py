from __future__ import annotations

import asyncio
import base64
import json
import threading
from typing import Callable, Optional

import websocket  # the websocket-client package
from websocket import WebSocketApp

from mitos._connect import _FLAG_END_STREAM, _FrameDecoder, _encode_frame

# NOTE: the async 'websockets' package is an OPTIONAL extra (mitos[async]); it is
# imported lazily inside AsyncPtyHandle.connect so that importing this module (and
# therefore mitos.sandbox) never fails when only the synchronous path is used and
# the extra is not installed.

# The ws subprotocol the host advertises for the Connect-over-WebSocket bidi
# Exec transport. The legacy JSON PTY route used "mitos.pty.v1"; the Connect
# transport negotiates this instead.
_EXEC_WS_SUBPROTOCOL = "connect.sandbox.v1"


def _open_request(cols: int, rows: int) -> bytes:
    """The first enveloped ExecRequest frame: open an interactive pty with the
    initial window size. The host reads cols/rows from this open, not from the
    URL query."""
    msg = {"open": {"pty": {"size": {"cols": int(cols), "rows": int(rows)}}}}
    return _encode_frame(json.dumps(msg).encode())


def _stdin_request(data: bytes) -> bytes:
    """An enveloped ExecRequest{stdin} frame carrying raw keystroke bytes
    (base64-encoded per the proto-JSON bytes encoding)."""
    msg = {"stdin": base64.b64encode(data).decode("ascii")}
    return _encode_frame(json.dumps(msg).encode())


def _resize_request(cols: int, rows: int) -> bytes:
    """An enveloped ExecRequest{resize} frame."""
    msg = {"resize": {"cols": int(cols), "rows": int(rows)}}
    return _encode_frame(json.dumps(msg).encode())


def _exit_code_from(exit_obj: dict) -> int:
    """Read the exit code from an ExecResponse exit object. The proto-JSON field
    is camelCase ``exitCode``; ``exit_code`` is accepted defensively."""
    if "exitCode" in exit_obj:
        return int(exit_obj.get("exitCode") or 0)
    return int(exit_obj.get("exit_code", 0) or 0)


class PtyHandle:
    """A live interactive pseudo-terminal in a sandbox, mirroring E2B's
    sandbox.pty handle. Output bytes are delivered to on_data on a background
    reader thread; send_input/resize write frames to the guest, kill() force
    closes, and wait() blocks for the exit code.

    The transport is a WebSocket carrying the Connect ``sandbox.v1.Sandbox.Exec``
    bidi schema: each binary ws message is one Connect-enveloped frame (a 5-byte
    header then the protojson payload). The client sends ExecRequest frames (the
    open first, then stdin/resize); the server sends ExecResponse frames
    (stdout/stderr/exit). The connection negotiates the ``connect.sandbox.v1``
    subprotocol and is gated by the per-sandbox bearer token, sent in the
    Authorization header and never logged.
    """

    def __init__(
        self,
        url: str,
        token: Optional[str],
        on_data: Callable[[bytes], None],
        cols: int = 80,
        rows: int = 24,
    ):
        self._on_data = on_data
        self._cols = cols
        self._rows = rows
        self._exit_code: Optional[int] = None
        self._done = threading.Event()
        self._open = threading.Event()
        self._lock = threading.Lock()
        # The host sends one enveloped frame per binary ws message, but decode
        # through the shared incremental decoder so a frame split across reads is
        # still reassembled correctly.
        self._decoder = _FrameDecoder()

        header = [f"Authorization: Bearer {token}"] if token else []
        self._ws = WebSocketApp(
            url,
            header=header,
            subprotocols=[_EXEC_WS_SUBPROTOCOL],
            on_open=self._handle_open,
            on_data=self._handle_data,
            on_close=self._handle_close,
            on_error=self._handle_error,
        )
        self._thread = threading.Thread(target=self._ws.run_forever, daemon=True)
        self._thread.start()
        # Block until the socket is open so the first send_input is not dropped.
        self._open.wait(timeout=10)

    def _handle_open(self, ws) -> None:  # noqa: ANN001
        # Send the open ExecRequest FIRST, before any input, so the host can
        # build the guest Exec stream with the requested window size.
        ws.send(_open_request(self._cols, self._rows), opcode=websocket.ABNF.OPCODE_BINARY)
        self._open.set()

    def _handle_data(self, ws, message, data_type, cont) -> None:  # noqa: ANN001
        # on_data fires for both text and binary frames; the transport is binary
        # only, so coerce a str frame (should not happen) to bytes.
        if isinstance(message, str):
            message = message.encode()
        for flag, payload in self._decoder.feed(message):
            self._consume(ws, flag, payload)

    def _consume(self, ws, flag: int, payload: bytes) -> None:  # noqa: ANN001
        if payload:
            resp = json.loads(payload)
            if "stdout" in resp and resp["stdout"]:
                self._on_data(base64.b64decode(resp["stdout"]))
            elif "stderr" in resp and resp["stderr"]:
                self._on_data(base64.b64decode(resp["stderr"]))
            elif "exit" in resp:
                self._exit_code = _exit_code_from(resp["exit"] or {})
        if flag & _FLAG_END_STREAM:
            if self._exit_code is None:
                self._exit_code = 0
            self._done.set()
            ws.close()

    def _handle_close(self, ws, status_code, msg) -> None:  # noqa: ANN001
        if self._exit_code is None:
            self._exit_code = -1
        self._done.set()

    def _handle_error(self, ws, error) -> None:  # noqa: ANN001
        # Error text never carries the token; record nothing sensitive.
        if self._exit_code is None:
            self._exit_code = -1
        self._done.set()

    def _send(self, frame: bytes) -> None:
        with self._lock:
            self._ws.send(frame, opcode=websocket.ABNF.OPCODE_BINARY)

    def send_input(self, data: bytes) -> None:
        """Send raw keystroke bytes to the shell."""
        self._send(_stdin_request(data))

    def resize(self, cols: int, rows: int) -> None:
        """Resize the terminal window (TIOCSWINSZ in the guest, then SIGWINCH)."""
        self._send(_resize_request(cols, rows))

    def kill(self) -> None:
        """Force-close the terminal. The guest kills the shell process group
        when the connection drops."""
        try:
            self._ws.close()
        finally:
            self._done.set()

    def wait(self, timeout: Optional[float] = None) -> int:
        """Block until the shell exits and return its exit code (or -1 if the
        connection dropped before a terminal exit frame)."""
        self._done.wait(timeout=timeout)
        return self._exit_code if self._exit_code is not None else -1


class AsyncPtyHandle:
    """Async counterpart to PtyHandle. Output is delivered to on_data from a
    background asyncio task; send_input/resize are coroutines, wait() awaits the
    exit code.

    Transport is a WebSocket carrying the Connect ``sandbox.v1.Sandbox.Exec``
    bidi schema (one enveloped frame per binary ws message), negotiating the
    ``connect.sandbox.v1`` subprotocol and gated by the bearer token."""

    def __init__(self, ws, on_data: Callable[[bytes], None]):
        self._ws = ws
        self._on_data = on_data
        self._exit_code: Optional[int] = None
        self._done = asyncio.Event()
        self._decoder = _FrameDecoder()
        self._reader = asyncio.create_task(self._read_loop())

    @classmethod
    async def connect(
        cls,
        url: str,
        token: Optional[str],
        on_data: Callable[[bytes], None],
        cols: int = 80,
        rows: int = 24,
    ) -> "AsyncPtyHandle":
        headers = [("Authorization", f"Bearer {token}")] if token else []
        try:
            import websockets  # the async package, the mitos[async] extra
        except ImportError as exc:  # pragma: no cover
            raise ImportError(
                "the async PTY requires the 'websockets' package; install mitos[async]. "
                "The synchronous pty.create() path needs only websocket-client."
            ) from exc
        ws = await websockets.connect(
            url,
            additional_headers=headers,
            subprotocols=[_EXEC_WS_SUBPROTOCOL],
        )
        # Send the open ExecRequest FIRST so the host builds the guest Exec
        # stream with the requested window size before any input arrives.
        await ws.send(_open_request(cols, rows))
        return cls(ws, on_data)

    async def _read_loop(self) -> None:
        try:
            async for raw in self._ws:
                if isinstance(raw, str):
                    raw = raw.encode()
                for flag, payload in self._decoder.feed(raw):
                    if payload:
                        resp = json.loads(payload)
                        if "stdout" in resp and resp["stdout"]:
                            self._on_data(base64.b64decode(resp["stdout"]))
                        elif "stderr" in resp and resp["stderr"]:
                            self._on_data(base64.b64decode(resp["stderr"]))
                        elif "exit" in resp:
                            self._exit_code = _exit_code_from(resp["exit"] or {})
                    if flag & _FLAG_END_STREAM:
                        if self._exit_code is None:
                            self._exit_code = 0
                        return
        finally:
            if self._exit_code is None:
                self._exit_code = -1
            self._done.set()
            await self._ws.close()

    async def send_input(self, data: bytes) -> None:
        await self._ws.send(_stdin_request(data))

    async def resize(self, cols: int, rows: int) -> None:
        await self._ws.send(_resize_request(cols, rows))

    async def kill(self) -> None:
        await self._ws.close()
        self._done.set()

    async def wait(self) -> int:
        await self._done.wait()
        return self._exit_code if self._exit_code is not None else -1
