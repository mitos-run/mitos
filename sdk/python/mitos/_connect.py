"""A dependency-light Connect protocol codec over the SDK's existing httpx client.

The native direct-mode runtime calls (exec, files, run_code) speak the Connect
``sandbox.v1.Sandbox`` service (issue #24) instead of the legacy JSON ``/v1/*``
routes. Rather than add a generated-stub + codegen dependency to the flagship,
dependency-light SDK (today only ``httpx``), this module implements the Connect
wire directly. The proto-JSON message shapes come straight from
``proto/sandbox/v1/sandbox.proto`` (camelCase field names, bytes as base64
strings).

The Connect protocol, the two shapes used here:

  - UNARY (List, Stat, Mkdir, Remove): ``POST /sandbox.v1.Sandbox/<Method>`` with
    ``Content-Type: application/json`` and the proto-JSON request as the body.
    On 2xx the body is the proto-JSON reply. On non-2xx the body is the Connect
    error envelope ``{"code": "...", "message": "..."}``.

  - STREAM (Exec, RunCode bidi; ReadFile, Vitals server-stream; WriteFile
    client-stream): ``Content-Type: application/connect+json``. Every message is
    an ENVELOPED frame: a 5-byte prefix (1 flag byte + 4-byte big-endian length)
    then the JSON message bytes. The final server frame sets the end-stream flag
    (``0x02``) and its JSON payload carries trailers and, on failure, an
    ``error`` object. The client sends its request messages as plain (flag 0x00)
    enveloped frames and closes the request body; it does NOT send an end-stream
    frame.

The SDK's direct-mode exec/run_code only send the opening message (no live
stdin), so each streaming call is half-duplex: the full request body (one or
more enveloped frames) is sent, then the response frames are read incrementally.
That is exactly what httpx supports over HTTP/1.1 (buffered request, streamed
response), and it is the same shape the Go acceptance test drives (send open,
CloseRequest, then receive).

Secret values (the bearer token) are never logged here; on error they are
redacted from any message text via the shared envelope redactor.
"""
from __future__ import annotations

import json
import struct
from typing import Iterator, Optional

import httpx

from mitos._envelope import _redact
from mitos.errors import AgentRunError, error_for_code

# The end-stream flag on a Connect enveloped frame (bit 1). The final server
# frame sets it; its payload carries trailers and an optional error object.
_FLAG_END_STREAM = 0b00000010
# The compressed flag (bit 0). The SDK never sends compressed frames and rejects
# a compressed response frame (it negotiates identity encoding), so this is only
# used to detect and refuse an unexpected compressed frame.
_FLAG_COMPRESSED = 0b00000001

# Content types. Unary uses application/json; the streaming protocol frames JSON
# messages under application/connect+json.
_UNARY_CONTENT_TYPE = "application/json"
_STREAM_CONTENT_TYPE = "application/connect+json"

# Map the Connect error codes (the textual codes in the error envelope) to the
# HTTP status the SDK's typed-error layer keys remediation on, and to the SDK's
# own stable error codes. Connect codes are a fixed enumeration; this is the
# subset the Sandbox service returns. An unmapped code falls back to a 500-class
# internal error, which still yields a typed AgentRunError.
_CONNECT_CODE_STATUS = {
    "canceled": 499,
    "unknown": 500,
    "invalid_argument": 400,
    "deadline_exceeded": 504,
    "not_found": 404,
    "already_exists": 409,
    "permission_denied": 403,
    "resource_exhausted": 429,
    "failed_precondition": 400,
    "aborted": 409,
    "out_of_range": 400,
    "unimplemented": 501,
    "internal": 500,
    "unavailable": 503,
    "data_loss": 500,
    "unauthenticated": 401,
}


def _path(method: str) -> str:
    """The Connect RPC path for a Sandbox method name (e.g. "ReadFile")."""
    return f"/sandbox.v1.Sandbox/{method}"


