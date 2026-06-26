"""Tests for the E2B-compat shim ``mitos.e2b`` (issue #206).

The shim is a one-way migration bridge for self-hosted / regulated / air-gapped
users leaving E2B's cloud: "change one import" and an E2B-style script runs
against a standalone mitos sandbox-server. The vocabulary is ~70% aligned, so
this is an ADAPTER over the existing DirectSandbox / sandbox-server surface, not
engine work.

Two layers, mirroring the LangChain adapter tests (test_langchain_integration.py):

  1. An E2B-STYLE script run UNCHANGED against ``mitos.e2b`` on a fake target
     (the mock-engine sandbox-server cannot answer exec / files / run_code
     without a vsock guest agent, the same reason test_direct.py only exercises
     the lifecycle). The fake proves create / commands.run / files.* / run_code /
     kill / set_timeout map onto the native ops correctly.

  2. An integration smoke test against the mock-engine sandbox-server (no KVM)
     for the create / connect / list / kill lifecycle the shim wraps.

No hard dependency on the ``e2b`` package: this suite never imports it.
"""

import os
import subprocess
import time

import pytest

from mitos.e2b import Sandbox
from mitos.errors import AgentRunError
from mitos.types import Execution, ExecResult, FileInfo, Result


# --------------------------------------------------------------------------
# Fakes: a target that quacks like DirectSandbox but touches no server / KVM.
# --------------------------------------------------------------------------


class _FakeFiles:
    def __init__(self):
        self.store: dict[str, object] = {}
        self.writes: list[tuple[str, object]] = []
        self.removed: list[str] = []
        self.mkdirs: list[str] = []

    def read(self, path: str):
        return self.store[path]

    def write(self, path: str, content, mode: int = 0o644):
        self.store[path] = content
        self.writes.append((path, content))

    def list(self, path: str = "/"):
        return [FileInfo(name="a.txt", is_dir=False, size=3, mode=0o644)]

    def exists(self, path: str) -> bool:
        return path in self.store

    def remove(self, path: str) -> None:
        self.removed.append(path)
        self.store.pop(path, None)

    def mkdir(self, path: str) -> None:
        self.mkdirs.append(path)


class _FakeSandbox:
    """Implements the surface the shim drives: exec, run_code, files,
    set_timeout, fork, terminate."""

    def __init__(self, id: str = "fake-1"):
        self.id = id
        self.template = "python"
        self.files = _FakeFiles()
        self.exec_calls: list[tuple[str, int]] = []
        self.run_code_calls: list[tuple[str, str, int]] = []
        self.set_timeout_calls: list[int] = []
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
        if on_stdout:
            on_stdout("hi\n")
        result = Result(
            data={"text/plain": "42", "image/png": "aGVsbG8="}, is_main_result=True
        )
        if on_result:
            on_result(result)
        return Execution(
            text="42",
            logs={"stdout": ["hi\n"], "stderr": []},
            results=[result],
            error=None,
        )

    def set_timeout(self, timeout_seconds: int) -> int:
        self.set_timeout_calls.append(timeout_seconds)
        return 1_700_000_000 + timeout_seconds

    def fork(self, n: int = 1, id=None):
        return [_FakeSandbox(id=f"child-{i}") for i in range(n)]

    def get_host(self, port: int = 80) -> str:
        return f"https://{self.id}.preview.example.com/?token=tok&port={port}"

    def terminate(self) -> None:
        self.terminated = True


def _fake_sandbox(*args, **kwargs) -> _FakeSandbox:
    return _FakeSandbox()


# --------------------------------------------------------------------------
# 1. The E2B-STYLE script, run UNCHANGED against mitos.e2b on a fake target.
# --------------------------------------------------------------------------


def test_e2b_style_script_runs_unchanged(monkeypatch):
    """An E2B user's script: only the import changed. Every method name and
    signature is E2B's; the body is what an E2B tutorial would show."""
    fake = _FakeSandbox()
    # Sandbox.create() builds a DirectSandbox; patch that one seam so the script
    # never touches a server. Everything else exercises the real shim.
    monkeypatch.setattr(
        "mitos.e2b._create_direct", lambda *a, **k: fake
    )

    # --- this block is verbatim E2B surface ---
    sandbox = Sandbox.create("python", base_url="http://localhost:8080")

    # commands.run -> exec
    result = sandbox.commands.run("echo hello")
    assert result.stdout == "ran:echo hello"
    assert result.exit_code == 0

    # files.write / read / list / exists / remove / make_dir
    sandbox.files.write("/tmp/x.txt", "data")
    assert sandbox.files.read("/tmp/x.txt") == "data"
    assert sandbox.files.exists("/tmp/x.txt") is True
    listing = sandbox.files.list("/tmp")
    assert listing[0].name == "a.txt"
    sandbox.files.make_dir("/tmp/sub")
    sandbox.files.remove("/tmp/x.txt")

    # run_code -> rich Execution with MIME Result types
    execution = sandbox.run_code("1 + 1")
    assert execution.text == "42"
    assert execution.results[0].png == "aGVsbG8="

    # set_timeout -> live TTL extension
    deadline = sandbox.set_timeout(300)
    assert deadline > 0

    # get_host(port) -> a signed preview URL for a guest port (the "Done when":
    # a real E2B sample runs unchanged INCLUDING a preview URL).
    host = sandbox.get_host(3000)
    assert host.startswith("https://") and "port=3000" in host

    # kill -> terminate
    sandbox.kill()
    # --- end verbatim E2B surface ---

    assert fake.exec_calls == [("echo hello", 60)]
    assert fake.files.writes == [("/tmp/x.txt", "data")]
    assert fake.files.mkdirs == ["/tmp/sub"]
    assert fake.files.removed == ["/tmp/x.txt"]
    assert fake.run_code_calls == [("1 + 1", "python", 60)]
    assert fake.set_timeout_calls == [300]
    assert fake.terminated is True


