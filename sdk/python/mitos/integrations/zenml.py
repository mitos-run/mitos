"""ZenML sandbox stack-component adapter backed by mitos (issue #205).

WHY: ZenML treats a sandbox as a pluggable stack component selected by a flavor.
Listing mitos there is near-free distribution: a ZenML user adds the mitos flavor
to their stack and their steps run inside a mitos snapshot-fork sandbox. The
external LISTING (registering the flavor in ZenML's integration registry) is a
human step; what is implemented here is the framework-neutral BACKEND the flavor
wraps, backed by the native ``DirectSandbox`` surface through the shared
``mitos.integrations._mapping`` op layer.

ZENML STACK-COMPONENT CONTRACT TARGETED (recorded so a reviewer can check
conformance when ZenML's sandbox component base class is in hand):

  A ZenML stack component is a configured class with:

    - a typed CONFIG object (here ``MitosSandboxConfig``: ``template`` /
      ``base_url`` / ``api_key``), the settings a user supplies in their stack.
    - a FLAVOR name (here ``"mitos"``), the key ZenML registers and a user
      selects.
    - backend logic the flavor invokes: provision a sandbox, run a command,
      filesystem ops, run code, deprovision. Mapped to ``mitos.create`` /
      ``DirectSandbox.exec`` / ``DirectSandbox.files.*`` / ``DirectSandbox.run_code``
      / ``DirectSandbox.terminate``.

  ADJUSTABILITY: every backend verb is translated through the shared ``_mapping``
  helpers, and the method names live in one place (``MitosSandboxComponent``). If
  ZenML's sandbox base class names a hook differently (e.g. ``prepare`` vs
  ``provision``, ``cleanup`` vs ``deprovision``), only the thin method name on the
  component moves; the wire-op mapping does not.

OPTIONAL DEPENDENCY: this module imports mitos ALWAYS and the ``zenml`` package
NEVER at runtime. ``MitosSandboxComponent`` and ``MitosSandboxConfig`` are plain
classes, fully usable and testable with ZenML absent. ``flavor()`` is the ONE
ZenML seam: it imports ZenML lazily and, when ZenML is not installed, raises a
clear typed error naming the extra rather than an ImportError stack trace.

REGISTERING WITH ZENML (the remaining MAINTAINER step, NOT done here): listing
mitos as a selectable flavor is a contribution to ZenML's own integration
registry (subclass ZenML's sandbox stack-component base with the config and
flavor below, register the flavor through ZenML's flavor API, and follow their
CONTRIBUTING process to open a PR). This adapter is the backend that subclass
delegates to; the external registration is a human step. See the SDK README
integrations section for the high-level pointer.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import TYPE_CHECKING, Any, Callable, List, Optional

from mitos.direct import DirectSandbox, create as _create
from mitos.errors import error_for_code
from mitos.integrations import _mapping
from mitos.types import Execution, FileInfo, Network

if TYPE_CHECKING:  # pragma: no cover - typing only; zenml is never imported.
    pass


@dataclass
class MitosSandboxConfig:
    """The settings a ZenML user supplies for the mitos sandbox component.

    Mirrors the typed config a ZenML stack component is configured with. Auth
    resolves like ``mitos.create``: explicit ``api_key`` / ``base_url`` if set,
    else ``MITOS_API_KEY`` / ``MITOS_BASE_URL`` from the environment.
    """

    template: str = "python"
    base_url: Optional[str] = None
    api_key: Optional[str] = None
    network: Optional[Network] = None


def _create_direct(
    template: str = "python",
    api_key: Optional[str] = None,
    base_url: Optional[str] = None,
    id: Optional[str] = None,
    network: Optional[Network] = None,
) -> DirectSandbox:
    """The single seam that builds a native ``DirectSandbox``.

    Factored out so tests can patch it to a fake target and exercise the
    component without a server (the same pattern the other adapters use)."""
    return _create(template, api_key=api_key, base_url=base_url, id=id, network=network)


class MitosSandboxComponent:
    """The mitos sandbox stack component for ZenML.

    Framework-neutral backend: holds a ``MitosSandboxConfig`` and lazily
    provisions one ``DirectSandbox`` the ZenML step runs inside. Exposes
    provision / run_command / read_file / write_file / list_files / run_code /
    deprovision, each mapped onto the native op via the shared ``_mapping`` layer.

        from mitos.integrations.zenml import (
            MitosSandboxComponent, MitosSandboxConfig,
        )

        comp = MitosSandboxComponent(
            MitosSandboxConfig(template="python", base_url="http://localhost:8080")
        )
        comp.provision()
        comp.run_command("echo hi")
        comp.deprovision()
    """

    #: The stable flavor key ZenML registers and a user selects.
    FLAVOR = "mitos"

    def __init__(self, config: MitosSandboxConfig):
        self.config = config
        self._sandbox: Optional[DirectSandbox] = None

    # -- lifecycle ---------------------------------------------------------

    def provision(self) -> DirectSandbox:
        """Provision the sandbox (ZenML "prepare" hook) -> ``mitos.create``.

        Idempotent: the first call creates a READY ``DirectSandbox``; later calls
        return the same handle until ``deprovision``."""
        if self._sandbox is None:
            self._sandbox = _create_direct(
                self.config.template,
                api_key=self.config.api_key,
                base_url=self.config.base_url,
                network=self.config.network,
            )
        return self._sandbox

    def deprovision(self) -> None:
        """Deprovision the sandbox (ZenML "cleanup" hook) ->
        ``DirectSandbox.terminate``. Safe to call when nothing is provisioned."""
        if self._sandbox is not None:
            self._sandbox.terminate()
            self._sandbox = None

    @property
    def sandbox(self) -> DirectSandbox:
        """The provisioned ``DirectSandbox``, provisioning on first access."""
        return self.provision()

    # -- ops (mapped through the shared _mapping layer) --------------------

    def run_command(self, command: str, timeout: int = 60) -> dict[str, Any]:
        """Run a shell command -> ``DirectSandbox.exec``.

        Returns the normalized dict (``stdout`` / ``stderr`` / ``exit_code`` /
        ``exec_time_ms``)."""
        return _mapping.map_execute(self.provision(), command, timeout=timeout)

    def read_file(self, path: str) -> str:
        """Read a file -> ``DirectSandbox.files.read``."""
        return _mapping.map_files_read(self.provision(), path)

    def write_file(self, path: str, content: str | bytes) -> None:
        """Write a file -> ``DirectSandbox.files.write``."""
        _mapping.map_files_write(self.provision(), path, content)

    def list_files(self, path: str = "/") -> List[FileInfo]:
        """List a directory -> ``DirectSandbox.files.list``."""
        return _mapping.map_files_list(self.provision(), path)

    def run_code(
        self,
        code: str,
        language: str = "python",
        timeout: int = 60,
        on_stdout: Optional[Callable[[str], None]] = None,
        on_stderr: Optional[Callable[[str], None]] = None,
        on_result: Optional[Callable[[Any], None]] = None,
    ) -> Execution:
        """Execute a code snippet -> ``DirectSandbox.run_code``.

        Returns the rich ``Execution`` (MIME ``Result`` artifacts, buffered logs,
        structured error)."""
        return _mapping.map_run_code(
            self.provision(),
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
        """``run_code`` flattened to a JSON-friendly dict.

        Useful for ZenML steps that persist the execution as a JSON artifact
        rather than the native dataclass. Reuses ``_mapping.execution_to_dict``."""
        return _mapping.execution_to_dict(
            self.run_code(code, language=language, timeout=timeout)
        )

    # -- ZenML seam --------------------------------------------------------

    @classmethod
    def flavor(cls) -> Any:
        """Build the ZenML flavor object for this component (the ONE ZenML seam).

        Imports ZenML lazily so this module never hard-depends on it. When ZenML
        is not installed, raises a clear typed ``AgentRunError`` naming the extra
        and its remediation, not a raw ImportError. When ZenML IS installed, a
        maintainer subclasses ZenML's sandbox flavor base and returns it here;
        the registration itself is the external maintainer step (see module
        docstring)."""
        try:
            import zenml  # noqa: F401
        except ImportError as exc:
            raise error_for_code(
                "integration_dependency_missing",
                "the zenml package is required to build the ZenML flavor",
                cause="zenml is an optional dependency and is not installed",
                remediation=(
                    "Install the optional extra: pip install \"mitos[zenml]\". "
                    "The component backend (provision / run_command / files / "
                    "run_code / deprovision) works without zenml; only flavor() "
                    "needs it."
                ),
                status=501,
            ) from exc
        raise error_for_code(
            "integration_registration_pending",
            "registering the mitos flavor with ZenML is a maintainer step",
            cause=(
                "the mitos backend is implemented, but binding it to ZenML's "
                "sandbox flavor base class and registering it in ZenML's "
                "integration registry is an external contribution to ZenML"
            ),
            remediation=(
                "Subclass ZenML's sandbox stack-component base with "
                "MitosSandboxConfig and FLAVOR='mitos', delegate its hooks to "
                "this component, and register the flavor through ZenML's flavor "
                "API. See the SDK README integrations section."
            ),
            status=501,
        )

    def __repr__(self) -> str:
        return f"MitosSandboxComponent(flavor={self.FLAVOR!r}, template={self.config.template!r})"