def _connect_error(
    code: str, message: str, status: int, token: Optional[str]
) -> AgentRunError:
    """Build a typed AgentRunError from a Connect error envelope.

    The Connect textual code is mapped to an HTTP-ish status so the SDK's typed
    hierarchy (issue #216) can pick the right subclass and remediation; the
    message is redacted of any bearer token before it becomes the cause. A
    deadline_exceeded surfaces as the same exec_timeout type the JSON path used,
    so a caller branches on the type, not the transport.
    """
    msg = _redact(message or "", token)
    sdk_code = code or "internal_error"
    # The execution-deadline code is named exec_timeout in the SDK's typed
    # hierarchy; map the Connect deadline_exceeded onto it so the streaming and
    # the legacy paths raise the SAME type.
    if code == "deadline_exceeded":
        sdk_code = "exec_timeout"
    elif code == "resource_exhausted":
        sdk_code = "too_many_streams"
    return error_for_code(
        sdk_code,
        f"sandbox RPC failed: {code or 'internal'}",
        cause=msg or f"connect error {code}",
        remediation="Inspect the request against the sandbox.v1.Sandbox contract.",
        status=status,
        context={},
    )


def _raise_unary_error(resp: httpx.Response, token: Optional[str]) -> None:
    """Raise a typed AgentRunError from a non-2xx unary Connect response.

    Prefers the Connect error envelope ``{"code","message"}``; falls back to the
    HTTP status when the body is not the envelope (a proxy 502, a transport
    error). The token value is never echoed.
    """
    code = ""
    message = ""
    try:
        parsed = resp.json()
    except Exception:  # noqa: BLE001  not JSON; fall back to status text
        parsed = None
    if isinstance(parsed, dict):
        code = parsed.get("code", "") or ""
        message = parsed.get("message", "") or ""
    status = _CONNECT_CODE_STATUS.get(code, resp.status_code)
    raise _connect_error(code, message or _redact(resp.text, token), status, token)


def _encode_frame(payload: bytes, end_stream: bool = False) -> bytes:
    """Wrap one message payload in the Connect 5-byte envelope prefix."""
    flag = _FLAG_END_STREAM if end_stream else 0
    return bytes([flag]) + struct.pack(">I", len(payload)) + payload


def _iter_frames(resp: httpx.Response) -> Iterator[tuple[int, bytes]]:
    """Yield (flag, payload) for each Connect enveloped frame in a streamed
    response body, reassembling frames across chunk boundaries.

    A frame is a 1-byte flag, a 4-byte big-endian length, then that many payload
    bytes. The function buffers raw bytes until a full frame is available, so it
    is robust to httpx delivering arbitrary chunk sizes.
    """
    buf = bytearray()
    for chunk in resp.iter_bytes():
        if not chunk:
            continue
        buf.extend(chunk)
        while True:
            if len(buf) < 5:
                break
            length = struct.unpack(">I", bytes(buf[1:5]))[0]
            if len(buf) < 5 + length:
                break
            flag = buf[0]
            payload = bytes(buf[5 : 5 + length])
            del buf[: 5 + length]
            yield flag, payload


