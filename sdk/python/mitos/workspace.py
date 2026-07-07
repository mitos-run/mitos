from __future__ import annotations

import os
import re
import time
import uuid
from dataclasses import dataclass, field
from typing import TYPE_CHECKING, Optional

from mitos._k8s import k8s
from mitos.errors import AgentRunError

if TYPE_CHECKING:
    from kubernetes import client as k8s_client

API_GROUP = "mitos.run"
API_VERSION = "v1"

# Polling interval (seconds) while waiting for a served sandbox to reach Ready.
# Tests override this to avoid sleeping.
_SERVE_POLL_INTERVAL: float = 0.5

# Reserved subdomain labels that tenants may not use. Mirrors
# internal/preview/route.go reservedLabels; keep the two lists in sync.
_RESERVED_EXPOSE_LABELS: frozenset[str] = frozenset({
    "www", "app", "api", "console", "gateway",
    "admin", "auth", "login", "account", "mail",
    "static", "assets", "cdn", "status",
})

# A valid single DNS label: starts and ends with alphanumeric, may contain
# hyphens in the middle, max 63 characters.
_EXPOSE_LABEL_RE = re.compile(r"^[a-z0-9]([a-z0-9-]*[a-z0-9])?$")


@dataclass
class ServedWorkspace:
    """Handle returned by :meth:`Workspace.serve`.

    The :attr:`url` attribute is the public HTTPS URL
    ``https://<label>.<expose_domain>/``. Token minting is a follow-up: the
    per-sandbox bearer token is not set on the expose route here; the proxy
    enforces the sharing tier independently.
    """

    url: str
    sandbox_name: str
    label: str
    sharing: str


