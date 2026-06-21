"""Shared wire-op mapping between agent-framework sandbox interfaces and the
native mitos SDK surface.

This is the ONE translation layer the framework adapters reuse:

  - LangChain ``MitosSandbox`` (#203)
  - OpenAI / Claude Agent SDK adapters (#204)
  - the E2B-compat shim (#206)

The mapping is deliberately framework agnostic. Every helper takes an
``OpsTarget`` (anything that quacks like a ``DirectSandbox``: it has ``exec``,
``run_code``, and ``files``) plus plain arguments, and returns a plain dict or a
native mitos type (``ExecResult``, ``Execution``, ``FileInfo``). Adapters then
reshape that into whatever the framework's contract wants. Because the helpers
never import a framework, they are unit-testable with no framework installed and
with a trivial fake target (no KVM, no running server).

Operation mapping (framework verb -> mitos op):

  - run a shell command        -> ``target.exec(command, timeout=...)``
  - execute a code snippet     -> ``target.run_code(code, language=...)``
  - read a file                -> ``target.files.read(path)``
  - write a file               -> ``target.files.write(path, content)``
  - list a directory           -> ``target.files.list(path)``

The shell-command result is normalized to the dict shape frameworks expect from
a sandbox command:  ``{"stdout", "stderr", "exit_code", "exec_time_ms"}``. The
code-execution result is the rich ``Execution`` (MIME ``Result`` artifacts,
buffered logs, structured error); ``execution_to_dict`` flattens it for callers
that want a JSON-friendly shape instead of the dataclass.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Any, Callable, List, Optional, Protocol

from mitos.types import Execution, ExecResult, FileInfo, Result

if TYPE_CHECKING:  # pragma: no cover - typing only
    from mitos.types import Result as _Result


class OpsTarget(Protocol):
    """Structural type for anything the mapping can drive.

    A ``DirectSandbox`` satisfies this, but so does any fake exposing the same
    three members, which is what keeps the helpers testable without a server.
    """

    files: Any

    def exec(self, command: str, timeout: int = ...) -> ExecResult: ...

    def run_code(
        self,
        code: str,
        language: str = ...,
        timeout: int = ...,
        on_stdout: Optional[Callable[[str], None]] = ...,
        on_stderr: Optional[Callable[[str], None]] = ...,
        on_result: Optional[Callable[["_Result"], None]] = ...,
    ) -> Execution: ...


def map_execute(
    target: OpsTarget, command: str, timeout: int = 30
) -> dict[str, Any]:
    """Map a framework "run shell command" call onto ``target.exec``.

    Returns the normalized command-result dict
    (``stdout``, ``stderr``, ``exit_code``, ``exec_time_ms``) that sandbox
    backends are expected to yield. Adapters that want the raw dataclass can
    call ``target.exec`` directly; this helper exists so every adapter produces
    the same shape.
    """
    result = target.exec(command, timeout=timeout)
    return exec_result_to_dict(result)


def exec_result_to_dict(result: ExecResult) -> dict[str, Any]:
    """Normalize a native ``ExecResult`` to the framework command-result dict."""
    return {
        "stdout": result.stdout,
        "stderr": result.stderr,
        "exit_code": result.exit_code,
        "exec_time_ms": result.exec_time_ms,
    }


def map_run_code(
    target: OpsTarget,
    code: str,
    language: str = "python",
    timeout: int = 60,
    on_stdout: Optional[Callable[[str], None]] = None,
    on_stderr: Optional[Callable[[str], None]] = None,
    on_result: Optional[Callable[["_Result"], None]] = None,
) -> Execution:
    """Map a framework "execute code" call onto ``target.run_code``.

    Returns the native ``Execution`` (rich MIME ``Result`` artifacts, buffered
    logs, structured error). Use ``execution_to_dict`` when a JSON-friendly
    shape is needed instead of the dataclass.
    """
    return target.run_code(
        code,
        language=language,
        timeout=timeout,
        on_stdout=on_stdout,
        on_stderr=on_stderr,
        on_result=on_result,
    )


def map_files_read(target: OpsTarget, path: str) -> str:
    """Map a framework "read file" call onto ``target.files.read``."""
    return target.files.read(path)


def map_files_write(
    target: OpsTarget, path: str, content: str | bytes
) -> None:
    """Map a framework "write file" call onto ``target.files.write``."""
    target.files.write(path, content)


def map_files_list(target: OpsTarget, path: str = "/") -> List[FileInfo]:
    """Map a framework "list directory" call onto ``target.files.list``."""
    return target.files.list(path)


def file_info_to_dict(info: FileInfo) -> dict[str, Any]:
    """Flatten one ``FileInfo`` to a JSON-friendly dict.

    Shared by the OpenAI / Claude adapters (#204), whose ``list_files`` tools
    return JSON the model reads rather than the native dataclass. ``mode`` is the
    integer file mode; ``modified_at`` is included only when present.
    """
    out: dict[str, Any] = {
        "name": info.name,
        "is_dir": info.is_dir,
        "size": info.size,
        "mode": info.mode,
    }
    if info.modified_at is not None:
        out["modified_at"] = info.modified_at
    return out


def result_to_dict(result: Result) -> dict[str, Any]:
    """Flatten one rich ``Result`` artifact to a JSON-friendly dict."""
    return {"data": dict(result.data), "is_main_result": result.is_main_result}


def execution_to_dict(execution: Execution) -> dict[str, Any]:
    """Flatten an ``Execution`` to a JSON-friendly dict.

    Keeps every field a framework might surface: the REPL ``text`` value, the
    buffered ``logs``, each rich ``results`` artifact (with its MIME map), and
    the structured ``error`` (or ``None``). Adapters that hand results back to an
    LLM tool call use this so the rich artifacts survive serialization.
    """
    error = None
    if execution.error is not None:
        error = {
            "name": execution.error.name,
            "value": execution.error.value,
            "traceback": list(execution.error.traceback),
        }
    return {
        "text": execution.text,
        "logs": {
            "stdout": list(execution.logs.get("stdout", [])),
            "stderr": list(execution.logs.get("stderr", [])),
        },
        "results": [result_to_dict(r) for r in execution.results],
        "error": error,
    }
