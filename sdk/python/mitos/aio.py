from __future__ import annotations

import asyncio
import base64
import uuid
from typing import TYPE_CHECKING, Awaitable, Callable, Optional, Union

import httpx

from mitos._connect import AsyncConnectClient
from mitos._k8s import k8s
from mitos._runtime import (
    aparse_run_code_connect,
    b64_decode as _b64_decode,
    fileinfo_from_proto as _fileinfo_from_proto,
)
from mitos._envelope import raise_for_status
from mitos.client import API_GROUP, API_VERSION, default_pool_name
from mitos.direct import _resolve_auth
from mitos.errors import AgentRunError, ExecutionDeadlineError
from mitos.sandbox import (
    EXEC_TIMEOUT_EXIT_CODE,
    _validate_timeout,
)
from mitos.types import Execution, ExecResult, FileInfo, Network, Result, SandboxPhase

if TYPE_CHECKING:
    from kubernetes import client as k8s_client

POLL_INTERVAL = 0.05

# A stdout/stderr callback may be sync or async.
StreamCallback = Callable[[bytes], Union[Awaitable[None], None]]


async def _aemit(cb: Optional[StreamCallback], chunk: bytes) -> None:
    """Fire an exec stream callback, awaiting it when it returns a coroutine so
    a sync or async callback both work."""
    if cb is None:
        return
    r = cb(chunk)
    if asyncio.iscoroutine(r):
        await r


async def _aexec_stream(
    client: AsyncConnectClient,
    command: str,
    timeout: int,
    working_dir: str,
    env: Optional[dict[str, str]],
    on_stdout: Optional[StreamCallback],
    on_stderr: Optional[StreamCallback],
) -> ExecResult:
    """Drive the Connect ``ExecStream`` server-streaming RPC and fold the
    proto-JSON ExecResponse frames into an ExecResult, firing the callbacks live.

    The request is the ExecStreamRequest unary shape (camelCase: command, args,
    env, cwd, timeoutSeconds); the reply frames are the ExecResponse oneof
    (stdout/stderr base64 bytes, then a terminal ExecExit{exitCode, execTimeMs,
    error}). A spawn failure rides ExecExit.error; exit 124 is the execution
    deadline, surfaced as the typed ExecutionDeadlineError. Shared by the async
    k8s and direct sandboxes so the exec wire folding is written once."""
    req: dict = {"command": command, "timeoutSeconds": timeout}
    if working_dir:
        req["cwd"] = working_dir
    if env:
        req["env"] = [{"key": k, "value": v} for k, v in env.items()]
    out = bytearray()
    err = bytearray()
    exit_code = 0
    exec_time_ms = 0.0
    async for frame in client.server_stream("ExecStream", req, timeout=timeout + 5):
        if "stdout" in frame:
            chunk = _b64_decode(frame.get("stdout"))
            out.extend(chunk)
            await _aemit(on_stdout, chunk)
        elif "stderr" in frame:
            chunk = _b64_decode(frame.get("stderr"))
            err.extend(chunk)
            await _aemit(on_stderr, chunk)
        elif "exit" in frame:
            ex = frame.get("exit") or {}
            exit_code = int(ex.get("exitCode", 0))
            exec_time_ms = float(ex.get("execTimeMs", 0.0))
            # A spawn/transport failure rides ExecExit.error (an LLM-legible
            # remediation string, never a secret); surface it rather than a
            # misleading clean exit.
            if ex.get("error"):
                raise AgentRunError(
                    "exec failed",
                    code="exec_failed",
                    cause=str(ex["error"]),
                    remediation="Check the command is accessible in the sandbox filesystem.",
                )
    if exit_code == EXEC_TIMEOUT_EXIT_CODE:
        raise ExecutionDeadlineError(
            f"command exceeded its {timeout}s execution deadline and was terminated",
            code="exec_timeout",
            cause=f"command ran past its {timeout}s deadline (exit 124)",
            remediation="Raise the timeout on the exec call or split the work into shorter steps.",
            context={"timeout_s": timeout},
        )
    return ExecResult(
        exit_code=exit_code,
        stdout=out.decode("utf-8", "replace"),
        stderr=err.decode("utf-8", "replace"),
        exec_time_ms=exec_time_ms,
    )


