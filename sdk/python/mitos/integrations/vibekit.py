"""VibeKit sandbox-provider adapter backed by mitos (issue #205).

WHY: VibeKit is a provider aggregator over sandbox backends. Its docs frame it
as "E2B today; Daytona, Modal, Fly.io coming soon", and listing a new provider
there is near-free distribution. Implementing the mitos PROVIDER against
VibeKit's provider shape lets a VibeKit user pick mitos as the sandbox backend
by name.

VIBEKIT PROVIDER CONTRACT TARGETED (recorded so a reviewer can check conformance
when VibeKit's interface is in hand; VibeKit's own surface mirrors E2B's, which
is its first provider):

  A VibeKit provider is an object that:

    - identifies itself by a stable ``name`` (here ``"mitos"``), the key VibeKit
      registers and a user selects.
    - creates a sandbox: ``provider.create(template, ...) -> Sandbox``, returning
      a READY handle. Mapped to ``mitos.create`` / ``DirectSandbox``.
    - the sandbox exposes command execution, a filesystem, and a lifecycle:
        * run a command   -> ``Sandbox.run_command(cmd) -> {stdout, stderr,
          exit_code, exec_time_ms}`` (alias ``execute``), backed by
          ``DirectSandbox.exec``.
        * filesystem      -> ``read_file`` / ``write_file`` / ``list_files``,
          backed by ``DirectSandbox.files.*``.
        * lifecycle       -> ``kill`` (alias ``close``), backed by
          ``DirectSandbox.terminate``.
    - code execution      -> ``run_code(code) -> Execution`` with rich MIME
      ``Result`` artifacts, backed by ``DirectSandbox.run_code``.

  ADJUSTABILITY: every framework verb is translated through the shared
  ``mitos.integrations._mapping`` helpers, and the method names live in one place
  (the two classes below). If VibeKit's real interface names a verb differently
  (e.g. ``runCommand`` vs ``run_command``, ``commands.run`` namespacing, or a
  ``destroy`` lifecycle), only the thin method name on the class moves; the
  wire-op mapping does not. The ``execute`` / ``close`` aliases make a name
  change a rename, not a rewrite.

OPTIONAL DEPENDENCY: this module imports mitos ALWAYS and the ``vibekit`` package
NEVER at runtime. The provider does not subclass any VibeKit base class, so it is
fully usable and testable with VibeKit absent.

FORK: VibeKit's provider interface has no branching slot, so fork is NOT forced
onto it. It stays reachable as a first-class native op via
``MitosVibeKitSandbox.fork(n)``, which returns sibling sandboxes, each wrapping
an independent forked ``DirectSandbox``.

REGISTERING WITH VIBEKIT (the remaining MAINTAINER step, NOT done here): listing
mitos as a selectable provider is a contribution to VibeKit's own repository
(add a provider entry that constructs ``MitosVibeKitProvider`` and wires its
create / command / filesystem methods to VibeKit's provider interface, following
their CONTRIBUTING process and opening a PR). This adapter is the code that entry
calls; the external registration is a human step. See the SDK README integrations
section for the high-level pointer.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Any, Callable, List, Optional

from mitos.direct import DirectSandbox, create as _create
from mitos.integrations import _mapping
from mitos.types import Execution, FileInfo, Network

if TYPE_CHECKING:  # pragma: no cover - typing only; vibekit is never imported.
    pass


def _create_direct(
    template: str = "python",
    api_key: Optional[str] = None,
    base_url: Optional[str] = None,
    id: Optional[str] = None,
    network: Optional[Network] = None,
) -> DirectSandbox:
    """The single seam that builds a native ``DirectSandbox``.

    Factored out so tests can patch it to a fake target and exercise the
    provider without a server (the same pattern the other adapters use)."""
    return _create(template, api_key=api_key, base_url=base_url, id=id, network=network)


class MitosVibeKitSandbox:
    """A VibeKit sandbox handle backed by a mitos ``DirectSandbox``.

    Exposes VibeKit's sandbox surface: ``run_command`` (alias ``execute``),
    ``read_file`` / ``write_file`` / ``list_files``, ``run_code``, and the
    ``kill`` (alias ``close``) lifecycle. Branching is available via the native
    ``fork``.
    """

    def __init__(self, sandbox: DirectSandbox):
        self._sandbox = sandbox

    @property
    def id(self) -> str:
        return self._sandbox.id

    @property
    def sandbox(self) -> DirectSandbox:
        """The underlying native ``DirectSandbox`` (full SDK surface, including
        ``fork`` and ``pause`` / ``resume``, which VibeKit does not expose)."""
        return self._sandbox

    def run_command(self, command: str, timeout: int = 60) -> dict[str, Any]:
        """VibeKit "run a command" -> ``DirectSandbox.exec``.

        Returns the normalized command-result dict
        (``stdout`` / ``stderr`` / ``exit_code`` / ``exec_time_ms``) VibeKit
        providers yield, via the shared ``_mapping`` op layer."""
        return _mapping.map_execute(self._sandbox, command, timeout=timeout)

    # VibeKit name parity: ``execute`` is an alias for ``run_command``.
    execute = run_command

    def read_file(self, path: str) -> str:
        """VibeKit "read file" -> ``DirectSandbox.files.read``."""
        return _mapping.map_files_read(self._sandbox, path)

    def write_file(self, path: str, content: str | bytes) -> None:
        """VibeKit "write file" -> ``DirectSandbox.files.write``."""
        _mapping.map_files_write(self._sandbox, path, content)

    def list_files(self, path: str = "/") -> List[FileInfo]:
        """VibeKit "list directory" -> ``DirectSandbox.files.list``."""
        return _mapping.map_files_list(self._sandbox, path)

    def run_code(
        self,
        code: str,
        language: str = "python",
        timeout: int = 60,
        on_stdout: Optional[Callable[[str], None]] = None,
        on_stderr: Optional[Callable[[str], None]] = None,
        on_result: Optional[Callable[[Any], None]] = None,
    ) -> Execution:
        """VibeKit "execute code" -> ``DirectSandbox.run_code``.

        Returns the rich ``Execution`` (MIME ``Result`` artifacts, buffered
        logs, structured error)."""
        return _mapping.map_run_code(
            self._sandbox,
            code,
            language=language,
            timeout=timeout,
            on_stdout=on_stdout,
            on_stderr=on_stderr,
            on_result=on_result,
        )

    def fork(self, n: int = 1) -> List["MitosVibeKitSandbox"]:
        """Native branching: fork into ``n`` sibling sandboxes.

        VibeKit's provider interface has no branching slot, so this is reached as
        a mitos-native op. Each child wraps an independent forked
        ``DirectSandbox``."""
        return [MitosVibeKitSandbox(child) for child in self._sandbox.fork(n)]

    def kill(self) -> None:
        """VibeKit "destroy sandbox" -> ``DirectSandbox.terminate``."""
        self._sandbox.terminate()

    # VibeKit lifecycle aliases.
    close = kill

    def __enter__(self) -> "MitosVibeKitSandbox":
        return self

    def __exit__(self, *args: Any) -> None:
        self.kill()

    def __repr__(self) -> str:
        return f"MitosVibeKitSandbox(id={self._sandbox.id!r})"


class MitosVibeKitProvider:
    """The mitos provider for VibeKit.

    VibeKit selects a provider by ``name`` and creates sandboxes through it.
    Construct it once with the standalone sandbox-server / hosted control plane
    coordinates, then ``create`` sandboxes::

        from mitos.integrations.vibekit import MitosVibeKitProvider

        provider = MitosVibeKitProvider(base_url="http://localhost:8080")
        sandbox = provider.create("python")
        out = sandbox.run_command("echo hi")
        sandbox.kill()
    """

    #: The stable provider key VibeKit registers and a user selects.
    name = "mitos"

    def __init__(
        self,
        api_key: Optional[str] = None,
        base_url: Optional[str] = None,
    ):
        self._api_key = api_key
        self._base_url = base_url

    def create(
        self,
        template: str = "python",
        network: Optional[Network] = None,
        **_ignored: Any,
    ) -> MitosVibeKitSandbox:
        """VibeKit ``provider.create(template)`` -> ``mitos.create``.

        Returns a READY ``MitosVibeKitSandbox`` over the standalone
        sandbox-server / hosted control plane (no Kubernetes). Auth resolves like
        ``mitos.create``: the provider's ``api_key`` / ``base_url`` if given, else
        ``MITOS_API_KEY`` / ``MITOS_BASE_URL``."""
        sb = _create_direct(
            template,
            api_key=self._api_key,
            base_url=self._base_url,
            network=network,
        )
        return MitosVibeKitSandbox(sb)

    def __repr__(self) -> str:
        return f"MitosVibeKitProvider(name={self.name!r})"
