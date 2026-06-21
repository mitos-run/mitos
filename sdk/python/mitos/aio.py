from __future__ import annotations

import asyncio
import base64
import uuid
from typing import Awaitable, Callable, Optional, Union

import httpx
from kubernetes import client as k8s_client
from kubernetes import config as k8s_config
from kubernetes.client.rest import ApiException

from mitos.client import API_GROUP, API_VERSION, default_pool_name
from mitos.direct import _resolve_auth
from mitos.errors import AgentRunError, ExecutionDeadlineError
from mitos._envelope import raise_for_status, raise_for_status_stream
from mitos.sandbox import (
    EXEC_TIMEOUT_EXIT_CODE,
    _parse_run_code_stream,
    _validate_timeout,
)
from mitos.types import Execution, ExecResult, FileInfo, Network, Result, SandboxPhase

POLL_INTERVAL = 0.05

# A stdout/stderr callback may be sync or async.
StreamCallback = Callable[[bytes], Union[Awaitable[None], None]]


class AsyncSandboxFiles:
    """Async file operations. Mirrors mitos.sandbox.SandboxFiles."""

    def __init__(self, sandbox: "AsyncSandbox"):
        self._sb = sandbox

    async def read(self, path: str) -> str:
        resp = await self._sb._http.post(
            f"{self._sb._base_url}/files/read",
            json={"sandbox": self._sb.id, "path": path},
            headers=self._sb._auth_headers(),
        )
        raise_for_status(resp, token=self._sb._token)
        return resp.json()["content"]

    async def write(self, path: str, content: Union[str, bytes], mode: int = 0o644) -> None:
        data: dict = {"sandbox": self._sb.id, "path": path, "mode": mode}
        if isinstance(content, bytes):
            data["content"] = content.hex()
            data["binary"] = True
        else:
            data["content"] = content
        resp = await self._sb._http.post(
            f"{self._sb._base_url}/files/write",
            json=data,
            headers=self._sb._auth_headers(),
        )
        raise_for_status(resp, token=self._sb._token)

    async def list(self, path: str = "/") -> list[FileInfo]:
        resp = await self._sb._http.post(
            f"{self._sb._base_url}/files/list",
            json={"sandbox": self._sb.id, "path": path},
            headers=self._sb._auth_headers(),
        )
        raise_for_status(resp, token=self._sb._token)
        return [
            FileInfo(
                name=f["name"], is_dir=f["is_dir"], size=f["size"],
                mode=f.get("mode", 0), modified_at=f.get("modified_at"),
            )
            for f in resp.json()["entries"]
        ]