class AsyncSandboxFiles:
    """Async file operations. Mirrors mitos.sandbox.SandboxFiles.

    Speaks the Connect ``sandbox.v1.Sandbox`` file RPCs (ReadFile, WriteFile,
    List) over the AsyncConnectClient instead of the legacy JSON ``/v1/files/*``
    routes (issue #24). ReadFile is a server-stream of byte chunks, WriteFile a
    client-stream; List is unary. The public async signatures and return types
    are UNCHANGED."""

    def __init__(self, sandbox: "AsyncSandbox"):
        self._sb = sandbox

    async def read(self, path: str) -> str:
        return (await self.read_bytes(path)).decode("utf-8", "replace")

    async def read_bytes(self, path: str) -> bytes:
        """Read a file's bytes via the ReadFile server-stream: concatenate each
        Chunk's bytes until the eof frame."""
        parts: list[bytes] = []
        async for frame in self._sb._aconnect().server_stream("ReadFile", {"path": path}):
            parts.append(_b64_decode(frame.get("data")))
        return b"".join(parts)

    async def write(self, path: str, content: Union[str, bytes], mode: int = 0o644) -> None:
        """Write a file via the WriteFile client-stream: an open frame with the
        path and mode, then one data frame with the base64-encoded bytes."""
        raw = content.encode("utf-8") if isinstance(content, str) else content
        messages = [
            {"open": {"path": path, "mode": mode}},
            {"data": base64.b64encode(raw).decode("ascii")},
        ]
        async for _ in self._sb._aconnect().bidi("WriteFile", messages):
            pass

    async def list(self, path: str = "/") -> list[FileInfo]:
        resp = await self._sb._aconnect().unary("List", {"parent": path})
        return [_fileinfo_from_proto(f) for f in (resp.get("entries") or [])]


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
    def _connect_base(self) -> str:
        """The Connect server base for this sandbox's runtime RPCs. The Connect
        paths are ``/sandbox.v1.Sandbox/<Method>`` off the endpoint root, so
        unlike _base_url this carries no ``/v1`` suffix."""
        ep = self.endpoint
        if "://" in ep:
            return ep.rstrip("/")
        return f"http://{ep}"

    def _aconnect(self) -> AsyncConnectClient:
        """An async Connect client for this sandbox's runtime RPCs (exec, files,
        run_code). It addresses the sandbox by id via the X-Sandbox-Id header
        (forkd routes on it) and sends the optional bearer token. The token VALUE
        is never logged."""
        return AsyncConnectClient(
            self._http, self._connect_base, self.id, self._token
        )

    @property
    def phase(self) -> SandboxPhase:
        return self._phase

    def _auth_headers(self) -> dict[str, str]:
        if self._token:
            return {"Authorization": f"Bearer {self._token}"}
        return {}

    def pty_url(self) -> str:
        # The Connect ``sandbox.v1.Sandbox.Exec`` bidi route over a WebSocket,
        # mounted at the endpoint root (no /v1 suffix). The window size rides the
        # open ExecRequest frame, not the query.
        root = self._connect_base  # http(s)://<endpoint>
        ws_base = root.replace("http://", "ws://", 1).replace("https://", "wss://", 1)
        return f"{ws_base}/sandbox.v1.Sandbox/Exec?sandbox={self.id}"

    async def create_pty(self, on_data, cols: int = 80, rows: int = 24):
        """Open an interactive PTY over a WebSocket and return an
        AsyncPtyHandle (send_input, resize, kill, wait -> exit_code). Gated by
        the per-sandbox bearer token, sent in the Authorization header."""
        from mitos.pty import AsyncPtyHandle

        return await AsyncPtyHandle.connect(
            url=self.pty_url(),
            token=self._token,
            on_data=on_data,
            cols=cols,
            rows=rows,
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
        """Run a command and return its aggregate result.

        Drives the Connect ``ExecStream`` server-streaming RPC (issue #24): a
        non-interactive exec whose unary request fully describes the command,
        with a reply stream of stdout/stderr chunks then a terminal ExecExit.
        When on_stdout/on_stderr are given they fire (awaited if a coroutine) per
        chunk as it arrives. A command killed at its execution deadline reports
        exit 124, surfaced as the typed ExecutionDeadlineError. The public return
        shape is unchanged."""
        _validate_timeout(timeout)
        return await _aexec_stream(
            self._aconnect(), command, timeout, working_dir, env, on_stdout, on_stderr
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
                plural="sandboxes", name=self.name,
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
                    cause=f"sandbox {self.name} reached the Failed phase",
                    remediation="Inspect the Sandbox status conditions and the pool capacity.",
                )
            await asyncio.sleep(POLL_INTERVAL)
        raise AgentRunError(
            f"sandbox {self.name} not ready after {timeout}s", code="ready_timeout",
            cause=f"sandbox {self.name} did not reach Ready within {timeout}s",
            remediation="Raise the timeout, or check the controller is reconciling and the pool has capacity.",
        )

    def _load_token(self) -> None:
        if self._core_api is None:
            return
        try:
            secret = self._core_api.read_namespaced_secret(
                name=f"{self.name}-sandbox-token", namespace=self.namespace
            )
        except k8s().ApiException:
            return
        token_b64 = (secret.data or {}).get("token")
        if token_b64:
            self._token = base64.b64decode(token_b64).decode()

    async def fork(self, n: int = 1, pause_source: bool = False) -> list["AsyncSandbox"]:
        """Fork into n copies. The CRD create + status poll run in a thread; the
        returned handles are async (own httpx.AsyncClient each)."""
        fork_name = f"{self.name}-fork-{uuid.uuid4().hex[:6]}"
        fork_obj = {
            "apiVersion": f"{API_GROUP}/{API_VERSION}", "kind": "Sandbox",
            "metadata": {"name": fork_name, "namespace": self.namespace},
            "spec": {
                "source": {"fromSandbox": {"name": self.name, "pauseSource": pause_source}},
                "replicas": n,
            },
        }
        await asyncio.to_thread(
            self._api.create_namespaced_custom_object,
            group=API_GROUP, version=API_VERSION, namespace=self.namespace,
            plural="sandboxes", body=fork_obj,
        )
        deadline = asyncio.get_event_loop().time() + 30.0
        while asyncio.get_event_loop().time() < deadline:
            obj = await asyncio.to_thread(
                self._api.get_namespaced_custom_object,
                group=API_GROUP, version=API_VERSION, namespace=self.namespace,
                plural="sandboxes", name=fork_name,
            )
            status = obj.get("status", {})
            # A refused fork (secret inheritance, capacity, budget) is terminal:
            # surface the Rejected condition as an LLM-legible error (#311).
            for c in status.get("conditions", []) or []:
                if c.get("type") == "Rejected" and c.get("status") == "True":
                    raise AgentRunError(
                        f"fork {fork_name} was rejected",
                        code="fork_rejected",
                        cause=c.get("reason", "Rejected"),
                        remediation=c.get(
                            "message",
                            "Inspect the fork's Rejected condition and adjust the request.",
                        ),
                    )
            ready = [f for f in status.get("children", []) if f.get("phase") == "Ready"]
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
                plural="sandboxes", name=self.name,
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
        _k8s = k8s()
        if in_cluster:
            _k8s.config.load_incluster_config()
        else:
            _k8s.config.load_kube_config(config_file=kubeconfig)
        self._api = _k8s.client.CustomObjectsApi()
        self._core_api = _k8s.client.CoreV1Api()
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
        sandbox_body = {
            "apiVersion": f"{API_GROUP}/{API_VERSION}", "kind": "Sandbox",
            "metadata": {"name": name, "namespace": self._namespace},
            "spec": {"source": {"poolRef": {"name": pool}}},
        }
        await asyncio.to_thread(
            self._api.create_namespaced_custom_object,
            group=API_GROUP, version=API_VERSION, namespace=self._namespace,
            plural="sandboxes", body=sandbox_body,
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
            plural="sandboxes", name=name,
        )
        status = obj.get("status", {})
        pool = obj.get("spec", {}).get("source", {}).get("poolRef", {}).get("name", "")
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
        """get-or-create the default SandboxPool for an image. In v1 the
        SandboxTemplate kind is removed; the image lives as inline
        SandboxPool.spec.template.image. A pre-existing pool is reused
        untouched; a missing one is created as a single SandboxPool."""
        name = default_pool_name(image)
        try:
            self._api.get_namespaced_custom_object(
                group=API_GROUP, version=API_VERSION, namespace=self._namespace,
                plural="sandboxpools", name=name,
            )
            return name
        except k8s().ApiException as exc:
            if exc.status != 404:
                raise
        pool = {
            "apiVersion": f"{API_GROUP}/{API_VERSION}", "kind": "SandboxPool",
            "metadata": {"name": name, "namespace": self._namespace},
            "spec": {"template": {"image": image}, "replicas": 1},
        }
        self._create_or_reuse(pool, "sandboxpools")
        return name

    def _create_or_reuse(self, body: dict, plural: str) -> None:
        try:
            self._api.create_namespaced_custom_object(
                group=API_GROUP, version=API_VERSION, namespace=self._namespace,
                plural=plural, body=body,
            )
        except k8s().ApiException as exc:
            if exc.status != 409:
                raise


# Async parity for the flat one-liner native onboarding path (issue #217). The
# async handle mirrors the sync DirectSandbox: exec / run_code / files / pty /
# fork / terminate against the sandbox-server REST API over httpx.AsyncClient.


class AsyncDirectSandboxFiles:
    """Async file operations on an AsyncDirectSandbox.

    Speaks the Connect ``sandbox.v1.Sandbox`` file RPCs (ReadFile, WriteFile,
    List, Mkdir, Remove) over the AsyncConnectClient instead of the legacy JSON
    ``/v1/files/*`` routes (issue #24), mirroring the sync DirectSandboxFiles.
    ReadFile is a server-stream of byte chunks, WriteFile a client-stream;
    List/Mkdir/Remove are unary. The public async signatures and return types
    are UNCHANGED."""

    def __init__(self, sandbox: "AsyncDirectSandbox"):
        self._sb = sandbox

    async def read(self, path: str) -> str:
        return (await self.read_bytes(path)).decode("utf-8", "replace")

    async def read_bytes(self, path: str) -> bytes:
        """Read a file's bytes via the ReadFile server-stream: concatenate each
        Chunk's bytes until the eof frame."""
        parts: list[bytes] = []
        async for frame in self._sb._aconnect().server_stream("ReadFile", {"path": path}):
            parts.append(_b64_decode(frame.get("data")))
        return b"".join(parts)

    async def write(self, path: str, content: Union[str, bytes], mode: int = 0o644) -> None:
        """Write a file via the WriteFile client-stream: an open frame with the
        path and mode, then one data frame with the base64-encoded bytes."""
        raw = content.encode("utf-8") if isinstance(content, str) else content
        messages = [
            {"open": {"path": path, "mode": mode}},
            {"data": base64.b64encode(raw).decode("ascii")},
        ]
        async for _ in self._sb._aconnect().bidi("WriteFile", messages):
            pass

    async def list(self, path: str = "/") -> list[FileInfo]:
        resp = await self._sb._aconnect().unary("List", {"parent": path})
        return [_fileinfo_from_proto(f) for f in (resp.get("entries") or [])]

    async def remove(self, path: str) -> None:
        await self._sb._aconnect().unary("Remove", {"path": path})

    async def mkdir(self, path: str) -> None:
        await self._sb._aconnect().unary("Mkdir", {"path": path})


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

    def _aconnect(self) -> AsyncConnectClient:
        """An async Connect client for this sandbox's runtime RPCs (exec,
        run_code, files). It addresses the sandbox by id via the X-Sandbox-Id
        header (the server routes on it in both the tokenless standalone case and
        the hosted bearer case) and sends the optional bearer key. The key VALUE
        is never logged."""
        return AsyncConnectClient(
            self._http, self._server_url, self.id, self._api_key
        )

    async def exec(self, command: str, timeout: int = 30) -> ExecResult:
        """Run a command and return its aggregate result.

        Drives the Connect ``ExecStream`` server-streaming RPC (issue #24): a
        non-interactive exec whose unary request fully describes the command,
        with a reply stream of stdout/stderr chunks then a terminal ExecExit.
        Mirrors the sync DirectSandbox.exec; the public return shape is
        unchanged."""
        _validate_timeout(timeout)
        return await _aexec_stream(
            self._aconnect(), command, timeout, "", None, None, None
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
        DirectSandbox.run_code.

        Drives the Connect ``RunCodeStream`` server-streaming RPC (issue #24): a
        non-interactive run whose unary request fully describes the snippet, with
        a reply stream of stdout/stderr chunks, rich RunResult frames, the
        structured RunError, then the terminal exit code; the callbacks fire live
        as frames arrive. The public return shape is unchanged."""
        _validate_timeout(timeout)
        req = {"code": code, "language": language, "timeoutSeconds": timeout}
        return await aparse_run_code_connect(
            self._aconnect().server_stream("RunCodeStream", req, timeout=timeout + 10),
            on_stdout,
            on_stderr,
            on_result,
        )

    def pty_url(self) -> str:
        # The Connect ``sandbox.v1.Sandbox.Exec`` bidi route over a WebSocket,
        # mounted at the server root. The window size rides the open ExecRequest
        # frame, not the query.
        ws_base = self._server_url.replace("http://", "ws://", 1).replace("https://", "wss://", 1)
        return f"{ws_base}/sandbox.v1.Sandbox/Exec?sandbox={self.id}"

    async def create_pty(self, on_data, cols: int = 80, rows: int = 24):
        """Open an interactive PTY over a WebSocket and return an
        AsyncPtyHandle. The bearer key is sent in the Authorization header,
        never logged."""
        from mitos.pty import AsyncPtyHandle

        return await AsyncPtyHandle.connect(
            url=self.pty_url(),
            token=self._api_key,
            on_data=on_data,
            cols=cols,
            rows=rows,
        )

    async def fork(self, n: int = 1, id: Optional[str] = None) -> list["AsyncDirectSandbox"]:
        """Fork this RUNNING sandbox into n independent sibling sandboxes (issue
        #596). A LIVE fork: each child inherits this sandbox's live memory AND its
        current on-disk filesystem, not a re-fork of the cold template. Each child
        is a READY AsyncDirectSandbox with its own id; the source keeps running."""
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
        # Live fork of THIS running sandbox: POST to the per-sandbox fork route so
        # the server checkpoints this sandbox (memory + on-disk filesystem) and
        # boots the child from it. template stays in the body for hosted-gateway
        # compatibility (the gateway maps this route to sandbox.create, which reads
        # template as the pool); pause_source freezes the parent across the
        # checkpoint so memory and disk are captured consistently.
        resp = await self._http.post(
            f"{self._server_url}/v1/sandboxes/{self.id}/fork",
            json={"id": child_id, "template": self.template, "pause_source": True},
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
