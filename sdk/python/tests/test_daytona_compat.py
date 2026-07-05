"""Tests for the Daytona-compat shim ``mitos.daytona``.

The shim is a one-way migration bridge for teams leaving Daytona's cloud for a
SELF-HOSTED sandbox runtime: "change one import" and a Daytona-style script runs
against a standalone Mitos sandbox-server. Like ``mitos.e2b`` this is an ADAPTER
over the existing ``DirectSandbox`` / ``SandboxServer`` surface, not engine work.

Two layers, mirroring the E2B shim tests (test_e2b_compat.py):

  1. A DAYTONA-STYLE script run UNCHANGED against ``mitos.daytona`` on a fake
     target (the mock-engine sandbox-server cannot answer process / fs without a
     vsock guest agent). The fake proves create / process.* / fs.* /
     get_preview_link / delete map onto the native ops correctly.

  2. An integration smoke test against the mock-engine sandbox-server (no KVM)
     for the create / get / list / delete lifecycle the shim wraps.

No hard dependency on the ``daytona`` package: this suite never imports it.
"""

import os
import subprocess
import time

import pytest

from mitos.daytona import (
    CreateSandboxFromSnapshotParams,
    Daytona,
    DaytonaConfig,
    Sandbox,
)
from mitos.errors import AgentRunError, NotFoundError
from mitos.types import Execution, ExecResult, FileInfo, Result


# --------------------------------------------------------------------------
# Fakes: a target that quacks like DirectSandbox but touches no server / KVM.
# --------------------------------------------------------------------------


class _FakeFiles:
    def __init__(self):
        self.store: dict[str, bytes] = {}
        self.writes: list[tuple[str, object]] = []
        self.removed: list[str] = []
        self.mkdirs: list[str] = []

    def read(self, path: str) -> str:
        return self.read_bytes(path).decode("utf-8", "replace")

    def read_bytes(self, path: str) -> bytes:
        return self.store[path]

    def write(self, path: str, content, mode: int = 0o644):
        raw = content.encode("utf-8") if isinstance(content, str) else bytes(content)
        self.store[path] = raw
        self.writes.append((path, raw))

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
    get_host, fork, pause, resume, terminate."""

    def __init__(self, id: str = "fake-1", run_code_error=None):
        self.id = id
        self.template = "python"
        self.files = _FakeFiles()
        self.exec_calls: list[tuple[str, int]] = []
        self.run_code_calls: list[tuple[str, str, int]] = []
        self._run_code_error = run_code_error
        self.paused = False
        self.resumed = False
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
        result = Result(data={"text/plain": "42"}, is_main_result=True)
        return Execution(
            text="42",
            logs={"stdout": ["hi\n"], "stderr": []},
            results=[result],
            error=self._run_code_error,
        )

    def get_host(self, port: int = 80) -> str:
        return f"https://{self.id}.preview.example.com/?token=tok&port={port}"

    def fork(self, n: int = 1, id=None):
        return [_FakeSandbox(id=f"child-{i}") for i in range(n)]

    def pause(self) -> None:
        self.paused = True

    def resume(self) -> None:
        self.resumed = True

    def terminate(self) -> None:
        self.terminated = True


# --------------------------------------------------------------------------
# 1. The DAYTONA-STYLE script, run UNCHANGED against mitos.daytona on a fake.
# --------------------------------------------------------------------------


def test_daytona_style_script_runs_unchanged(monkeypatch, tmp_path):
    """A Daytona user's script: only the import changed. Every method name and
    signature is Daytona's; the body is what a Daytona tutorial would show."""
    fake = _FakeSandbox()
    # daytona.create() builds a DirectSandbox; patch that one seam so the script
    # never touches a server. Everything else exercises the real shim.
    monkeypatch.setattr("mitos.daytona._create_direct", lambda *a, **k: fake)

    # --- this block is verbatim Daytona surface ---
    daytona = Daytona(DaytonaConfig(api_key="dtn_x", api_url="http://localhost:8080"))
    sandbox = daytona.create(CreateSandboxFromSnapshotParams(language="python"))

    # process.exec -> exec
    response = sandbox.process.exec("echo hello")
    assert response.exit_code == 0
    assert response.result == "ran:echo hello"
    assert response.artifacts.stdout == "ran:echo hello"

    # process.code_run -> rich run mapped to ExecuteResponse
    run = sandbox.process.code_run("1 + 1")
    assert run.exit_code == 0
    assert run.result == "42"

    # fs.upload_file / download_file / list_files / create_folder / delete_file
    sandbox.fs.upload_file(b"data", "/tmp/x.txt")
    assert sandbox.fs.download_file("/tmp/x.txt") == b"data"
    listing = sandbox.fs.list_files("/tmp")
    assert listing[0].name == "a.txt"
    info = sandbox.fs.get_file_info("/tmp/a.txt")
    assert info.name == "a.txt"
    sandbox.fs.create_folder("/tmp/sub", "0755")
    sandbox.fs.delete_file("/tmp/x.txt")

    # get_preview_link(port) -> a signed preview URL plus token
    link = sandbox.get_preview_link(3000)
    assert link.url.startswith("https://") and "port=3000" in link.url
    assert link.token == "tok"

    # delete -> terminate
    daytona.delete(sandbox)
    # --- end verbatim Daytona surface ---

    assert fake.exec_calls == [("echo hello", 30)]
    assert fake.run_code_calls == [("1 + 1", "python", 60)]
    assert fake.files.writes == [("/tmp/x.txt", b"data")]
    assert fake.files.mkdirs == ["/tmp/sub"]
    assert fake.files.removed == ["/tmp/x.txt"]
    assert fake.terminated is True


