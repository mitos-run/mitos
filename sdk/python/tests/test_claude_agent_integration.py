"""Tests for the Claude Agent SDK adapter (issue #204).

Two layers, mirroring test_langchain_integration.py:

  1. Adapter unit tests that run with NO claude-agent-sdk installed and NO
     server. A fake target (DirectSandbox-shaped) exercises the thin MCP-style
     tool wrappers and asserts they call the shared ``_mapping`` and return the
     documented MCP content shapes.

  2. An integration smoke test against the mock-engine sandbox-server (no KVM).

The adapter must be importable and usable without claude-agent-sdk; this suite
proves that by never importing it.
"""

import os
import subprocess
import sys
import time

import pytest

from mitos.integrations import claude_agent
from mitos.integrations.claude_agent import (
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


def _text(mcp_result: dict) -> str:
    """Extract the text block from an MCP tool result."""
    return mcp_result["content"][0]["text"]


# --------------------------------------------------------------------------
# Importable / usable with NO claude-agent-sdk installed.
# --------------------------------------------------------------------------


def test_adapter_importable_without_claude_agent_sdk():
    assert "claude_agent_sdk" not in sys.modules
    tools = MitosSandboxTools(_FakeSandbox())
    assert isinstance(tools, MitosSandboxTools)


# --------------------------------------------------------------------------
# The thin tool wrappers call the right mapping and return MCP content shapes.
# --------------------------------------------------------------------------


def test_run_command_maps_to_exec_and_returns_mcp_content():
    fake = _FakeSandbox()
    tools = MitosSandboxTools(fake)
    out = tools.run_command({"command": "ls", "timeout": 5})
    # MCP tool result: a content list of typed blocks.
    assert out["content"][0]["type"] == "text"
    assert "ran:ls" in _text(out)
    assert fake.exec_calls == [("ls", 5)]


def test_read_and_write_file_map_to_files():
    fake = _FakeSandbox()
    tools = MitosSandboxTools(fake)
    tools.write_file({"path": "/x.txt", "content": "data"})
    assert fake.files.writes == [("/x.txt", "data")]
    out = tools.read_file({"path": "/x.txt"})
    assert _text(out) == "data"


def test_list_files_returns_mcp_text_block():
    fake = _FakeSandbox()
    tools = MitosSandboxTools(fake)
    out = tools.list_files({"path": "/"})
    assert "a.txt" in _text(out)


def test_run_code_returns_mcp_text_with_flattened_execution():
    fake = _FakeSandbox()
    tools = MitosSandboxTools(fake)
    out = tools.run_code({"code": "1+1"})
    assert "42" in _text(out)
    assert fake.run_code_calls == [("1+1", "python", 60)]


def test_sandbox_property_exposes_native_handle():
    fake = _FakeSandbox()
    tools = MitosSandboxTools(fake)
    assert tools.sandbox is fake


# --------------------------------------------------------------------------
# tools_for_sandbox factory: MCP-style tool definitions bound to the sandbox.
# --------------------------------------------------------------------------


def test_tools_for_sandbox_returns_named_tool_defs():
    fake = _FakeSandbox()
    defs = tools_for_sandbox(fake)
    names = {d.name for d in defs}
    assert names == {"run_command", "read_file", "write_file", "run_code"}
    for d in defs:
        assert callable(d.handler)
        assert d.description
        assert d.input_schema["type"] == "object"


def test_tools_for_sandbox_run_command_invokes_target():
    fake = _FakeSandbox()
    defs = {d.name: d for d in tools_for_sandbox(fake)}
    out = defs["run_command"].handler({"command": "echo hi"})
    assert "ran:echo hi" in _text(out)
    assert fake.exec_calls == [("echo hi", 30)]


def test_tools_for_sandbox_file_roundtrip():
    fake = _FakeSandbox()
    defs = {d.name: d for d in tools_for_sandbox(fake)}
    defs["write_file"].handler({"path": "/p.txt", "content": "hello"})
    assert _text(defs["read_file"].handler({"path": "/p.txt"})) == "hello"


def test_as_mcp_server_is_lazy_without_sdk():
    """Building the real claude-agent-sdk MCP server must be lazy: with the SDK
    absent it raises a clear ImportError naming the extra, NOT at import time."""
    tools = MitosSandboxTools(_FakeSandbox())
    with pytest.raises(ImportError) as exc:
        tools.as_mcp_server()
    assert "claude-agent" in str(exc.value)
    assert claude_agent is not None


# --------------------------------------------------------------------------
# Integration: adapter against the mock-engine sandbox-server (no KVM).
# --------------------------------------------------------------------------


SERVER_URL = "http://localhost:18084"
server_process = None


@pytest.fixture(scope="module")
def mock_server():
    global server_process
    result = subprocess.run(
        ["go", "build", "-o", "/tmp/sandbox-server-claude-test", "./cmd/sandbox-server/"],
        cwd=os.path.join(os.path.dirname(__file__), "..", "..", ".."),
        capture_output=True,
    )
    if result.returncode != 0:
        pytest.skip(f"Could not build sandbox-server: {result.stderr.decode()}")

    server_process = subprocess.Popen(
        ["/tmp/sandbox-server-claude-test", "--mock", "--addr", ":18084"],
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
    tools = MitosSandboxTools.create("claude-python", base_url=mock_server)
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
