"""Claude Agent SDK sandbox tools backed by mitos.

WHY: the Claude Agent SDK (the ``claude-agent-sdk`` package) lets an agent call
tools. The idiomatic way to give it custom tools is an in-process MCP server
built from ``@tool``-decorated handlers. When a developer wants those tool calls
to run in a sandbox, they bind a tool set to a backend. This adapter binds
``run_command`` / ``read_file`` / ``write_file`` / ``run_code`` to a mitos
sandbox so any Claude Agent SDK agent can execute and edit files inside a mitos
snapshot-fork sandbox. This is an ADAPTER over the existing SDK ops (exec, files,
run_code), not engine work.

CLAUDE AGENT SDK CONTRACT TARGETED (recorded so reviewers can check conformance;
the package cannot be browsed here, so the shape is documented and ADJUSTABLE):

  The Claude Agent SDK exposes custom tools via the ``@tool(name, description,
  input_schema)`` decorator over an async handler ``handler(args) -> result``,
  where ``result`` is an MCP tool result: ``{"content": [{"type": "text",
  "text": ...}], ...}``. Those tools are grouped into an in-process MCP server
  with ``create_sdk_mcp_server(name=..., tools=[...])`` and handed to the agent
  via its ``mcp_servers`` option.

  The natural adapter is therefore a SET of MCP-style tool definitions bound to a
  sandbox, plus a "tools for a sandbox" factory and an "as MCP server" seam:

    - ``run_command({command, timeout})``  -> ``target.exec``
    - ``read_file({path})``                -> ``target.files.read``
    - ``write_file({path, content})``      -> ``target.files.write``
    - ``run_code({code, language})``       -> ``target.run_code``

  Each handler returns an MCP tool result (a ``content`` list of typed text
  blocks); the rich ``run_code`` execution is serialized into the text block as
  JSON so the artifacts survive.

  ADJUSTABILITY: every handler body is a one-line call into the shared
  ``mitos.integrations._mapping`` helpers, and the tool name / description /
  schema live in one table (``tools_for_sandbox``). If upstream names or the
  result envelope differ (``input_schema`` vs ``parameters``, a different content
  block shape), only ``_ToolDef`` / ``_text_result`` and that table move; the
  wire-op mapping does not. ``as_mcp_server()`` is the single seam that wraps
  these defs in a real ``claude-agent-sdk`` MCP server, and it imports the SDK
  LAZILY.

OPTIONAL DEPENDENCY: this module imports mitos ALWAYS and imports
``claude-agent-sdk`` NEVER at runtime. The SDK is referenced only under
``TYPE_CHECKING`` and inside ``as_mcp_server()``. The adapter is fully usable and
testable with ``claude-agent-sdk`` absent; only ``as_mcp_server()`` needs it, and
it raises a clear ImportError (naming the ``[claude-agent]`` extra) when missing.

STANDALONE TARGET: ``MitosSandboxTools.create`` builds over the standalone
sandbox-server / hosted control plane (``mitos.create`` / ``DirectSandbox``), so
the adapter runs WITHOUT Kubernetes.
"""

from __future__ import annotations

import json
from dataclasses import dataclass
from typing import TYPE_CHECKING, Any, Callable, List, Optional

from mitos.direct import DirectSandbox, create as _create
from mitos.integrations import _mapping
from mitos.types import Network

if TYPE_CHECKING:  # pragma: no cover - typing only; never imported at runtime.
    pass


@dataclass
class _ToolDef:
    """Framework-neutral MCP-style tool definition.

    Mirrors the Claude Agent SDK custom-tool surface (``name``, ``description``,
    ``input_schema``, plus the ``handler`` invoked with the parsed args) without
    importing the SDK. ``as_mcp_server`` wraps a list of these in a real
    in-process MCP server when the SDK is installed.
    """

    name: str
    description: str
    input_schema: dict[str, Any]
    handler: Callable[[dict[str, Any]], dict[str, Any]]


