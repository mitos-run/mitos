"""Flat, API-key-authed native client for the standalone sandbox-server and the
hosted control plane (no Kubernetes required).

The headline one-liner is ``mitos.create(...)`` (aliased as
``Sandbox.create(...)``): given an API key and a base URL it returns a READY
``DirectSandbox`` handle that exposes ``exec`` / ``run_code`` / ``files`` /
``pty`` / ``fork`` / ``terminate`` directly. The k8s operator path
(``AgentRun(...).sandbox(...)``) is unchanged and lives in ``client.py``.

Auth resolution (issue #217): the API key comes from the explicit ``api_key``
argument, else ``MITOS_API_KEY``; the base URL from ``base_url``, else
``MITOS_BASE_URL``, else the hosted production endpoint ``https://mitos.run``.
The key is sent as ``Authorization: Bearer <key>`` on every
request and is NEVER logged or placed in an error message. The standalone
sandbox-server runs tokenless and ignores the header today; the hosted SaaS
front door (#210) will verify the same header without any SDK change, so the
verification simply slots in server-side later.

Usage:
    import mitos

    sb = mitos.create("python")            # MITOS_API_KEY / MITOS_BASE_URL from env
    print(sb.exec("echo hi").stdout)
    sb.terminate()

    # explicit args override the environment
    sb = mitos.create("python", api_key="sk-...", base_url="http://localhost:8080")
"""
from __future__ import annotations

import base64
import json
import os
import uuid
from typing import Callable, Optional

import httpx

from mitos._connect import ConnectClient
from mitos._envelope import raise_for_status
from mitos.errors import AgentRunError, ExecutionDeadlineError
from mitos.types import Execution, ExecResult, ExecutionError, FileInfo, Network, Result
from mitos.sandbox import EXEC_TIMEOUT_EXIT_CODE, _validate_timeout


# Environment variables for the flat onboarding path. Explicit constructor or
# create() arguments always take precedence over these.
ENV_API_KEY = "MITOS_API_KEY"
ENV_BASE_URL = "MITOS_BASE_URL"

# Where `mitos auth login` writes its credential profile. MITOS_CONFIG_DIR
# relocates it (used by tests and by users who move their config); otherwise it
# lives under ~/.config/mitos/credentials.json. Kept in sync with the CLI's
# credentialsPath() (internal/agentcli/auth.go).
ENV_CONFIG_DIR = "MITOS_CONFIG_DIR"


def _credentials_path() -> Optional[str]:
    """Return the path of the CLI login profile, or None if it cannot be located.

    Honors MITOS_CONFIG_DIR, else $HOME/.config/mitos/credentials.json. Returns
    None when no home directory is resolvable rather than raising; a missing path
    just means no credential file fallback is available.
    """
    config_dir = os.environ.get(ENV_CONFIG_DIR)
    if config_dir:
        return os.path.join(config_dir, "credentials.json")
    home = os.path.expanduser("~")
    if not home or home == "~":
        return None
    return os.path.join(home, ".config", "mitos", "credentials.json")


def _token_from_credential_file() -> Optional[str]:
    """Read the bearer token from the CLI login profile, or None.

    A missing, unreadable, or non-JSON file (or one without a ``token``) is NOT
    an error: it simply yields no token, so the SDK stays usable tokenless. The
    token VALUE is never logged.
    """
    path = _credentials_path()
    if not path:
        return None
    try:
        with open(path, "r") as f:
            data = json.load(f)
    except (OSError, ValueError):
        return None
    if not isinstance(data, dict):
        return None
    token = data.get("token")
    if isinstance(token, str) and token:
        return token
    return None

# The hosted production control plane. When neither the base_url argument nor
# MITOS_BASE_URL is set, the flat path targets the hosted endpoint so the
# examples work without a base URL. Self-hosted or local standalone users opt
# out by setting MITOS_BASE_URL (e.g. http://localhost:8080).
DEFAULT_BASE_URL = "https://mitos.run"


