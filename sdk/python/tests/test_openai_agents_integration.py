"""Tests for the OpenAI Agents SDK adapter (issue #204).

Two layers, mirroring test_langchain_integration.py:

  1. Adapter unit tests that run with NO openai-agents installed and NO server.
     A fake target (DirectSandbox-shaped) exercises the thin tool wrappers and
     asserts they call the shared ``_mapping`` and return the documented shapes.

  2. An integration smoke test against the mock-engine sandbox-server (no KVM),
     reusing the build-and-start fixture style from test_direct.py.

The adapter must be importable and usable without openai-agents; this suite
proves that by never importing openai-agents.
"""

import os
import subprocess
import sys
import time

import pytest

from mitos.integrations import openai_agents
from mitos.integrations.openai_agents import (
    MitosSandboxTools,
    tools_for_sandbox,
)
from mitos.types import Execution, ExecResult, FileInfo, Result


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
# Importable / usable with NO openai-agents installed.
# --------------------------------------------------------------------------


def test_adapter_importable_without_openai_agents():
    assert "agents" not in sys.modules
    tools = MitosSandboxTools(_FakeSandbox())
    assert isinstance(tools, MitosSandboxTools)


# --------------------------------------------------------------------------
# The thin tool wrappers call the right mapping and return the right shapes.
# --------------------------------------------------------------------------


def test_run_command_maps_to_exec_and_returns_dict():
    fake = _FakeSandbox()
    tools = MitosSandboxTools(fake)
    out = tools.run_command("ls", timeout=5)
    assert out == {
        "stdout": "ran:ls",
        "stderr": "",
        "exit_code": 0,
        "exec_time_ms": 1.5,
    }
    assert fake.exec_calls == [("ls", 5)]


def test_read_and_write_file_map_to_files():
    fake = _FakeSandbox()
    tools = MitosSandboxTools(fake)
    tools.write_file("/x.txt", "data")
    assert fake.files.writes == [("/x.txt", "data")]
    assert tools.read_file("/x.txt") == "data"


def test_list_files_returns_json_friendly_dicts():
    fake = _FakeSandbox()
    tools = MitosSandboxTools(fake)
    listing = tools.list_files("/")
    assert listing == [
        {"name": "a.txt", "is_dir": False, "size": 3, "mode": 0o644}
    ]


def test_run_code_returns_flattened_dict():
    fake = _FakeSandbox()
    tools = MitosSandboxTools(fake)
    out = tools.run_code("1+1")
    assert out["text"] == "42"
    assert out["results"][0]["data"]["image/png"] == "aGVsbG8="
    assert out["error"] is None
    assert fake.run_code_calls == [("1+1", "python", 60)]


def test_sandbox_property_exposes_native_handle():
    fake = _FakeSandbox()
    tools = MitosSandboxTools(fake)
    assert tools.sandbox is fake


# --------------------------------------------------------------------------
# tools_for_sandbox factory: builds the function-tool definitions.
# --------------------------------------------------------------------------


def test_tools_for_sandbox_returns_named_function_tools():
    fake = _FakeSandbox()
    defs = tools_for_sandbox(fake)
    names = {d.name for d in defs}
    assert names == {"run_command", "read_file", "write_file", "run_code"}
    for d in defs:
        assert callable(d.func)
        assert d.description
        # params_json_schema is a JSON-schema object the SDK feeds the model.
        assert d.params_json_schema["type"] == "object"


def test_tools_for_sandbox_run_command_invokes_target():
    fake = _FakeSandbox()
    defs = {d.name: d for d in tools_for_sandbox(fake)}
    out = defs["run_command"].func("echo hi")
    assert out["stdout"] == "ran:echo hi"
    assert fake.exec_calls == [("echo hi", 30)]


def test_tools_for_sandbox_file_roundtrip():
    fake = _FakeSandbox()
    defs = {d.name: d for d in tools_for_sandbox(fake)}
    defs["write_file"].func("/p.txt", "hello")
    assert defs["read_file"].func("/p.txt") == "hello"


def test_tools_for_sandbox_run_code_returns_dict():
    fake = _FakeSandbox()
    defs = {d.name: d for d in tools_for_sandbox(fake)}
    out = defs["run_code"].func("2+2")
    assert out["text"] == "42"


def test_as_function_tools_is_lazy_without_sdk():
    """Converting to real openai-agents FunctionTool objects must be lazy: with
    the SDK absent it raises a clear ImportError naming the extra, NOT at import
    time of this module."""
    tools = MitosSandboxTools(_FakeSandbox())
    with pytest.raises(ImportError) as exc:
        tools.as_function_tools()
    assert "openai-agents" in str(exc.value)
    # module imported fine even though the conversion seam is unavailable
    assert openai_agents is not None


# --------------------------------------------------------------------------
# Integration: adapter against the mock-engine sandbox-server (no KVM).
# --------------------------------------------------------------------------


SERVER_URL = "http://localhost:18083"
server_process = None


@pytest.fixture(scope="module")
def mock_server():
    global server_process
    result = subprocess.run(
        ["go", "build", "-o", "/tmp/sandbox-server-oai-test", "./cmd/sandbox-server/"],
        cwd=os.path.join(os.path.dirname(__file__), "..", "..", ".."),
        capture_output=True,
    )
    if result.returncode != 0:
        pytest.skip(f"Could not build sandbox-server: {result.stderr.decode()}")

    server_process = subprocess.Popen(
        ["/tmp/sandbox-server-oai-test", "--mock", "--addr", ":18083"],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    time.sleep(1)
    yield SERVER_URL
    server_process.terminate()
    server_process.wait(timeout=5)


def test_create_lifecycle_against_mock_server(mock_server):
    """Lifecycle: build tools over a READY sandbox on the standalone (no-k8s)
    mock server, then close. exec / files / run_code need a vsock guest agent
    the mock lacks (same as test_direct.py), so they are covered by the fake
    target above and run end-to-end in the KVM CI job."""
    tools = MitosSandboxTools.create("oai-python", base_url=mock_server)
    assert isinstance(tools, MitosSandboxTools)
    assert tools.sandbox.id
    assert tools.sandbox.fork_time_ms > 0
    defs = tools_for_sandbox(tools.sandbox)
    assert {d.name for d in defs} == {
        "run_command",
        "read_file",
        "write_file",
        "run_code",
    }
    tools.close()
