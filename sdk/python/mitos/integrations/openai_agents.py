"""OpenAI Agents SDK sandbox tools backed by mitos.

WHY: the OpenAI Agents SDK (the ``openai-agents`` package) lets an agent call
tools defined as function tools. When a developer wants those tool calls to run
in a sandbox, they bind a set of function tools to a backend. This adapter binds
``run_command`` / ``read_file`` / ``write_file`` / ``run_code`` to a mitos
sandbox so any OpenAI Agents agent can execute and edit files inside a mitos
snapshot-fork sandbox. This is an ADAPTER over the existing SDK ops (exec, files,
run_code), not engine work.

OPENAI AGENTS SDK CONTRACT TARGETED (recorded so reviewers can check conformance;
the package cannot be browsed here, so the shape is documented and ADJUSTABLE):

  The OpenAI Agents SDK exposes tools as ``FunctionTool`` definitions, each with
  a ``name``, a ``description``, a JSON-schema for its parameters
  (``params_json_schema``), and an invocation callable the runtime calls with the
  parsed arguments. The idiomatic way to author one is the ``@function_tool``
  decorator over a plain Python function (it derives the schema from the
  signature / docstring).

  The natural adapter is therefore a SET of function tools bound to a sandbox,
  plus a "tools for a sandbox" factory:

    - ``run_command(command, timeout)``  -> ``target.exec``
    - ``read_file(path)``                -> ``target.files.read``
    - ``write_file(path, content)``      -> ``target.files.write``
    - ``run_code(code, language, ...)``  -> ``target.run_code``

  ADJUSTABILITY: every tool body is a one-line call into the shared
  ``mitos.integrations._mapping`` helpers, and the tool name / description /
  schema live in one table (``_TOOL_SPECS``). If upstream names or schema fields
  differ (``run_shell`` vs ``run_command``, ``parameters`` vs
  ``params_json_schema``), only ``_FunctionToolDef`` and that table move; the
  wire-op mapping does not. ``as_function_tools()`` is the single seam that turns
  these framework-neutral defs into real ``openai-agents`` ``FunctionTool``
  objects, and it imports the SDK LAZILY.

OPTIONAL DEPENDENCY: this module imports mitos ALWAYS and imports
``openai-agents`` NEVER at runtime. The SDK is referenced only under
``TYPE_CHECKING`` and inside ``as_function_tools()``. The adapter is fully usable
and testable with ``openai-agents`` absent; only ``as_function_tools()`` needs
it, and it raises a clear ImportError (naming the ``[openai-agents]`` extra)
when it is missing.

STANDALONE TARGET: ``MitosSandboxTools.create`` builds over the standalone
sandbox-server / hosted control plane (``mitos.create`` / ``DirectSandbox``), so
the adapter runs WITHOUT Kubernetes.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import TYPE_CHECKING, Any, Callable, List, Optional

from mitos.direct import DirectSandbox, create as _create
from mitos.integrations import _mapping
from mitos.types import Network

if TYPE_CHECKING:  # pragma: no cover - typing only; never imported at runtime.
    pass


@dataclass
class _FunctionToolDef:
    """Framework-neutral function-tool definition.

    Mirrors the OpenAI Agents SDK ``FunctionTool`` surface (``name``,
    ``description``, ``params_json_schema``, plus the invocation callable
    ``func``) without importing the SDK. ``as_function_tools`` converts a list of
    these into real ``FunctionTool`` objects when the SDK is installed.
    """

    name: str
    description: str
    params_json_schema: dict[str, Any]
    func: Callable[..., Any]


def _obj_schema(
    properties: dict[str, Any], required: List[str]
) -> dict[str, Any]:
    return {
        "type": "object",
        "properties": properties,
        "required": required,
        "additionalProperties": False,
    }


class MitosSandboxTools:
    """OpenAI Agents SDK sandbox tools bound to a mitos sandbox.

    Wraps a ready ``DirectSandbox`` and exposes the four sandbox tool wrappers
    (``run_command``, ``read_file``, ``write_file``, ``run_code``) plus the
    factory ``function_tools()`` / ``as_function_tools()`` and the
    ``create`` / ``close`` lifecycle.

    Construct it with an existing handle::

        from mitos import create
        from mitos.integrations.openai_agents import MitosSandboxTools

        tools = MitosSandboxTools(create("python", base_url="http://localhost:8080"))

    or with the lifecycle classmethod::

        tools = MitosSandboxTools.create("python", base_url="http://localhost:8080")
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
    ) -> "MitosSandboxTools":
        """Create a READY tool set over the standalone sandbox-server / hosted
        control plane (no Kubernetes). Resolves auth exactly like
        ``mitos.create``."""
        sandbox = _create(
            image, api_key=api_key, base_url=base_url, id=id, network=network
        )
        return cls(sandbox)

    @property
    def sandbox(self) -> DirectSandbox:
        """The underlying native ``DirectSandbox`` (the full SDK surface)."""
        return self._sandbox

    def close(self) -> None:
        """Terminate the sandbox and release its handle."""
        self._sandbox.terminate()

    def __enter__(self) -> "MitosSandboxTools":
        return self

    def __exit__(self, *args: Any) -> None:
        self.close()

    # -- tool wrappers (thin; each one call into _mapping) ------------------

    def run_command(self, command: str, timeout: int = 30) -> dict[str, Any]:
        """Run a shell command; return the normalized result dict
        (``stdout``, ``stderr``, ``exit_code``, ``exec_time_ms``)."""
        return _mapping.map_execute(self._sandbox, command, timeout=timeout)

    def read_file(self, path: str) -> str:
        """Read a file's text content."""
        return _mapping.map_files_read(self._sandbox, path)

    def write_file(self, path: str, content: str) -> dict[str, Any]:
        """Write content to a file; return a small ack dict for the tool call."""
        _mapping.map_files_write(self._sandbox, path, content)
        return {"path": path, "written": True}

    def list_files(self, path: str = "/") -> List[dict[str, Any]]:
        """List a directory; return JSON-friendly file dicts."""
        return [
            _mapping.file_info_to_dict(info)
            for info in _mapping.map_files_list(self._sandbox, path)
        ]

    def run_code(
        self, code: str, language: str = "python", timeout: int = 60
    ) -> dict[str, Any]:
        """Execute a code snippet in the stateful kernel; return the JSON-friendly
        flattened ``Execution`` (text, logs, rich MIME results, structured
        error) so the rich artifacts survive the tool-call serialization."""
        execution = _mapping.map_run_code(
            self._sandbox, code, language=language, timeout=timeout
        )
        return _mapping.execution_to_dict(execution)

    # -- function-tool factory --------------------------------------------

    def function_tools(self) -> List[_FunctionToolDef]:
        """Return the framework-neutral function-tool definitions bound to this
        sandbox. Convert to real ``openai-agents`` ``FunctionTool`` objects with
        ``as_function_tools`` (which imports the SDK lazily)."""
        return tools_for_sandbox(self._sandbox)

    def as_function_tools(self) -> list:
        """Convert the function-tool defs into real ``openai-agents``
        ``FunctionTool`` objects.

        This is the ONLY seam that needs ``openai-agents`` installed; it imports
        the SDK lazily so the rest of the module works without it. Raises a clear
        ImportError naming the optional extra when the SDK is absent.
        """
        try:
            from agents import FunctionTool  # type: ignore
        except ImportError as exc:  # pragma: no cover - exercised via the fake path
            raise ImportError(
                "as_function_tools() needs the OpenAI Agents SDK. Install it with "
                "'pip install \"mitos[openai-agents]\"' (the openai-agents "
                "package). The rest of this adapter works without it."
            ) from exc

        tools: list = []
        for spec in self.function_tools():
            tools.append(
                FunctionTool(
                    name=spec.name,
                    description=spec.description,
                    params_json_schema=spec.params_json_schema,
                    on_invoke_tool=spec.func,
                )
            )
        return tools

    def __repr__(self) -> str:
        return f"MitosSandboxTools(id={self._sandbox.id!r})"


