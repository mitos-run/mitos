"""Tests for workload and resources fields on template create (issue #314).

Verifies that create_template forwards the optional workload and resources
dicts into the POST body so callers can register a headless-Chromium (or any
other serving-workload) template in one call.
"""


def test_create_template_includes_workload_and_resources(monkeypatch):
    captured = {}

    class _FakeResp:
        status_code = 200
        is_success = True

        def json(self):
            return {"id": "chrome", "ready": True, "creation_time_ms": 1.0}

        @property
        def text(self):
            return ""

    class _FakeHTTP:
        def post(self, url, json=None, headers=None):
            captured["body"] = json
            return _FakeResp()

    from mitos.direct import SandboxServer

    srv = SandboxServer(url="http://x", api_key=None)
    srv._http = _FakeHTTP()
    srv.create_template(
        "chrome",
        workload={
            "command": ["/usr/local/bin/start-chromium.sh"],
            "ready": {"port": 9222, "path": "/json/version", "expect": 200},
        },
        resources={"vcpu_count": 2, "mem_size_mib": 1024},
    )
    assert captured["body"]["workload"]["command"] == ["/usr/local/bin/start-chromium.sh"]
    assert captured["body"]["resources"]["mem_size_mib"] == 1024
