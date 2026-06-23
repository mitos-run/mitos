from __future__ import annotations

from dataclasses import dataclass, field
from typing import TYPE_CHECKING

from mitos._k8s import k8s
from mitos.errors import AgentRunError

if TYPE_CHECKING:
    from kubernetes import client as k8s_client

API_GROUP = "mitos.run"
API_VERSION = "v1"


@dataclass
class RevisionInfo:
    name: str
    phase: str
    lineage: str
    resumable: bool
    created: str


@dataclass
class DiffInfo:
    parent: str
    added: list[str] = field(default_factory=list)
    removed: list[str] = field(default_factory=list)
    modified: list[str] = field(default_factory=list)


def _lineage(spec: dict) -> str:
    src = spec.get("source", {}) or {}
    if src.get("fromClaim"):
        return "fromClaim:" + src["fromClaim"]
    fwr = src.get("fromWorkspaceRevision")
    if fwr:
        return "fromWorkspaceRevision:" + fwr.get("revision", "")
    return "root"


class Workspace:
    """A durable, forkable agent workspace handle. Lazy: it does not touch the
    cluster until a verb is called. Mirrors the Sandbox ergonomics; verbs are
    git-shaped (log, diff, fork, revert)."""

    def __init__(self, name: str, namespace: str, api: k8s_client.CustomObjectsApi):
        self.name = name
        self.namespace = namespace
        self._api = api

    def _get(self) -> dict:
        try:
            return self._api.get_namespaced_custom_object(
                group=API_GROUP, version=API_VERSION, namespace=self.namespace,
                plural="workspaces", name=self.name,
            )
        except k8s().ApiException as e:
            raise AgentRunError(
                f"workspace {self.name} not found", code="workspace_not_found",
                cause=str(e.reason), status=e.status,
                remediation="Create it with client.create_workspace(name) first.",
            ) from e

    @property
    def head(self) -> str:
        return self._get().get("status", {}).get("head", "")

    @property
    def resumable(self) -> bool:
        return bool(self._get().get("status", {}).get("resumable", False))

    def log(self) -> list[RevisionInfo]:
        objs = self._api.list_namespaced_custom_object(
            group=API_GROUP, version=API_VERSION, namespace=self.namespace,
            plural="workspacerevisions",
        )
        revs: list[RevisionInfo] = []
        for o in objs.get("items", []):
            spec = o.get("spec", {})
            if spec.get("workspaceRef", {}).get("name") != self.name:
                continue
            revs.append(RevisionInfo(
                name=o["metadata"]["name"],
                phase=o.get("status", {}).get("phase", ""),
                lineage=_lineage(spec),
                resumable=spec.get("memorySnapshotRef") is not None,
                created=o["metadata"].get("creationTimestamp", ""),
            ))
        revs.sort(key=lambda r: r.created, reverse=True)
        return revs

    def diff(self, revision: str) -> DiffInfo:
        o = self._api.get_namespaced_custom_object(
            group=API_GROUP, version=API_VERSION, namespace=self.namespace,
            plural="workspacerevisions", name=revision,
        )
        summary = o.get("status", {}).get("diffSummary")
        if not summary:
            raise AgentRunError(
                f"revision {revision} has no recorded diff", code="no_diff",
                cause="the revision was not captured with a {diff: true} output",
                remediation="Terminate with outputs=[{'diff': True}] to record a diff.",
            )
        return DiffInfo(
            parent=summary.get("parentRevision", ""),
            added=summary.get("added", []) or [],
            removed=summary.get("removed", []) or [],
            modified=summary.get("modified", []) or [],
        )

    def fork(self, revision: str, dst_workspace: str) -> str:
        """Branch a committed revision into dst_workspace (a content-addressed
        branch). Returns the new revision name. dst_workspace must exist."""
        parent = self._api.get_namespaced_custom_object(
            group=API_GROUP, version=API_VERSION, namespace=self.namespace,
            plural="workspacerevisions", name=revision,
        )
        manifest = parent.get("spec", {}).get("contentManifest", "")
        if parent.get("status", {}).get("phase") != "Committed" or not manifest:
            raise AgentRunError(
                f"cannot fork uncommitted revision {revision}", code="revision_not_committed",
                cause=f"revision {revision} is not committed",
                remediation="Wait for the revision to commit before forking it.",
            )
        body = {
            "apiVersion": f"{API_GROUP}/{API_VERSION}", "kind": "WorkspaceRevision",
            "metadata": {"generateName": dst_workspace + "-", "namespace": self.namespace,
                         "labels": {"mitos.run/workspace": dst_workspace}},
            "spec": {
                "workspaceRef": {"name": dst_workspace},
                "source": {"fromWorkspaceRevision": {"workspace": self.name, "revision": revision}},
                "contentManifest": manifest,
            },
        }
        created = self._api.create_namespaced_custom_object(
            group=API_GROUP, version=API_VERSION, namespace=self.namespace,
            plural="workspacerevisions", body=body,
        )
        return created["metadata"]["name"]

    def revert(self, revision: str) -> str:
        """Set this workspace head to a past revision by creating a new tip that
        shares its content (revisions are immutable; a revert is a new tip)."""
        return self.fork(revision, self.name)

    # checkout is an alias for revert: make a past state the new head.
    checkout = revert
