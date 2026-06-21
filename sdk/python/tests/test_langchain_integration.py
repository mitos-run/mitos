"""Tests for the LangChain sandbox backend adapter (issue #203).

Two layers:

  1. Mapping / adapter unit tests that run with NO langchain installed and NO
     server (a fake target exercises the wire-op mapping and MitosSandbox
     methods directly). This is the bulk of the conformance coverage.

  2. An integration smoke test against the mock-engine sandbox-server (no KVM),
     reusing the build-and-start fixture style from test_direct.py.

The adapter must be importable and usable without langchain; these tests prove
that by never importing langchain.
"""

import os
import subprocess
import time

import pytest

from mitos.integrations import _mapping
from mitos.integrations.langchain import MitosSandbox, as_langchain_backend
from mitos.types import Execution, ExecResult, ExecutionError, FileInfo, Result


# --------------------------------------------------------------------------
# Fakes: a target that quacks like DirectSandbox but touches no server / KVM.
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
    """Implements the OpsTarget surface: exec, run_code, files, fork, terminate."""

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
        if on_stdout:
            on_stdout("hi\n")
        result = Result(data={"text/plain": "42", "image/png": "aGVsbG8="}, is_main_result=True)
        if on_result:
            on_result(result)
        return Execution(
            text="42",
            logs={"stdout": ["hi\n"], "stderr": []},
            results=[result],
            error=None,
        )

    def fork(self, n: int = 1, id=None):
        return [_FakeSandbox(id=f"child-{i}") for i in range(n)]

    def terminate(self) -> None:
        self.terminated = True


# --------------------------------------------------------------------------
# Shared mapping helpers (the layer #204 / #206 reuse).
# --------------------------------------------------------------------------


def test_map_execute_normalizes_command_result():
    target = _FakeSandbox()
    out = _mapping.map_execute(target, "echo hi", timeout=10)
    assert out == {
        "stdout": "ran:echo hi",
        "stderr": "",
        "exit_code": 0,
        "exec_time_ms": 1.5,
    }
    assert target.exec_calls == [("echo hi", 10)]


def test_map_files_roundtrip():
    target = _FakeSandbox()
    _mapping.map_files_write(target, "/p.txt", "hello")
    assert _mapping.map_files_read(target, "/p.txt") == "hello"
    listing = _mapping.map_files_list(target, "/")
    assert listing[0].name == "a.txt"


def test_map_run_code_returns_execution_with_results():
    target = _FakeSandbox()
    seen = []
    ex = _mapping.map_run_code(target, "1+1", on_stdout=seen.append)
    assert isinstance(ex, Execution)
    assert ex.text == "42"
    assert ex.results[0].png == "aGVsbG8="
    assert seen == ["hi\n"]


def test_execution_to_dict_flattens_rich_results():
    ex = Execution(
        text="42",
        logs={"stdout": ["hi\n"], "stderr": []},
        results=[Result(data={"image/png": "aGVsbG8="}, is_main_result=True)],
        error=ExecutionError(name="ValueError", value="bad", traceback=["ValueError: bad"]),
    )
    d = _mapping.execution_to_dict(ex)
    assert d["text"] == "42"
    assert d["logs"]["stdout"] == ["hi\n"]
    assert d["results"][0]["data"]["image/png"] == "aGVsbG8="
    assert d["results"][0]["is_main_result"] is True
    assert d["error"]["name"] == "ValueError"
    assert d["error"]["traceback"] == ["ValueError: bad"]


def test_execution_to_dict_no_error():
    ex = Execution(text="1", logs={"stdout": [], "stderr": []}, results=[], error=None)
    assert _mapping.execution_to_dict(ex)["error"] is None


# --------------------------------------------------------------------------
# MitosSandbox adapter (LangChain backend surface) without langchain installed.
# --------------------------------------------------------------------------


def test_adapter_importable_without_langchain():
    """The whole point: the module imports and the class instantiates with no
    langchain present (this suite never imports langchain)."""
    import sys

    assert "langchain" not in sys.modules
    sb = MitosSandbox(_FakeSandbox())
    assert isinstance(sb, MitosSandbox)


def test_execute_maps_to_exec_and_run_alias():
    fake = _FakeSandbox()
    sb = MitosSandbox(fake)
    out = sb.execute("ls", timeout=5)
    assert out["stdout"] == "ran:ls"
    assert out["exit_code"] == 0
    # run is an alias for execute (adjustable to upstream naming).
    out2 = sb.run("pwd")
    assert out2["stdout"] == "ran:pwd"
    assert fake.exec_calls == [("ls", 5), ("pwd", 30)]


