"""Daytona-compat shim: ``mitos.daytona``.

A ONE-WAY migration bridge for teams leaving Daytona's cloud for a SELF-HOSTED
sandbox runtime. Daytona moved its runtime to a private codebase; this shim lets
a Daytona-style script keep running by changing one import:

    # before
    from daytona import Daytona, CreateSandboxFromSnapshotParams
    # after
    from mitos.daytona import Daytona, CreateSandboxFromSnapshotParams

Like ``mitos.e2b``, this is an ADAPTER over the native ``DirectSandbox`` /
``SandboxServer`` surface, not engine work and not a re-implementation. It has NO
dependency on the ``daytona`` package and imports it NEVER. The class and
namespace shapes (``daytona.create()``, ``sandbox.process.code_run``,
``sandbox.fs.upload_file``, ``sandbox.get_preview_link``) mirror Daytona's object
model so the import swap is the only change.

Daytona verb -> mitos op (and whether it works TODAY against the standalone server):

    Daytona(config)                  -> hold api_key + base_url + target   works today
    daytona.create(params)           -> mitos.create(region=target)       works today
    daytona.get(id)                  -> SandboxServer.list_sandboxes lookup works today
    daytona.list()                   -> SandboxServer.list_sandboxes       works today
    daytona.delete(sandbox)          -> DirectSandbox.terminate            works today
    daytona.start(sandbox)           -> DirectSandbox.resume               works today
    daytona.stop(sandbox)            -> DirectSandbox.pause                 works today
    sandbox.process.exec(cmd, ...)   -> DirectSandbox.exec                 needs guest agent
    sandbox.process.code_run(code)   -> DirectSandbox.run_code             needs guest agent
    sandbox.fs.upload_file(...)      -> DirectSandbox.files.write          needs guest agent
    sandbox.fs.download_file(...)    -> DirectSandbox.files.read_bytes     needs guest agent
    sandbox.fs.list_files(path)      -> DirectSandbox.files.list           needs guest agent
    sandbox.fs.create_folder(path)   -> DirectSandbox.files.mkdir          needs guest agent
    sandbox.fs.delete_file(path)     -> DirectSandbox.files.remove         needs guest agent
    sandbox.fs.get_file_info(path)   -> DirectSandbox.files.list (lookup)  needs guest agent
    sandbox.get_preview_link(port)   -> DirectSandbox.get_host             works (proxy deployed)

"works today" vs "needs guest agent": the create / get / list / delete /
start / stop lifecycle answers on the bare mock-engine sandbox-server (no KVM).
process / fs need a real guest agent over vsock, so they are proven against a
fake target in the unit tests and run end-to-end in the KVM CI job; this exactly
mirrors the E2B shim (``mitos.e2b``).

``DirectSandbox.exec`` is shell based and takes no ``cwd`` / ``env`` argument, so
Daytona's ``process.exec(cmd, cwd=..., env=...)`` folds those into the command
(``export K=V; cd DIR; CMD``) rather than dropping them. ``env_vars`` / ``labels``
on create are accepted for signature parity and not applied today (no server
slot yet), the same honest stance the E2B shim takes for E2B's ``metadata``.
"""

from __future__ import annotations

import posixpath
import shlex
from typing import Any, List, Optional
from urllib.parse import parse_qs, urlsplit

from mitos.direct import DirectSandbox, SandboxServer, create as _create
from mitos.errors import NotFoundError
from mitos.types import FileInfo, Network


def _create_direct(
    image: str = "python",
    api_key: Optional[str] = None,
    base_url: Optional[str] = None,
    id: Optional[str] = None,
    network: Optional[Network] = None,
    region: Optional[str] = None,
) -> DirectSandbox:
    """The single seam that builds a native ``DirectSandbox``.

    Factored out so tests can patch it to a fake target and exercise the shim
    without a server, the same pattern ``mitos.e2b`` uses.

    region (issue #712 phase 0) is Daytona's ``target`` mapped onto Mitos
    placement (see ``Daytona.create``); passed through to ``mitos.create``
    unchanged."""
    return _create(
        image, api_key=api_key, base_url=base_url, id=id, network=network, region=region
    )


