"""Lazy accessor for the optional ``kubernetes`` client (issue #22).

The cluster-mode modules (``client``, ``aio``, ``sandbox``, ``workspace``) need
the official Kubernetes client, but direct mode (``mitos.direct`` over the
sandbox-server REST API) and the in-guest SDK (``mitos.guest``) speak only
httpx and must import with no Kubernetes installed. To keep ``import mitos`` and
``from mitos.direct import SandboxServer`` light, those modules import the
``kubernetes`` symbols through :func:`k8s` at the moment a cluster code path
runs, never at module import time. This mirrors the TypeScript SDK, whose
``@kubernetes/client-node`` is lazy-loaded so direct mode never pulls it in.

A missing ``kubernetes`` raises a clear, actionable AgentRunError naming the
install command, rather than a bare ModuleNotFoundError from deep in an import.
"""

from __future__ import annotations

from typing import TYPE_CHECKING

from mitos.errors import AgentRunError

if TYPE_CHECKING:
    # Annotation-only imports: TYPE_CHECKING is False at runtime, so these never
    # force the kubernetes import for callers that only use direct mode.
    from kubernetes import client as _client_mod
    from kubernetes import config as _config_mod
    from kubernetes.client.rest import ApiException as _ApiException


class _K8s:
    """The three kubernetes symbols the cluster modules use, bound lazily."""

    __slots__ = ("client", "config", "ApiException")

    def __init__(self, client, config, ApiException):
        self.client = client
        self.config = config
        self.ApiException = ApiException


_cached: "_K8s | None" = None


def k8s() -> "_K8s":
    """Import and return the kubernetes client/config/ApiException, cached.

    Called only from cluster code paths. If the optional ``kubernetes`` package
    is not installed, raises an AgentRunError that names the fix instead of
    letting a raw ModuleNotFoundError surface from an unexpected place."""
    global _cached
    if _cached is not None:
        return _cached
    try:
        from kubernetes import client as k8s_client
        from kubernetes import config as k8s_config
        from kubernetes.client.rest import ApiException
    except ModuleNotFoundError as exc:
        raise AgentRunError(
            "cluster mode requires the kubernetes client, which is not installed",
            code="kubernetes_not_installed",
            cause=f"importing the kubernetes package failed: {exc}",
            remediation=(
                "Install it with 'pip install kubernetes' or 'pip install mitos[k8s]'. "
                "Direct mode (mitos.create / mitos.direct) and the in-guest SDK "
                "(mitos.guest) do not need it."
            ),
        ) from exc
    _cached = _K8s(k8s_client, k8s_config, ApiException)
    return _cached
