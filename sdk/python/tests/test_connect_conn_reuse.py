"""A streaming Connect RPC must return its connection to the pool.

HTTP/1.1 can only reuse a connection whose response body was consumed to EOF. The
Connect stream reader used to stop the instant it saw the terminal end-stream
frame, leaving the body short of EOF, so httpx closed the socket rather than
pooling it. Every exec then paid a fresh TCP and TLS handshake: measured against
the hosted API, roughly 70 ms per call, on the single hottest path an agent has
(one exec per tool call).

These tests count TCP accepts on an in-process HTTP/1.1 keep-alive server, which
is the only observation that actually distinguishes a pooled connection from a
re-established one.
"""
from __future__ import annotations

import base64
import json
import struct
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import httpx
import pytest

from mitos._connect import AsyncConnectClient, ConnectClient
from mitos._runtime import parse_run_code_connect

_END = 0b00000010


def _frame(payload: bytes, end: bool = False) -> bytes:
    flag = _END if end else 0
    return bytes([flag]) + struct.pack(">I", len(payload)) + payload


def _body() -> bytes:
    return _frame(json.dumps({"stdout": "1\n"}).encode()) + _frame(b"{}", end=True)


class _Handler(BaseHTTPRequestHandler):
    # Without this the server answers HTTP/1.0 and closes every connection, so the
    # test would pass for the wrong reason.
    protocol_version = "HTTP/1.1"

    def do_POST(self) -> None:
        length = int(self.headers.get("Content-Length") or 0)
        if length:
            self.rfile.read(length)
        body = _body()
        self.send_response(200)
        self.send_header("Content-Type", "application/connect+json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args) -> None:
        pass


class _CountingServer(ThreadingHTTPServer):
    """Counts accepted TCP connections."""

    accepts = 0

    def get_request(self):
        self.accepts += 1
        return super().get_request()


@pytest.fixture()
def server():
    srv = _CountingServer(("127.0.0.1", 0), _Handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    yield srv
    srv.shutdown()
    srv.server_close()


def _drain(client: ConnectClient) -> list[dict]:
    return list(client.server_stream("RunCodeStream", {"code": "print(1)"}))


def test_streaming_rpc_reuses_the_pooled_connection(server):
    base = "http://127.0.0.1:%d" % server.server_address[1]
    with httpx.Client() as http:
        cc = ConnectClient(http, base, "sb-1", None)
        assert _drain(cc) == [{"stdout": "1\n"}]
        assert _drain(cc) == [{"stdout": "1\n"}]
        assert _drain(cc) == [{"stdout": "1\n"}]

    # Three streaming RPCs, one connection. Before the drain fix this was three.
    assert server.accepts == 1, (
        "each streaming RPC opened a new TCP connection: the response body was not "
        "consumed to EOF, so httpx could not pool it"
    )


def test_streamed_frames_are_still_delivered_before_the_terminal_frame(server):
    """Draining must not swallow payloads or the end-stream semantics."""
    base = "http://127.0.0.1:%d" % server.server_address[1]
    with httpx.Client() as http:
        cc = ConnectClient(http, base, "sb-1", None)
        assert _drain(cc) == [{"stdout": "1\n"}]


def _run_code_body() -> bytes:
    """stdout, then the terminal exitCode message frame, then the end-stream frame."""
    return (
        _frame(json.dumps({"stdout": base64.b64encode(b"1\n").decode()}).encode())
        + _frame(json.dumps({"exitCode": 0}).encode())
        + _frame(b"{}", end=True)
    )


class _RunCodeHandler(_Handler):
    def do_POST(self) -> None:
        length = int(self.headers.get("Content-Length") or 0)
        if length:
            self.rfile.read(length)
        body = _run_code_body()
        self.send_response(200)
        self.send_header("Content-Type", "application/connect+json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def test_run_code_reuses_the_pooled_connection():
    """run_code stops folding at the exitCode frame, which is a MESSAGE frame.

    It used to break out of the iterator there, leaving the end-stream frame unread
    and the body short of EOF, so every run_code threw away its connection. An agent
    doing one exec per tool call paid a TLS handshake on every single call.
    """
    srv = _CountingServer(("127.0.0.1", 0), _RunCodeHandler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    try:
        base = "http://127.0.0.1:%d" % srv.server_address[1]
        with httpx.Client() as http:
            cc = ConnectClient(http, base, "sb-1", None)
            for _ in range(3):
                ex = parse_run_code_connect(
                    cc.server_stream("RunCodeStream", {"code": "print(1)"}),
                    None,
                    None,
                    None,
                )
                assert ex.logs["stdout"] == ["1\n"]
        assert srv.accepts == 1, "each run_code opened a new TCP connection"
    finally:
        srv.shutdown()
        srv.server_close()


@pytest.mark.asyncio
async def test_async_streaming_rpc_reuses_the_pooled_connection(server):
    base = "http://127.0.0.1:%d" % server.server_address[1]
    async with httpx.AsyncClient() as http:
        cc = AsyncConnectClient(http, base, "sb-1", None)
        for _ in range(3):
            out = [m async for m in cc.server_stream("RunCodeStream", {"code": "print(1)"})]
            assert out == [{"stdout": "1\n"}]

    assert server.accepts == 1, "each async streaming RPC opened a new TCP connection"
