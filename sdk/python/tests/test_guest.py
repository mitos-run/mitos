"""In-guest self-service SDK (mitos.guest), issue #22 / API v2 section 2.2.

The in-VM workload connects to the unix socket the guest agent serves at
$MITOS_SOCKET and reads its own identity and budget. These tests stand up a fake
unix-socket server speaking the same newline-delimited JSON protocol the guest
agent serves (internal/guestsock), so the SDK shape and the connect/read-own-
identity call are exercised without a VM.
"""

import json
import os
import socket
import tempfile
import threading
import uuid

import pytest

import mitos.guest as guest


# ---------------------------------------------------------------------------
# Fake guest self-service socket server (mirrors internal/guestsock).
# ---------------------------------------------------------------------------


def _short_sock_path():
    # AF_UNIX paths are capped (~104 chars on macOS); pytest tmp_path is too
    # long, so bind under the system temp dir with a short unique name.
    return os.path.join(tempfile.gettempdir(), f"mg-{uuid.uuid4().hex[:8]}.sock")


def _fake_socket_server(tmp_path, info_payload=None, fork_ok=False):
    sock_path = _short_sock_path()
    srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    srv.bind(sock_path)
    srv.listen(8)

    info = info_payload or {
        "identity": {"sandboxId": "sb-test", "claim": "claim-1", "workspace": "proj-x"},
        "budget": {"maxForks": 5, "forksUsed": 1},
    }

    def serve():
        while True:
            try:
                conn, _ = srv.accept()
            except OSError:
                return
            with conn:
                buf = b""
                while b"\n" not in buf:
                    chunk = conn.recv(4096)
                    if not chunk:
                        break
                    buf += chunk
                if not buf:
                    continue
                req = json.loads(buf.split(b"\n", 1)[0])
                if req.get("type") == "info":
                    resp = {"ok": True, "info": info}
                elif req.get("type") == "fork":
                    if fork_ok:
                        resp = {"ok": True, "fork": {"children": ["sb-a", "sb-b"]}}
                    else:
                        resp = {"ok": False, "error": "self-fork is not enabled (issue #25)"}
                else:
                    resp = {"ok": False, "error": "unknown type"}
                conn.sendall((json.dumps(resp) + "\n").encode())

    t = threading.Thread(target=serve, daemon=True)
    t.start()
    return sock_path, srv


@pytest.fixture
def guest_socket(tmp_path, monkeypatch):
    sock_path, srv = _fake_socket_server(tmp_path)
    monkeypatch.setenv("MITOS_SOCKET", sock_path)
    yield sock_path
    srv.close()


def test_info_reads_own_identity(guest_socket):
    """mitos.guest.info() connects to $MITOS_SOCKET and returns the sandbox's
    own identity and budget."""
    info = guest.info()
    assert info.identity.sandbox_id == "sb-test"
    assert info.identity.claim == "claim-1"
    assert info.identity.workspace == "proj-x"
    assert info.budget.max_forks == 5
    assert info.budget.forks_used == 1


def test_identity_shortcut(guest_socket):
    """guest.identity() returns just the Identity (read-own-identity)."""
    ident = guest.identity()
    assert ident.sandbox_id == "sb-test"


def test_missing_socket_env_raises(monkeypatch):
    """Outside a sandbox (no MITOS_SOCKET) the call raises a clear error rather
    than connecting to a default that does not exist."""
    monkeypatch.delenv("MITOS_SOCKET", raising=False)
    with pytest.raises(guest.GuestUnavailableError):
        guest.info()


def test_fork_not_enabled_surfaces_error(guest_socket):
    """guest.fork() reaches the socket; today the budget-gated self-fork is
    continuation (issue #25), so the server's not-enabled error surfaces with
    its remediation rather than a silent failure."""
    with pytest.raises(guest.GuestError) as exc:
        guest.fork(3)
    assert "issue #25" in str(exc.value)


def test_fork_returns_children_when_enabled(tmp_path, monkeypatch):
    """When the guest agent wires the budget-gated fork (issue #25), fork()
    returns the sibling names. Verified against a fake that answers ok."""
    sock_path, srv = _fake_socket_server(tmp_path, fork_ok=True)
    monkeypatch.setenv("MITOS_SOCKET", sock_path)
    try:
        children = guest.fork(2)
        assert children == ["sb-a", "sb-b"]
    finally:
        srv.close()


def test_connect_uses_explicit_path(tmp_path):
    """A caller may pass the socket path explicitly (e.g. a test), bypassing the
    env var."""
    sock_path, srv = _fake_socket_server(tmp_path)
    try:
        client = guest.connect(sock_path)
        info = client.info()
        assert info.identity.sandbox_id == "sb-test"
    finally:
        srv.close()


def test_socket_path_helper(monkeypatch):
    """guest.socket_path() returns $MITOS_SOCKET, the advertised path."""
    monkeypatch.setenv("MITOS_SOCKET", "/run/mitos.sock")
    assert guest.socket_path() == "/run/mitos.sock"