def test_commands_run_background_maps_to_exec(monkeypatch):
    fake = _FakeSandbox()
    sb = Sandbox(fake)
    handle = sb.commands.run("sleep 1", background=True)
    # background returns a handle exposing the result; here it is the same exec.
    assert handle.stdout == "ran:sleep 1"


def test_make_dir_maps_to_mkdir():
    fake = _FakeSandbox()
    sb = Sandbox(fake)
    sb.files.make_dir("/a/b")
    assert fake.files.mkdirs == ["/a/b"]


def test_set_timeout_reuses_native_set_timeout():
    fake = _FakeSandbox()
    sb = Sandbox(fake)
    out = sb.set_timeout(120)
    assert fake.set_timeout_calls == [120]
    assert out == 1_700_000_000 + 120


def test_kill_terminates():
    fake = _FakeSandbox()
    Sandbox(fake).kill()
    assert fake.terminated is True


def test_context_manager_kills():
    fake = _FakeSandbox()
    with Sandbox(fake) as sb:
        assert sb.sandbox_id == "fake-1"
    assert fake.terminated is True


def test_sandbox_id_property():
    sb = Sandbox(_FakeSandbox("e2b-1"))
    assert sb.sandbox_id == "e2b-1"


def test_run_code_returns_rich_execution():
    sb = Sandbox(_FakeSandbox())
    ex = sb.run_code("import math; math.sqrt(4)")
    assert ex.text == "42"
    assert ex.results[0].png == "aGVsbG8="


# --------------------------------------------------------------------------
# get_host: preview URLs (#126). Delegates to the native DirectSandbox.get_host
# and returns the signed preview URL.
# --------------------------------------------------------------------------


def test_get_host_returns_preview_url():
    sb = Sandbox(_FakeSandbox(id="sb-x"))
    url = sb.get_host(3000)
    assert url == "https://sb-x.preview.example.com/?token=tok&port=3000"


# --------------------------------------------------------------------------
# connect / list against the mock-engine sandbox-server (no KVM).
# --------------------------------------------------------------------------


SERVER_URL = "http://localhost:18083"
server_process = None


@pytest.fixture(scope="module")
def mock_server():
    global server_process
    result = subprocess.run(
        ["go", "build", "-o", "/tmp/sandbox-server-e2b-test", "./cmd/sandbox-server/"],
        cwd=os.path.join(os.path.dirname(__file__), "..", "..", ".."),
        capture_output=True,
    )
    if result.returncode != 0:
        pytest.skip(f"Could not build sandbox-server: {result.stderr.decode()}")

    server_process = subprocess.Popen(
        ["/tmp/sandbox-server-e2b-test", "--mock", "--addr", ":18083"],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    time.sleep(1)
    yield SERVER_URL
    server_process.terminate()
    server_process.wait(timeout=5)


def test_create_and_kill_lifecycle(mock_server):
    """Sandbox.create -> a READY handle -> kill, on the mock server. Proves the
    shim drives the standalone (no-Kubernetes) path. exec / files / run_code
    need a vsock guest agent the mock server lacks (covered by the fake above
    and the KVM CI job)."""
    sb = Sandbox.create("e2b-python", base_url=mock_server)
    assert sb.sandbox_id
    sb.kill()


def test_connect_reattaches_to_running_sandbox(mock_server):
    """Sandbox.connect(id) reattaches to a running sandbox by id, the E2B
    reconnect verb mapped onto the standalone server's listing."""
    sb = Sandbox.create("e2b-connect", base_url=mock_server)
    try:
        again = Sandbox.connect(sb.sandbox_id, base_url=mock_server)
        assert again.sandbox_id == sb.sandbox_id
    finally:
        sb.kill()


def test_connect_unknown_id_raises_not_found(mock_server):
    with pytest.raises(AgentRunError) as ei:
        Sandbox.connect("does-not-exist", base_url=mock_server)
    assert ei.value.code == "not_found"


def test_list_returns_running_sandboxes(mock_server):
    sb = Sandbox.create("e2b-list", base_url=mock_server)
    try:
        ids = [info.sandbox_id for info in Sandbox.list(base_url=mock_server)]
        assert sb.sandbox_id in ids
    finally:
        sb.kill()
