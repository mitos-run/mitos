from __future__ import annotations

import re
import uuid
from typing import Optional

from mitos._k8s import k8s
from mitos.errors import AgentRunError
from mitos.sandbox import Sandbox
from mitos.types import PoolStatus, SandboxPhase
from mitos.workspace import Workspace


API_GROUP = "mitos.run"
API_VERSION = "v1"

_DEFAULT_POOL_PREFIX = "mitos-default-"
_SLUG_RE = re.compile(r"[^a-z0-9.-]+")


def default_pool_name(image: str) -> str:
    """Derives a deterministic default-pool name for an image. The image is
    lowercased, "/" and ":" become "-", any other unsafe character collapses to
    "-", leading/trailing "-" and "." are stripped (a trailing "." is an invalid
    object name), and the slug is bounded so the pool name stays a valid object
    name. Kept byte-for-byte equivalent to the TypeScript defaultPoolName."""
    slug = image.lower().replace("/", "-").replace(":", "-")
    slug = _SLUG_RE.sub("-", slug)
    # Bound first, then strip trailing/leading "-" and "." so truncation can
    # never leave a name ending in "." or "-" (both invalid object-name tails).
    slug = slug[:40].strip("-.")
    return _DEFAULT_POOL_PREFIX + slug


class AgentRun:
    """Client for the mitos sandbox runtime."""

    def __init__(
        self,
        namespace: str = "default",
        kubeconfig: Optional[str] = None,
        in_cluster: bool = False,
        allow_default_pool: bool = True,
    ):
        _k8s = k8s()
        if in_cluster:
            _k8s.config.load_incluster_config()
        else:
            _k8s.config.load_kube_config(config_file=kubeconfig)

        self._api = _k8s.client.CustomObjectsApi()
        # Same loaded config as the CustomObjectsApi; used to read the
        # per-sandbox bearer token Secrets.
        self._core_api = _k8s.client.CoreV1Api()
        self._namespace = namespace
        self._allow_default_pool = allow_default_pool

    def sandbox(
        self,
        image: Optional[str] = None,
        pool: Optional[str] = None,
        name: Optional[str] = None,
        env: Optional[dict[str, str]] = None,
        secrets: Optional[dict[str, tuple[str, str]]] = None,
        timeout: Optional[str] = None,
        workspace: Optional[str] = None,
        ready: bool = False,
    ) -> Sandbox:
        """The one-liner entry point (docs/api/v2-spec.md section 1.2).

        Pass image= for the lazy path: the client ensures a default pool named
        mitos-default-<image-slug> exists (creating it with an inline template
        if absent and allowed), then starts a Sandbox from it. Pass pool= for
        the explicit path, which never creates anything. Exactly one of image or
        pool is required.

        With ready=True the call blocks until the sandbox is Ready (or raises),
        so the caller stops sleeping-and-hoping; with ready=False (default) the
        first exec/files call lazily waits, preserving today's behavior.
        """
        if pool is None and image is None:
            raise AgentRunError(
                "sandbox() needs an image or a pool",
                code="missing_image_or_pool",
                remediation='Pass image="python" for a lazy default pool, or pool="my-pool" for an existing pool.',
            )
        if pool is None:
            if not self._allow_default_pool:
                raise AgentRunError(
                    "default pools are disabled on this client",
                    code="no_default_pool",
                    remediation="Pass pool=<name> for an existing pool, or construct AgentRun(allow_default_pool=True).",
                )
            pool = self._ensure_default_pool(image)  # type: ignore[arg-type]

        sb = self.create(
            pool=pool,
            name=name,
            env=env,
            secrets=secrets,
            timeout=timeout,
            workspace=workspace,
        )
        if ready:
            sb.wait_until_ready()
        return sb

    def _ensure_default_pool(self, image: str) -> str:
        """get-or-create the default SandboxPool for an image. Returns the pool
        name. A pre-existing pool is reused untouched; a missing one is created
        as a single SandboxPool with inline spec.template (v1: SandboxTemplate
        is gone; the image lives in SandboxPool.spec.template.image)."""
        name = default_pool_name(image)
        try:
            existing = self._api.get_namespaced_custom_object(
                group=API_GROUP,
                version=API_VERSION,
                namespace=self._namespace,
                plural="sandboxpools",
                name=name,
            )
            self._verify_pool_image(existing, name, image)
            return name
        except k8s().ApiException as exc:
            if exc.status != 404:
                raise

        pool = {
            "apiVersion": f"{API_GROUP}/{API_VERSION}",
            "kind": "SandboxPool",
            "metadata": {"name": name, "namespace": self._namespace},
            "spec": {
                "template": {"image": image},
                "replicas": 1,
            },
        }
        self._create_or_reuse(pool, "sandboxpools")
        return name

    def _verify_pool_image(self, pool: dict, name: str, image: str) -> None:
        """Guards the default-pool reuse path against a slug collision serving
        the wrong image. The slug normalizes ":"/"/" and other characters to
        "-", so two distinct images can map to one default pool (for example
        "python:3.11" and "python-3.11"). Reading the inline spec.template.image
        and comparing it to the requested image ensures a reused pool actually
        runs the requested image; a mismatch raises rather than silently running
        the first caller's image. In v1 the image lives inline in the pool's
        spec.template; there is no separate SandboxTemplate object."""
        existing_image = ((pool.get("spec") or {}).get("template") or {}).get("image")
        if not existing_image:
            # Pool with no resolvable inline image: cannot prove the image
            # matches, so fail closed rather than risk the wrong image.
            raise AgentRunError(
                f"default pool {name} has no readable inline template image",
                code="pool_image_mismatch",
                cause=f"pool {name} spec.template.image is absent or unreadable",
                remediation=f'Pass pool="{name}" explicitly to reuse this pool, or use a distinct image that maps to a different default pool.',
            )
        if existing_image != image:
            raise AgentRunError(
                f"default pool {name} already exists for a different image",
                code="pool_image_mismatch",
                cause=f"pool {name} runs image {existing_image!r}, not the requested {image!r} (the image slug collides)",
                remediation=f'Pass pool="{name}" explicitly to reuse this pool, or use a distinct image that maps to a different default pool.',
            )

    def _create_or_reuse(self, body: dict, plural: str) -> None:
        """Create a namespaced custom object, tolerating a 409 from a concurrent
        creator (the object is reused untouched)."""
        try:
            self._api.create_namespaced_custom_object(
                group=API_GROUP,
                version=API_VERSION,
                namespace=self._namespace,
                plural=plural,
                body=body,
            )
        except k8s().ApiException as exc:
            if exc.status != 409:  # raced another creator; reuse it
                raise

    def from_name(self, name: str) -> Sandbox:
        """Reconnect to an existing sandbox by name, returning a live Sandbox
        handle (a durable handle across processes). The handle resolves its
        endpoint, phase, and per-sandbox token from the cluster; if the sandbox
        is Ready you can exec against it immediately. Alias-quality wrapper over
        get(), named for the reconnect use case."""
        return self.get(name)

    def create(
        self,
        pool: str,
        name: Optional[str] = None,
        env: Optional[dict[str, str]] = None,
        secrets: Optional[dict[str, tuple[str, str]]] = None,
        timeout: Optional[str] = None,
        workspace: Optional[str] = None,
    ) -> Sandbox:
        """Create a sandbox from a pool.

        Args:
            pool: Name of the SandboxPool to claim from.
            name: Optional sandbox name. Generated if not provided.
            env: Environment variables to inject.
            secrets: Map of env var name to (secret_name, secret_key) tuples.
            timeout: Maximum lifetime, e.g. "30m", "1h".
            workspace: Bind the sandbox to a durable Workspace by name. On
                activation the controller hydrates the workspace head into
                /workspace; on terminate it dehydrates a new committed revision.
        """
        if name is None:
            name = f"sandbox-{uuid.uuid4().hex[:8]}"

        sandbox_body = {
            "apiVersion": f"{API_GROUP}/{API_VERSION}",
            "kind": "Sandbox",
            "metadata": {
                "name": name,
                "namespace": self._namespace,
            },
            "spec": {
                "source": {"poolRef": {"name": pool}},
            },
        }

        if env:
            sandbox_body["spec"]["env"] = [
                {"name": k, "value": v} for k, v in env.items()
            ]

        if secrets:
            sandbox_body["spec"]["secrets"] = [
                {
                    "name": env_var,
                    "secretRef": {"name": secret_name, "key": secret_key},
                    "envVar": env_var,
                }
                for env_var, (secret_name, secret_key) in secrets.items()
            ]

        if timeout:
            sandbox_body["spec"].setdefault("lifetime", {})["ttl"] = timeout

        if workspace:
            sandbox_body["spec"]["workspaceRef"] = {"name": workspace}

        self._api.create_namespaced_custom_object(
            group=API_GROUP,
            version=API_VERSION,
            namespace=self._namespace,
            plural="sandboxes",
            body=sandbox_body,
        )

        return Sandbox(
            name=name,
            namespace=self._namespace,
            pool=pool,
            api=self._api,
            core_api=self._core_api,
        )

    def get(self, name: str) -> Sandbox:
        """Get an existing sandbox by name."""
        obj = self._api.get_namespaced_custom_object(
            group=API_GROUP,
            version=API_VERSION,
            namespace=self._namespace,
            plural="sandboxes",
            name=name,
        )
        status = obj.get("status", {})
        pool = obj.get("spec", {}).get("source", {}).get("poolRef", {}).get("name", "")

        sandbox = Sandbox(
            name=name,
            namespace=self._namespace,
            pool=pool,
            api=self._api,
            core_api=self._core_api,
            _endpoint=status.get("endpoint"),
            _phase=SandboxPhase(status.get("phase", "Pending")),
        )
        if sandbox._phase == SandboxPhase.READY:
            sandbox._load_token()
        return sandbox

    def list(self, pool: Optional[str] = None) -> list[Sandbox]:
        """List sandboxes, optionally filtered by pool."""
        objs = self._api.list_namespaced_custom_object(
            group=API_GROUP,
            version=API_VERSION,
            namespace=self._namespace,
            plural="sandboxes",
        )

        sandboxes = []
        for obj in objs.get("items", []):
            obj_pool = obj.get("spec", {}).get("source", {}).get("poolRef", {}).get("name", "")
            if pool and obj_pool != pool:
                continue
            status = obj.get("status", {})
            sandboxes.append(Sandbox(
                name=obj["metadata"]["name"],
                namespace=self._namespace,
                pool=obj_pool,
                api=self._api,
                core_api=self._core_api,
                _endpoint=status.get("endpoint"),
                _phase=SandboxPhase(status.get("phase", "Pending")),
            ))
        return sandboxes

    def create_workspace(self, name: str) -> Workspace:
        """Create an empty durable Workspace."""
        body = {
            "apiVersion": f"{API_GROUP}/{API_VERSION}", "kind": "Workspace",
            "metadata": {"name": name, "namespace": self._namespace}, "spec": {},
        }
        self._api.create_namespaced_custom_object(
            group=API_GROUP, version=API_VERSION, namespace=self._namespace,
            plural="workspaces", body=body,
        )
        return Workspace(name, self._namespace, self._api)

    def workspace(self, name: str) -> Workspace:
        """Lazy handle to a workspace (create_workspace if it must exist)."""
        return Workspace(name, self._namespace, self._api)

    def get_workspace(self, name: str) -> Workspace:
        """Reconnect to an existing workspace, raising if it is absent."""
        ws = Workspace(name, self._namespace, self._api)
        ws._get()  # raises workspace_not_found if absent
        return ws

    def list_workspaces(self) -> list[Workspace]:
        """List the workspaces in the client's namespace."""
        objs = self._api.list_namespaced_custom_object(
            group=API_GROUP, version=API_VERSION, namespace=self._namespace,
            plural="workspaces",
        )
        return [Workspace(o["metadata"]["name"], self._namespace, self._api)
                for o in objs.get("items", [])]

    def pool_status(self, name: str) -> PoolStatus:
        """Get the status of a SandboxPool."""
        obj = self._api.get_namespaced_custom_object(
            group=API_GROUP,
            version=API_VERSION,
            namespace=self._namespace,
            plural="sandboxpools",
            name=name,
        )
        status = obj.get("status", {})
        spec = obj.get("spec", {})
        return PoolStatus(
            name=name,
            ready_snapshots=status.get("readySnapshots", 0),
            desired=spec.get("replicas", 0),
            node_distribution=status.get("nodeDistribution", {}),
        )
