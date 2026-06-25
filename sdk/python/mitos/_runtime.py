"""Proto-JSON decoding and folding shared by every Connect runtime caller.

The native runtime calls (exec, run_code, files) speak the Connect
``sandbox.v1.Sandbox`` service. The wire is the same regardless of whether the
caller is the sync ``DirectSandbox``, the sync k8s ``Sandbox``, the async
``AsyncDirectSandbox``, or the async k8s ``AsyncSandbox``: every response frame
is a proto-JSON message (camelCase fields, bytes as base64 strings). This module
holds the pure decode-and-fold helpers so that mapping is written ONCE and the
four clients differ only in their transport (sync vs async ConnectClient).
"""
from __future__ import annotations

import base64
from typing import AsyncIterator, Callable, Iterable, Optional

from mitos.types import Execution, ExecutionError, FileInfo, Result


def b64_decode(value) -> bytes:
    """Decode a proto-JSON bytes field (a base64 string) to raw bytes. None and
    the empty string both decode to empty bytes; already-bytes pass through."""
    if not value:
        return b""
    if isinstance(value, (bytes, bytearray)):
        return bytes(value)
    return base64.b64decode(value)


def fileinfo_from_proto(f: dict) -> FileInfo:
    """Map one proto-JSON FileInfo (camelCase: isDir, modifiedAtUnix) to the
    SDK's FileInfo type."""
    return FileInfo(
        name=f.get("name", ""),
        is_dir=f.get("isDir", False),
        size=int(f.get("size", 0)),
        mode=int(f.get("mode", 0)),
        modified_at=f.get("modifiedAtUnix"),
    )


def decode_result_data(data: dict) -> dict[str, str]:
    """Decode a Connect RunResult.data map (proto-JSON map<string,bytes>: every
    value is base64) back to the MIME->payload form the Result type expects.

    The guest stores each display value as the raw bytes of the kernel's string
    output (text/plain is "42"; image/png is the already-base64 string), and
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


# The exception raised when a RunCodeStream ends before its terminal exit frame.
# A truncated or dropped connection is surfaced rather than a misleading clean
# Execution with error=None.
_TRUNCATED_RUN_CODE = (
    "run_code stream ended before the terminal exit frame: "
    "the connection was truncated or dropped; the result is unknown"
)


def _apply_run_code_frame(
    ex: Execution,
    frame: dict,
    on_stdout: Optional[Callable[[str], None]],
    on_stderr: Optional[Callable[[str], None]],
    on_result: Optional[Callable[[Result], None]],
) -> bool:
    """Fold one Connect RunCodeResponse frame into ``ex`` and fire the matching
    callback. Returns True when this is the terminal ``exitCode`` frame.

    The proto-JSON frames are the RunCodeResponse oneof: stdout/stderr (base64
    bytes), result (RunResult{text,data}), error (RunError{name,value,traceback}),
    and the terminal exitCode. Result and error payloads are tenant code output
    and are never logged here."""
    if "stdout" in frame:
        text = b64_decode(frame.get("stdout")).decode("utf-8", "replace")
        ex.logs["stdout"].append(text)
        if on_stdout:
            on_stdout(text)
    elif "stderr" in frame:
        text = b64_decode(frame.get("stderr")).decode("utf-8", "replace")
        ex.logs["stderr"].append(text)
        if on_stderr:
            on_stderr(text)
    elif "result" in frame:
        payload = frame.get("result") or {}
        data = decode_result_data(payload.get("data") or {})
        text = payload.get("text") or ""
        is_main = bool(text)
        # The REPL last-value is delivered in RunResult.text; mirror it into the
        # text/plain MIME slot so Result.text resolves the same way the NDJSON
        # path did.
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
        return True
    return False


def parse_run_code_connect(
    frames: Iterable[dict],
    on_stdout: Optional[Callable[[str], None]],
    on_stderr: Optional[Callable[[str], None]],
    on_result: Optional[Callable[[Result], None]],
) -> Execution:
    """Fold a sync Connect RunCodeStream response-frame iterator into an
    Execution, firing the callbacks live as frames arrive."""
    ex = Execution()
    saw_exit = False
    for frame in frames:
        if _apply_run_code_frame(ex, frame, on_stdout, on_stderr, on_result):
            saw_exit = True
            break
    if not saw_exit:
        raise RuntimeError(_TRUNCATED_RUN_CODE)
    return ex


async def aparse_run_code_connect(
    frames: AsyncIterator[dict],
    on_stdout: Optional[Callable[[str], None]],
    on_stderr: Optional[Callable[[str], None]],
    on_result: Optional[Callable[[Result], None]],
) -> Execution:
    """Async mirror of parse_run_code_connect: fold an async RunCodeStream
    response-frame iterator into an Execution, firing the callbacks live."""
    ex = Execution()
    saw_exit = False
    async for frame in frames:
        if _apply_run_code_frame(ex, frame, on_stdout, on_stderr, on_result):
            saw_exit = True
            break
    if not saw_exit:
        raise RuntimeError(_TRUNCATED_RUN_CODE)
    return ex