# --------------------------------------------------------------------------
# Config + create params: Daytona's input shapes, accepted for parity.
# --------------------------------------------------------------------------


class DaytonaConfig:
    """Daytona's ``DaytonaConfig``.

    Holds the credentials the ``Daytona`` client uses. ``api_url`` is Daytona's
    name for the server base; it maps onto the Mitos ``base_url``. ``target``
    (issue #712 phase 0) maps onto Mitos placement's ``region``: no longer
    silently ignored, ``Daytona(DaytonaConfig(target="fra")).create(...)``
    requests that placement value the same way ``mitos.create(region="fra")``
    does. Other extra Daytona fields (``organization_id``, ...) are still
    accepted and ignored so a Daytona config literal constructs unchanged."""

    def __init__(
        self,
        api_key: Optional[str] = None,
        api_url: Optional[str] = None,
        server_url: Optional[str] = None,
        target: Optional[str] = None,
        **_ignored: Any,
    ):
        self.api_key = api_key
        # Daytona has used both api_url and server_url across versions; accept
        # either and prefer api_url.
        self.api_url = api_url or server_url
        self.target = target


class CreateSandboxBaseParams:
    """Daytona's create-params shape.

    Captures the fields a Mitos sandbox can honor (``language`` selects the
    code_run default; ``image`` / ``snapshot`` name a base image) and accepts the
    rest (``env_vars``, ``labels``, ``resources``, ...) for parity. ``env_vars``
    and ``labels`` are held but not applied today (no server slot yet)."""

    def __init__(
        self,
        language: str = "python",
        image: Optional[str] = None,
        snapshot: Optional[str] = None,
        env_vars: Optional[dict[str, str]] = None,
        labels: Optional[dict[str, str]] = None,
        **_ignored: Any,
    ):
        self.language = language or "python"
        self.image = image
        self.snapshot = snapshot
        self.env_vars = env_vars or {}
        self.labels = labels or {}

    def _image(self) -> str:
        """The base image to create from: an explicit image / snapshot wins,
        else fall back to the language name (Mitos resolves a default)."""
        return self.image or self.snapshot or self.language or "python"


# Daytona has shipped several names for the same input across versions. Alias
# them all to the one base shape so any of these imports constructs unchanged.
CreateSandboxFromSnapshotParams = CreateSandboxBaseParams
CreateSandboxFromImageParams = CreateSandboxBaseParams
CreateSandboxParams = CreateSandboxBaseParams


class CodeRunParams:
    """Daytona's ``CodeRunParams`` (``argv`` / ``env`` for a code_run).

    Accepted for parity; ``env`` is folded into the run when given, ``argv`` is
    held but not forwarded (Mitos run_code has no argv channel)."""

    def __init__(
        self,
        argv: Optional[list[str]] = None,
        env: Optional[dict[str, str]] = None,
        **_ignored: Any,
    ):
        self.argv = argv or []
        self.env = env or {}


# --------------------------------------------------------------------------
# Result shapes: Daytona's return objects, so caller code reads them unchanged.
# --------------------------------------------------------------------------


class ExecutionArtifacts:
    """Daytona's ``ExecutionArtifacts``: ``stdout`` plus optional ``charts``.

    Mitos does not parse matplotlib charts out of a plain exec, so ``charts`` is
    ``None``; ``stdout`` mirrors ``ExecuteResponse.result``."""

    def __init__(self, stdout: str = "", charts: Optional[list] = None):
        self.stdout = stdout
        self.charts = charts

    def __repr__(self) -> str:
        return f"ExecutionArtifacts(stdout={self.stdout!r})"