def test_create_maps_config_target_onto_region(monkeypatch):
    """DaytonaConfig(target=...) (issue #712 phase 0) is no longer a silent
    no-op: Daytona.create forwards it as mitos.create's region= kwarg, so a
    Daytona-style script gets placement selection for free by supplying the
    field it already had reason to set."""
    fake = _FakeSandbox()
    captured = {}

    def fake_create_direct(*args, **kwargs):
        captured.update(kwargs)
        return fake

    monkeypatch.setattr("mitos.daytona._create_direct", fake_create_direct)

    daytona = Daytona(DaytonaConfig(api_key="dtn_x", api_url="http://localhost:8080", target="fra"))
    daytona.create(CreateSandboxFromSnapshotParams(language="python"))

    assert captured.get("region") == "fra"


def test_create_with_no_target_leaves_region_none(monkeypatch):
    """A DaytonaConfig with no target requests no region (the org's home
    region), matching mitos.create's own default."""
    fake = _FakeSandbox()
    captured = {}

    def fake_create_direct(*args, **kwargs):
        captured.update(kwargs)
        return fake

    monkeypatch.setattr("mitos.daytona._create_direct", fake_create_direct)

    daytona = Daytona(DaytonaConfig(api_key="dtn_x", api_url="http://localhost:8080"))
    daytona.create(CreateSandboxFromSnapshotParams(language="python"))

    assert captured.get("region") is None


def test_exec_folds_cwd_and_env_into_command():
    """DirectSandbox.exec has no cwd / env slot, so the shim folds them into the
    shell command rather than dropping them."""
    fake = _FakeSandbox()
    sb = Sandbox(fake)
    sb.process.exec("ls", cwd="/workspace/app", env={"TOKEN": "s3cret"})
    (cmd, _timeout) = fake.exec_calls[0]
    assert "export TOKEN=s3cret;" in cmd
    assert "cd /workspace/app;" in cmd
    assert cmd.endswith("ls")


def test_code_run_error_maps_to_nonzero_exit():
    """A structured kernel error yields a non-zero ExecuteResponse.exit_code."""
    from mitos.types import ExecutionError

    fake = _FakeSandbox(
        run_code_error=ExecutionError(name="ValueError", value="boom", traceback=[])
    )
    sb = Sandbox(fake)
    run = sb.process.code_run("raise ValueError('boom')")
    assert run.exit_code == 1


def test_upload_file_bytes_and_local_path(tmp_path):
    """upload_file accepts raw bytes (content) or a str (a local path to read)."""
    fake = _FakeSandbox()
    sb = Sandbox(fake)

    sb.fs.upload_file(b"raw-bytes", "/remote/a.bin")
    assert fake.files.store["/remote/a.bin"] == b"raw-bytes"

    local = tmp_path / "local.txt"
    local.write_bytes(b"from-disk")
    sb.fs.upload_file(str(local), "/remote/b.txt")
    assert fake.files.store["/remote/b.txt"] == b"from-disk"