class AsyncSandbox:
    """Async sandbox handle over httpx.AsyncClient. Hot paths only: exec, files,
    fork, terminate. Construct via AsyncAgentRun.sandbox(); the test path passes
    _http directly."""

    def __init__(
        self,
        id: str,
        endpoint: str,
        token: Optional[str] = None,
        namespace: str = "default",
        pool: str = "",
        api: Optional[k8s_client.CustomObjectsApi] = None,
        core_api: Optional[k8s_client.CoreV1Api] = None,
        _http: Optional[httpx.AsyncClient] = None,
    ):
        self.id = id
        self.name = id
        self.endpoint = endpoint
        self.namespace = namespace
        self.pool = pool
        self._phase = SandboxPhase.PENDING
        self._token = token
        self._api = api
        self._core_api = core_api
        self._http = _http or httpx.AsyncClient(timeout=30.0)
        self._owns_http = _http is None
        self.files = AsyncSandboxFiles(self)

    @property
    def _base_url(self) -> str:
        ep = self.endpoint
        if "://" in ep:
            return f"{ep.rstrip('/')}/v1"
        return f"http://{ep}/v1"

    @property
    def phase(self) -> SandboxPhase:
        return self._phase

    def _auth_headers(self) -> dict[str, str]:
        if self._token:
            return {"Authorization": f"Bearer {self._token}"}
        return {}

    def pty_url(self, cols: int = 80, rows: int = 24) -> str:
        base = self._base_url  # http(s)://<endpoint>/v1
        ws_base = base.replace("http://", "ws://", 1).replace("https://", "wss://", 1)
        return f"{ws_base}/pty?sandbox={self.id}&cols={cols}&rows={rows}"

    async def create_pty(self, on_data, cols: int = 80, rows: int = 24):
        """Open an interactive PTY over a WebSocket and return an
        AsyncPtyHandle (send_input, resize, kill, wait -> exit_code). Gated by
        the per-sandbox bearer token, sent in the Authorization header."""
        from mitos.pty import AsyncPtyHandle

        return await AsyncPtyHandle.connect(
            url=self.pty_url(cols, rows),
            token=self._token,
            on_data=on_data,
        )

    async def set_timeout(self, timeout_seconds: int) -> int:
        """Adjust this RUNNING sandbox's TTL to now + timeout_seconds (issue
        #218). Returns the new absolute deadline as a unix timestamp. A value
        over the server ceiling raises TimeoutTooLargeError; the server never
        silently clamps it (issue #216)."""
        _validate_timeout(timeout_seconds)
        resp = await self._http.post(
            f"{self._base_url}/set_timeout",
            json={"sandbox": self.id, "timeout_seconds": timeout_seconds},
            headers=self._auth_headers(),
        )
        raise_for_status(resp, token=self._token)
        return int(resp.json().get("deadline_unix", 0))

    async def pause(self) -> None:
        """Pause this sandbox: snapshot full state (memory + filesystem) and
        stop the clock (issue #218). A paused sandbox is never idle-reaped."""
        resp = await self._http.post(
            f"{self._base_url}/pause",
            json={"sandbox": self.id},
            headers=self._auth_headers(),
        )
        raise_for_status(resp, token=self._token)

    async def resume(self) -> None:
        """Resume a paused sandbox: restore its full state and restart the
        clock (issue #218)."""
        resp = await self._http.post(
            f"{self._base_url}/resume",
            json={"sandbox": self.id},
            headers=self._auth_headers(),
        )
        raise_for_status(resp, token=self._token)

    async def exec(
        self,
        command: str,
        timeout: int = 30,
        working_dir: str = "/workspace",
        env: Optional[dict[str, str]] = None,
        on_stdout: Optional[StreamCallback] = None,
        on_stderr: Optional[StreamCallback] = None,
    ) -> ExecResult:
        """Run a command. With on_stdout/on_stderr it streams /v1/exec/stream
        NDJSON and awaits-or-calls the callback per chunk; without them it uses
        the blocking /v1/exec path. Mirrors the sync Sandbox.exec."""
        _validate_timeout(timeout)
        if on_stdout is None and on_stderr is None:
            payload: dict = {"sandbox": self.id, "command": command,
                             "timeout": timeout, "working_dir": working_dir}
            if env:
                payload["env"] = env
            resp = await self._http.post(
                f"{self._base_url}/exec", json=payload,
                timeout=timeout + 5, headers=self._auth_headers(),
            )
            raise_for_status(resp, token=self._token)
            data = resp.json()
            return ExecResult(
                exit_code=data["exit_code"], stdout=data.get("stdout", ""),
                stderr=data.get("stderr", ""), exec_time_ms=data.get("exec_time_ms", 0),
            )
        return await self._stream_exec(command, timeout, working_dir, env, on_stdout, on_stderr)

    async def _stream_exec(self, command, timeout, working_dir, env, on_stdout, on_stderr) -> ExecResult:
        import json as _json
        payload: dict = {"sandbox": self.id, "command": command,
                         "timeout": timeout, "working_dir": working_dir}
        if env:
            payload["env"] = env
        out_parts: list[bytes] = []
        err_parts: list[bytes] = []
        exit_code = 0
        exec_time_ms = 0.0
        saw_exit = False

        async def emit(cb, chunk):
            if cb is None:
                return
            r = cb(chunk)
            if asyncio.iscoroutine(r):
                await r

        async with self._http.stream(
            "POST", f"{self._base_url}/exec/stream",
            json=payload, timeout=None, headers=self._auth_headers(),
        ) as resp:
            if not resp.is_success:
                await resp.aread()
                raise_for_status(resp, token=self._token)
            async for line in resp.aiter_lines():
                if not line:
                    continue
                frame = _json.loads(line)
                if "exit_code" in frame and "stream" not in frame:
                    exit_code = frame["exit_code"]
                    exec_time_ms = frame.get("exec_time_ms", 0.0)
                    saw_exit = True
                    if frame.get("error"):
                        raise AgentRunError(
                            "exec stream error", code="exec_stream_error",
                            cause=frame["error"],
                            remediation="Inspect the command and the forkd logs for the failure.",
                        )
                    continue
                data = base64.b64decode(frame["data"]) if frame.get("data") else b""
                if frame.get("stream") == "stderr":
                    err_parts.append(data)
                    await emit(on_stderr, data)
                else:
                    out_parts.append(data)
                    await emit(on_stdout, data)
        if not saw_exit:
            raise AgentRunError(
                "exec stream ended before the terminal exit frame",
                code="exec_stream_truncated",
                cause="the connection was truncated or dropped; the exit code is unknown",
                remediation="Retry the command; if it persists, inspect the forkd or sandbox-server logs.",
            )
        if exit_code == EXEC_TIMEOUT_EXIT_CODE:
            # The streaming terminal frame reports 124 inside a 200 response;
            # surface the typed deadline error to match the blocking path's 504
            # exec_timeout (issue #216).
            raise ExecutionDeadlineError(
                f"command exceeded its {timeout}s execution deadline and was terminated",
                code="exec_timeout",
                cause=f"command ran past its {timeout}s deadline (exit 124)",
                remediation="Raise the timeout on the exec call or split the work into shorter steps.",
                context={"timeout_s": timeout},
            )
        return ExecResult(
            exit_code=exit_code,
            stdout=b"".join(out_parts).decode("utf-8", "replace"),
            stderr=b"".join(err_parts).decode("utf-8", "replace"),
            exec_time_ms=exec_time_ms,
        )

    async def wait_until_ready(self, timeout: float = 30.0) -> "AsyncSandbox":
        """Block until Ready (Modal-style), then return self so it chains. Raises
        AgentRunError (sandbox_failed, ready_timeout) otherwise."""
        if self._phase == SandboxPhase.READY and self.endpoint:
            return self
        if self._api is None:
            raise AgentRunError(
                "this AsyncSandbox is not bound to a cluster client",
                code="not_bound",
                cause="wait_until_ready needs the k8s API the AsyncAgentRun client supplies",
                remediation="Create the sandbox through AsyncAgentRun.sandbox(); do not construct AsyncSandbox directly.",
            )
        deadline = asyncio.get_event_loop().time() + timeout
        while asyncio.get_event_loop().time() < deadline:
            obj = await asyncio.to_thread(
                self._api.get_namespaced_custom_object,
                group=API_GROUP, version=API_VERSION, namespace=self.namespace,
                plural="sandboxclaims", name=self.name,
            )
            status = obj.get("status", {})
            self._phase = SandboxPhase(status.get("phase", "Pending"))
            self.endpoint = status.get("endpoint") or self.endpoint
            self.id = status.get("sandboxID") or self.id
            if self._phase == SandboxPhase.READY and self.endpoint:
                await asyncio.to_thread(self._load_token)
                return self
            if self._phase == SandboxPhase.FAILED:
                raise AgentRunError(
                    f"sandbox {self.name} failed", code="sandbox_failed",
                    cause=f"claim {self.name} reached the Failed phase",
                    remediation="Inspect the SandboxClaim status conditions and the pool capacity.",
                )
            await asyncio.sleep(POLL_INTERVAL)
        raise AgentRunError(
            f"sandbox {self.name} not ready after {timeout}s", code="ready_timeout",
            cause=f"claim {self.name} did not reach Ready within {timeout}s",
            remediation="Raise the timeout, or check the controller is reconciling and the pool has capacity.",
        )

    def _load_token(self) -> None:
        if self._core_api is None:
            return
        try:
            secret = self._core_api.read_namespaced_secret(
                name=f"{self.name}-sandbox-token", namespace=self.namespace
            )
        except ApiException:
            return
        token_b64 = (secret.data or {}).get("token")
        if token_b64:
            self._token = base64.b64decode(token_b64).decode()

    async def fork(self, n: int = 1, pause_source: bool = False) -> list["AsyncSandbox"]:
        """Fork into n copies. The CRD create + status poll run in a thread; the
        returned handles are async (own httpx.AsyncClient each)."""
        fork_name = f"{self.name}-fork-{uuid.uuid4().hex[:6]}"
        fork_obj = {
            "apiVersion": f"{API_GROUP}/{API_VERSION}", "kind": "SandboxFork",
            "metadata": {"name": fork_name, "namespace": self.namespace},
            "spec": {"sourceRef": {"name": self.name}, "replicas": n, "pauseSource": pause_source},
        }
        await asyncio.to_thread(
            self._api.create_namespaced_custom_object,
            group=API_GROUP, version=API_VERSION, namespace=self.namespace,
            plural="sandboxforks", body=fork_obj,
        )
        deadline = asyncio.get_event_loop().time() + 30.0
        while asyncio.get_event_loop().time() < deadline:
            obj = await asyncio.to_thread(
                self._api.get_namespaced_custom_object,
                group=API_GROUP, version=API_VERSION, namespace=self.namespace,
                plural="sandboxforks", name=fork_name,
            )
            ready = [f for f in obj.get("status", {}).get("forks", []) if f.get("phase") == "Ready"]
            if len(ready) >= n:
                out = []
                for f in ready:
                    child = AsyncSandbox(
                        id=f.get("sandboxID") or f["name"], endpoint=f.get("endpoint", ""),
                        namespace=self.namespace, pool=self.pool,
                        api=self._api, core_api=self._core_api,
                    )
                    child.name = f["name"]
                    child._phase = SandboxPhase.READY
                    await asyncio.to_thread(child._load_token)
                    out.append(child)
                return out
            await asyncio.sleep(POLL_INTERVAL)
        raise AgentRunError(
            "forks not ready after 30s", code="ready_timeout",
            cause=f"fork {fork_name} did not produce {n} Ready children",
            remediation="Raise the timeout or check pool/node capacity.",
        )

    async def terminate(self) -> None:
        if self._api is not None:
            await asyncio.to_thread(
                self._api.delete_namespaced_custom_object,
                group=API_GROUP, version=API_VERSION, namespace=self.namespace,
                plural="sandboxclaims", name=self.name,
            )
        await self.aclose()

    async def aclose(self) -> None:
        if self._owns_http:
            await self._http.aclose()

    async def __aenter__(self) -> "AsyncSandbox":
        return self

    async def __aexit__(self, *args) -> None:
        await self.terminate()


