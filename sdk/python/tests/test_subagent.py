"""Fork-native subagent hook (mitos.subagent), issue #340.

A multi-agent harness asks for a subagent. On mitos, spawning a subagent IS
forking the current warm sandbox via the in-guest self-service socket
($MITOS_SOCKET, see mitos.guest). Off mitos, there is nothing to fork, so the
call returns a graceful no-op fallback and never raises.

These tests do not boot a VM. The on-mitos detection signal is the presence of
$MITOS_SOCKET; the fork itself is routed through mitos.guest.fork, which is
replaced with a spy so we can assert it is (and is not) called.
"""

import mitos.guest as guest
import mitos.subagent as sub


# ---------------------------------------------------------------------------
# Detection: is_on_mitos reflects the $MITOS_SOCKET self-identification signal.
# ---------------------------------------------------------------------------


def test_is_on_mitos_true_when_socket_env_set(monkeypatch):
    monkeypatch.setenv("MITOS_SOCKET", "/run/mitos.sock")
    assert sub.is_on_mitos() is True


def test_is_on_mitos_false_when_socket_env_absent(monkeypatch):
    monkeypatch.delenv("MITOS_SOCKET", raising=False)
    assert sub.is_on_mitos() is False


# ---------------------------------------------------------------------------
# On mitos: spawn_subagent routes through a fork of the current sandbox.
# ---------------------------------------------------------------------------


def test_on_mitos_routes_spawn_through_fork(monkeypatch):
    """Inside a sandbox, spawn_subagent forks the current warm sandbox via the
    guest self-service socket and returns handles to the warm children."""
    monkeypatch.setenv("MITOS_SOCKET", "/run/mitos.sock")
    calls = []

    def fake_fork(n=1, label=""):
        calls.append((n, label))
        return ["sb-child-a", "sb-child-b"]

    monkeypatch.setattr(guest, "fork", fake_fork)

    result = sub.spawn_subagent(2, label="researcher")

    assert calls == [(2, "researcher")]  # fork WAS called, with our args
    assert result.on_mitos is True
    assert [c.sandbox_id for c in result.children] == ["sb-child-a", "sb-child-b"]
    assert all(c.on_mitos and c.label == "researcher" for c in result.children)
    assert result.first is not None and result.first.sandbox_id == "sb-child-a"
    assert bool(result) is True


def test_on_mitos_default_n_is_one(monkeypatch):
    monkeypatch.setenv("MITOS_SOCKET", "/run/mitos.sock")
    calls = []

    def fake_fork(n=1, label=""):
        calls.append((n, label))
        return ["sb-only"]

    monkeypatch.setattr(guest, "fork", fake_fork)

    result = sub.spawn_subagent()
    assert calls == [(1, "")]
    assert len(result.children) == 1


# ---------------------------------------------------------------------------
# Off mitos: graceful fallback. No fork, no raise, first-class result.
# ---------------------------------------------------------------------------


def test_off_mitos_returns_fallback_and_does_not_fork(monkeypatch):
    """Outside a sandbox (no $MITOS_SOCKET) spawn_subagent returns a no-op
    fallback, never calls fork, and never raises, so a harness using it does not
    break off-platform."""
    monkeypatch.delenv("MITOS_SOCKET", raising=False)
    calls = []
    monkeypatch.setattr(guest, "fork", lambda *a, **k: calls.append(True) or [])

    result = sub.spawn_subagent(3, label="x")

    assert calls == []  # fork was NOT called
    assert result.on_mitos is False
    assert result.children == ()
    assert result.first is None
    assert bool(result) is False
    assert result.reason  # carries an explanation of the off-mitos fallback


def test_off_mitos_never_raises_even_with_large_n(monkeypatch):
    monkeypatch.delenv("MITOS_SOCKET", raising=False)
    # Must not raise; the fallback path is first-class.
    result = sub.spawn_subagent(100)
    assert result.on_mitos is False


# ---------------------------------------------------------------------------
# current_sandbox(): graceful self-identification.
# ---------------------------------------------------------------------------


def test_current_sandbox_off_mitos_is_none(monkeypatch):
    monkeypatch.delenv("MITOS_SOCKET", raising=False)
    assert sub.current_sandbox() is None


def test_current_sandbox_on_mitos_returns_identity(monkeypatch):
    monkeypatch.setenv("MITOS_SOCKET", "/run/mitos.sock")
    ident = guest.Identity(sandbox_id="sb-self", pool="p1")
    monkeypatch.setattr(guest, "identity", lambda: ident)
    got = sub.current_sandbox()
    assert got is not None and got.sandbox_id == "sb-self"