def _build_expose_url(label: str, expose_domain: str) -> str:
    """Validate *label* and *expose_domain* then return the HTTPS URL.

    Normalises *label* to lowercase before validation. Raises
    :class:`~mitos.errors.AgentRunError` for any invalid input so the caller
    fails fast before touching the cluster.
    """
    label = label.lower()

    if not expose_domain:
        raise AgentRunError(
            "expose domain is required",
            code="missing_expose_domain",
            cause="no expose domain was provided and MITOS_EXPOSE_DOMAIN is not set",
            remediation=(
                "Pass expose_domain=... to serve() or set the "
                "MITOS_EXPOSE_DOMAIN environment variable."
            ),
        )
    if not label:
        raise AgentRunError(
            "expose label is required",
            code="invalid_expose_label",
            cause="label is empty",
            remediation=(
                "Pass label=... to serve() or use a sandbox name that is a "
                "valid single DNS label."
            ),
        )
    if len(label) > 63:
        raise AgentRunError(
            f"expose label {label!r} exceeds 63 characters",
            code="invalid_expose_label",
            cause=f"label length {len(label)} > 63",
            remediation="Use a shorter label (at most 63 characters).",
        )
    if not _EXPOSE_LABEL_RE.match(label):
        raise AgentRunError(
            f"expose label {label!r} is not a valid single DNS label",
            code="invalid_expose_label",
            cause="label must match ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$",
            remediation=(
                "Use only lowercase letters, digits, and hyphens; "
                "do not start or end with a hyphen."
            ),
        )
    if label in _RESERVED_EXPOSE_LABELS:
        raise AgentRunError(
            f"expose label {label!r} is reserved and may not be used by tenants",
            code="reserved_expose_label",
            cause=f"label {label!r} is in the reserved set",
            remediation=(
                "Choose a different label that is not a well-known "
                "control-plane name."
            ),
        )
    return f"https://{label}.{expose_domain}/"


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

    def set_git(self, git: object) -> None:
        """Declare this workspace's ``spec.git`` in place (issue #619).

        Accepts a :class:`~mitos.git.GitSpec` from ``mitos.git(paths=[...])`` or a
        bare list of paths. Patches ``spec.git`` on the existing Workspace so a
        caller declares the repo paths that get version history and the
        rendezvous remote first-class, instead of hand-patching the CRD. Prefer
        :meth:`AgentRun.create_workspace` with ``git=`` to set it at create time.
        """
        from mitos.git import coerce_git

        self._api.patch_namespaced_custom_object(
            group=API_GROUP, version=API_VERSION, namespace=self.namespace,
            plural="workspaces", name=self.name,
            body={"spec": {"git": coerce_git(git).to_spec()}},
        )

    def serve(
        self,
        *,
        pool: str,
        port: int = 8080,
        sharing: str = "private",
        label: Optional[str] = None,
        expose_domain: Optional[str] = None,
    ) -> ServedWorkspace:
        """Create a Sandbox bound to this workspace with ``spec.expose`` set,
        wait until it is Ready, then return a :class:`ServedWorkspace` handle
        carrying the public HTTPS URL.

        Args:
            pool: Name of the SandboxPool to claim from. Required.
            port: Guest TCP port to expose. Defaults to 8080. Must be 1-65535.
            sharing: Access tier. One of ``"private"``, ``"link"``, ``"org"``,
                ``"authenticated"``, ``"public"``. Defaults to ``"private"``.
            label: Explicit subdomain label (a single DNS label). Defaults to
                the generated sandbox name. Lowercased before validation.
            expose_domain: Base expose domain (for example ``"mitos.app"``).
                Falls back to the ``MITOS_EXPOSE_DOMAIN`` environment variable.

        Returns:
            A :class:`ServedWorkspace` with ``.url``, ``.sandbox_name``,
            ``.label``, and ``.sharing``.

        Raises:
            :class:`~mitos.errors.AgentRunError` with codes:

            - ``missing_serve_pool`` if *pool* is empty.
            - ``invalid_serve_port`` if *port* is outside 1-65535.
            - ``missing_expose_domain`` if no domain is available.
            - ``invalid_expose_label`` if *label* fails DNS-label validation.
            - ``reserved_expose_label`` if *label* is a reserved control-plane name.
            - ``sandbox_failed`` if the sandbox reaches the Failed phase.
        """
        if not pool:
            raise AgentRunError(
                "serve() needs a pool",
                code="missing_serve_pool",
                cause="pool argument was not provided",
                remediation="Pass pool=<name> to select the SandboxPool to claim from.",
            )
        if not (1 <= port <= 65535):
            raise AgentRunError(
                f"serve port {port} out of range",
                code="invalid_serve_port",
                cause=f"port {port} is not in 1-65535",
                remediation="Pass port=n with a port in the range 1-65535.",
            )

        # Resolve expose domain: argument first, then environment variable.
        resolved_domain = expose_domain or os.environ.get("MITOS_EXPOSE_DOMAIN", "")
        if not resolved_domain:
            raise AgentRunError(
                "expose domain is required",
                code="missing_expose_domain",
                cause="no expose domain was provided and MITOS_EXPOSE_DOMAIN is not set",
                remediation=(
                    "Pass expose_domain=... to serve() or set the "
                    "MITOS_EXPOSE_DOMAIN environment variable."
                ),
            )

        # Generate the sandbox name up front so it can serve as the default
        # label before the cluster creates the object.
        sb_name = f"sandbox-{uuid.uuid4().hex[:8]}"

        # Determine the effective label; fall back to the sandbox name.
        # Lowercase here (not only inside _build_expose_url) so the returned
        # ServedWorkspace.label is always the normalised form.
        effective_label = (label.lower() if label is not None else sb_name)

        # Validate and construct the URL before sending anything to the
        # cluster so a bad label fails fast without leaving a partially
        # configured sandbox.
        url = _build_expose_url(effective_label, resolved_domain)

        # Build the Sandbox CRD body with spec.expose in the initial POST.
        # JSON shape matches api/v1 SandboxExpose: port, label, sharing.
        sandbox_body = {
            "apiVersion": f"{API_GROUP}/{API_VERSION}",
            "kind": "Sandbox",
            "metadata": {"name": sb_name, "namespace": self.namespace},
            "spec": {
                "source": {"poolRef": {"name": pool}},
                "workspaceRef": {"name": self.name},
                "expose": {
                    "port": port,
                    "label": effective_label,
                    "sharing": sharing,
                },
            },
        }
        self._api.create_namespaced_custom_object(
            group=API_GROUP,
            version=API_VERSION,
            namespace=self.namespace,
            plural="sandboxes",
            body=sandbox_body,
        )

        # Poll until the sandbox reaches Ready or the caller's process
        # terminates. A Failed phase is returned immediately as an error.
        self._wait_sandbox_ready(sb_name)

        return ServedWorkspace(
            url=url,
            sandbox_name=sb_name,
            label=effective_label,
            sharing=sharing,
        )

    def _wait_sandbox_ready(self, name: str) -> None:
        """Poll a Sandbox by *name* until it reaches Ready or fails.

        Raises :class:`~mitos.errors.AgentRunError` with
        ``code="sandbox_failed"`` on a Failed phase. Blocks until the
        sandbox is Ready or the process exits; there is intentionally no
        per-call deadline here (mirror Go: the caller owns the context
        cancel, Python callers can interrupt with Ctrl-C or a thread).
        """
        while True:
            obj = self._api.get_namespaced_custom_object(
                group=API_GROUP,
                version=API_VERSION,
                namespace=self.namespace,
                plural="sandboxes",
                name=name,
            )
            phase = (obj.get("status") or {}).get("phase", "Pending")
            if phase == "Ready":
                return
            if phase == "Failed":
                raise AgentRunError(
                    f"sandbox {name} reached Failed phase",
                    code="sandbox_failed",
                    cause="the controller reported a Failed phase before Ready",
                    remediation=(
                        "Check the Sandbox status for more detail "
                        f"(kubectl describe sandbox {name})."
                    ),
                )
            time.sleep(_SERVE_POLL_INTERVAL)
