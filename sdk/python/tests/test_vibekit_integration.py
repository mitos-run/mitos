"""Tests for the VibeKit sandbox-provider adapter ``mitos.integrations.vibekit``
(issue #205).

VibeKit is a provider aggregator over sandbox backends ("E2B today; Daytona,
Modal, Fly.io coming soon"). Listing mitos there is near-free distribution. The
external LISTING (a PR to VibeKit's repo) is a human step; what is implemented
here is the mitos PROVIDER each aggregator expects, backed by the native
``DirectSandbox`` surface through the shared ``mitos.integrations._mapping`` op
layer.

This suite mirrors test_e2b_compat.py / test_langchain_integration.py:

  1. A VibeKit-style script driven against ``MitosVibeKitProvider`` on a fake
     target (no server / KVM): create -> command -> filesystem -> run_code ->
     kill, asserting every verb maps onto the native op.
  2. The provider never imports the ``vibekit`` package: this suite proves the
     adapter is fully usable with the framework absent.
"""

import pytest

from mitos.integrations.vibekit import MitosVibeKitProvider, MitosVibeKitSandbox
from mitos.types import Execution, ExecResult, FileInfo, Result


# --------------------------------------------------------------------------
# Fake target: quacks like DirectSandbox, touches no server / KVM.
# --------------------------------------------------------------------------


class _FakeFiles:
    def __init__(self):
        self.store: dict[str, object] = {}
        self.writes: list[tuple[str, object]] = []

    def read(self, path: str):
        return self.store[path]

    def write(self, path: str, content, mode: int = 0o644):
        self.store[path] = content
        self.writes.append((path, content))

    def list(self, path: str = "/"):
        return [FileInfo(name="a.txt", is_dir=False, size=3, mode=0o644)]


class _FakeSandbox:
    def __init__(self, id: str = "fake-1"):
        self.id = id
        self.template = "python"
        self.files = _FakeFiles()
        self.exec_calls: list[tuple[str, int]] = []
        self.run_code_calls: list[tuple[str, str, int]] = []
        self.terminated = False

    def exec(self, command: str, timeout: int = 30) -> ExecResult:
        self.exec_calls.append((command, timeout))
        return ExecResult(
            exit_code=0, stdout=f"ran:{command}", stderr="", exec_time_ms=1.5
        )

    def run_code(
        self,
        code: str,
        language: str = "python",
        timeout: int = 60,
        on_stdout=None,
        on_stderr=None,
        on_result=None,
    ) -> Execution:
        self.run_code_calls.append((code, language, timeout))
        result = Result(
            data={"text/plain": "42", "image/png": "aGVsbG8="}, is_main_result=True
        )
        if on_result:
            on_result(result)
        return Execution(
            text="42",
            logs={"stdout": [], "stderr": []},
            results=[result],
            error=None,
        )

    def fork(self, n: int = 1, id=None):
        return [_FakeSandbox(id=f"child-{i}") for i in range(n)]

    def terminate(self) -> None:
        self.terminated = True


# --------------------------------------------------------------------------
# 1. VibeKit-style provider script on a fake target.
# --------------------------------------------------------------------------


def test_provider_create_returns_sandbox(monkeypatch):
    fake = _FakeSandbox()
    monkeypatch.setattr(
        "mitos.integrations.vibekit._create_direct", lambda *a, **k: fake
    )

    provider = MitosVibeKitProvider(base_url="http://localhost:8080")
    sandbox = provider.create("python")
    assert isinstance(sandbox, MitosVibeKitSandbox)
    assert sandbox.id == "fake-1"


def test_vibekit_style_script_runs(monkeypatch):
    """The VibeKit provider contract: create a sandbox, run a command, touch the
    filesystem, run code, kill. Every verb maps onto the native op."""
    fake = _FakeSandbox()
    monkeypatch.setattr(
        "mitos.integrations.vibekit._create_direct", lambda *a, **k: fake
    )

    provider = MitosVibeKitProvider(base_url="http://localhost:8080")
    sandbox = provider.create("python")

    # command execution -> normalized dict (VibeKit provider command result shape)
    out = sandbox.run_command("echo hello")
    assert out["stdout"] == "ran:echo hello"
    assert out["exit_code"] == 0
    assert "exec_time_ms" in out

    # filesystem
    sandbox.write_file("/tmp/x.txt", "data")
    assert sandbox.read_file("/tmp/x.txt") == "data"
    listing = sandbox.list_files("/tmp")
    assert listing[0].name == "a.txt"

    # rich code execution
    ex = sandbox.run_code("1 + 1")
    assert ex.text == "42"
    assert ex.results[0].png == "aGVsbG8="

    # lifecycle
    sandbox.kill()

    assert fake.exec_calls == [("echo hello", 60)]
    assert fake.files.writes == [("/tmp/x.txt", "data")]
    assert fake.run_code_calls == [("1 + 1", "python", 60)]
    assert fake.terminated is True


def test_run_command_alias_execute(monkeypatch):
    fake = _FakeSandbox()
    sb = MitosVibeKitSandbox(fake)
    # ``execute`` is an alias for ``run_command`` for VibeKit name parity.
    out = sb.execute("ls")
    assert out["stdout"] == "ran:ls"
    assert fake.exec_calls == [("ls", 60)]


def test_kill_alias_close(monkeypatch):
    fake = _FakeSandbox()
    sb = MitosVibeKitSandbox(fake)
    sb.close()
    assert fake.terminated is True


def test_fork_returns_sibling_providers():
    fake = _FakeSandbox()
    sb = MitosVibeKitSandbox(fake)
    children = sb.fork(2)
    assert len(children) == 2
    assert all(isinstance(c, MitosVibeKitSandbox) for c in children)


def test_provider_metadata():
    provider = MitosVibeKitProvider(base_url="http://localhost:8080")
    # VibeKit identifies providers by a stable name.
    assert provider.name == "mitos"


def test_context_manager_kills(monkeypatch):
    fake = _FakeSandbox()
    with MitosVibeKitSandbox(fake) as sb:
        assert sb.id == "fake-1"
    assert fake.terminated is True


def test_no_vibekit_import():
    """The adapter module must never import the vibekit package at runtime."""
    import sys

    import mitos.integrations.vibekit as mod  # noqa: F401

    assert "vibekit" not in sys.modules
