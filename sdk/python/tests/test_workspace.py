import pytest
from unittest.mock import MagicMock

from mitos.client import AgentRun
from mitos.errors import AgentRunError


def _fake_api():
    api = MagicMock()
    return api


def test_create_workspace_posts_crd():
    c = AgentRun.__new__(AgentRun)
    c._namespace = "ns"
    c._api = _fake_api()
    c._core_api = MagicMock()
    ws = c.create_workspace("proj-x")
    assert ws.name == "proj-x"
    args, kwargs = c._api.create_namespaced_custom_object.call_args
    body = kwargs["body"]
    assert body["kind"] == "Workspace"
    assert body["metadata"]["name"] == "proj-x"


def test_log_returns_revisions_newest_first():
    c = AgentRun.__new__(AgentRun)
    c._namespace = "ns"
    c._api = _fake_api()
    c._core_api = MagicMock()
    c._api.list_namespaced_custom_object.return_value = {
        "items": [
            {"metadata": {"name": "proj-x-1", "creationTimestamp": "2026-06-01T00:00:00Z"},
             "spec": {"workspaceRef": {"name": "proj-x"}, "source": {"fromClaim": "c1"}},
             "status": {"phase": "Committed"}},
            {"metadata": {"name": "proj-x-2", "creationTimestamp": "2026-06-02T00:00:00Z"},
             "spec": {"workspaceRef": {"name": "proj-x"}, "source": {"fromClaim": "c2"}},
             "status": {"phase": "Committed"}},
        ]
    }
    ws = c.workspace("proj-x")
    revs = ws.log()
    assert [r.name for r in revs] == ["proj-x-2", "proj-x-1"]
    assert revs[0].lineage == "fromClaim:c2"


def test_fork_uncommitted_raises_llm_legible_error():
    c = AgentRun.__new__(AgentRun)
    c._namespace = "ns"
    c._api = _fake_api()
    c._core_api = MagicMock()
    c._api.get_namespaced_custom_object.return_value = {
        "metadata": {"name": "proj-x-1"},
        "spec": {"workspaceRef": {"name": "proj-x"}},
        "status": {"phase": "Pending"},
    }
    ws = c.workspace("proj-x")
    with pytest.raises(AgentRunError) as ei:
        ws.fork("proj-x-1", "branch")
    assert ei.value.code == "revision_not_committed"
    assert ei.value.remediation


def test_terminate_with_outputs_patches_and_returns_workspace():
    from mitos.sandbox import Sandbox

    api = MagicMock()
    api.get_namespaced_custom_object.return_value = {
        "spec": {"workspaceRef": {"name": "proj-x"}},
    }
    sb = Sandbox(name="sbx-1", namespace="ns", pool="p", api=api, core_api=MagicMock())
    ws_name = sb.terminate(outputs=["/workspace/dist", {"diff": True}], checkpoint=True)
    assert ws_name == "proj-x"
    patch_body = api.patch_namespaced_custom_object.call_args.kwargs["body"]
    assert patch_body["spec"]["outputs"] == [{"path": "/workspace/dist"}, {"diff": True}]
    assert patch_body["spec"]["checkpointOnTerminate"] is True
    api.delete_namespaced_custom_object.assert_called_once()
