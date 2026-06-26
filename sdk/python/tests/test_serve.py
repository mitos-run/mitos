"""Tests for Workspace.serve() - the HTTPS-expose path (issue #312, slice 5b)."""
from __future__ import annotations

import re
from unittest.mock import MagicMock, call, patch

import pytest

import mitos.workspace as ws_module
from mitos.client import AgentRun
from mitos.errors import AgentRunError
from mitos.workspace import (
    ServedWorkspace,
    Workspace,
    _build_expose_url,
    _RESERVED_EXPOSE_LABELS,
)


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _fake_api() -> MagicMock:
    api = MagicMock()
    # Default: sandbox starts Pending then becomes Ready on the second poll.
    api.get_namespaced_custom_object.side_effect = [
        {"status": {"phase": "Pending"}},
        {"status": {"phase": "Ready"}},
    ]
    return api


def _workspace(api: MagicMock | None = None, name: str = "proj-x") -> Workspace:
    return Workspace(name=name, namespace="ns", api=api or _fake_api())


# ---------------------------------------------------------------------------
# _build_expose_url unit tests
# ---------------------------------------------------------------------------

class TestBuildExposeURL:
    def test_happy_path(self):
        assert _build_expose_url("myagent", "mitos.app") == "https://myagent.mitos.app/"

    def test_normalises_to_lowercase(self):
        assert _build_expose_url("MyAgent", "mitos.app") == "https://myagent.mitos.app/"

    def test_empty_domain_raises(self):
        with pytest.raises(AgentRunError) as ei:
            _build_expose_url("ok", "")
        assert ei.value.code == "missing_expose_domain"

    def test_empty_label_raises(self):
        with pytest.raises(AgentRunError) as ei:
            _build_expose_url("", "mitos.app")
        assert ei.value.code == "invalid_expose_label"

    def test_label_too_long_raises(self):
        long_label = "a" * 64
        with pytest.raises(AgentRunError) as ei:
            _build_expose_url(long_label, "mitos.app")
        assert ei.value.code == "invalid_expose_label"

    def test_label_63_chars_is_valid(self):
        label = "a" * 63
        url = _build_expose_url(label, "mitos.app")
        assert url == f"https://{label}.mitos.app/"

    def test_hyphen_start_raises(self):
        with pytest.raises(AgentRunError) as ei:
            _build_expose_url("-bad", "mitos.app")
        assert ei.value.code == "invalid_expose_label"

    def test_hyphen_end_raises(self):
        with pytest.raises(AgentRunError) as ei:
            _build_expose_url("bad-", "mitos.app")
        assert ei.value.code == "invalid_expose_label"

    def test_underscore_raises(self):
        with pytest.raises(AgentRunError) as ei:
            _build_expose_url("my_agent", "mitos.app")
        assert ei.value.code == "invalid_expose_label"

    def test_single_char_is_valid(self):
        url = _build_expose_url("x", "mitos.app")
        assert url == "https://x.mitos.app/"

    @pytest.mark.parametrize("reserved", sorted(_RESERVED_EXPOSE_LABELS))
    def test_reserved_labels_raise(self, reserved: str):
        with pytest.raises(AgentRunError) as ei:
            _build_expose_url(reserved, "mitos.app")
        assert ei.value.code == "reserved_expose_label"


# ---------------------------------------------------------------------------
# Workspace.serve() happy path
# ---------------------------------------------------------------------------