class AsyncAgentRun:
    """Async cluster client. Mirrors the sync AgentRun hot paths over
    httpx.AsyncClient; the k8s control-plane calls run in a thread."""

    def __init__(
        self,
        namespace: str = "default",
        kubeconfig: Optional[str] = None,
        in_cluster: bool = False,
        allow_default_pool: bool = True,
    ):
        if in_cluster:
            k8s_config.load_incluster_config()
        else:
            k8s_config.load_kube_config(config_file=kubeconfig)
        self._api = k8s_client.CustomObjectsApi()
        self._core_api = k8s_client.CoreV1Api()
        self._namespace = namespace
        self._allow_default_pool = allow_default_pool

    async def sandbox(
        self,
        image: Optional[str] = None,
        pool: Optional[str] = None,
        name: Optional[str] = None,
        ready: bool = False,
    ) -> AsyncSandbox:
        if pool is None and image is None:
            raise AgentRunError(
                "sandbox() needs an image or a pool", code="missing_image_or_pool",
                remediation='Pass image="python" or pool="my-pool".',
            )
        if pool is None:
            if not self._allow_default_pool:
                raise AgentRunError(
                    "default pools are disabled on this client", code="no_default_pool",
                    remediation="Pass pool=<name>, or construct AsyncAgentRun(allow_default_pool=True).",
                )
            pool = await asyncio.to_thread(self._ensure_default_pool, image)
        if name is None:
            name = f"sandbox-{uuid.uuid4().hex[:8]}"
        claim = {
            "apiVersion": f"{API_GROUP}/{API_VERSION}", "kind": "SandboxClaim",
            "metadata": {"name": name, "namespace": self._namespace},
            "spec": {"poolRef": {"name": pool}},
        }
        await asyncio.to_thread(
            self._api.create_namespaced_custom_object,
            group=API_GROUP, version=API_VERSION, namespace=self._namespace,
            plural="sandboxclaims", body=claim,
        )
        sb = AsyncSandbox(
            id=name, endpoint="", namespace=self._namespace, pool=pool,
            api=self._api, core_api=self._core_api,
        )
        sb.name = name
        if ready:
            await sb.wait_until_ready()
        return sb

    async def from_name(self, name: str) -> AsyncSandbox:
        obj = await asyncio.to_thread(
            self._api.get_namespaced_custom_object,
            group=API_GROUP, version=API_VERSION, namespace=self._namespace,
            plural="sandboxclaims", name=name,
        )
        status = obj.get("status", {})
        pool = obj.get("spec", {}).get("poolRef", {}).get("name", "")
        sb = AsyncSandbox(
            id=status.get("sandboxID") or name, endpoint=status.get("endpoint", ""),
            namespace=self._namespace, pool=pool, api=self._api, core_api=self._core_api,
        )
        sb.name = name
        sb._phase = SandboxPhase(status.get("phase", "Pending"))
        if sb._phase == SandboxPhase.READY:
            await asyncio.to_thread(sb._load_token)
        return sb

    def _ensure_default_pool(self, image: str) -> str:
        """get-or-create the default SandboxTemplate + SandboxPool for an image.
        Kept identical to the sync AgentRun._ensure_default_pool: the CRD splits
        image (SandboxTemplate.spec.image) from the pool (SandboxPool.spec.
        templateRef), so both objects are materialized under the same name."""
        name = default_pool_name(image)
        try:
            self._api.get_namespaced_custom_object(
                group=API_GROUP, version=API_VERSION, namespace=self._namespace,
                plural="sandboxpools", name=name,
            )
            return name
        except ApiException as exc:
            if exc.status != 404:
                raise
        template = {
            "apiVersion": f"{API_GROUP}/{API_VERSION}", "kind": "SandboxTemplate",
            "metadata": {"name": name, "namespace": self._namespace},
            "spec": {"image": image},
        }
        self._create_or_reuse(template, "sandboxtemplates")
        pool = {
            "apiVersion": f"{API_GROUP}/{API_VERSION}", "kind": "SandboxPool",
            "metadata": {"name": name, "namespace": self._namespace},
            "spec": {"templateRef": {"name": name}, "replicas": 1},
        }
        self._create_or_reuse(pool, "sandboxpools")
        return name

    def _create_or_reuse(self, body: dict, plural: str) -> None:
        try:
            self._api.create_namespaced_custom_object(
                group=API_GROUP, version=API_VERSION, namespace=self._namespace,
                plural=plural, body=body,
            )
        except ApiException as exc:
            if exc.status != 409:
                raise


