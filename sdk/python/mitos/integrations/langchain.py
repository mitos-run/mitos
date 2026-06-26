"""LangChain / deepagents sandbox backend backed by mitos.

WHY: when a developer picks a sandbox in LangChain or deepagents, they pick
against LangChain's sandbox-backend interface. Implementing that interface once
lets any LangChain agent run its tool calls inside a mitos snapshot-fork
sandbox by swapping the backend, with no change to the agent code. This is an
ADAPTER over the existing SDK ops (exec, files, run_code), not engine work.

LANGCHAIN CONTRACT TARGETED (record so reviewers can check conformance against
https://docs.langchain.com/oss/python/deepagents/sandboxes):

  A deepagents sandbox backend is a pluggable object (alongside the shipped
  ``E2BSandbox`` and ``DaytonaSandbox``) exposing roughly:

    - a shell-command op: run a command, return stdout / stderr / exit code.
      Mapped here to ``MitosSandbox.execute(command) -> CommandResult`` (with a
      ``run`` alias), backed by ``DirectSandbox.exec``.
    - filesystem ops: read / write / list files. Mapped to
      ``read_file`` / ``write_file`` / ``list_files``, backed by
      ``DirectSandbox.files.*``.
    - a lifecycle: create / close. Mapped to ``MitosSandbox.create(...)``
      (classmethod, returns a READY backend) and ``MitosSandbox.close()``
      (alias ``stop``), backed by ``mitos.create`` and ``DirectSandbox.terminate``.

  ADJUSTABILITY: every framework verb is translated through the shared
  ``mitos.integrations._mapping`` helpers, and the method names live in one
  place (this class). If upstream LangChain names differ (e.g. ``run_command``
  vs ``execute``, or ``files.read`` vs ``read_file``), only the thin method
  names on ``MitosSandbox`` move; the wire-op mapping does not. The aliases
  (``run``, ``stop``) and the explicit-mapping design make that a rename, not a
  rewrite.

OPTIONAL DEPENDENCY: this module imports mitos ALWAYS and imports langchain
NEVER at runtime. LangChain types are referenced only under ``TYPE_CHECKING``.
``MitosSandbox`` does not subclass any langchain base class, so it is fully
usable and testable with langchain absent. ``as_langchain_backend()`` is the
opt-in seam for the duck-typed surface. For LangChain's deepagents, which
requires a concrete ``deepagents.backends.sandbox.BaseSandbox`` subclass whose
``execute()`` returns an ``ExecuteResponse``, ``as_deepagents_backend()`` builds
exactly that (importing deepagents lazily); pass it to
``create_deep_agent(backend=...)``.

CODE EXECUTION: a code snippet (vs a shell command) maps to ``run_code``, which
returns a native ``Execution`` carrying MIME ``Result`` artifacts (image/png,
text/html, application/json, ...), buffered logs, and a structured error. Use
``run_code`` for REPL-style tool calls so the rich artifacts survive.

FORK: LangChain's sandbox backend interface has no branching / parallel-run
slot, so fork is NOT forced onto it. Fork stays reachable as a first-class
native op via ``MitosSandbox.fork(n)``, which returns sibling ``MitosSandbox``
backends, each wrapping an independent forked ``DirectSandbox``. Document this
in the quickstart: branching is a mitos superpower the LangChain interface does
not expose, so reach it through the adapter's native ``fork``.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Any, Callable, List, Optional

from mitos.direct import DirectSandbox, create as _create
from mitos.integrations import _mapping
from mitos.types import Execution, ExecResult, FileInfo, Network

if TYPE_CHECKING:  # pragma: no cover - typing only; never imported at runtime.
    # Referenced for type hints only. Importing langchain is NOT required to use
    # this module; these names resolve only when a type checker runs.
    pass


class MitosSandbox:
    """A LangChain / deepagents sandbox backend backed by a mitos sandbox.

    Wraps a ready ``DirectSandbox`` and exposes the LangChain sandbox-backend
    surface: ``execute`` (shell command), ``read_file`` / ``write_file`` /
    ``list_files`` (filesystem), and the ``create`` / ``close`` lifecycle. Code
    execution with rich MIME results is available via ``run_code``. Branching is
    available via the native ``fork``.

    Construct it with an existing handle::

        from mitos import create
        from mitos.integrations.langchain import MitosSandbox

        sb = MitosSandbox(create("python", base_url="http://localhost:8080"))

    or with the lifecycle classmethod::

        sb = MitosSandbox.create("python", base_url="http://localhost:8080")
    """

    def __init__(self, sandbox: DirectSandbox):
        self._sandbox = sandbox

    # -- lifecycle ---------------------------------------------------------

    @classmethod
    def create(
        cls,
        image: str = "python",
        api_key: Optional[str] = None,
        base_url: Optional[str] = None,
        id: Optional[str] = None,
        network: Optional[Network] = None,
    ) -> "MitosSandbox":
        """Create a READY backend over the standalone sandbox-server / hosted
        control plane (no Kubernetes). Resolves auth exactly like
        ``mitos.create`` (explicit arg, else ``MITOS_API_KEY`` /
        ``MITOS_BASE_URL``)."""
        sandbox = _create(
            image, api_key=api_key, base_url=base_url, id=id, network=network
        )
        return cls(sandbox)

    @property
    def sandbox(self) -> DirectSandbox:
        """The underlying native ``DirectSandbox`` (the full SDK surface)."""
        return self._sandbox

    @property
    def id(self) -> str:
        return self._sandbox.id

    def close(self) -> None:
        """Terminate the sandbox and release its handle (LangChain lifecycle
        close)."""
        self._sandbox.terminate()

    # LangChain naming may use stop(); alias so a rename upstream is trivial.
    stop = close

    def __enter__(self) -> "MitosSandbox":
        return self

    def __exit__(self, *args: Any) -> None:
        self.close()

    # -- shell command -----------------------------------------------------

    def execute(self, command: str, timeout: int = 30) -> dict[str, Any]:
        """Run a shell command, return the normalized command-result dict
        (``stdout``, ``stderr``, ``exit_code``, ``exec_time_ms``).

        This is the LangChain "run command" op. The mapping lives in the shared
        ``_mapping.map_execute`` so #204 / #206 reuse it.
        """
        return _mapping.map_execute(self._sandbox, command, timeout=timeout)

    # LangChain naming may use run(); alias so a rename upstream is trivial.
    run = execute

    def exec_result(self, command: str, timeout: int = 30) -> ExecResult:
        """Run a shell command and return the native ``ExecResult`` dataclass
        (when a caller wants the typed object rather than the dict)."""
        return self._sandbox.exec(command, timeout=timeout)

    # -- filesystem --------------------------------------------------------

    def read_file(self, path: str) -> str:
        """Read a file's text content (LangChain filesystem read)."""
        return _mapping.map_files_read(self._sandbox, path)

    def write_file(self, path: str, content: str | bytes) -> None:
        """Write content to a file (LangChain filesystem write)."""
        _mapping.map_files_write(self._sandbox, path, content)

    def list_files(self, path: str = "/") -> List[FileInfo]:
        """List a directory (LangChain filesystem list)."""
        return _mapping.map_files_list(self._sandbox, path)

    # -- code execution (rich MIME results) --------------------------------

    def run_code(
        self,
        code: str,
        language: str = "python",
        timeout: int = 60,
        on_stdout: Optional[Callable[[str], None]] = None,
        on_stderr: Optional[Callable[[str], None]] = None,
        on_result: Optional[Callable[[Any], None]] = None,
    ) -> Execution:
        """Execute a code snippet in the stateful kernel; return the rich
        ``Execution`` (MIME ``Result`` artifacts, buffered logs, structured
        error). Prefer this over ``execute`` for REPL-style tool calls."""
        return _mapping.map_run_code(
            self._sandbox,
            code,
            language=language,
            timeout=timeout,
            on_stdout=on_stdout,
            on_stderr=on_stderr,
            on_result=on_result,
        )

    def run_code_dict(
        self, code: str, language: str = "python", timeout: int = 60
    ) -> dict[str, Any]:
        """Execute a code snippet and return the JSON-friendly flattened shape
        (for LLM tool-call results that must serialize)."""
        execution = self.run_code(code, language=language, timeout=timeout)
        return _mapping.execution_to_dict(execution)

    # -- fork (native superpower; no LangChain slot) -----------------------

    def fork(self, n: int = 1, id: Optional[str] = None) -> List["MitosSandbox"]:
        """Fork the sandbox into ``n`` independent sibling backends.

        LangChain's sandbox-backend interface has no branching slot, so this is
        the NATIVE seam, not a LangChain op: each child is a ready
        ``MitosSandbox`` wrapping an independent forked ``DirectSandbox``. Use
        it for parallel / branching agent runs.
        """
        children = self._sandbox.fork(n, id=id)
        return [MitosSandbox(child) for child in children]

    def __repr__(self) -> str:
        return f"MitosSandbox(id={self._sandbox.id!r})"


