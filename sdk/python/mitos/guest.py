"""In-guest self-service SDK (issue #22, API v2 section 2.2).

From inside a sandbox, the in-VM workload connects to the unix socket the guest
agent serves at ``$MITOS_SOCKET`` to read its own identity and budget, and (once
issue #25 wires it) to request a budget-gated fork: no network egress, no
external orchestrator round-trip.

    import mitos.guest as me

    info = me.info()                 # who am I, what is my budget
    print(info.identity.sandbox_id)
    ckpt = me.fork(3)                # budget-gated; not enabled until #25

The wire protocol mirrors the guest agent's vsock convention: newline-delimited
JSON, one request per line, one response per line (see internal/guestsock). The
socket carries the sandbox's OWN identity and budget, names and numbers only,
never a secret value.
"""

from __future__ import annotations

import json
import os
import socket
from dataclasses import dataclass, field
from typing import Optional

# SOCKET_ENV is the env var the host sets at claim time to advertise the socket
# path. It matches guestsock.SocketEnvVar on the Go side.
SOCKET_ENV = "MITOS_SOCKET"


class GuestError(Exception):
    """A self-service call reached the guest agent but it returned an error.

    The message is the agent's one-line cause; for a not-enabled call it names
    the escalation path (LLM-legible, issue #28).
    """


class GuestUnavailableError(GuestError):
    """The self-service socket is not reachable: $MITOS_SOCKET is unset (the
    code is not running inside a mitos sandbox) or the socket does not exist.
    """


@dataclass
class Identity:
    """The sandbox's own identity: the names it was created under. Names only,
    never a secret. Empty fields mean the host did not deliver them."""

    sandbox_id: str = ""
    claim: str = ""
    pool: str = ""
    workspace: str = ""

    @classmethod
    def _from_dict(cls, d: dict) -> "Identity":
        d = d or {}
        return cls(
            sandbox_id=d.get("sandboxId", ""),
            claim=d.get("claim", ""),
            pool=d.get("pool", ""),
            workspace=d.get("workspace", ""),
        )


@dataclass
class Budget:
    """The capability budget the sandbox carries (issue #25). Numbers only."""

    max_forks: int = 0
    max_checkpoints: int = 0
    forks_used: int = 0

    @classmethod
    def _from_dict(cls, d: dict) -> "Budget":
        d = d or {}
        return cls(
            max_forks=d.get("maxForks", 0),
            max_checkpoints=d.get("maxCheckpoints", 0),
            forks_used=d.get("forksUsed", 0),
        )


@dataclass
class Info:
    """The read-own-identity payload: who am I, and what is my budget."""

    identity: Identity = field(default_factory=Identity)
    budget: Budget = field(default_factory=Budget)


class GuestClient:
    """A connection to the in-guest self-service socket.

    Each call opens a short-lived connection to the unix socket and exchanges one
    newline-delimited JSON request/response, mirroring the guest agent's
    per-call vsock pattern.
    """

    def __init__(self, socket_path: str):
        self._socket_path = socket_path

    def _call(self, request: dict) -> dict:
        try:
            sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            sock.connect(self._socket_path)
        except OSError as exc:
            raise GuestUnavailableError(
                f"could not connect to the in-guest self-service socket at "
                f"{self._socket_path!r}: {exc}. This call only works from inside "
                f"a mitos sandbox."
            ) from exc
        try:
            sock.sendall((json.dumps(request) + "\n").encode())
            buf = b""
            while b"\n" not in buf:
                chunk = sock.recv(4096)
                if not chunk:
                    break
                buf += chunk
        finally:
            sock.close()
        if not buf:
            raise GuestError("the guest self-service socket closed without a response")
        resp = json.loads(buf.split(b"\n", 1)[0])
        if not resp.get("ok"):
            raise GuestError(resp.get("error") or "self-service call failed")
        return resp

    def info(self) -> Info:
        """Read the sandbox's own identity and budget (read-only)."""
        resp = self._call({"type": "info"})
        data = resp.get("info") or {}
        return Info(
            identity=Identity._from_dict(data.get("identity")),
            budget=Budget._from_dict(data.get("budget")),
        )

    def identity(self) -> Identity:
        """Read just the sandbox's own identity."""
        return self.info().identity

    def fork(self, n: int = 1, label: str = "") -> list[str]:
        """Request n self-initiated sibling forks within budget (issue #25).

        Returns the sibling sandbox names. Budget-gated self-fork is continuation
        work; until the guest agent wires it, this raises GuestError carrying the
        orchestrator escalation path.
        """
        resp = self._call({"type": "fork", "fork": {"n": n, "label": label}})
        return (resp.get("fork") or {}).get("children", [])


def socket_path() -> Optional[str]:
    """Return the advertised self-service socket path ($MITOS_SOCKET), or None
    when it is unset (the code is not running inside a mitos sandbox)."""
    return os.environ.get(SOCKET_ENV) or None


def connect(path: Optional[str] = None) -> GuestClient:
    """Connect to the in-guest self-service socket.

    path defaults to $MITOS_SOCKET. Raises GuestUnavailableError when neither is
    available (the code is not running inside a mitos sandbox).
    """
    resolved = path or socket_path()
    if not resolved:
        raise GuestUnavailableError(
            f"{SOCKET_ENV} is not set: the in-guest self-service socket is only "
            f"available from inside a mitos sandbox."
        )
    return GuestClient(resolved)


def info() -> Info:
    """Read the calling sandbox's own identity and budget via $MITOS_SOCKET."""
    return connect().info()


def identity() -> Identity:
    """Read the calling sandbox's own identity via $MITOS_SOCKET."""
    return connect().identity()


def fork(n: int = 1, label: str = "") -> list[str]:
    """Request n self-initiated forks via $MITOS_SOCKET (budget-gated, #25)."""
    return connect().fork(n, label=label)
