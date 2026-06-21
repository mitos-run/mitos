"""Tests for the ZenML sandbox stack-component adapter
``mitos.integrations.zenml`` (issue #205).

ZenML treats sandboxes as a pluggable stack component selected by a flavor. The
external LISTING (registering the flavor in ZenML's integration registry) is a
human step; what is implemented here is the framework-neutral backend logic
(create / exec / files / run_code) the ZenML flavor wraps, backed by the native
``DirectSandbox`` surface through ``mitos.integrations._mapping``.

This suite mirrors test_e2b_compat.py / test_vibekit_integration.py:

  1. The stack component is configured with a ``MitosSandboxConfig`` and driven
     against a fake target (no server / KVM).
  2. The component never imports the ``zenml`` package: this proves the backend
     is fully usable with ZenML absent. ``flavor()`` is the lazy ZenML seam.
"""

import pytest

from mitos.integrations.zenml import MitosSandboxComponent, MitosSandboxConfig
from mitos.types import Execution, ExecResult, FileInfo, Result


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
        result = Result(data={"text/plain": "42"}, is_main_result=True)
        return Execution(
            text="42",
            logs={"stdout": [], "stderr": []},
            results=[result],
            error=None,
        )

    def terminate(self) -> None:
        self.terminated = True


def _component(monkeypatch, **cfg) -> MitosSandboxComponent:
    fake = _FakeSandbox()
    monkeypatch.setattr(
        "mitos.integrations.zenml._create_direct", lambda *a, **k: fake
    )
    config = MitosSandboxConfig(template="python", base_url="http://localhost:8080", **cfg)
    comp = MitosSandboxComponent(config)
    comp._fake = fake  # type: ignore[attr-defined]
    return comp


def test_config_holds_settings():
    config = MitosSandboxConfig(
        template="node", base_url="http://localhost:8080", api_key="k"
    )
    assert config.template == "node"
    assert config.base_url == "http://localhost:8080"
    assert config.api_key == "k"


def test_provision_creates_sandbox(monkeypatch):
    comp = _component(monkeypatch)
    sb = comp.provision()
    assert sb.id == "fake-1"
    # provision is idempotent: the same handle is returned.
    assert comp.provision() is sb


def test_run_command(monkeypatch):
    comp = _component(monkeypatch)
    out = comp.run_command("echo hi")
    assert out["stdout"] == "ran:echo hi"
    assert out["exit_code"] == 0
    assert comp._fake.exec_calls == [("echo hi", 60)]


def test_files_roundtrip(monkeypatch):
    comp = _component(monkeypatch)
    comp.write_file("/tmp/x.txt", "data")
    assert comp.read_file("/tmp/x.txt") == "data"
    listing = comp.list_files("/tmp")
    assert listing[0].name == "a.txt"
    assert comp._fake.files.writes == [("/tmp/x.txt", "data")]


def test_run_code(monkeypatch):
    comp = _component(monkeypatch)
    ex = comp.run_code("1 + 1")
    assert ex.text == "42"
    assert comp._fake.run_code_calls == [("1 + 1", "python", 60)]


def test_run_code_dict(monkeypatch):
    comp = _component(monkeypatch)
    out = comp.run_code_dict("1 + 1")
    assert out["text"] == "42"
    assert out["results"][0]["data"]["text/plain"] == "42"


def test_deprovision_terminates(monkeypatch):
    comp = _component(monkeypatch)
    comp.provision()
    comp.deprovision()
    assert comp._fake.terminated is True
    # deprovision is safe to call twice.
    comp.deprovision()


def test_flavor_name():
    # ZenML selects components by a flavor name; the constant is stable.
    assert MitosSandboxComponent.FLAVOR == "mitos"


def test_flavor_seam_lazy_imports_zenml():
    """``flavor()`` is the only ZenML seam; calling it without zenml installed
    raises a clear error naming the extra, not an ImportError stack trace."""
    from mitos.errors import AgentRunError

    with pytest.raises(AgentRunError) as ei:
        MitosSandboxComponent.flavor()
    assert "zenml" in str(ei.value).lower()


def test_no_zenml_import():
    import sys

    import mitos.integrations.zenml as mod  # noqa: F401

    assert "zenml" not in sys.modules
