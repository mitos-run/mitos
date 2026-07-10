"""Shared test fixtures.

The SDK keeps one pooled, keep-alive httpx.Client per (base URL, timeout)
(mitos.direct._http_for), shared across every SandboxServer and DirectSandbox in the
process. That is exactly right for production (a create-plus-first-exec used to pay two
full TLS handshakes, ~197 ms of a ~572 ms time-to-interactive) but wrong for test
ISOLATION: several modules point at the same localhost URL while starting their own
mock server, or monkeypatch the transport, and a pooled client cached by one module
must never leak into the next. Reset the pool around every test so each one sees the
same world a fresh process would.
"""

import pytest

from mitos import direct


@pytest.fixture(autouse=True)
def _isolate_http_pools():
    direct.close_http_pools()
    yield
    direct.close_http_pools()