# Async parity for the flat one-liner native onboarding path (issue #217). The
# async handle mirrors the sync DirectSandbox: exec / run_code / files / pty /
# fork / terminate against the sandbox-server REST API over httpx.AsyncClient.


class AsyncDirectSandboxFiles:
    """Async file operations on an AsyncDirectSandbox. Mirrors the sync
    DirectSandboxFiles wire calls against the sandbox-server REST API."""

    def __init__(self, sandbox: "AsyncDirectSandbox"):
        self._sb = sandbox

    async def read(self, path: str) -> str:
        resp = await self._sb._http.post(
            f"{self._sb._server_url}/v1/files/read",
            json={"sandbox": self._sb.id, "path": path},
            headers=self._sb._auth_headers(),
        )
        raise_for_status(resp, token=self._sb._api_key)
        return resp.json()["content"]

    async def write(self, path: str, content: Union[str, bytes], mode: int = 0o644) -> None:
        data: dict = {"sandbox": self._sb.id, "path": path, "mode": mode}
        if isinstance(content, bytes):
            data["content"] = content.hex()
            data["binary"] = True
        else:
            data["content"] = content
        resp = await self._sb._http.post(
            f"{self._sb._server_url}/v1/files/write",
            json=data,
            headers=self._sb._auth_headers(),
        )
        raise_for_status(resp, token=self._sb._api_key)

    async def list(self, path: str = "/") -> list[FileInfo]:
        resp = await self._sb._http.post(
            f"{self._sb._server_url}/v1/files/list",
            json={"sandbox": self._sb.id, "path": path},
            headers=self._sb._auth_headers(),
        )
        raise_for_status(resp, token=self._sb._api_key)
        return [
            FileInfo(
                name=f["name"], is_dir=f["is_dir"], size=f["size"],
                mode=f.get("mode", 0), modified_at=f.get("modified_at"),
            )
            for f in resp.json()["entries"]
        ]

    async def remove(self, path: str) -> None:
        resp = await self._sb._http.post(
            f"{self._sb._server_url}/v1/files/remove",
            json={"sandbox": self._sb.id, "path": path},
            headers=self._sb._auth_headers(),
        )
        raise_for_status(resp, token=self._sb._api_key)

    async def mkdir(self, path: str) -> None:
        resp = await self._sb._http.post(
            f"{self._sb._server_url}/v1/files/mkdir",
            json={"sandbox": self._sb.id, "path": path},
            headers=self._sb._auth_headers(),
        )
        raise_for_status(resp, token=self._sb._api_key)