def test_download_file_returns_bytes_or_writes_local(tmp_path):
    fake = _FakeSandbox()
    fake.files.store["/remote/c.txt"] = b"payload"
    sb = Sandbox(fake)

    assert sb.fs.download_file("/remote/c.txt") == b"payload"

    out = tmp_path / "out.txt"
    assert sb.fs.download_file("/remote/c.txt", str(out)) is None
    assert out.read_bytes() == b"payload"


def test_get_file_info_missing_raises_not_found():
    fake = _FakeSandbox()
    sb = Sandbox(fake)
    with pytest.raises(NotFoundError) as ei:
        sb.fs.get_file_info("/tmp/does-not-exist.txt")
    assert ei.value.code == "not_found"


def test_get_preview_link_parses_token():
    sb = Sandbox(_FakeSandbox(id="sb-x"))
    link = sb.get_preview_link(8080)
    assert link.url == "https://sb-x.preview.example.com/?token=tok&port=8080"
    assert link.token == "tok"


def test_start_stop_map_to_resume_pause():
    fake = _FakeSandbox()
    sb = Sandbox(fake)
    sb.stop()
    sb.start()
    assert fake.paused is True
    assert fake.resumed is True


def test_context_manager_deletes():
    fake = _FakeSandbox()
    with Sandbox(fake) as sb:
        assert sb.id == "fake-1"
    assert fake.terminated is True


def test_fork_returns_wrapped_sandboxes():
    """fork is a Mitos superpower exposed on the Daytona handle for convenience;
    it returns Daytona Sandbox wrappers, not raw DirectSandboxes."""
    sb = Sandbox(_FakeSandbox())
    children = sb.fork(3)
    assert len(children) == 3
    assert all(isinstance(c, Sandbox) for c in children)


# --------------------------------------------------------------------------
# 2. create / get / list / delete against the mock-engine server (no KVM).
# --------------------------------------------------------------------------


SERVER_URL = "http://localhost:18085"
server_process = None


@pytest.fixture(scope="module")
def mock_server():
    global server_process
    result = subprocess.run(
        ["go", "build", "-o", "/tmp/sandbox-server-daytona-test", "./cmd/sandbox-server/"],
        cwd=os.path.join(os.path.dirname(__file__), "..", "..", ".."),
        capture_output=True,
    )
    if result.returncode != 0:
        pytest.skip(f"Could not build sandbox-server: {result.stderr.decode()}")

    server_process = subprocess.Popen(
        ["/tmp/sandbox-server-daytona-test", "--mock", "--addr", ":18085"],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    time.sleep(1)
    yield SERVER_URL
    server_process.terminate()
    server_process.wait(timeout=5)


def _client(base_url: str) -> Daytona:
    return Daytona(DaytonaConfig(api_url=base_url))


def test_create_and_delete_lifecycle(mock_server):
    """daytona.create -> a READY handle -> delete, on the mock server. Proves the
    shim drives the standalone (no-Kubernetes) path. process / fs need a vsock
    guest agent the mock server lacks (covered by the fake above and KVM CI)."""
    daytona = _client(mock_server)
    sandbox = daytona.create(CreateSandboxFromSnapshotParams(language="daytona-python"))
    assert sandbox.id
    daytona.delete(sandbox)


def test_get_reattaches_to_running_sandbox(mock_server):
    daytona = _client(mock_server)
    sandbox = daytona.create(CreateSandboxFromSnapshotParams(language="daytona-get"))
    try:
        again = daytona.get(sandbox.id)
        assert again.id == sandbox.id
    finally:
        daytona.delete(sandbox)


def test_get_unknown_id_raises_not_found(mock_server):
    daytona = _client(mock_server)
    with pytest.raises(AgentRunError) as ei:
        daytona.get("does-not-exist")
    assert ei.value.code == "not_found"


def test_list_returns_running_sandboxes(mock_server):
    daytona = _client(mock_server)
    sandbox = daytona.create(CreateSandboxFromSnapshotParams(language="daytona-list"))
    try:
        ids = [s.id for s in daytona.list()]
        assert sandbox.id in ids
    finally:
        daytona.delete(sandbox)