def _resolve_auth(
    api_key: Optional[str], base_url: Optional[str]
) -> tuple[Optional[str], str]:
    """Resolve the API key and base URL for the flat path.

    Precedence for the API key: explicit ``api_key`` argument, then
    MITOS_API_KEY, then the bearer token in the CLI login profile written by
    ``mitos auth login`` (so one login authenticates the SDK too), then None
    (tokenless). The credential file is read only as the last fallback and its
    absence is never an error.

    Precedence for the base URL: explicit argument, then MITOS_BASE_URL, then
    the hosted production endpoint (DEFAULT_BASE_URL). The API key is optional
    (the standalone server is tokenless); when present it rides on the
    Authorization header so the hosted front door (#210) can verify it. The
    file token is sent as-is; the gateway decides its validity. The key VALUE is
    never logged or placed in an error message.
    """
    if api_key is not None:
        key: Optional[str] = api_key
    else:
        key = os.environ.get(ENV_API_KEY)
        if key is None:
            key = _token_from_credential_file()
    url = base_url if base_url is not None else os.environ.get(ENV_BASE_URL)
    if not url:
        url = DEFAULT_BASE_URL
    return key, url.rstrip("/")


def _b64_decode(value) -> bytes:
    """Decode a proto-JSON bytes field (base64 string) to raw bytes. None and
    the empty string both decode to empty bytes."""
    if not value:
        return b""
    if isinstance(value, (bytes, bytearray)):
        return bytes(value)
    return base64.b64decode(value)


class DirectSandboxFiles:
    """File operations on a DirectSandbox.

    Speaks the Connect ``sandbox.v1.Sandbox`` file RPCs (ReadFile, WriteFile,
    List, Stat, Mkdir, Remove) the sandbox-server and forkd serve at
    ``/sandbox.v1.Sandbox/<Method>`` (issue #24), instead of the legacy JSON
    ``/v1/files/*`` routes. ReadFile is a server-stream of byte chunks, WriteFile
    a client-stream of chunks; List/Mkdir/Remove are unary. The public method
    signatures and return types are UNCHANGED, so the E2B shim (#206) and the
    framework adapters that sit on this surface are carried onto Connect for
    free.
    """

    def __init__(self, sandbox: "DirectSandbox"):
        self._sb = sandbox

    def read(self, path: str) -> str:
        return self.read_bytes(path).decode("utf-8", "replace")

    def read_bytes(self, path: str) -> bytes:
        """Read a file's bytes via the ReadFile server-stream: concatenate each
        Chunk's bytes until the eof frame. The same bytes back both read (utf-8
        decoded) and read_bytes (raw)."""
        client = self._sb._connect()
        parts: list[bytes] = []
        for frame in client.server_stream("ReadFile", {"path": path}):
            parts.append(_b64_decode(frame.get("data")))
        return b"".join(parts)

    def write(self, path: str, content: str | bytes, mode: int = 0o644) -> None:
        """Write a file via the WriteFile client-stream: an open frame carrying
        the path and mode, then one data frame with the (base64-encoded) bytes.
        str content is utf-8 encoded; bytes are written verbatim."""
        raw = content.encode("utf-8") if isinstance(content, str) else content
        messages = [
            {"open": {"path": path, "mode": mode}},
            {"data": base64.b64encode(raw).decode("ascii")},
        ]
        # WriteFile is a client-stream returning a unary WriteFileResult; the
        # bidi helper drives the half-duplex send-then-read, and the single
        # result frame is consumed (bytes_written is not part of the public API).
        for _ in self._sb._connect().bidi("WriteFile", messages):
            pass

    def list(self, path: str = "/") -> list[FileInfo]:
        resp = self._sb._connect().unary("List", {"parent": path})
        return [
            FileInfo(
                name=f.get("name", ""),
                is_dir=f.get("isDir", False),
                size=int(f.get("size", 0)),
                mode=int(f.get("mode", 0)),
                modified_at=f.get("modifiedAtUnix"),
            )
            for f in (resp.get("entries") or [])
        ]

    def exists(self, path: str) -> bool:
        try:
            self.list(path)
            return True
        except AgentRunError as exc:
            if exc.status == 404:
                return False
            raise

    def remove(self, path: str) -> None:
        self._sb._connect().unary("Remove", {"path": path})

    def mkdir(self, path: str) -> None:
        self._sb._connect().unary("Mkdir", {"path": path})


