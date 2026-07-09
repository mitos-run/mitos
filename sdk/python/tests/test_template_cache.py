"""create() resolves the same template on every call; do it once per process.

`mitos.create("python")` issued `POST /v1/templates` (a get-or-create that answers
409 when the template already exists) before every single `POST /v1/fork`. Measured
against the hosted API on a warm connection that round trip cost ~50 ms, on a create
whose total was ~300 ms, and it resolved the identical template every time.

The cache is per process and keyed by (base URL, image, api key). It must:
  - skip the second and later ensure for the same key,
  - never let one API key observe another key's cached template,
  - re-ensure and retry when the template disappeared server side (fork 404),
  - stay disabled whenever the caller shapes the template (network / workload /
    resources), because those arguments change what gets created.
"""
from __future__ import annotations

import json

import httpx
import pytest

import mitos
from mitos import direct
from mitos.errors import AgentRunError
from mitos.types import Network


def _fork_response(request):
    body = json.loads(request.content)
    return httpx.Response(
        200,
        json={"id": body.get("id") or "sb-1", "template_id": "python",
              "endpoint": "http://localhost", "fork_time_ms": 0.5},
    )


class _Recorder(httpx.BaseTransport):
    def __init__(self, fork_status=200):
        self.paths: list[str] = []
        self.fork_status = fork_status

    def handle_request(self, request):
        self.paths.append(request.url.path)
        if request.url.path == "/v1/templates":
            return httpx.Response(200, json={"id": "python", "ready": True})
        if request.url.path == "/v1/fork":
            if self.fork_status == 404:
                # Serve 404 once, then succeed, so the retry path is observable.
                self.fork_status = 200
                return httpx.Response(
                    404,
                    json={"error": {"code": "not_found", "message": "no such pool"}},
                )
            return _fork_response(request)
        return httpx.Response(404, json={"error": {"code": "not_found"}})


@pytest.fixture()
def recorder(monkeypatch):
    rec = _Recorder()
    monkeypatch.setattr(direct, "_transport", lambda: rec)
    return rec


def _ensures(rec) -> int:
    return rec.paths.count("/v1/templates")


def test_second_create_skips_the_template_round_trip(recorder):
    mitos.create("python", api_key="sk-a", base_url="http://testserver")
    assert _ensures(recorder) == 1

    mitos.create("python", api_key="sk-a", base_url="http://testserver")
    mitos.create("python", api_key="sk-a", base_url="http://testserver")
    assert _ensures(recorder) == 1, "the template was re-resolved on a later create"
    assert recorder.paths.count("/v1/fork") == 3


def test_a_different_image_is_ensured_separately(recorder):
    mitos.create("python", api_key="sk-a", base_url="http://testserver")
    mitos.create("node", api_key="sk-a", base_url="http://testserver")
    assert _ensures(recorder) == 2


def test_a_different_api_key_never_reuses_another_keys_cache_entry(recorder):
    """Two orgs share a base URL. Skipping the ensure for org B because org A
    warmed the cache would let B's create depend on A's template."""
    mitos.create("python", api_key="sk-a", base_url="http://testserver")
    mitos.create("python", api_key="sk-b", base_url="http://testserver")
    assert _ensures(recorder) == 2

    mitos.create("python", api_key="sk-a", base_url="http://testserver")
    assert _ensures(recorder) == 2, "org A's second create should still be cached"


def test_a_different_base_url_is_ensured_separately(recorder):
    mitos.create("python", api_key="sk-a", base_url="http://testserver")
    mitos.create("python", api_key="sk-a", base_url="http://other")
    assert _ensures(recorder) == 2


def test_a_shaped_template_is_never_cached(recorder):
    """network / workload / resources change what create_template creates, so the
    ensure must run every time rather than trusting a cache entry made without them."""
    for _ in range(2):
        mitos.create("python", api_key="sk-a", base_url="http://testserver",
                     network=Network(egress="deny"))
    assert _ensures(recorder) == 2

    for _ in range(2):
        mitos.create("python", api_key="sk-a", base_url="http://testserver",
                     resources={"cpu": 2})
    assert _ensures(recorder) == 4


def test_a_vanished_template_is_re_ensured_and_the_fork_retried(monkeypatch):
    """The cache is a bet that the template still exists. When the server says it
    does not (fork 404), drop the entry, ensure again, and retry once."""
    rec = _Recorder()
    monkeypatch.setattr(direct, "_transport", lambda: rec)

    mitos.create("python", api_key="sk-a", base_url="http://testserver")
    assert _ensures(rec) == 1

    # The template is deleted server side; the next fork 404s.
    rec.fork_status = 404
    sb = mitos.create("python", api_key="sk-a", base_url="http://testserver")

    assert sb.id
    assert _ensures(rec) == 2, "a 404 fork must re-ensure the template"
    assert rec.paths.count("/v1/fork") == 3, "the fork must be retried after re-ensuring"


def test_a_non_404_fork_error_is_not_retried(monkeypatch):
    """Only a missing template justifies a retry. A quota denial must surface at once."""

    class _Denier(httpx.BaseTransport):
        def __init__(self):
            self.paths: list[str] = []

        def handle_request(self, request):
            self.paths.append(request.url.path)
            if request.url.path == "/v1/templates":
                return httpx.Response(200, json={"id": "python", "ready": True})
            return httpx.Response(
                429, json={"error": {"code": "rate_limited", "message": "slow down"}}
            )

    rec = _Denier()
    monkeypatch.setattr(direct, "_transport", lambda: rec)

    with pytest.raises(AgentRunError):
        mitos.create("python", api_key="sk-a", base_url="http://testserver")

    assert rec.paths.count("/v1/fork") == 1, "a 429 must not be retried"