class TestWorkspaceServeHappyPath:
    def test_returns_served_workspace_with_url(self):
        api = _fake_api()
        workspace = _workspace(api)
        with patch.object(ws_module, "_SERVE_POLL_INTERVAL", 0):
            result = workspace.serve(pool="python", expose_domain="mitos.app")

        assert isinstance(result, ServedWorkspace)
        assert result.url.startswith("https://")
        assert result.url.endswith(".mitos.app/")
        assert result.sharing == "private"
        assert result.sandbox_name.startswith("sandbox-")
        assert result.label == result.sandbox_name  # default label = sandbox name

    def test_url_uses_explicit_label(self):
        api = _fake_api()
        workspace = _workspace(api)
        with patch.object(ws_module, "_SERVE_POLL_INTERVAL", 0):
            result = workspace.serve(pool="python", expose_domain="mitos.app", label="mybot")

        assert result.url == "https://mybot.mitos.app/"
        assert result.label == "mybot"

    def test_url_resolves_expose_domain_from_env(self, monkeypatch):
        monkeypatch.setenv("MITOS_EXPOSE_DOMAIN", "example.com")
        api = _fake_api()
        workspace = _workspace(api)
        with patch.object(ws_module, "_SERVE_POLL_INTERVAL", 0):
            result = workspace.serve(pool="python", label="mybot")

        assert result.url == "https://mybot.example.com/"

    def test_sandbox_crd_has_expose_and_workspace_ref(self):
        api = _fake_api()
        workspace = _workspace(api, name="my-workspace")
        with patch.object(ws_module, "_SERVE_POLL_INTERVAL", 0):
            result = workspace.serve(pool="python", port=3000, sharing="link",
                                     expose_domain="mitos.app", label="mybot")

        create_call = api.create_namespaced_custom_object.call_args
        body = create_call.kwargs["body"]

        assert body["kind"] == "Sandbox"
        assert body["spec"]["source"]["poolRef"]["name"] == "python"
        assert body["spec"]["workspaceRef"]["name"] == "my-workspace"
        assert body["spec"]["expose"]["port"] == 3000
        assert body["spec"]["expose"]["label"] == "mybot"
        assert body["spec"]["expose"]["sharing"] == "link"

    def test_sandbox_crd_namespace_matches_workspace(self):
        api = _fake_api()
        workspace = _workspace(api)
        with patch.object(ws_module, "_SERVE_POLL_INTERVAL", 0):
            workspace.serve(pool="python", expose_domain="mitos.app")

        create_call = api.create_namespaced_custom_object.call_args
        assert create_call.kwargs["body"]["metadata"]["namespace"] == "ns"
        assert create_call.kwargs["namespace"] == "ns"
        assert create_call.kwargs["plural"] == "sandboxes"

    def test_sandbox_name_is_default_label_when_no_label_given(self):
        api = _fake_api()
        workspace = _workspace(api)
        with patch.object(ws_module, "_SERVE_POLL_INTERVAL", 0):
            result = workspace.serve(pool="python", expose_domain="mitos.app")

        create_call = api.create_namespaced_custom_object.call_args
        body = create_call.kwargs["body"]
        sb_name = body["metadata"]["name"]
        assert result.label == sb_name
        assert result.url == f"https://{sb_name}.mitos.app/"

    def test_polls_until_ready(self):
        api = MagicMock()
        api.get_namespaced_custom_object.side_effect = [
            {"status": {"phase": "Pending"}},
            {"status": {"phase": "Restoring"}},
            {"status": {"phase": "Ready"}},
        ]
        workspace = _workspace(api)
        with patch.object(ws_module, "_SERVE_POLL_INTERVAL", 0):
            result = workspace.serve(pool="python", expose_domain="mitos.app")

        assert api.get_namespaced_custom_object.call_count == 3
        assert isinstance(result, ServedWorkspace)

    def test_sharing_default_is_private(self):
        api = _fake_api()
        workspace = _workspace(api)
        with patch.object(ws_module, "_SERVE_POLL_INTERVAL", 0):
            result = workspace.serve(pool="python", expose_domain="mitos.app")

        assert result.sharing == "private"
        body = api.create_namespaced_custom_object.call_args.kwargs["body"]
        assert body["spec"]["expose"]["sharing"] == "private"

    def test_default_port_is_8080(self):
        api = _fake_api()
        workspace = _workspace(api)
        with patch.object(ws_module, "_SERVE_POLL_INTERVAL", 0):
            workspace.serve(pool="python", expose_domain="mitos.app")

        body = api.create_namespaced_custom_object.call_args.kwargs["body"]
        assert body["spec"]["expose"]["port"] == 8080


# ---------------------------------------------------------------------------
# Workspace.serve() error cases
# ---------------------------------------------------------------------------