class ConnectClient:
    """Speaks the Connect ``sandbox.v1.Sandbox`` protocol over an httpx.Client.

    Constructed with the server base URL, the per-sandbox id, and the optional
    bearer token. Every call sets ``X-Sandbox-Id`` (the server routes on it, both
    in the tokenless standalone case and the hosted/forkd bearer case) and, when
    a key is set, ``Authorization: Bearer <key>``. The token value is never
    logged.
    """

    def __init__(
        self,
        http: httpx.Client,
        base_url: str,
        sandbox_id: str,
        token: Optional[str] = None,
    ):
        self._http = http
        self._base = base_url.rstrip("/")
        self._sandbox_id = sandbox_id
        self._token = token

    def _headers(self, content_type: str) -> dict[str, str]:
        h = {
            "Content-Type": content_type,
            "X-Sandbox-Id": self._sandbox_id,
        }
        if self._token:
            h["Authorization"] = f"Bearer {self._token}"
        return h

    def unary(self, method: str, message: dict, timeout: Optional[float] = None) -> dict:
        """Make a unary Connect call and return the proto-JSON reply as a dict.

        Raises a typed AgentRunError on a Connect error envelope or a non-2xx
        status.
        """
        url = f"{self._base}{_path(method)}"
        kwargs: dict = {
            "headers": self._headers(_UNARY_CONTENT_TYPE),
            "content": json.dumps(message).encode(),
        }
        if timeout is not None:
            kwargs["timeout"] = timeout
        resp = self._http.post(url, **kwargs)
        if not resp.is_success:
            _raise_unary_error(resp, self._token)
        if not resp.content:
            return {}
        return resp.json()

    def server_stream(
        self,
        method: str,
        message: dict,
        timeout: Optional[float] = None,
    ) -> Iterator[dict]:
        """Open a server-streaming (or half-duplex bidi) Connect call.

        Sends ``message`` as the single opening enveloped frame, then yields each
        response message as a dict the instant its frame arrives. The terminal
        end-stream frame is consumed here: a clean end ends the iterator, an
        error end raises a typed AgentRunError. Use this for ReadFile, Vitals,
        and for the direct-mode Exec/RunCode whose only client message is the
        open frame.
        """
        yield from self.bidi(method, [message], timeout=timeout)

    def bidi(
        self,
        method: str,
        messages: list[dict],
        timeout: Optional[float] = None,
    ) -> Iterator[dict]:
        """Send the given client messages as enveloped frames, then yield each
        response message dict. The request body is fully buffered (the SDK's
        direct-mode streams send only an opening message, so the call is
        half-duplex); the response is read incrementally.

        On the terminal end-stream frame: a payload with an ``error`` object
        raises a typed AgentRunError; a clean end simply stops the iterator.
        """
        url = f"{self._base}{_path(method)}"
        body = b"".join(_encode_frame(json.dumps(m).encode()) for m in messages)
        stream_kwargs: dict = {
            "headers": self._headers(_STREAM_CONTENT_TYPE),
            "content": body,
        }
        # A streaming response has no a-priori length, so a per-read timeout (not
        # a whole-call timeout) is what httpx needs; None disables it and lets the
        # caller bound the overall call.
        stream_kwargs["timeout"] = timeout
        with self._http.stream("POST", url, **stream_kwargs) as resp:
            if not resp.is_success:
                # A streaming RPC that fails before the first frame returns a
                # normal HTTP error body (the Connect error envelope), not an
                # end-stream frame. Read it and raise the typed error.
                resp.read()
                _raise_unary_error(resp, self._token)
            for flag, payload in _iter_frames(resp):
                if flag & _FLAG_COMPRESSED:
                    raise AgentRunError(
                        "sandbox RPC returned a compressed frame the SDK did not negotiate",
                        code="internal_error",
                        cause="unexpected compressed Connect frame",
                        remediation="Report this; the SDK negotiates identity encoding.",
                        status=500,
                    )
                if flag & _FLAG_END_STREAM:
                    self._handle_end_stream(payload)
                    return
                if not payload:
                    continue
                yield json.loads(payload)

    def _handle_end_stream(self, payload: bytes) -> None:
        """Process the terminal end-stream frame. A payload carrying an ``error``
        object raises a typed AgentRunError; a clean end (empty or trailers only)
        returns normally."""
        if not payload:
            return
        try:
            end = json.loads(payload)
        except Exception:  # noqa: BLE001  malformed trailer: treat as clean end
            return
        err = end.get("error") if isinstance(end, dict) else None
        if isinstance(err, dict):
            code = err.get("code", "") or ""
            message = err.get("message", "") or ""
            status = _CONNECT_CODE_STATUS.get(code, 500)
            raise _connect_error(code, message, status, self._token)
