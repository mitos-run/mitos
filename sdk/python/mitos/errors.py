from __future__ import annotations

from typing import Any, Optional


class AgentRunError(Exception):
    """An LLM-legible error from the mitos SDK.

    Mirrors the server envelope {error:{code, message, cause, remediation}} and
    the TypeScript AgentRunError. ``code`` is a stable machine identifier;
    ``cause`` is the underlying detail (with any bearer token redacted);
    ``remediation`` is a short actionable hint. ``status`` is the HTTP status
    when the error came from a response. No token or secret value appears in any
    field.

    Callers branch on the exception TYPE, never on the message text: the
    discriminable subclasses below (IdleTimeoutError, ExecutionDeadlineError,
    RequestCanceledError, RateLimitedError, NotFoundError, UnauthorizedError,
    TimeoutTooLargeError) are selected from the server ``code`` by
    :func:`error_for_code`. An unknown code falls back to this base class, so a
    forward-compatible ``except AgentRunError`` always works (issue #216).
    """

    def __init__(
        self,
        message: str,
        code: str,
        cause: str = "",
        remediation: str = "",
        status: Optional[int] = None,
        context: Optional[dict[str, Any]] = None,
    ):
        super().__init__(message)
        self.code = code
        self.cause = cause
        self.remediation = remediation
        self.status = status
        self.context = context or {}

    def __str__(self) -> str:
        parts = [f"[{self.code}] {super().__str__()}"]
        if self.cause:
            parts.append(f"cause: {self.cause}")
        if self.remediation:
            parts.append(f"remediation: {self.remediation}")
        return " | ".join(parts)


class IdleTimeoutError(AgentRunError):
    """The sandbox was reaped after exceeding its idle timeout, so the call hit a
    sandbox that is no longer running. Distinct from NotFoundError (never
    existed) and from ExecutionDeadlineError (a per-command deadline). Server
    code ``idle_timeout`` (HTTP 410)."""


class ExecutionDeadlineError(AgentRunError):
    """A command or run_code execution ran past its requested timeout (its
    execution deadline) and was terminated. Distinct from IdleTimeoutError
    (sandbox inactivity). Server code ``exec_timeout`` (HTTP 504); also raised
    when an exec returns the conventional timeout exit code 124."""


class RequestCanceledError(AgentRunError):
    """The request was canceled by the caller (the client hung up or the context
    was canceled) before it completed. Server code ``canceled`` (HTTP 499)."""


class RateLimitedError(AgentRunError):
    """The request rate limit was exceeded. Distinct from too_many_streams (a
    concurrent-stream ceiling): this is a per-window request-rate refusal. Server
    code ``rate_limited`` (HTTP 429). ``context['retry_after_ms']`` carries the
    back-off delay when the server supplies one."""


class NotFoundError(AgentRunError):
    """No such sandbox. Server code ``not_found`` (HTTP 404)."""


class UnauthorizedError(AgentRunError):
    """The per-sandbox bearer token is missing or invalid. Server code
    ``unauthorized`` (HTTP 401)."""


class TimeoutTooLargeError(AgentRunError):
    """The requested timeout exceeds the server ceiling and was REJECTED, never
    silently reduced (the determinism rule, issue #216). Server code
    ``timeout_too_large`` (HTTP 400). ``context['max_timeout_s']`` carries the
    ceiling and ``context['requested_s']`` the rejected value."""


# Maps the server error `code` (the apierr catalogue, docs/api/errors.md) to the
# typed subclass a caller branches on. A code absent here yields the base
# AgentRunError so an unknown or newly added code never breaks a client.
_CODE_TO_CLASS: dict[str, type[AgentRunError]] = {
    "idle_timeout": IdleTimeoutError,
    "exec_timeout": ExecutionDeadlineError,
    "canceled": RequestCanceledError,
    "rate_limited": RateLimitedError,
    "not_found": NotFoundError,
    "unauthorized": UnauthorizedError,
    "timeout_too_large": TimeoutTooLargeError,
}


def error_for_code(
    code: str,
    message: str,
    *,
    cause: str = "",
    remediation: str = "",
    status: Optional[int] = None,
    context: Optional[dict[str, Any]] = None,
) -> AgentRunError:
    """Construct the typed AgentRunError subclass for a server ``code``.

    An unknown code falls back to the base AgentRunError, so a caller's
    ``except AgentRunError`` keeps working as the catalogue grows. Every instance
    carries code/cause/remediation/status/context."""
    cls = _CODE_TO_CLASS.get(code, AgentRunError)
    return cls(
        message,
        code=code,
        cause=cause,
        remediation=remediation,
        status=status,
        context=context,
    )