class AsyncDirectSandbox:
    """Async flat handle over the sandbox-server REST API. Returned by
    mitos.aio.create()."""

    def __init__(
        self,
        id: str,
        template: str,
        endpoint: str,
        server_url: str,
        fork_time_ms: float,
        api_key: Optional[str] = None,
        _http: Optional[httpx.AsyncClient] = None,
    ):
        self.id = id
        self.template = template
        self.endpoint = endpoint
        self.fork_time_ms = fork_time_ms
        self._server_url = server_url.rstrip("/")
        self._api_key = api_key
        self._http = _http or httpx.AsyncClient(timeout=30.0)
        self.files = AsyncDirectSandboxFiles(self)

    def _auth_headers(self) -> dict[str, str]:
        if self._api_key:
            return {"Authorization": f"Bearer {self._api_key}"}
        return {}

    async def exec(self, command: str, timeout: int = 30) -> ExecResult:
        _validate_timeout(timeout)
        resp = await self._http.post(
            f"{self._server_url}/v1/exec",
            json={"sandbox": self.id, "command": command, "timeout": timeout},
            timeout=timeout + 5,
            headers=self._auth_headers(),
        )
        raise_for_status(resp, token=self._api_key)
        data = resp.json()
        return ExecResult(
            exit_code=data["exit_code"], stdout=data.get("stdout", ""),
            stderr=data.get("stderr", ""), exec_time_ms=data.get("exec_time_ms", 0),
        )

    async def run_code(
        self,
        code: str,
        language: str = "python",
        timeout: int = 60,
        on_stdout: Optional[Callable[[str], None]] = None,
        on_stderr: Optional[Callable[[str], None]] = None,
        on_result: Optional[Callable[[Result], None]] = None,
    ) -> Execution:
        """Run a code snippet in the sandbox's stateful kernel. Mirrors the sync
        DirectSandbox.run_code; folds the NDJSON stream into an Execution."""
        _validate_timeout(timeout)
        payload = {"sandbox": self.id, "code": code, "language": language, "timeout": timeout}
        lines: list[bytes] = []
        async with self._http.stream(
            "POST", f"{self._server_url}/v1/run_code/stream",
            json=payload, timeout=timeout + 10, headers=self._auth_headers(),
        ) as resp:
            if not resp.is_success:
                await resp.aread()
                raise_for_status_stream(resp, token=self._api_key)
            async for line in resp.aiter_lines():
                if line.strip():
                    lines.append(line.encode() if isinstance(line, str) else line)
        return _parse_run_code_stream(iter(lines), on_stdout, on_stderr, on_result)

    def pty_url(self, cols: int = 80, rows: int = 24) -> str:
        ws_base = self._server_url.replace("http://", "ws://", 1).replace("https://", "wss://", 1)
        return f"{ws_base}/v1/pty?sandbox={self.id}&cols={cols}&rows={rows}"

    async def create_pty(self, on_data, cols: int = 80, rows: int = 24):
        """Open an interactive PTY over a WebSocket and return an
        AsyncPtyHandle. The bearer key is sent in the Authorization header,
        never logged."""
        from mitos.pty import AsyncPtyHandle

        return await AsyncPtyHandle.connect(
            url=self.pty_url(cols, rows), token=self._api_key, on_data=on_data
        )

    async def fork(self, n: int = 1, id: Optional[str] = None) -> list["AsyncDirectSandbox"]:
        """Fork into n independent sibling sandboxes on the server. Each child is
        a READY AsyncDirectSandbox with its own id; the source keeps running."""
        children: list[AsyncDirectSandbox] = []
        for i in range(n):
            child_id = None
            if id is not None:
                child_id = id if n == 1 else f"{id}-{i}"
            children.append(await self._fork_one(child_id))
        return children

    async def _fork_one(self, child_id: Optional[str]) -> "AsyncDirectSandbox":
        if child_id is None:
            child_id = f"sandbox-{uuid.uuid4().hex[:8]}"
        resp = await self._http.post(
            f"{self._server_url}/v1/fork",
            json={"template": self.template, "id": child_id},
            headers=self._auth_headers(),
        )
        raise_for_status(resp, token=self._api_key)
        data = resp.json()
        return AsyncDirectSandbox(
            id=data["id"], template=data["template_id"], endpoint=data["endpoint"],
            server_url=self._server_url, fork_time_ms=data["fork_time_ms"], api_key=self._api_key,
        )

    async def terminate(self) -> None:
        await self._http.delete(
            f"{self._server_url}/v1/sandboxes/{self.id}", headers=self._auth_headers()
        )
        await self._http.aclose()

    async def __aenter__(self) -> "AsyncDirectSandbox":
        return self

    async def __aexit__(self, *args) -> None:
        await self.terminate()

    def __repr__(self) -> str:
        return f"AsyncDirectSandbox(id={self.id!r}, fork_time_ms={self.fork_time_ms:.2f})"


