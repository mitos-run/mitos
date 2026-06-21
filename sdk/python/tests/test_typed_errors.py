"""Typed, discriminable error hierarchy (issue #216).

A caller must be able to branch on the exception TYPE, never on the message
text: idle-timeout vs execution-deadline vs request-canceled vs the rest are
distinct subclasses parsed from the server `code`.
"""

import httpx
import pytest

from mitos.errors import (
    AgentRunError,
    ExecutionDeadlineError,
    IdleTimeoutError,
    NotFoundError,
    RateLimitedError,
    RequestCanceledError,
    TimeoutTooLargeError,
    UnauthorizedError,
    error_for_code,
)
from mitos._envelope import error_from_response


def _envelope(status, code, **fields):
    request = httpx.Request("POST", "http://sb/v1/exec")
    err = {"code": code, "message": f"{code} happened", "remediation": "do x"}
    err.update(fields)
    return httpx.Response(status, json={"error": err}, request=request)


CASES = [
    ("idle_timeout", IdleTimeoutError),
    ("exec_timeout", ExecutionDeadlineError),
    ("canceled", RequestCanceledError),
    ("rate_limited", RateLimitedError),
    ("not_found", NotFoundError),
    ("unauthorized", UnauthorizedError),
    ("timeout_too_large", TimeoutTooLargeError),
]


@pytest.mark.parametrize("code,cls", CASES)
def test_code_maps_to_typed_subclass(code, cls):
    resp = _envelope(400, code)
    err = error_from_response(resp)
    assert isinstance(err, cls)
    # Every typed subclass is still an AgentRunError so a broad except works.
    assert isinstance(err, AgentRunError)
    # The code/remediation survive onto the typed instance.
    assert err.code == code
    assert err.remediation == "do x"


def test_unknown_code_falls_back_to_base():
    resp = _envelope(500, "some_new_code")
    err = error_from_response(resp)
    assert type(err) is AgentRunError
    assert err.code == "some_new_code"


def test_timeout_family_is_discriminable_without_message():
    """The whole point of #216: tell idle vs deadline vs canceled apart by TYPE,
    never by reading .message / str(e)."""
    idle = error_from_response(_envelope(410, "idle_timeout"))
    deadline = error_from_response(_envelope(504, "exec_timeout"))
    canceled = error_from_response(_envelope(499, "canceled"))

    assert isinstance(idle, IdleTimeoutError)
    assert not isinstance(idle, ExecutionDeadlineError)
    assert not isinstance(idle, RequestCanceledError)

    assert isinstance(deadline, ExecutionDeadlineError)
    assert not isinstance(deadline, IdleTimeoutError)

    assert isinstance(canceled, RequestCanceledError)
    assert not isinstance(canceled, IdleTimeoutError)


def test_factory_preserves_context():
    resp = _envelope(
        400, "timeout_too_large",
        context={"requested_s": 1000, "max_timeout_s": 100},
    )
    err = error_from_response(resp)
    assert isinstance(err, TimeoutTooLargeError)
    assert err.context["max_timeout_s"] == 100


def test_error_for_code_factory_direct():
    err = error_for_code("exec_timeout", "msg")
    assert isinstance(err, ExecutionDeadlineError)
    assert err.code == "exec_timeout"


def test_client_rejects_over_ceiling_timeout_without_clamping():
    """Determinism (issue #216): the SDK rejects an over-ceiling timeout with a
    typed TimeoutTooLargeError BEFORE any request, never silently reducing it."""
    from mitos.sandbox import MAX_EXEC_TIMEOUT_SECONDS, _validate_timeout

    _validate_timeout(MAX_EXEC_TIMEOUT_SECONDS)  # at the ceiling: no raise
    with pytest.raises(TimeoutTooLargeError) as ei:
        _validate_timeout(MAX_EXEC_TIMEOUT_SECONDS + 1)
    assert ei.value.code == "timeout_too_large"
    assert ei.value.context["max_timeout_s"] == MAX_EXEC_TIMEOUT_SECONDS