def _decode_result_data(data: dict) -> dict[str, str]:
    """Decode a Connect RunResult.data map (proto-JSON map<string,bytes>: every
    value is base64) back to the MIME->payload form the Result type expects.

    The guest stores each display value as raw bytes of the kernel's string
    output (text/plain is "42", image/png is the already-base64 string), and
    proto-JSON base64-encodes the byte map for the wire. Decoding the base64 back
    to a utf-8 string recovers exactly the kernel value, so text stays text and
    an image stays its base64 payload, matching Result.text / Result.png."""
    out: dict[str, str] = {}
    for mime, value in (data or {}).items():
        try:
            out[mime] = base64.b64decode(value).decode("utf-8", "replace")
        except Exception:  # noqa: BLE001  not base64: keep the value as-is
            out[mime] = value if isinstance(value, str) else str(value)
    return out


def _parse_run_code_connect(
    frames,
    on_stdout: Optional[Callable[[str], None]],
    on_stderr: Optional[Callable[[str], None]],
    on_result: Optional[Callable[[Result], None]],
) -> Execution:
    """Fold a Connect RunCodeStream response-frame stream into an Execution,
    firing the callbacks live as frames arrive. The proto-JSON frames are the
    RunCodeResponse oneof: stdout/stderr (base64 bytes), result
    (RunResult{text,data}), error (RunError{name,value,traceback}), and the
    terminal exitCode. Result and error payloads are tenant code output and are
    never logged here."""
    ex = Execution()
    saw_exit = False
    for frame in frames:
        if "stdout" in frame:
            text = _b64_decode(frame.get("stdout")).decode("utf-8", "replace")
            ex.logs["stdout"].append(text)
            if on_stdout:
                on_stdout(text)
        elif "stderr" in frame:
            text = _b64_decode(frame.get("stderr")).decode("utf-8", "replace")
            ex.logs["stderr"].append(text)
            if on_stderr:
                on_stderr(text)
        elif "result" in frame:
            payload = frame.get("result") or {}
            data = _decode_result_data(payload.get("data") or {})
            text = payload.get("text") or ""
            is_main = bool(text)
            # The REPL last-value is delivered in RunResult.text; mirror it into
            # the text/plain MIME slot so Result.text resolves the same way the
            # NDJSON path did.
            if is_main and "text/plain" not in data:
                data["text/plain"] = text
            result = Result(data=data, is_main_result=is_main)
            ex.results.append(result)
            if is_main and text:
                ex.text = text
            if on_result:
                on_result(result)
        elif "error" in frame:
            payload = frame.get("error") or {}
            ex.error = ExecutionError(
                name=payload.get("name", ""),
                value=payload.get("value", ""),
                traceback=payload.get("traceback", []) or [],
            )
        elif "exitCode" in frame:
            saw_exit = True
            break
    if not saw_exit:
        # The stream ended before the terminal exit frame: it was truncated or
        # dropped. Surface it rather than a misleading clean Execution.
        raise RuntimeError(
            "run_code stream ended before the terminal exit frame: "
            "the connection was truncated or dropped; the result is unknown"
        )
    return ex