class ExecuteResponse:
    """Daytona's ``ExecuteResponse``: ``exit_code``, ``result``, ``artifacts``."""

    def __init__(
        self, exit_code: int, result: str, artifacts: Optional[ExecutionArtifacts] = None
    ):
        self.exit_code = exit_code
        self.result = result
        self.artifacts = artifacts

    def __repr__(self) -> str:
        return f"ExecuteResponse(exit_code={self.exit_code}, result={self.result!r})"


class PortPreviewUrl:
    """Daytona's ``get_preview_link`` return: ``url`` plus an access ``token``.

    Mitos mints a single signed preview URL with the token embedded as a query
    param; this surfaces both the full URL and the parsed token so Daytona code
    reading ``.url`` / ``.token`` works unchanged."""

    def __init__(self, url: str, token: str = ""):
        self.url = url
        self.token = token

    def __repr__(self) -> str:
        return f"PortPreviewUrl(url={self.url!r})"


# --------------------------------------------------------------------------
# Namespaces on a sandbox: process + fs, mirroring Daytona's object model.
# --------------------------------------------------------------------------


class Process:
    """Daytona's ``sandbox.process`` namespace: ``exec`` and ``code_run``.

    Maps onto ``DirectSandbox.exec`` / ``DirectSandbox.run_code``.

    needs a guest agent: works end-to-end only against a real guest (proven
    against a fake target in tests, run on KVM in CI)."""

    def __init__(self, sandbox: DirectSandbox, language: str = "python"):
        self._sb = sandbox
        self._language = language

    def exec(
        self,
        command: str,
        cwd: Optional[str] = None,
        env: Optional[dict[str, str]] = None,
        timeout: Optional[int] = None,
    ) -> ExecuteResponse:
        """Daytona ``process.exec(command, cwd, env, timeout)`` -> ``exec``.

        ``DirectSandbox.exec`` is shell based and takes no ``cwd`` / ``env``, so
        both are folded into the command (``export K=V; cd DIR; CMD``) rather
        than silently dropped. Returns a Daytona ``ExecuteResponse``."""
        full = self._wrap(command, cwd, env)
        res = self._sb.exec(full, timeout=timeout if timeout is not None else 30)
        return ExecuteResponse(
            exit_code=res.exit_code,
            result=res.stdout,
            artifacts=ExecutionArtifacts(stdout=res.stdout, charts=None),
        )

    @staticmethod
    def _wrap(
        command: str, cwd: Optional[str], env: Optional[dict[str, str]]
    ) -> str:
        """Fold cwd / env into a single shell command.

        env is exported and cwd is changed into before the command runs, so the
        Daytona semantics survive even though the native exec has no slot for
        them. Values are shell quoted."""
        parts: list[str] = []
        for k, v in (env or {}).items():
            parts.append(f"export {k}={shlex.quote(str(v))};")
        if cwd:
            parts.append(f"cd {shlex.quote(cwd)};")
        parts.append(command)
        return " ".join(parts)

    def code_run(
        self,
        code: str,
        params: Optional[CodeRunParams] = None,
        timeout: Optional[int] = None,
    ) -> ExecuteResponse:
        """Daytona ``process.code_run(code, params, timeout)`` -> ``run_code``.

        Runs in the sandbox's stateful kernel (Daytona's stateful mode). The rich
        Mitos ``Execution`` is flattened to Daytona's ``ExecuteResponse``: stdout
        logs become ``result`` / ``artifacts.stdout``, a structured kernel error
        yields a non-zero ``exit_code``."""
        ex = self._sb.run_code(
            code,
            language=self._language,
            timeout=timeout if timeout is not None else 60,
        )
        stdout = "".join(ex.logs.get("stdout", []))
        # Daytona's result is the run's output; prefer the REPL value, fall back
        # to buffered stdout.
        result = ex.text or stdout
        exit_code = 1 if ex.error is not None else 0
        return ExecuteResponse(
            exit_code=exit_code,
            result=result,
            artifacts=ExecutionArtifacts(stdout=stdout, charts=None),
        )


