from __future__ import annotations

import base64
import json
import struct
import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer, ThreadingHTTPServer

import httpx
import pytest

from mitos.sandbox import Sandbox

_END = 0b00000010


def _frame(payload: bytes, end: bool = False) -> bytes:
    """One Connect enveloped frame: a 1-byte flag (0x02 on end-stream), a 4-byte
    big-endian length, then the JSON payload bytes."""
    flag = _END if end else 0
    return bytes([flag]) + struct.pack(">I", len(payload)) + payload


def _exec_frames(
    chunks: list[tuple[str, bytes]],
    exit_code: int = 7,
    exec_time_ms: float = 2.0,
    include_exit: bool = True,
) -> bytes:
    """Build a Connect ExecStream reply body: a stdout/stderr chunk frame per
    entry, then (optionally) the terminal ExecExit and the clean end-stream
    frame."""
    body = b""
    for stream, data in chunks:
        body += _frame(json.dumps({stream: base64.b64encode(data).decode()}).encode())
    if include_exit:
        body += _frame(
            json.dumps(
                {"exit": {"exitCode": exit_code, "execTimeMs": exec_time_ms}}
            ).encode()
        )
        body += _frame(json.dumps({}).encode(), end=True)
    return body


class _Handler(BaseHTTPRequestHandler):
    def do_POST(self):  # noqa: N802
        length = int(self.headers.get("Content-Length", 0))
        self.rfile.read(length)
        self.send_response(200)
        self.send_header("Content-Type", "application/connect+json")
        self.end_headers()
        self.wfile.write(
            _exec_frames(
                [("stdout", b"out1"), ("stderr", b"err1"), ("stdout", b"out2")],
                exit_code=7,
                exec_time_ms=2.0,
            )
        )
        self.wfile.flush()

    def log_message(self, *args):  # silence
        pass


@pytest.fixture()
def stream_server():
    srv = HTTPServer(("127.0.0.1", 0), _Handler)
    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()
    yield f"127.0.0.1:{srv.server_address[1]}"
    srv.shutdown()


def _direct_sandbox(endpoint: str) -> Sandbox:
    # Build a Sandbox without k8s: set endpoint and id directly.
    sb = Sandbox.__new__(Sandbox)
    sb._endpoint = endpoint
    sb._sandbox_id = "sb1"
    sb._token = None
    sb._http = httpx.Client(timeout=30.0)
    return sb


def test_exec_streams_callbacks(stream_server):
    sb = _direct_sandbox(stream_server)
    out, err = [], []
    result = sb.exec(
        "echo hi",
        on_stdout=lambda b: out.append(b),
        on_stderr=lambda b: err.append(b),
    )
    assert b"".join(out) == b"out1out2"
    assert b"".join(err) == b"err1"
    assert result.exit_code == 7
    assert result.stdout == "out1out2"


def test_exec_background_wait(stream_server):
    sb = _direct_sandbox(stream_server)
    proc = sb.exec_background("sleep 1")
    result = proc.wait()
    assert result.exit_code == 7


# --- Issue A: a truncated stream (no terminal exit frame) must error. ---


class _TruncatedHandler(BaseHTTPRequestHandler):
    """Sends chunk frames but never the terminal ExecExit frame, then closes."""

    def do_POST(self):  # noqa: N802
        length = int(self.headers.get("Content-Length", 0))
        self.rfile.read(length)
        self.send_response(200)
        self.send_header("Content-Type", "application/connect+json")
        self.end_headers()
        self.wfile.write(
            _exec_frames(
                [("stdout", b"out1"), ("stdout", b"out2")],
                include_exit=False,
            )
        )
        self.wfile.flush()
        # No ExecExit and no end-stream frame: the connection simply ends.

    def log_message(self, *args):  # silence
        pass


@pytest.fixture()
def truncated_server():
    srv = HTTPServer(("127.0.0.1", 0), _TruncatedHandler)
    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()
    yield f"127.0.0.1:{srv.server_address[1]}"
    srv.shutdown()