class DirectSandbox:
    """A sandbox connected directly to the sandbox-server / hosted control plane.

    The flat handle returned by mitos.create(). Exposes exec, run_code, files,
    pty, fork, and terminate against the sandbox-server / hosted control plane.
    The exec, run_code, and file calls ride the Connect sandbox.v1.Sandbox
    service (issue #24): exec via the ExecStream server-streaming RPC, run_code
    via RunCodeStream, and files via ReadFile/WriteFile/List/Mkdir/Remove. pty and
    the lifecycle calls (create/fork/terminate/set_timeout/pause/resume/preview)
    ride their existing WebSocket / JSON transports (interactive pty needs bidi /
    HTTP-2, a documented #24 follow-up).
    """

    def __init__(
        self,
        id: str,
        template: str,
        endpoint: str,
        server_url: str,
        fork_time_ms: float,
        api_key: Optional[str] = None,
    ):
        self.id = id
        self.template = template
        self.endpoint = endpoint
        self.fork_time_ms = fork_time_ms
        self._server_url = server_url.rstrip("/")
        self._api_key = api_key
        self._http = httpx.Client(timeout=30.0)
        self.files = DirectSandboxFiles(self)

    @classmethod
    def create(
        cls,
        image: str = "python",
        api_key: Optional[str] = None,
        base_url: Optional[str] = None,
        id: Optional[str] = None,
        network: Optional[Network] = None,
        idempotency_key: Optional[str] = None,
    ) -> "DirectSandbox":
        """Flat one-liner: return a READY sandbox handle for image.

        Resolves the API key and base URL (explicit arg, else MITOS_API_KEY /
        MITOS_BASE_URL), gets-or-creates the template for image, forks it, and
        returns the running DirectSandbox. The standalone sandbox-server is
        tokenless; the hosted path (#210) verifies the same Authorization
        header server-side without an SDK change.

        network is the per-sandbox network posture (issue #219): pass a
        ``Network(...)`` to set egress/ingress allowlists, ``block`` total-deny,
        or CIDR rules. The SECURE DEFAULT when network is omitted is
        deny-by-default in both directions (the server applies it), so an
        untrusted sandbox cannot reach out or be dialed into unless you opt in.
        """
        server = SandboxServer.from_auth(api_key=api_key, base_url=base_url)
        server.ensure_template(image, network=network)
        return server.fork(image, id=id, idempotency_key=idempotency_key)

    def _auth_headers(self) -> dict[str, str]:
        """Bearer auth for the sandbox API; empty when no key is configured.

        The standalone server is tokenless and ignores it; the hosted front
        door verifies it. The key VALUE is never logged.
        """
        if self._api_key:
            return {"Authorization": f"Bearer {self._api_key}"}
        return {}

    def _connect(self) -> ConnectClient:
        """A Connect client for this sandbox's runtime RPCs. It addresses the
        sandbox by id via the X-Sandbox-Id header (the server routes on it in
        both the tokenless standalone case and the hosted/forkd bearer case) and
        sends the optional bearer key. The key VALUE is never logged."""
        return ConnectClient(self._http, self._server_url, self.id, self._api_key)

    def exec(self, command: str, timeout: int = 30) -> ExecResult:
        """Run a command and return its aggregate result.

        Drives the Connect ``ExecStream`` server-streaming RPC (a non-interactive
        exec: the unary request fully describes the command, the reply is a
        stream of stdout/stderr chunks then a terminal ExecExit). Connect serves
        server-streaming over HTTP/1.1, so the dependency-light httpx client
        reaches it. A command killed at its execution deadline reports exit 124,
        surfaced as the typed ExecutionDeadlineError (matching the cluster path).
        The public return shape is unchanged."""
        _validate_timeout(timeout)
        out = bytearray()
        err = bytearray()
        exit_code = 0
        exec_time_ms = 0.0
        req = {"command": command, "timeoutSeconds": timeout}
        for frame in self._connect().server_stream("ExecStream", req, timeout=timeout + 5):
            if "stdout" in frame:
                out.extend(_b64_decode(frame.get("stdout")))
            elif "stderr" in frame:
                err.extend(_b64_decode(frame.get("stderr")))
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
                remediation=(
                    "Raise the timeout on the exec call or split the work into shorter steps."
                ),
                context={"timeout_s": timeout},
            )
        return ExecResult(
            exit_code=exit_code,
            stdout=out.decode("utf-8", "replace"),
            stderr=err.decode("utf-8", "replace"),
            exec_time_ms=exec_time_ms,
        )

    def run_code(
        self,
        code: str,
        language: str = "python",
        timeout: int = 60,
        on_stdout: Optional[Callable[[str], None]] = None,
        on_stderr: Optional[Callable[[str], None]] = None,
        on_result: Optional[Callable[[Result], None]] = None,
    ) -> Execution:
        """Run a code snippet in the sandbox's stateful kernel (sandbox-server
        mode). State persists across calls for the sandbox lifetime. Returns an
        Execution and streams via the callbacks; requires a base image with the
        code-interpreter kernel, else the Execution carries a KernelUnavailable
        error.

        Drives the Connect ``RunCodeStream`` server-streaming RPC (a
        non-interactive run: the unary request fully describes the snippet, the
        reply is a stream of stdout/stderr chunks, rich RunResult frames, the
        structured RunError, and the terminal exit code). Connect serves
        server-streaming over HTTP/1.1, so the httpx client reaches it. The
        callbacks fire live as frames arrive; the public return shape is
        unchanged."""
        _validate_timeout(timeout)
        req = {"code": code, "language": language, "timeoutSeconds": timeout}
        return _parse_run_code_connect(
            self._connect().server_stream("RunCodeStream", req, timeout=timeout + 10),
            on_stdout,
            on_stderr,
            on_result,
        )

    def pty_url(self, cols: int = 80, rows: int = 24) -> str:
        """The WebSocket URL for an interactive PTY in this sandbox. The bearer
        key (when set) is sent in the Authorization header on connect, never on
        the URL."""
        ws_base = self._server_url.replace("http://", "ws://", 1).replace(
            "https://", "wss://", 1
        )
        return f"{ws_base}/v1/pty?sandbox={self.id}&cols={cols}&rows={rows}"

    def pty(self, on_data: Callable[[bytes], None], cols: int = 80, rows: int = 24):
        """Open an interactive PTY (a shell) in the sandbox over a WebSocket.

        Output bytes arrive on on_data on a background thread. Returns a
        PtyHandle with send_input(bytes), resize(cols, rows), kill(), and
        wait() -> exit_code. The bearer key is sent in the Authorization
        header, never logged."""
        from mitos.pty import PtyHandle

        return PtyHandle(url=self.pty_url(cols, rows), token=self._api_key, on_data=on_data)

    def set_timeout(self, timeout_seconds: int) -> int:
        """Adjust this RUNNING sandbox's TTL to now + timeout_seconds (issue
        #218). Returns the new absolute deadline as a unix timestamp. A value
        over the server ceiling raises TimeoutTooLargeError; the server never
        silently clamps it (issue #216). This is the native method the E2B
        compat shim (#206) maps its setTimeout onto."""
        _validate_timeout(timeout_seconds)
        resp = self._http.post(
            f"{self._server_url}/v1/set_timeout",
            json={"sandbox": self.id, "timeout_seconds": timeout_seconds},
            headers=self._auth_headers(),
        )
        raise_for_status(resp, token=self._api_key)
        return int(resp.json().get("deadline_unix", 0))

    def pause(self) -> None:
        """Pause this sandbox: snapshot full state (memory + filesystem) and
        stop the clock (issue #218). On a real forkd the VM is snapshotted and
        held; a paused sandbox is never idle-reaped. Resume restores it."""
        resp = self._http.post(
            f"{self._server_url}/v1/pause",
            json={"sandbox": self.id},
            headers=self._auth_headers(),
        )
        raise_for_status(resp, token=self._api_key)

    def resume(self) -> None:
        """Resume a paused sandbox: restore its full state and restart the
        clock (issue #218)."""
        resp = self._http.post(
            f"{self._server_url}/v1/resume",
            json={"sandbox": self.id},
            headers=self._auth_headers(),
        )
        raise_for_status(resp, token=self._api_key)

    def get_host(self, port: int = 80) -> str:
        """Return a signed, expiring preview URL for a port on this sandbox
        (issue #126; E2B ``sandbox.get_host(port)`` maps onto this).

        The URL has the shape ``https://<sandbox-id>.preview.<domain>/?token=...``
        and is served by the per-sandbox preview reverse proxy: it routes the
        vhost to this sandbox's backend, verifies the signed token plus the
        per-sandbox bearer gate, and proxies to ``port`` inside the sandbox. The
        token expires (Daytona style), so the URL stops working after its TTL.

        The signing secret lives on the server, so the SDK asks the server to
        mint the URL (POST /v1/preview) and returns it; the URL VALUE carries a
        bearer credential and should be treated as a secret. A server that does
        not yet expose the preview proxy returns a typed error.
        """
        if not isinstance(port, int) or port < 1 or port > 65535:
            raise ValueError(f"port {port!r} out of range 1-65535")
        resp = self._http.post(
            f"{self._server_url}/v1/preview",
            json={"sandbox": self.id, "port": port},
            headers=self._auth_headers(),
        )
        raise_for_status(resp, token=self._api_key)
        url = resp.json().get("url", "")
        if not url:
            raise ValueError("server returned no preview URL")
        return url

    def fork(self, n: int = 1, id: Optional[str] = None) -> list["DirectSandbox"]:
        """Fork this sandbox into n independent sibling copies on the server.

        On the standalone server a fork re-forks the same template into a fresh,
        independent sandbox (the snapshot-fork engine reseeds each child's CRNG
        before it is served). Each child is a READY DirectSandbox with its own
        id. Returns the list of children; the source keeps running.
        """
        children: list[DirectSandbox] = []
        for i in range(n):
            child_id = None
            if id is not None:
                child_id = id if n == 1 else f"{id}-{i}"
            child = self._fork_one(child_id)
            children.append(child)
        return children

    def _fork_one(
        self, child_id: Optional[str], idempotency_key: Optional[str] = None
    ) -> "DirectSandbox":
        if child_id is None:
            child_id = f"sandbox-{uuid.uuid4().hex[:8]}"
        # Auto-generate a key when the caller gave none so a transparently
        # retried fork never double-creates a sibling (issue #22).
        if idempotency_key is None:
            idempotency_key = uuid.uuid4().hex
        headers = self._auth_headers()
        headers["Idempotency-Key"] = idempotency_key
        resp = self._http.post(
            f"{self._server_url}/v1/fork",
            json={"template": self.template, "id": child_id},
            headers=headers,
        )
        raise_for_status(resp, token=self._api_key)
        data = resp.json()
        return DirectSandbox(
            id=data["id"],
            template=data["template_id"],
            endpoint=data["endpoint"],
            server_url=self._server_url,
            fork_time_ms=data["fork_time_ms"],
            api_key=self._api_key,
        )

    def terminate(self) -> None:
        self._http.delete(
            f"{self._server_url}/v1/sandboxes/{self.id}",
            headers=self._auth_headers(),
        )
        self._http.close()

    def __enter__(self) -> DirectSandbox:
        return self

    def __exit__(self, *args) -> None:
        self.terminate()

    def __repr__(self) -> str:
        return f"DirectSandbox(id={self.id!r}, fork_time_ms={self.fork_time_ms:.2f})"