class FileSystem:
    """Daytona's ``sandbox.fs`` namespace.

    Maps onto ``DirectSandbox.files.*``. Vocabulary renames: Daytona's
    ``upload_file`` / ``download_file`` -> ``write`` / ``read``,
    ``create_folder`` -> ``mkdir``, ``delete_file`` -> ``remove``.

    needs a guest agent: works end-to-end only against a real guest."""

    def __init__(self, sandbox: DirectSandbox):
        self._sb = sandbox

    def upload_file(
        self, file: str | bytes, remote_path: str, timeout: int = 30 * 60
    ) -> None:
        """Daytona ``fs.upload_file(file, remote_path)`` -> ``files.write``.

        Daytona overloads ``file``: raw ``bytes`` are written verbatim; a ``str``
        is treated as a LOCAL path whose contents are read and uploaded (the
        Daytona local-path overload)."""
        if isinstance(file, (bytes, bytearray)):
            self._sb.files.write(remote_path, bytes(file))
        elif isinstance(file, str):
            with open(file, "rb") as fh:
                self._sb.files.write(remote_path, fh.read())
        else:
            raise TypeError(
                "upload_file(file, remote_path): file must be bytes (content) "
                "or str (a local path to read), not "
                f"{type(file).__name__}"
            )

    def download_file(
        self, remote_path: str, local_path: Optional[str] = None, timeout: int = 30 * 60
    ) -> Optional[bytes]:
        """Daytona ``fs.download_file(remote_path[, local_path])`` -> ``files.read_bytes``.

        Returns the bytes when no ``local_path`` is given (Daytona's bytes
        overload); writes them to ``local_path`` and returns ``None`` otherwise."""
        data = self._sb.files.read_bytes(remote_path)
        if local_path is None:
            return data
        with open(local_path, "wb") as fh:
            fh.write(data)
        return None

    def list_files(self, path: str = "/") -> List[FileInfo]:
        """Daytona ``fs.list_files(path)`` -> ``files.list``.

        Returns Mitos ``FileInfo`` rows. The fields Daytona code reads most
        (``name``, ``is_dir``, ``size``, ``mode``) are present; Daytona's
        ``owner`` / ``group`` / ``permissions`` extras are not populated."""
        return self._sb.files.list(path)

    def create_folder(self, path: str, mode: str = "0755") -> None:
        """Daytona ``fs.create_folder(path, mode)`` -> ``files.mkdir``.

        ``mode`` is accepted for signature parity; the native ``mkdir`` applies
        the server default."""
        self._sb.files.mkdir(path)

    def delete_file(self, path: str, recursive: bool = False) -> None:
        """Daytona ``fs.delete_file(path)`` -> ``files.remove``."""
        self._sb.files.remove(path)

    def get_file_info(self, path: str) -> FileInfo:
        """Daytona ``fs.get_file_info(path)`` -> a single ``FileInfo``.

        Mitos has no stat RPC, so this lists the parent directory and returns the
        matching entry. A path with no matching entry raises the typed
        ``NotFoundError``."""
        parent = posixpath.dirname(path.rstrip("/")) or "/"
        name = posixpath.basename(path.rstrip("/"))
        for entry in self._sb.files.list(parent):
            if entry.name == name:
                return entry
        raise NotFoundError(
            f"no file or directory at {path!r}",
            code="not_found",
            cause=f"{name!r} was not present in the listing of {parent!r}",
            remediation=(
                "Check the path exists with fs.list_files(parent), or create it "
                "with fs.upload_file / fs.create_folder first."
            ),
            status=404,
        )


# --------------------------------------------------------------------------
# Sandbox: Daytona's per-sandbox handle, backed by a native DirectSandbox.
# --------------------------------------------------------------------------