class TestWorkspaceServeErrors:
    def test_missing_pool_raises(self):
        workspace = _workspace()
        with pytest.raises(AgentRunError) as ei:
            workspace.serve(pool="", expose_domain="mitos.app")
        assert ei.value.code == "missing_serve_pool"
        assert ei.value.remediation

    def test_port_zero_raises(self):
        workspace = _workspace()
        with pytest.raises(AgentRunError) as ei:
            workspace.serve(pool="python", port=0, expose_domain="mitos.app")
        assert ei.value.code == "invalid_serve_port"

    def test_port_too_large_raises(self):
        workspace = _workspace()
        with pytest.raises(AgentRunError) as ei:
            workspace.serve(pool="python", port=65536, expose_domain="mitos.app")
        assert ei.value.code == "invalid_serve_port"

    def test_port_boundary_1_is_valid(self):
        api = _fake_api()
        workspace = _workspace(api)
        with patch.object(ws_module, "_SERVE_POLL_INTERVAL", 0):
            result = workspace.serve(pool="python", port=1, expose_domain="mitos.app")
        assert isinstance(result, ServedWorkspace)

    def test_port_boundary_65535_is_valid(self):
        api = _fake_api()
        workspace = _workspace(api)
        with patch.object(ws_module, "_SERVE_POLL_INTERVAL", 0):
            result = workspace.serve(pool="python", port=65535, expose_domain="mitos.app")
        assert isinstance(result, ServedWorkspace)

    def test_missing_domain_raises(self, monkeypatch):
        monkeypatch.delenv("MITOS_EXPOSE_DOMAIN", raising=False)
        workspace = _workspace()
        with pytest.raises(AgentRunError) as ei:
            workspace.serve(pool="python")
        assert ei.value.code == "missing_expose_domain"
        assert ei.value.remediation

    def test_explicit_domain_overrides_env(self, monkeypatch):
        monkeypatch.setenv("MITOS_EXPOSE_DOMAIN", "env.example.com")
        api = _fake_api()
        workspace = _workspace(api)
        with patch.object(ws_module, "_SERVE_POLL_INTERVAL", 0):
            result = workspace.serve(pool="python", expose_domain="arg.example.com",
                                     label="bot")
        assert "arg.example.com" in result.url

    def test_reserved_label_raises(self):
        workspace = _workspace()
        with pytest.raises(AgentRunError) as ei:
            workspace.serve(pool="python", expose_domain="mitos.app", label="api")
        assert ei.value.code == "reserved_expose_label"

    def test_invalid_label_raises(self):
        workspace = _workspace()
        with pytest.raises(AgentRunError) as ei:
            workspace.serve(pool="python", expose_domain="mitos.app", label="Bad_Label!")
        assert ei.value.code == "invalid_expose_label"

    def test_label_with_uppercase_is_accepted_after_lowercasing(self):
        api = _fake_api()
        workspace = _workspace(api)
        with patch.object(ws_module, "_SERVE_POLL_INTERVAL", 0):
            result = workspace.serve(pool="python", expose_domain="mitos.app", label="MyBot")
        assert result.label == "mybot"
        assert result.url == "https://mybot.mitos.app/"

    def test_sandbox_failed_raises(self):
        api = MagicMock()
        api.get_namespaced_custom_object.return_value = {"status": {"phase": "Failed"}}
        workspace = _workspace(api)
        with patch.object(ws_module, "_SERVE_POLL_INTERVAL", 0):
            with pytest.raises(AgentRunError) as ei:
                workspace.serve(pool="python", expose_domain="mitos.app")
        assert ei.value.code == "sandbox_failed"
        assert ei.value.remediation

    def test_no_create_call_when_label_invalid(self):
        """Validate-then-create: the Sandbox CRD is never POSTed for a bad label."""
        api = MagicMock()
        workspace = _workspace(api)
        with pytest.raises(AgentRunError):
            workspace.serve(pool="python", expose_domain="mitos.app", label="-bad")
        api.create_namespaced_custom_object.assert_not_called()

    def test_no_create_call_when_domain_missing(self, monkeypatch):
        """Validate-then-create: the Sandbox CRD is never POSTed without a domain."""
        monkeypatch.delenv("MITOS_EXPOSE_DOMAIN", raising=False)
        api = MagicMock()
        workspace = _workspace(api)
        with pytest.raises(AgentRunError):
            workspace.serve(pool="python")
        api.create_namespaced_custom_object.assert_not_called()


# ---------------------------------------------------------------------------
# AgentRun integration: workspace().serve() uses the same _api
# ---------------------------------------------------------------------------

class TestAgentRunWorkspaceServe:
    def test_workspace_serve_via_agent_run(self):
        c = AgentRun.__new__(AgentRun)
        c._namespace = "ns"
        api = MagicMock()
        api.get_namespaced_custom_object.side_effect = [
            {"status": {"phase": "Pending"}},
            {"status": {"phase": "Ready"}},
        ]
        c._api = api
        c._core_api = MagicMock()

        ws = c.workspace("proj-x")
        with patch.object(ws_module, "_SERVE_POLL_INTERVAL", 0):
            result = ws.serve(pool="python", expose_domain="mitos.app")

        assert result.url.endswith(".mitos.app/")
        body = api.create_namespaced_custom_object.call_args.kwargs["body"]
        assert body["spec"]["workspaceRef"]["name"] == "proj-x"
        assert "expose" in body["spec"]