def _text_result(text: str) -> dict[str, Any]:
    """Build an MCP tool result carrying a single text block."""
    return {"content": [{"type": "text", "text": text}]}


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
    """Claude Agent SDK sandbox tools bound to a mitos sandbox.

    Wraps a ready ``DirectSandbox`` and exposes the four MCP-style tool handlers
    (``run_command``, ``read_file``, ``write_file``, ``run_code``) plus the
    factory ``tool_defs()`` / ``as_mcp_server()`` and the ``create`` / ``close``
    lifecycle.

    Construct it with an existing handle::

        from mitos import create
        from mitos.integrations.claude_agent import MitosSandboxTools

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

    # -- tool handlers (thin; each one call into _mapping) -----------------

    def run_command(self, args: dict[str, Any]) -> dict[str, Any]:
        """Run a shell command; return an MCP text result holding the JSON of the
        normalized result dict (``stdout``, ``stderr``, ``exit_code``,
        ``exec_time_ms``)."""
        out = _mapping.map_execute(
            self._sandbox,
            args["command"],
            timeout=int(args.get("timeout", 30)),
        )
        return _text_result(json.dumps(out))

    def read_file(self, args: dict[str, Any]) -> dict[str, Any]:
        """Read a file's text content; return it in an MCP text result."""
        content = _mapping.map_files_read(self._sandbox, args["path"])
        return _text_result(content)

    def write_file(self, args: dict[str, Any]) -> dict[str, Any]:
        """Write content to a file; return an MCP text ack."""
        _mapping.map_files_write(self._sandbox, args["path"], args["content"])
        return _text_result(f"wrote {args['path']}")

    def list_files(self, args: dict[str, Any]) -> dict[str, Any]:
        """List a directory; return the JSON of the file dicts in a text result."""
        listing = [
            _mapping.file_info_to_dict(info)
            for info in _mapping.map_files_list(self._sandbox, args.get("path", "/"))
        ]
        return _text_result(json.dumps(listing))

    def run_code(self, args: dict[str, Any]) -> dict[str, Any]:
        """Execute a code snippet; return an MCP text result holding the JSON of
        the flattened ``Execution`` (text, logs, rich MIME results, structured
        error) so the rich artifacts survive."""
        execution = _mapping.map_run_code(
            self._sandbox,
            args["code"],
            language=args.get("language", "python"),
            timeout=int(args.get("timeout", 60)),
        )
        return _text_result(json.dumps(_mapping.execution_to_dict(execution)))

    # -- MCP tool factory --------------------------------------------------

    def tool_defs(self) -> List[_ToolDef]:
        """Return the framework-neutral MCP-style tool defs bound to this
        sandbox. Wrap them in a real in-process MCP server with
        ``as_mcp_server`` (which imports the SDK lazily)."""
        return tools_for_sandbox(self._sandbox)

    def as_mcp_server(self, name: str = "mitos-sandbox") -> Any:
        """Wrap the tool defs in a real ``claude-agent-sdk`` in-process MCP
        server.

        This is the ONLY seam that needs ``claude-agent-sdk`` installed; it
        imports the SDK lazily so the rest of the module works without it. Raises
        a clear ImportError naming the optional extra when the SDK is absent.
        """
        try:
            from claude_agent_sdk import (  # type: ignore
                create_sdk_mcp_server,
                tool,
            )
        except ImportError as exc:  # pragma: no cover - exercised via the fake path
            raise ImportError(
                "as_mcp_server() needs the Claude Agent SDK. Install it with "
                "'pip install \"mitos[claude-agent]\"' (the claude-agent-sdk "
                "package). The rest of this adapter works without it."
            ) from exc

        sdk_tools = []
        for spec in self.tool_defs():
            handler = spec.handler

            @tool(spec.name, spec.description, spec.input_schema)
            async def _t(args: dict, _handler=handler):  # noqa: ANN001
                return _handler(args)

            sdk_tools.append(_t)
        return create_sdk_mcp_server(name=name, tools=sdk_tools)

    def __repr__(self) -> str:
        return f"MitosSandboxTools(id={self._sandbox.id!r})"


def tools_for_sandbox(target: Any) -> List[_ToolDef]:
    """Build the four sandbox MCP-style tool defs bound to ``target``.

    ``target`` is anything OpsTarget-shaped (a ``DirectSandbox`` or a fake), so
    this factory is usable with no server and no ``claude-agent-sdk`` installed.
    Each handler is a thin closure over a ``_mapping`` helper.
    """
    tools = MitosSandboxTools(target)

    return [
        _ToolDef(
            name="run_command",
            description=(
                "Run a shell command inside the sandbox and return its stdout, "
                "stderr, and exit code."
            ),
            input_schema=_obj_schema(
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
            handler=tools.run_command,
        ),
        _ToolDef(
            name="read_file",
            description="Read a text file from the sandbox and return its contents.",
            input_schema=_obj_schema(
                {
                    "path": {
                        "type": "string",
                        "description": "Absolute path of the file to read.",
                    }
                },
                ["path"],
            ),
            handler=tools.read_file,
        ),
        _ToolDef(
            name="write_file",
            description="Write text content to a file in the sandbox.",
            input_schema=_obj_schema(
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
            handler=tools.write_file,
        ),
        _ToolDef(
            name="run_code",
            description=(
                "Execute a code snippet in the sandbox's stateful kernel and "
                "return its text result, logs, rich results, and any error."
            ),
            input_schema=_obj_schema(
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
            handler=tools.run_code,
        ),
    ]