class Sandbox:
    """Daytona's ``Sandbox`` handle, backed by a Mitos sandbox.

    Wraps a ready ``DirectSandbox`` and exposes Daytona's namespaces and methods:
    ``process`` (``exec`` / ``code_run``), ``fs`` (``upload_file`` /
    ``download_file`` / ``list_files`` / ``create_folder`` / ``delete_file`` /
    ``get_file_info``), ``get_preview_link``, ``start`` / ``stop`` / ``delete``,
    and ``id``.
    """

    def __init__(self, sandbox: DirectSandbox, language: str = "python"):
        self._sb = sandbox
        self._language = language
        self.process = Process(sandbox, language)
        self.fs = FileSystem(sandbox)

    @property
    def id(self) -> str:
        """Daytona's ``sandbox.id``."""
        return self._sb.id

    @property
    def sandbox(self) -> DirectSandbox:
        """The native ``DirectSandbox`` (the full SDK surface, including
        ``fork``, ``pause`` / ``resume``, and ``pty``)."""
        return self._sb

    def get_preview_link(self, port: int) -> PortPreviewUrl:
        """Daytona ``sandbox.get_preview_link(port)`` -> preview URL.

        Delegates to the native ``DirectSandbox.get_host`` and splits the signed
        token out of the URL so both ``.url`` and ``.token`` are populated."""
        url = self._sb.get_host(port)
        token = ""
        try:
            qs = parse_qs(urlsplit(url).query)
            token = (qs.get("token") or [""])[0]
        except Exception:  # noqa: BLE001 - a malformed URL just yields no token.
            token = ""
        return PortPreviewUrl(url=url, token=token)

    def fork(self, n: int = 1) -> List["Sandbox"]:
        """Not in Daytona's surface: a Mitos superpower exposed for convenience.

        Forks this sandbox into ``n`` independent copies, each wrapped as a
        Daytona ``Sandbox``."""
        return [Sandbox(child, self._language) for child in self._sb.fork(n)]

    def start(self, timeout: Optional[float] = 60) -> None:
        """Daytona ``sandbox.start()`` -> ``DirectSandbox.resume``."""
        self._sb.resume()

    def stop(self, timeout: Optional[float] = 60, force: bool = False) -> None:
        """Daytona ``sandbox.stop()`` -> ``DirectSandbox.pause``."""
        self._sb.pause()

    def delete(self, timeout: Optional[float] = 60) -> None:
        """Daytona ``sandbox.delete()`` -> ``DirectSandbox.terminate``."""
        self._sb.terminate()

    def __enter__(self) -> "Sandbox":
        return self

    def __exit__(self, *args: Any) -> None:
        self.delete()

    def __repr__(self) -> str:
        return f"Sandbox(id={self._sb.id!r})"


# --------------------------------------------------------------------------
# Daytona: the client. Owns auth and the create / get / list / delete verbs.
# --------------------------------------------------------------------------