class SandboxServer:
    """Client for the sandbox-server REST API (standalone mode, no k8s).

    Carries the optional API key resolved for the flat onboarding path so every
    DirectSandbox it produces sends the same Authorization header.
    """

    def __init__(self, url: str = DEFAULT_BASE_URL, api_key: Optional[str] = None):
        self.url = url.rstrip("/")
        self._api_key = api_key
        self._http = httpx.Client(timeout=60.0)

    @classmethod
    def from_auth(
        cls, api_key: Optional[str] = None, base_url: Optional[str] = None
    ) -> "SandboxServer":
        """Build a SandboxServer from the resolved auth (arg, else env)."""
        key, url = _resolve_auth(api_key, base_url)
        return cls(url=url, api_key=key)

    def _auth_headers(self) -> dict[str, str]:
        if self._api_key:
            return {"Authorization": f"Bearer {self._api_key}"}
        return {}

    def _creating_headers(self, idempotency_key: Optional[str]) -> dict[str, str]:
        """Auth headers plus an optional Idempotency-Key for a creating call.

        A creating call (template create, fork) that carries an idempotency key
        is safe to retry: the server returns the resource the first call created
        instead of a duplicate (issue #22). The key VALUE is an opaque caller
        token, never a secret, so it travels as a plain header.
        """
        headers = self._auth_headers()
        if idempotency_key:
            headers["Idempotency-Key"] = idempotency_key
        return headers

    def health(self) -> dict:
        resp = self._http.get(f"{self.url}/v1/health", headers=self._auth_headers())
        raise_for_status(resp, token=self._api_key)
        return resp.json()

    def list_templates(self) -> list[dict]:
        resp = self._http.get(f"{self.url}/v1/templates", headers=self._auth_headers())
        raise_for_status(resp, token=self._api_key)
        return resp.json()

    def create_template(
        self,
        id: str,
        init_wait_seconds: int = 5,
        network: Optional[Network] = None,
        idempotency_key: Optional[str] = None,
    ) -> dict:
        """Create the template for id, optionally attaching a per-sandbox network
        posture (issue #219). The network applies to every sandbox forked from
        the template. Omit it for the secure default (deny-by-default both ways,
        applied server-side).

        idempotency_key (issue #22): when set, a retried create with the same key
        returns the template the first call created instead of a duplicate."""
        body: dict = {"id": id, "init_wait_seconds": init_wait_seconds}
        if network is not None:
            body["network"] = network.to_dict()
        resp = self._http.post(
            f"{self.url}/v1/templates",
            json=body,
            headers=self._creating_headers(idempotency_key),
        )
        raise_for_status(resp, token=self._api_key)
        return resp.json()

    def ensure_template(
        self,
        id: str,
        init_wait_seconds: int = 5,
        network: Optional[Network] = None,
        idempotency_key: Optional[str] = None,
    ) -> dict:
        """get-or-create the template for id. A 409 (already exists) is treated
        as success so the flat create path is idempotent across calls. network
        is the per-sandbox posture attached at create time (issue #219).
        idempotency_key (issue #22) makes a retried create safe server-side."""
        try:
            return self.create_template(
                id,
                init_wait_seconds=init_wait_seconds,
                network=network,
                idempotency_key=idempotency_key,
            )
        except AgentRunError as exc:
            if exc.status == 409:
                return {"id": id, "ready": True}
            raise

    def fork(
        self,
        template: str,
        id: Optional[str] = None,
        idempotency_key: Optional[str] = None,
    ) -> DirectSandbox:
        """Fork template into a fresh sandbox. idempotency_key (issue #22): when
        set, a retried fork with the same key returns the sandbox the first call
        created instead of a duplicate; when None one is auto-generated so a
        transparently retried fork never double-creates."""
        if id is None:
            id = f"sandbox-{uuid.uuid4().hex[:8]}"
        if idempotency_key is None:
            idempotency_key = uuid.uuid4().hex
        resp = self._http.post(
            f"{self.url}/v1/fork",
            json={"template": template, "id": id},
            headers=self._creating_headers(idempotency_key),
        )
        raise_for_status(resp, token=self._api_key)
        data = resp.json()
        return DirectSandbox(
            id=data["id"],
            template=data["template_id"],
            endpoint=data["endpoint"],
            server_url=self.url,
            fork_time_ms=data["fork_time_ms"],
            api_key=self._api_key,
        )

    def list_sandboxes(self) -> list[dict]:
        resp = self._http.get(f"{self.url}/v1/sandboxes", headers=self._auth_headers())
        raise_for_status(resp, token=self._api_key)
        return resp.json()


def create(
    image: str = "python",
    api_key: Optional[str] = None,
    base_url: Optional[str] = None,
    id: Optional[str] = None,
    network: Optional[Network] = None,
    idempotency_key: Optional[str] = None,
) -> DirectSandbox:
    """Flat one-liner native onboarding: return a READY sandbox handle.

    The canonical hosted / standalone entry point. Resolves the API key and
    base URL (explicit arg, else MITOS_API_KEY / MITOS_BASE_URL), gets-or-
    creates the template for image, and returns a running DirectSandbox that
    exposes exec / run_code / files / pty / fork / terminate.

    network is the per-sandbox egress/ingress posture (issue #219); see
    ``mitos.Network``. Omitting it applies the secure deny-by-default both ways.

    idempotency_key (issue #22): a creating call accepts an optional key so a
    retried create returns the SAME sandbox instead of a duplicate. When None,
    the fork step auto-generates one, so even a transparently retried create is
    safe; pass your own to make a retry across processes idempotent.

    The k8s operator path stays available as AgentRun(...).sandbox(...).
    """
    return DirectSandbox.create(
        image,
        api_key=api_key,
        base_url=base_url,
        id=id,
        network=network,
        idempotency_key=idempotency_key,
    )