def as_langchain_backend(sandbox: MitosSandbox) -> MitosSandbox:
    """Opt-in seam for the duck-typed LangChain backend surface.

    ``MitosSandbox`` already exposes ``execute`` / ``read_file`` / ``write_file``
    / ``list_files`` directly, so this returns it unchanged. For LangChain's
    deepagents, which requires a concrete ``BaseSandbox`` subclass with an
    ``ExecuteResponse``-returning ``execute``, use ``as_deepagents_backend``
    instead (this passthrough does NOT satisfy that protocol).
    """
    return sandbox


def as_deepagents_backend(sandbox: "MitosSandbox | DirectSandbox") -> Any:
    """Adapt a Mitos sandbox to a deepagents sandbox backend.

    deepagents (LangChain) runs an agent's shell and filesystem tools through a
    pluggable ``backend=`` object that subclasses
    ``deepagents.backends.sandbox.BaseSandbox`` and implements
    ``execute(command) -> ExecuteResponse`` (plus ``id`` / ``upload_files`` /
    ``download_files``); BaseSandbox builds ls/read/write/grep/glob/edit on top of
    ``execute``. Pass the result straight to::

        from deepagents import create_deep_agent
        agent = create_deep_agent(
            model=...,
            backend=as_deepagents_backend(MitosSandbox.create("python", base_url=...)),
        )

    deepagents is imported HERE, never at module top level, so the SDK keeps no
    hard dependency on it; ``pip install "mitos-run[langchain]"`` brings it in.
    Accepts a ``MitosSandbox`` or a raw ``DirectSandbox``.
    """
    from deepagents.backends.protocol import (
        ExecuteResponse,
        FileDownloadResponse,
        FileUploadResponse,
    )
    from deepagents.backends.sandbox import BaseSandbox

    direct = sandbox.sandbox if isinstance(sandbox, MitosSandbox) else sandbox

    class _MitosDeepAgentsBackend(BaseSandbox):
        """Mitos-backed deepagents SandboxBackendProtocol implementation."""

        def __init__(self, sb: DirectSandbox) -> None:
            self._sb = sb

        @property
        def id(self) -> str:
            return self._sb.id

        def execute(self, command: str, *, timeout: Optional[int] = None) -> ExecuteResponse:
            # deepagents passes timeout=None for the backend default; map to the
            # SDK's default exec timeout. ExecuteResponse.output is the COMBINED
            # stdout+stderr stream.
            res = self._sb.exec(command, timeout=timeout if timeout is not None else 60)
            return ExecuteResponse(
                output=(res.stdout or "") + (res.stderr or ""),
                exit_code=res.exit_code,
            )

        def upload_files(self, files: List[tuple]) -> List[Any]:
            out: List[Any] = []
            for path, content in files:
                if not path.startswith("/"):
                    out.append(FileUploadResponse(path=path, error="invalid_path"))
                    continue
                try:
                    self._sb.files.write(path, content)
                    out.append(FileUploadResponse(path=path, error=None))
                except Exception as exc:  # per-file failure, never fail the batch
                    out.append(FileUploadResponse(path=path, error=str(exc)))
            return out

        def download_files(self, paths: List[str]) -> List[Any]:
            out: List[Any] = []
            for path in paths:
                if not path.startswith("/"):
                    out.append(FileDownloadResponse(path=path, content=None, error="invalid_path"))
                    continue
                try:
                    data = self._sb.files.read_bytes(path)
                    out.append(FileDownloadResponse(path=path, content=data, error=None))
                except Exception as exc:
                    out.append(FileDownloadResponse(path=path, content=None, error=str(exc)))
            return out

    return _MitosDeepAgentsBackend(direct)
