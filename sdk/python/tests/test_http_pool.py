"""The SDK must reuse one pooled, keep-alive HTTP connection per base URL.

Every ``mitos.create()`` used to build TWO fresh ``httpx.Client`` objects (one for the
server, one for the returned sandbox), so a create-plus-first-exec paid TWO full TLS
handshakes. Measured from a same-region client against the hosted API, that was ~197 ms
of a ~572 ms time-to-interactive: about a third of the number a user actually feels,
spent re-negotiating TLS the process had already negotiated.

``terminate()`` also closed the client, which is why the pool never survived a sandbox.
"""

import httpx
import pytest

from mitos import direct


@pytest.fixture(autouse=True)
def _fresh_pools():
    direct.close_http_pools()
    yield
    direct.close_http_pools()


def test_same_base_url_shares_one_client():
    a = direct._http_for("https://api.example.test")
    b = direct._http_for("https://api.example.test")
    assert a is b, "two calls for the same base URL must share one pooled client"


def test_different_base_urls_get_different_clients():
    a = direct._http_for("https://api.example.test")
    b = direct._http_for("https://other.example.test")
    assert a is not b


def test_pool_is_keyed_by_timeout_so_call_semantics_are_preserved():
    # The sandbox path used a 30s timeout and the server path 60s. Collapsing them onto
    # one client would silently change one of them, so the pool keys on timeout too.
    a = direct._http_for("https://api.example.test", timeout=30.0)
    b = direct._http_for("https://api.example.test", timeout=60.0)
    assert a is not b
    assert direct._http_for("https://api.example.test", timeout=30.0) is a


def test_a_closed_pool_entry_is_rebuilt():
    a = direct._http_for("https://api.example.test")
    a.close()
    b = direct._http_for("https://api.example.test")
    assert b is not a and not b.is_closed


def test_sandbox_and_server_for_one_url_share_the_pool():
    server = direct.SandboxServer(url="https://api.example.test", api_key="k")
    sb = direct.DirectSandbox(
        id="sb-1", template="python", endpoint="1.2.3.4:9091",
        server_url="https://api.example.test", fork_time_ms=1.0, api_key="k",
    )
    assert sb._http is direct._http_for("https://api.example.test", timeout=30.0)
    assert server._http is direct._http_for("https://api.example.test", timeout=60.0)


def test_terminate_does_not_close_the_shared_pool():
    calls = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(request.url.path)
        return httpx.Response(200, json={})

    pooled = direct._http_for("https://api.example.test", timeout=30.0)
    pooled._transport = httpx.MockTransport(handler)

    sb = direct.DirectSandbox(
        id="sb-1", template="python", endpoint="1.2.3.4:9091",
        server_url="https://api.example.test", fork_time_ms=1.0, api_key="k",
    )
    sb.terminate()
    assert calls, "terminate must still issue the DELETE"
    assert not pooled.is_closed, (
        "terminate closed the shared pool; the next sandbox would re-handshake TLS"
    )
    # And the pool is still the one handed to the next sandbox.
    assert direct._http_for("https://api.example.test", timeout=30.0) is pooled


def _pool_of(client):
    """Reach the underlying httpcore pool. Private attrs, so assert they exist rather
    than silently pass if httpx renames them."""
    transport = getattr(client, "_transport", None)
    pool = getattr(transport, "_pool", None)
    assert pool is not None, "httpx internals moved; update this accessor"
    return pool


def test_pool_keepalive_outlives_a_thinking_pause():
    """httpx's default keepalive_expiry is 5 s, which silently defeats the pool.

    An agent pauses between tool calls (model latency, its own reasoning), so the gap
    between two SDK requests is routinely longer than 5 seconds. With the default, the
    connection is evicted and the next request pays a full TLS handshake, which is the
    exact cost this pool exists to remove. Measured same-region against the hosted API:
    after a 13 s idle gap, expiry=5 s cost 101.5 ms (re-handshake) and expiry=60 s cost
    8.6 ms (reused).

    So the expiry must comfortably exceed a thinking pause, not merely be non-zero.
    """
    client = direct._http_for("https://api.example.test")
    expiry = _pool_of(client)._keepalive_expiry
    assert expiry >= 60.0, (
        "pooled client keepalive_expiry is %r; httpx's 5 s default evicts the "
        "connection across a normal agent pause and re-handshakes TLS" % expiry
    )


def test_pool_bounds_its_connections():
    """A shared, never-closed pool must stay bounded or a long-lived process leaks sockets."""
    client = direct._http_for("https://api.example.test")
    pool = _pool_of(client)
    assert pool._max_keepalive_connections is not None
    assert pool._max_connections is not None


def test_clients_with_different_timeouts_share_one_connection():
    """The create path uses a 60 s client and the first exec a 30 s client.

    They must remain distinct clients (each enforces its own timeout) yet hand each
    other warm sockets, otherwise the first exec of every sandbox re-handshakes TLS.
    Counting TCP accepts is the only observation that proves reuse.
    """
    import threading
    from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

    class _H(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"  # else the server closes every connection

        def do_GET(self):
            self.send_response(200)
            self.send_header("Content-Length", "2")
            self.end_headers()
            self.wfile.write(b"ok")

        def log_message(self, *a):
            pass

    class _S(ThreadingHTTPServer):
        accepts = 0

        def get_request(self):
            self.accepts += 1
            return super().get_request()

    srv = _S(("127.0.0.1", 0), _H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    try:
        base = "http://127.0.0.1:%d" % srv.server_address[1]
        server_client = direct._http_for(base, timeout=60.0)
        sandbox_client = direct._http_for(base, timeout=30.0)

        assert server_client is not sandbox_client
        assert server_client.timeout.read == 60.0
        assert sandbox_client.timeout.read == 30.0

        server_client.get(base + "/")
        sandbox_client.get(base + "/")
        server_client.get(base + "/")

        assert srv.accepts == 1, "the two pooled clients did not share a connection"
    finally:
        srv.shutdown()
        srv.server_close()


def test_pooled_transport_disables_nagle():
    """Nagle must be off on every pooled connection.

    httpcore 0.16 (shipped with httpx 0.23) did NOT set TCP_NODELAY. With Nagle on,
    httpcore's separate header and body writes make the second segment wait on the
    peer's delayed ACK. Measured against the hosted API, that cost 37 ms on EVERY
    request: `exec true` took 61.8 ms through httpx and 25.1 ms through http.client
    over the same warm connection, interleaved call by call. Newer httpcore sets it,
    but the SDK must not depend on a transitive default for a 37 ms regression.
    """
    import socket
    import threading
    from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

    class _H(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def do_GET(self):
            self.send_response(200)
            self.send_header("Content-Length", "2")
            self.end_headers()
            self.wfile.write(b"ok")

        def log_message(self, *a):
            pass

    srv = ThreadingHTTPServer(("127.0.0.1", 0), _H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    try:
        base = "http://127.0.0.1:%d" % srv.server_address[1]
        resp = direct._http_for(base).get(base + "/")
        stream = resp.extensions.get("network_stream")
        assert stream is not None, "no network_stream extension; cannot inspect the socket"
        sock = stream.get_extra_info("socket")
        assert sock is not None
        assert sock.getsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY) != 0, (
            "Nagle is enabled on the pooled connection: every request pays a delayed ACK"
        )
    finally:
        srv.shutdown()
        srv.server_close()
