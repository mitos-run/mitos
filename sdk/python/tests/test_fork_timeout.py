"""The fork POST must give the server room to finish.

A hosted live fork is polled to Ready by the control plane for up to its ready
deadline (120s by default). The DirectSandbox http client defaults to a 30s
timeout, which would raise fork_unavailable client-side on a slow-but-succeeding
fork. _fork_post therefore overrides the per-request timeout to exceed the
server's own deadline.
"""

import httpx
import pytest

from mitos.direct import _fork_post, FORK_CLIENT_TIMEOUT_SECONDS
from mitos.errors import AgentRunError


class _RecordingClient:
    """Stands in for httpx.Client, recording the timeout the fork POST used."""

    def __init__(self):
        self.last_timeout = None

    def post(self, url, json=None, headers=None, timeout=None):
        self.last_timeout = timeout
        request = httpx.Request("POST", url)
        return httpx.Response(200, json={"ok": True}, request=request)


def test_fork_post_default_timeout_exceeds_server_ready_deadline():
    # The gateway ready deadline is 120s; the client must wait longer so the
    # server's real answer surfaces instead of a premature client timeout.
    assert FORK_CLIENT_TIMEOUT_SECONDS > 120.0
    client = _RecordingClient()
    _fork_post(client, "http://x/v1/sandboxes/a/fork", {"id": "a"}, {})
    assert client.last_timeout == FORK_CLIENT_TIMEOUT_SECONDS


def test_fork_post_timeout_is_overridable():
    client = _RecordingClient()
    _fork_post(client, "http://x/v1/fork", {"id": "a"}, {}, timeout=5.0)
    assert client.last_timeout == 5.0


class _TimingOutClient:
    def post(self, url, json=None, headers=None, timeout=None):
        raise httpx.ReadTimeout("read timed out")


def test_fork_post_maps_timeout_to_structured_error():
    with pytest.raises(AgentRunError) as excinfo:
        _fork_post(_TimingOutClient(), "http://x/v1/fork", {"id": "a"}, {})
    assert excinfo.value.code == "fork_unavailable"