class Daytona:
    """Daytona's top-level client, backed by the Mitos standalone server.

    Drop-in for ``from daytona import Daytona`` against a SELF-HOSTED Mitos
    sandbox-server. Auth resolves exactly like ``mitos.create``: from the
    ``DaytonaConfig`` (``api_key`` / ``api_url``), else ``MITOS_API_KEY`` /
    ``MITOS_BASE_URL``.

        from mitos.daytona import Daytona, CreateSandboxFromSnapshotParams

        daytona = Daytona()
        sandbox = daytona.create(CreateSandboxFromSnapshotParams(language="python"))
        sandbox.process.code_run("print(1 + 1)")
        daytona.delete(sandbox)
    """

    def __init__(self, config: Optional[DaytonaConfig] = None):
        cfg = config or DaytonaConfig()
        self._api_key = cfg.api_key
        self._base_url = cfg.api_url
        # target (issue #712 phase 0): Daytona's region/target selector, mapped
        # onto Mitos placement at create time (Daytona's per-create target, if
        # ever added, would take priority; DaytonaConfig has no such field
        # today, so the client-level target is the only source).
        self._target = cfg.target

    def create(
        self,
        params: Optional[CreateSandboxBaseParams] = None,
        timeout: Optional[float] = 60,
    ) -> Sandbox:
        """Daytona ``daytona.create(params)`` -> ``mitos.create``.

        Returns a READY Daytona ``Sandbox`` over the standalone server / hosted
        control plane (no Kubernetes). ``language`` sets the code_run default and
        ``image`` / ``snapshot`` selects the base; ``env_vars`` / ``labels`` are
        accepted and not applied today.

        ``DaytonaConfig(target=...)`` (issue #712 phase 0) maps onto Mitos's
        ``region``: previously accepted and silently ignored, it now selects
        a placement value the same way ``mitos.create(region=...)`` does. A
        self-hosted single-cluster server still ignores it; a hosted
        deployment validates it against its placement registry.

        works today against the bare mock server."""
        params = params or CreateSandboxBaseParams()
        sb = _create_direct(
            params._image(),
            api_key=self._api_key,
            base_url=self._base_url,
            region=self._target,
        )
        return Sandbox(sb, language=params.language)

    def get(self, sandbox_id: str) -> Sandbox:
        """Daytona ``daytona.get(id)`` -> reattach to a RUNNING sandbox by id.

        The standalone server has no get-one endpoint, so this finds the id in
        ``list_sandboxes`` and rebuilds a handle. An unknown id raises the typed
        ``NotFoundError``.

        works today against the bare mock server."""
        server = SandboxServer.from_auth(api_key=self._api_key, base_url=self._base_url)
        for info in server.list_sandboxes():
            if info.get("id") == sandbox_id:
                return Sandbox(self._rebuild(server, info))
        raise NotFoundError(
            f"no running sandbox with id {sandbox_id!r}",
            code="not_found",
            cause="the id was not present in the server's running-sandbox listing",
            remediation=(
                "List running sandboxes with daytona.list() and use one of those "
                "ids, or create a new sandbox with daytona.create(...)."
            ),
            status=404,
        )

    def list(self) -> List[Sandbox]:
        """Daytona ``daytona.list()`` -> ``SandboxServer.list_sandboxes``.

        Returns the running sandboxes as Daytona ``Sandbox`` handles.

        works today against the bare mock server."""
        server = SandboxServer.from_auth(api_key=self._api_key, base_url=self._base_url)
        return [Sandbox(self._rebuild(server, info)) for info in server.list_sandboxes()]

    @staticmethod
    def _rebuild(server: SandboxServer, info: dict) -> DirectSandbox:
        """Rebuild a native ``DirectSandbox`` handle from a listing row, the same
        reattach the E2B shim's ``connect`` uses."""
        return DirectSandbox(
            id=info["id"],
            template=info.get("template_id", ""),
            endpoint=info.get("endpoint", ""),
            server_url=server.url,
            fork_time_ms=info.get("fork_time_ms", 0.0),
            api_key=server._api_key,
        )

    def delete(self, sandbox: Sandbox, timeout: Optional[float] = 60) -> None:
        """Daytona ``daytona.delete(sandbox)`` -> ``DirectSandbox.terminate``."""
        sandbox.delete(timeout=timeout)

    def start(self, sandbox: Sandbox, timeout: Optional[float] = 60) -> None:
        """Daytona ``daytona.start(sandbox)`` -> ``DirectSandbox.resume``."""
        sandbox.start(timeout=timeout)

    def stop(self, sandbox: Sandbox, timeout: Optional[float] = 60) -> None:
        """Daytona ``daytona.stop(sandbox)`` -> ``DirectSandbox.pause``."""
        sandbox.stop(timeout=timeout)