def tools_for_sandbox(target: Any) -> List[_FunctionToolDef]:
    """Build the four sandbox function-tool defs bound to ``target``.

    ``target`` is anything OpsTarget-shaped (a ``DirectSandbox`` or a fake), so
    this factory is usable with no server and no ``openai-agents`` installed.
    Each def's ``func`` is a thin closure over a ``_mapping`` helper.
    """
    tools = MitosSandboxTools(target)

    return [
        _FunctionToolDef(
            name="run_command",
            description=(
                "Run a shell command inside the sandbox and return its stdout, "
                "stderr, and exit code."
            ),
            params_json_schema=_obj_schema(
                {
                    "command": {
                        "type": "string",
                        "description": "The shell command to run.",
                    },
                    "timeout": {
                        "type": "integer",
                        "description": "Seconds before the command is killed.",
                        "default": 30,
                    },
                },
                ["command"],
            ),
            func=tools.run_command,
        ),
        _FunctionToolDef(
            name="read_file",
            description="Read a text file from the sandbox and return its contents.",
            params_json_schema=_obj_schema(
                {
                    "path": {
                        "type": "string",
                        "description": "Absolute path of the file to read.",
                    }
                },
                ["path"],
            ),
            func=tools.read_file,
        ),
        _FunctionToolDef(
            name="write_file",
            description="Write text content to a file in the sandbox.",
            params_json_schema=_obj_schema(
                {
                    "path": {
                        "type": "string",
                        "description": "Absolute path of the file to write.",
                    },
                    "content": {
                        "type": "string",
                        "description": "Text content to write.",
                    },
                },
                ["path", "content"],
            ),
            func=tools.write_file,
        ),
        _FunctionToolDef(
            name="run_code",
            description=(
                "Execute a code snippet in the sandbox's stateful kernel and "
                "return its text result, logs, rich results, and any error."
            ),
            params_json_schema=_obj_schema(
                {
                    "code": {
                        "type": "string",
                        "description": "The code snippet to execute.",
                    },
                    "language": {
                        "type": "string",
                        "description": "Language of the snippet.",
                        "default": "python",
                    },
                },
                ["code"],
            ),
            func=tools.run_code,
        ),
    ]