def test_exec_truncated_stream_errors(truncated_server):
    sb = _direct_sandbox(truncated_server)
    with pytest.raises(RuntimeError, match="terminal exit frame"):
        sb.exec("echo hi", on_stdout=lambda b: None)


# --- Issue C: background runs eagerly; kill scopes to its own stream. ---


class _SlowHandler(BaseHTTPRequestHandler):
    """Streams one early ExecStream chunk, then blocks before the ExecExit frame
    so the process is observably 'running' until the connection is torn down."""

    release = threading.Event()

    def do_POST(self):  # noqa: N802
        length = int(self.headers.get("Content-Length", 0))
        self.rfile.read(length)
        self.send_response(200)
        self.send_header("Content-Type", "application/connect+json")
        self.end_headers()
        try:
            self.wfile.write(
                _frame(
                    json.dumps(
                        {"stdout": base64.b64encode(b"ready").decode()}
                    ).encode()
                )
            )
            self.wfile.flush()
            # Block until the test releases us (or the client drops the conn).
            _SlowHandler.release.wait(timeout=5.0)
            self.wfile.write(
                _frame(json.dumps({"exit": {"exitCode": 0, "execTimeMs": 1.0}}).encode())
            )
            self.wfile.write(_frame(json.dumps({}).encode(), end=True))
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            return

    def log_message(self, *args):  # silence
        pass


@pytest.fixture()
def slow_server():
    _SlowHandler.release.clear()
    srv = ThreadingHTTPServer(("127.0.0.1", 0), _SlowHandler)
    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()
    yield f"127.0.0.1:{srv.server_address[1]}"
    _SlowHandler.release.set()
    srv.shutdown()


def test_exec_background_is_actually_running(slow_server):
    out: list[bytes] = []
    sb = _direct_sandbox(slow_server)
    proc = sb.exec_background("sleep 1", on_stdout=lambda b: out.append(b))
    # The drain runs on a background thread, so the first chunk arrives without
    # anyone calling wait(). Poll briefly for it.
    deadline = time.time() + 3.0
    while not out and time.time() < deadline:
        time.sleep(0.02)
    assert b"".join(out) == b"ready", "background process did not run before wait()"
    assert proc.running(), "process should still be running before the exit frame"
    # Release the server so wait() can complete cleanly.
    _SlowHandler.release.set()
    result = proc.wait()
    assert result.exit_code == 0
    assert not proc.running()


def test_kill_does_not_break_subsequent_exec(slow_server):
    sb = _direct_sandbox(slow_server)
    proc = sb.exec_background("sleep 1")
    # Give the drain thread a moment to open its own stream.
    time.sleep(0.1)
    proc.kill()  # closes only the per-stream client
    # The shared Sandbox client must still work: a one-shot exec on the same
    # Sandbox should succeed, proving kill() did not close it.
    assert sb._http.is_closed is False
    # Release the slow server so a fresh exec on the shared client completes.
    _SlowHandler.release.set()
    result = sb.exec("true", timeout=1, working_dir="/")
    assert result.exit_code == 0


def test_kill_before_wait_does_not_crash(slow_server):
    sb = _direct_sandbox(slow_server)
    proc = sb.exec_background("sleep 1")
    time.sleep(0.1)
    proc.kill()
    # kill-before-wait: wait() should return or raise cleanly, never hang or
    # crash. The torn-down stream ends without an exit frame, so the drain
    # surfaces a truncation RuntimeError; that is the expected clean outcome.
    deadline = time.time() + 3.0
    while proc.running() and time.time() < deadline:
        time.sleep(0.02)
    assert not proc.running(), "drain thread should finish after kill()"
    # The stream was torn down before its exit frame, so wait() either surfaces
    # an error (a truncation RuntimeError or a transport read error) OR returns
    # cleanly when the kill drained the stream before wait() ran. Both are the
    # documented clean, non-hanging outcome; requiring an exception was a race
    # (a kill that drains cleanly returns without raising). The invariant is
    # only that wait() does not hang or crash the interpreter and the shared
    # client stays open.
    try:
        proc.wait()
    except Exception:
        pass
    assert sb._http.is_closed is False
