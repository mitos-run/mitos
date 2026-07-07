"""Tests for the first-class spec.git helper (issue #619).

``mitos.git(...)`` builds a Workspace ``spec.git`` (the mitos.run/v1
WorkspaceGit) declaratively: the repo paths inside the workspace that get
version history and the fork-and-merge rendezvous remote, plus the optional
credentials used to authenticate a rendezvous push to an external remote. It
replaces hand-patching the Workspace CRD to declare those paths. The helper is
pure spec construction: no server, no cluster, no KVM.
"""

from unittest.mock import MagicMock

import pytest

import mitos
from mitos.client import AgentRun
from mitos.errors import AgentRunError
from mitos.git import GitSpec


def _fake_client():
    c = AgentRun.__new__(AgentRun)
    c._namespace = "ns"
    c._api = MagicMock()
    c._core_api = MagicMock()
    return c


def test_git_factory_returns_gitspec():
    g = mitos.git(paths=["/workspace/repo"])
    assert isinstance(g, GitSpec)
    assert g.paths == ["/workspace/repo"]
    assert g.credentials_secret is None
    assert g.credentials_username is None


def test_to_spec_minimal_is_paths_only():
    spec = mitos.git(paths=["/workspace/repo", "/workspace/docs"]).to_spec()
    assert spec == {"paths": ["/workspace/repo", "/workspace/docs"]}


def test_to_spec_with_credentials_emits_secret_ref_and_username():
    spec = mitos.git(
        paths=["/workspace/repo"],
        credentials_secret=("git-creds", "token"),
        credentials_username="x-access-token",
    ).to_spec()
    assert spec == {
        "paths": ["/workspace/repo"],
        "credentialsSecretRef": {"name": "git-creds", "key": "token"},
        "credentialsUsername": "x-access-token",
    }


def test_credentials_username_defaults_off_without_secret():
    """Username is only meaningful paired with a secret; without one it is a
    fail-fast rather than a silently ignored field."""
    with pytest.raises(AgentRunError) as ei:
        mitos.git(paths=["/workspace/repo"], credentials_username="x-access-token")
    assert ei.value.code == "invalid_git_credentials"


def test_empty_paths_raises_llm_legible_error():
    with pytest.raises(AgentRunError) as ei:
        mitos.git(paths=[])
    assert ei.value.code == "invalid_git_paths"
    assert ei.value.remediation


def test_blank_path_entry_raises():
    with pytest.raises(AgentRunError) as ei:
        mitos.git(paths=["/workspace/repo", "  "])
    assert ei.value.code == "invalid_git_paths"


def test_malformed_credentials_secret_raises():
    with pytest.raises(AgentRunError) as ei:
        mitos.git(paths=["/workspace/repo"], credentials_secret=("only-name",))  # type: ignore[arg-type]
    assert ei.value.code == "invalid_git_credentials"


def test_create_workspace_sets_spec_git_from_helper():
    c = _fake_client()
    c.create_workspace("proj-x", git=mitos.git(paths=["/workspace/repo"]))
    _, kwargs = c._api.create_namespaced_custom_object.call_args
    body = kwargs["body"]
    assert body["kind"] == "Workspace"
    assert body["spec"]["git"] == {"paths": ["/workspace/repo"]}


def test_create_workspace_accepts_bare_paths_list():
    """A plain list of paths is a convenience form for the common case."""
    c = _fake_client()
    c.create_workspace("proj-y", git=["/workspace/repo"])
    _, kwargs = c._api.create_namespaced_custom_object.call_args
    assert kwargs["body"]["spec"]["git"] == {"paths": ["/workspace/repo"]}


def test_create_workspace_without_git_omits_the_key():
    c = _fake_client()
    c.create_workspace("proj-z")
    _, kwargs = c._api.create_namespaced_custom_object.call_args
    assert "git" not in kwargs["body"]["spec"]


def test_workspace_set_git_patches_spec_git():
    c = _fake_client()
    ws = c.create_workspace("proj-w")
    c._api.reset_mock()
    ws.set_git(mitos.git(paths=["/workspace/repo"]))
    _, kwargs = c._api.patch_namespaced_custom_object.call_args
    assert kwargs["name"] == "proj-w"
    assert kwargs["plural"] == "workspaces"
    assert kwargs["body"] == {"spec": {"git": {"paths": ["/workspace/repo"]}}}


def test_git_exported_from_package():
    assert hasattr(mitos, "git")
    assert mitos.GitSpec is GitSpec