def test_exec_result_returns_typed_dataclass():
    sb = MitosSandbox(_FakeSandbox())
    res = sb.exec_result("whoami")
    assert isinstance(res, ExecResult)
    assert res.stdout == "ran:whoami"


def test_filesystem_ops_map_to_files():
    fake = _FakeSandbox()
    sb = MitosSandbox(fake)
    sb.write_file("/x.txt", "data")
    assert sb.read_file("/x.txt") == "data"
    assert fake.files.writes == [("/x.txt", "data")]
    listing = sb.list_files("/")
    assert listing[0].name == "a.txt"


def test_run_code_returns_rich_execution():
    sb = MitosSandbox(_FakeSandbox())
    ex = sb.run_code("import math; math.sqrt(4)")
    assert ex.text == "42"
    assert ex.results[0].png == "aGVsbG8="


def test_run_code_dict_serializes():
    sb = MitosSandbox(_FakeSandbox())
    d = sb.run_code_dict("1+1")
    assert d["text"] == "42"
    assert d["results"][0]["data"]["image/png"] == "aGVsbG8="


def test_close_and_stop_terminate():
    fake = _FakeSandbox()
    sb = MitosSandbox(fake)
    sb.close()
    assert fake.terminated is True

    fake2 = _FakeSandbox()
    MitosSandbox(fake2).stop()
    assert fake2.terminated is True


def test_context_manager_closes():
    fake = _FakeSandbox()
    with MitosSandbox(fake) as sb:
        assert sb.id == "fake-1"
    assert fake.terminated is True


def test_fork_returns_sibling_adapters():
    sb = MitosSandbox(_FakeSandbox())
    children = sb.fork(2)
    assert len(children) == 2
    assert all(isinstance(c, MitosSandbox) for c in children)
    assert children[0].id == "child-0"
    assert children[1].id == "child-1"


def test_as_langchain_backend_is_lazy_passthrough():
    sb = MitosSandbox(_FakeSandbox())
    assert as_langchain_backend(sb) is sb


def test_sandbox_property_exposes_native_handle():
    fake = _FakeSandbox()
    sb = MitosSandbox(fake)
    assert sb.sandbox is fake


def test_repr_contains_id():
    sb = MitosSandbox(_FakeSandbox("rep"))
    assert "rep" in repr(sb)


# --------------------------------------------------------------------------
# Integration: adapter against the mock-engine sandbox-server (no KVM).
# --------------------------------------------------------------------------


SERVER_URL = "http://localhost:18081"
server_process = None


@pytest.fixture(scope="module")
def mock_server():
    global server_process
    result = subprocess.run(
        ["go", "build", "-o", "/tmp/sandbox-server-lc-test", "./cmd/sandbox-server/"],
        cwd=os.path.join(os.path.dirname(__file__), "..", "..", ".."),
        capture_output=True,
    )
    if result.returncode != 0:
        pytest.skip(f"Could not build sandbox-server: {result.stderr.decode()}")

    server_process = subprocess.Popen(
        ["/tmp/sandbox-server-lc-test", "--mock", "--addr", ":18081"],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    time.sleep(1)
    yield SERVER_URL
    server_process.terminate()
    server_process.wait(timeout=5)


def test_create_lifecycle_against_mock_server(mock_server):
    """End-to-end lifecycle: MitosSandbox.create -> a READY backend -> close, on
    the mock server. Proves the adapter drives the standalone (no-Kubernetes)
    path.

    The mock server registers no vsock guest agent, so exec / files / run_code
    cannot run against it (the same reason test_direct.py only exercises the
    template / fork / terminate lifecycle there); those ops are covered by the
    fake-target unit tests above and run end-to-end in the KVM CI job. Here we
    assert the create / fork / close lifecycle the adapter wraps."""
    sb = MitosSandbox.create("lc-python", base_url=mock_server)
    assert isinstance(sb, MitosSandbox)
    assert sb.id
    assert sb.sandbox.fork_time_ms > 0
    sb.close()


def test_fork_against_mock_server(mock_server):
    sb = MitosSandbox.create("lc-fork", base_url=mock_server)
    try:
        children = sb.fork(2)
        assert len(children) == 2
        for c in children:
            assert isinstance(c, MitosSandbox)
            c.close()
    finally:
        sb.close()
