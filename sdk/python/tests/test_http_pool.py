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
