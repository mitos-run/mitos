"""Fork-native subagent spawning (issue #340), the differentiated hook.

A multi-agent harness asks for a subagent. On mitos, spawning a subagent IS
forking the current warm sandbox: the child boots from a copy-on-write snapshot
of the live parent, so it inherits the parent's warm state with no cold start and
no external orchestrator round-trip. Off mitos there is nothing to fork, so the
call returns a graceful no-op fallback and the harness drops back to its normal
(cold) spawn path. The fallback is first-class: ``spawn_subagent`` never raises
just because the code is not running on mitos.

    import mitos.subagent as sub

    result = sub.spawn_subagent(3, label="researcher")
    if result.on_mitos:
        for child in result.children:
            print("warm subagent:", child.sandbox_id)
    else:
        # not on mitos: spawn subagents the normal way
        ...

DETECTION SIGNAL: the in-guest self-service socket advertised at ``$MITOS_SOCKET``
(see ``mitos.guest``). The host sets it at claim time only inside a mitos
sandbox, so its presence is the honest "am I running inside mitos" signal. The
fork itself goes through the budget-gated self-fork on that socket
(``mitos.guest.fork``), which needs no network egress and no API credentials.

HONEST SCOPE: the warm fork requires a running mitos sandbox context. The
budget-gated self-fork is wired progressively by the guest agent (issue #25);
where it is not yet enabled the socket returns its remediation, which surfaces as
a ``mitos.guest.GuestError``. That is a real on-mitos error (with an actionable
message), distinct from the off-mitos fallback this module guarantees. The
returned handles carry the child sandbox names; driving a child from a different
process needs control-plane credentials and is a documented follow-up.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Optional, Tuple

from mitos import guest


@dataclass(frozen=True)
class SubagentHandle:
    """A handle to a warm child subagent forked from the current sandbox.

    ``sandbox_id`` is the child's sibling name as returned by the self-service
    fork. ``on_mitos`` is always True for a real warm child (it is only ever
    constructed on the on-mitos path); it is carried so callers can branch on the
    handle alone.
    """

    sandbox_id: str
    label: str = ""
    on_mitos: bool = True


@dataclass(frozen=True)
class SubagentResult:
    """The outcome of :func:`spawn_subagent`.

    ``on_mitos`` True means warm-fork children were spawned and ``children``
    holds their handles. ``on_mitos`` False is the graceful off-mitos fallback:
    ``children`` is empty and ``reason`` explains why (so a harness can log it and
    fall back to its normal spawn path). The result is truthy iff it spawned warm
    children, so ``if spawn_subagent(...):`` reads naturally.
    """

    on_mitos: bool
    children: Tuple[SubagentHandle, ...] = ()
    reason: str = ""

    def __bool__(self) -> bool:
        return self.on_mitos

    @property
    def first(self) -> Optional[SubagentHandle]:
        """The first warm child, or None on the off-mitos fallback."""
        return self.children[0] if self.children else None


# Message used for the off-mitos fallback. Actionable per the LLM-legible error
# rule (issue #28): it names the signal and what to do, without raising.
_OFF_MITOS_REASON = (
    "not running inside a mitos sandbox ($MITOS_SOCKET is unset), so there is no "
    "warm sandbox to fork; fall back to your normal subagent spawn path. To get "
    "warm fork-native subagents, run this harness inside a mitos sandbox."
)


def is_on_mitos() -> bool:
    """Return True when running inside a mitos sandbox.

    The signal is the in-guest self-service socket advertised at ``$MITOS_SOCKET``
    (``mitos.guest.socket_path``), which the host sets only inside a sandbox. This
    is a pure environment check: it does not open the socket.
    """
    return guest.socket_path() is not None


def current_sandbox() -> Optional[guest.Identity]:
    """Return the calling sandbox's own identity, or None when not on mitos.

    On mitos this reads the sandbox's own identity over ``$MITOS_SOCKET``
    (``mitos.guest.identity``). Off mitos it returns None rather than raising, so
    a harness can probe "what sandbox am I" without a guard.
    """
    if not is_on_mitos():
        return None
    return guest.identity()


def spawn_subagent(n: int = 1, *, label: str = "") -> SubagentResult:
    """Spawn ``n`` subagents by forking the current warm sandbox.

    On mitos (detected via ``$MITOS_SOCKET``) this routes the spawn through a
    budget-gated self-fork (``mitos.guest.fork``) and returns a
    :class:`SubagentResult` with ``on_mitos=True`` and a handle per warm child.

    Off mitos (the signal is absent) it returns a :class:`SubagentResult` with
    ``on_mitos=False`` and an explanatory ``reason``, and does NOT call fork. It
    never raises in that case: the fallback path is first-class so a harness using
    this hook does not break off-platform.

    On mitos, a genuine fork error (for example the self-fork not yet being
    enabled by the guest agent, issue #25) surfaces as a
    ``mitos.guest.GuestError`` with its remediation; that is intentionally not
    swallowed, because it is a real on-mitos failure rather than the off-mitos
    fallback.
    """
    if not is_on_mitos():
        return SubagentResult(on_mitos=False, children=(), reason=_OFF_MITOS_REASON)
    names = guest.fork(n, label=label)
    children = tuple(
        SubagentHandle(sandbox_id=name, label=label, on_mitos=True) for name in names
    )
    return SubagentResult(on_mitos=True, children=children)
