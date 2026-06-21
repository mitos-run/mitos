"""E2B-compat shim: ``mitos.e2b`` (issue #206).

A ONE-WAY migration bridge for self-hosted / regulated / air-gapped teams
leaving E2B's cloud. The promise is "change one import": an E2B-style script
runs UNCHANGED against a standalone mitos sandbox-server because E2B's
per-operation vocabulary is ~70% aligned with ours.

    # before
    from e2b_code_interpreter import Sandbox
    # after
    from mitos.e2b import Sandbox

This is an ADAPTER over the existing native surface (``DirectSandbox`` /
``SandboxServer``, issue #217) and the shared op-mapping helper
(``mitos.integrations._mapping``, issue #203). It performs NO engine work and
adds NO server endpoint: ``set_timeout`` already exists server-side (issue
#218) and is reused verbatim.

E2B method -> mitos op (and whether it works TODAY against the standalone server):

    Sandbox.create(...)        -> mitos.create() / DirectSandbox      works today
    Sandbox.connect(id)        -> SandboxServer.list_sandboxes lookup works today
    Sandbox.list()             -> SandboxServer.list_sandboxes        works today
    sandbox.commands.run(cmd)  -> DirectSandbox.exec                  needs guest agent
    sandbox.commands.run(.., background=True) -> exec (handle)        needs guest agent
    sandbox.files.read/write/list/exists/remove -> DirectSandbox.files.*  needs guest agent
    sandbox.files.make_dir(path) -> DirectSandbox.files.mkdir         needs guest agent
    sandbox.run_code(code)     -> DirectSandbox.run_code (rich MIME)  needs guest agent
    sandbox.set_timeout(s)     -> DirectSandbox.set_timeout (issue #218)  works today
    sandbox.kill()             -> DirectSandbox.terminate             works today
    sandbox.get_host(port)     -> preview URLs (issue #126)           NOT AVAILABLE

"works today" vs "needs guest agent": the create / connect / list / kill /
set_timeout lifecycle answers on the bare mock-engine sandbox-server (no KVM).
exec / files / run_code need a real guest agent over vsock, so they are proven
against a fake target in the unit tests and run end-to-end in the KVM CI job;
this exactly mirrors the LangChain / OpenAI adapters (#203 / #204).

``get_host`` is the ONE op with no honest mapping yet: it depends on preview
URLs (issue #126), which are not built. Rather than fabricate a URL, it raises a
clear typed ``AgentRunError`` (the #216 apierr envelope: stable ``code``, a
``cause``, and actionable ``remediation``) naming the missing feature.

NO hard dependency on the ``e2b`` package: this module imports it NEVER. The
class and namespace shapes (``sandbox.commands.run``, ``sandbox.files.read``)
mirror E2B's object model so the user's import swap is the only change.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Any, Callable, List, Optional

from mitos.direct import DirectSandbox, SandboxServer, create as _create
from mitos.errors import NotFoundError
from mitos.integrations import _mapping
from mitos.types import Execution, ExecResult, FileInfo, Network

if TYPE_CHECKING:  # pragma: no cover - typing only; the e2b package is never imported.
    pass


def _create_direct(
    image: str = "python",
    api_key: Optional[str] = None,
    base_url: Optional[str] = None,
    id: Optional[str] = None,
    network: Optional[Network] = None,
) -> DirectSandbox:
    """The single seam that builds a native ``DirectSandbox``.

    Factored out so tests can patch it to a fake target and exercise the shim
    without a server (the same pattern the other adapters use)."""
    return _create(image, api_key=api_key, base_url=base_url, id=id, network=network)


class Commands:
    """E2B's ``sandbox.commands`` namespace.

    Mirrors E2B's object shape so ``sandbox.commands.run(cmd)`` works unchanged.
    Maps onto ``DirectSandbox.exec`` via the shared ``_mapping`` op layer.

    works: needs a guest agent (proven against a fake target in tests, run
    end-to-end on KVM).
    """

    def __init__(self, sandbox: DirectSandbox):
        self._sb = sandbox

    def run(
        self, cmd: str, background: bool = False, timeout: int = 60
    ) -> ExecResult:
        """Run a shell command and return the result.

        E2B signature: ``commands.run(cmd, background=False, ...)``. We map both
        foreground and background onto ``DirectSandbox.exec``. The standalone
        sandbox-server runs the command to completion, so the returned
        ``ExecResult`` (``stdout`` / ``stderr`` / ``exit_code`` / ``exec_time_ms``)
        is the handle E2B's background mode exposes as well; a future streaming
        background handle (E2B's ``CommandHandle``) is the upgrade seam.

        needs a guest agent: works end-to-end only against a real guest, not the
        bare mock server.
        """
        # background is accepted for E2B signature parity; the standalone server
        # exec endpoint is blocking, so both paths return the completed result.
        return self._sb.exec(cmd, timeout=timeout)


class Files:
    """E2B's ``sandbox.files`` namespace.

    Mirrors E2B's object shape so ``sandbox.files.read(path)`` etc. work
    unchanged. Maps onto ``DirectSandbox.files.*``. The ONE rename is E2B's
    ``make_dir`` -> our ``mkdir``.

    works: needs a guest agent (proven against a fake target in tests).
    """

    def __init__(self, sandbox: DirectSandbox):
        self._sb = sandbox

    def read(self, path: str) -> str:
        """E2B ``files.read(path)`` -> ``DirectSandbox.files.read``."""
        return _mapping.map_files_read(self._sb, path)

    def write(self, path: str, data: str | bytes) -> None:
        """E2B ``files.write(path, data)`` -> ``DirectSandbox.files.write``."""
        _mapping.map_files_write(self._sb, path, data)

    def list(self, path: str = "/") -> List[FileInfo]:
        """E2B ``files.list(path)`` -> ``DirectSandbox.files.list``."""
        return _mapping.map_files_list(self._sb, path)

    def exists(self, path: str) -> bool:
        """E2B ``files.exists(path)`` -> ``DirectSandbox.files.exists``."""
        return self._sb.files.exists(path)

    def remove(self, path: str) -> None:
        """E2B ``files.remove(path)`` -> ``DirectSandbox.files.remove``."""
        self._sb.files.remove(path)

    def make_dir(self, path: str) -> None:
        """E2B ``files.make_dir(path)`` -> ``DirectSandbox.files.mkdir``.

        The ONE vocabulary rename in the shim: E2B says ``make_dir``, mitos says
        ``mkdir``. Mapped here so the E2B name works unchanged."""
        self._sb.files.mkdir(path)


class SandboxInfo:
    """A row returned by ``Sandbox.list()``.

    Mirrors E2B's listing entry shape with a ``sandbox_id`` attribute, fed from
    the standalone server's ``list_sandboxes`` response."""

    def __init__(self, sandbox_id: str, template_id: str = "", endpoint: str = ""):
        self.sandbox_id = sandbox_id
        self.template_id = template_id
        self.endpoint = endpoint

    def __repr__(self) -> str:
        return f"SandboxInfo(sandbox_id={self.sandbox_id!r})"


class Sandbox:
    """E2B's ``Sandbox`` surface, backed by a mitos sandbox.

    Drop-in for ``from e2b import Sandbox`` / ``from e2b_code_interpreter import
    Sandbox`` against a SELF-HOSTED mitos sandbox-server. Wraps a ready
    ``DirectSandbox`` and exposes E2B's method names and namespaces:
    ``commands.run``, ``files.read/write/list/exists/remove/make_dir``,
    ``run_code``, ``set_timeout``, ``kill``, plus ``create`` / ``connect`` /
    ``list`` classmethods.

        from mitos.e2b import Sandbox

        sandbox = Sandbox.create("python", base_url="http://localhost:8080")
        sandbox.commands.run("echo hi")
        sandbox.kill()
    """

    def __init__(self, sandbox: DirectSandbox):
        self._sb = sandbox
        self.commands = Commands(sandbox)
        self.files = Files(sandbox)

    # -- lifecycle ---------------------------------------------------------

    @classmethod
    def create(
        cls,
        template: str = "python",
        api_key: Optional[str] = None,
        base_url: Optional[str] = None,
        timeout: Optional[int] = None,
        metadata: Optional[dict[str, Any]] = None,
        network: Optional[Network] = None,
        **_ignored: Any,
    ) -> "Sandbox":
        """E2B ``Sandbox.create(template, ...)`` -> ``mitos.create``.

        Returns a READY handle over the standalone sandbox-server / hosted
        control plane (no Kubernetes). ``template`` is E2B's first positional
        (an image / template name); auth resolves exactly like ``mitos.create``
        (explicit arg, else ``MITOS_API_KEY`` / ``MITOS_BASE_URL``). When
        ``timeout`` is given it is applied as a live TTL via the native
        ``set_timeout`` (issue #218) right after create, matching E2B's create-
        time timeout. ``metadata`` is accepted for signature parity and ignored
        today (no server slot yet).

        works today against the bare mock server.
        """
        sb = _create_direct(
            template, api_key=api_key, base_url=base_url, network=network
        )
        wrapped = cls(sb)
        if timeout is not None:
            wrapped.set_timeout(timeout)
        return wrapped

    @classmethod
    def connect(
        cls,
        sandbox_id: str,
        api_key: Optional[str] = None,
        base_url: Optional[str] = None,
    ) -> "Sandbox":
        """E2B ``Sandbox.connect(id)`` -> reattach to a RUNNING sandbox by id.

        Maps onto the standalone server's listing: the server has no
        get-one-by-id endpoint, so we find the id in ``list_sandboxes`` and
        rebuild a ``DirectSandbox`` handle pointed at it. An unknown id raises
        the typed ``NotFoundError`` (server code ``not_found``). The k8s path's
        equivalent is ``AgentRun.from_name`` / ``get``.

        works today against the bare mock server.
        """
        server = SandboxServer.from_auth(api_key=api_key, base_url=base_url)
        for info in server.list_sandboxes():
            if info.get("id") == sandbox_id:
                direct = DirectSandbox(
                    id=info["id"],
                    template=info.get("template_id", ""),
                    endpoint=info.get("endpoint", ""),
                    server_url=server.url,
                    fork_time_ms=info.get("fork_time_ms", 0.0),
                    api_key=server._api_key,
                )
                return cls(direct)
        raise NotFoundError(
            f"no running sandbox with id {sandbox_id!r}",
            code="not_found",
            cause="the id was not present in the server's running-sandbox listing",
            remediation=(
                "List running sandboxes with Sandbox.list(base_url=...) and "
                "connect to one of those ids, or create a new sandbox with "
                "Sandbox.create(...)."
            ),
            status=404,
        )

    @classmethod
    def list(
        cls,
        api_key: Optional[str] = None,
        base_url: Optional[str] = None,
    ) -> List[SandboxInfo]:
        """E2B ``Sandbox.list()`` -> ``SandboxServer.list_sandboxes``.

        Returns the running sandboxes as ``SandboxInfo`` rows (each carrying a
        ``sandbox_id``), matching E2B's listing shape.

        works today against the bare mock server.
        """
        server = SandboxServer.from_auth(api_key=api_key, base_url=base_url)
        return [
            SandboxInfo(
                sandbox_id=info.get("id", ""),
                template_id=info.get("template_id", ""),
                endpoint=info.get("endpoint", ""),
            )
            for info in server.list_sandboxes()
        ]

    @property
    def sandbox(self) -> DirectSandbox:
        """The underlying native ``DirectSandbox`` (the full SDK surface,
        including ``fork``, ``pause`` / ``resume``, and ``pty``, which E2B does
        not expose)."""
        return self._sb

    @property
    def sandbox_id(self) -> str:
        """E2B's ``sandbox.sandbox_id``."""
        return self._sb.id

    def set_timeout(self, timeout: int) -> int:
        """E2B ``sandbox.set_timeout(seconds)`` -> ``DirectSandbox.set_timeout``.

        Reuses the native live-TTL method (issue #218): extends this RUNNING
        sandbox's deadline to now + ``timeout`` seconds and returns the new
        absolute unix deadline. A value over the server ceiling raises
        ``TimeoutTooLargeError`` (never silently clamped, the #216 rule).

        works today against the bare mock server.
        """
        return self._sb.set_timeout(timeout)

    def run_code(
        self,
        code: str,
        language: str = "python",
        timeout: int = 60,
        on_stdout: Optional[Callable[[str], None]] = None,
        on_stderr: Optional[Callable[[str], None]] = None,
        on_result: Optional[Callable[[Any], None]] = None,
    ) -> Execution:
        """E2B ``sandbox.run_code(code)`` -> ``DirectSandbox.run_code``.

        Already matches E2B: returns a rich ``Execution`` carrying MIME
        ``Result`` artifacts (image/png, text/html, application/json, ...),
        buffered logs, and a structured error, streamed via the callbacks.

        needs a guest agent: works end-to-end only against a real guest (the
        mock server has no code-interpreter kernel).
        """
        return _mapping.map_run_code(
            self._sb,
            code,
            language=language,
            timeout=timeout,
            on_stdout=on_stdout,
            on_stderr=on_stderr,
            on_result=on_result,
        )

    def get_host(self, port: int) -> str:
        """E2B ``sandbox.get_host(port)`` -> preview URLs (issue #126).

        Returns a signed, expiring preview URL for ``port`` on this sandbox by
        delegating to the native ``DirectSandbox.get_host`` (the per-sandbox
        preview reverse proxy). A server that does not expose the preview proxy
        raises a typed ``AgentRunError`` from the underlying call.
        """
        return self._sb.get_host(port)

    def kill(self) -> None:
        """E2B ``sandbox.kill()`` -> ``DirectSandbox.terminate``.

        works today against the bare mock server.
        """
        self._sb.terminate()

    def __enter__(self) -> "Sandbox":
        return self

    def __exit__(self, *args: Any) -> None:
        self.kill()

    def __repr__(self) -> str:
        return f"Sandbox(sandbox_id={self._sb.id!r})"