async def create(
    image: str = "python",
    api_key: Optional[str] = None,
    base_url: Optional[str] = None,
    id: Optional[str] = None,
    network: Optional[Network] = None,
) -> AsyncDirectSandbox:
    """Async flat one-liner native onboarding (issue #217). Resolves the API
    key and base URL (explicit arg, else MITOS_API_KEY / MITOS_BASE_URL),
    gets-or-creates the template for image, forks it, and returns a running
    AsyncDirectSandbox. The standalone server is tokenless; the hosted front
    door (#210) verifies the same Authorization header server-side later.

    network is the per-sandbox egress/ingress posture (issue #219); see
    ``mitos.Network``. Omitting it applies the secure deny-by-default both ways.
    This is the async parity of ``mitos.create``."""
    key, url = _resolve_auth(api_key, base_url)
    http = httpx.AsyncClient(timeout=60.0)
    headers = {"Authorization": f"Bearer {key}"} if key else {}
    try:
        # get-or-create the template; a 409 (exists) is success so create is idempotent.
        tmpl_body: dict = {"id": image, "init_wait_seconds": 5}
        if network is not None:
            tmpl_body["network"] = network.to_dict()
        resp = await http.post(
            f"{url}/v1/templates", json=tmpl_body, headers=headers,
        )
        if resp.status_code != 409:
            raise_for_status(resp, token=key)
        if id is None:
            id = f"sandbox-{uuid.uuid4().hex[:8]}"
        resp = await http.post(
            f"{url}/v1/fork", json={"template": image, "id": id}, headers=headers,
        )
        raise_for_status(resp, token=key)
        data = resp.json()
    finally:
        await http.aclose()
    return AsyncDirectSandbox(
        id=data["id"], template=data["template_id"], endpoint=data["endpoint"],
        server_url=url, fork_time_ms=data["fork_time_ms"], api_key=key,
    )
