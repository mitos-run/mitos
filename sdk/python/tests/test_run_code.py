import base64
import json

import pytest

from mitos.types import Execution, Result, ExecutionError
from mitos.sandbox import _parse_run_code_stream


def b64(s: str) -> str:
    return base64.b64encode(s.encode()).decode()


def _frames(*objs):
    return "".join(json.dumps(o) + "\n" for o in objs).encode()


def _line_iter(body: bytes):
    for line in body.splitlines():
        if line.strip():
            yield line


def test_parse_run_code_stream_accumulates():
    body = _frames(
        {"kind": "stdout", "stdout": b64("hi\n")},
        {"kind": "result", "result": {"text": "42", "data": {"text/plain": "42", "image/png": "aGVsbG8="}}},
        {"kind": "exit", "exit_code": 0},
    )
    seen_stdout, seen_results = [], []
    ex = _parse_run_code_stream(
        _line_iter(body),
        on_stdout=seen_stdout.append,
        on_stderr=lambda s: None,
        on_result=seen_results.append,
    )
    assert ex.text == "42"
    assert ex.logs["stdout"] == ["hi\n"]
    assert ex.results[0].png == "aGVsbG8="
    assert seen_stdout == ["hi\n"]
    assert len(seen_results) == 1
    assert ex.error is None


def test_parse_run_code_stream_error():
    body = _frames(
        {"kind": "error", "error": {"name": "ValueError", "value": "bad", "traceback": ["ValueError: bad"]}},
        {"kind": "exit", "exit_code": 1},
    )
    ex = _parse_run_code_stream(_line_iter(body), None, None, None)
    assert ex.error is not None
    assert ex.error.name == "ValueError"
    assert ex.text is None


def test_parse_run_code_stream_truncated_raises():
    """A body that ends without the terminal exit frame is a dropped or
    truncated connection, not a clean success. It must raise rather than return
    a silent Execution with error=None."""
    body = _frames(
        {"kind": "stdout", "stdout": b64("partial\n")},
        {"kind": "result", "result": {"text": "7", "data": {"text/plain": "7"}}},
    )
    with pytest.raises(RuntimeError, match="terminal exit frame"):
        _parse_run_code_stream(_line_iter(body), None, None, None)


def test_direct_sandbox_run_code_routes(monkeypatch):
    """DirectSandbox.run_code drives the Connect RunCodeStream server-streaming
    RPC (/sandbox.v1.Sandbox/RunCodeStream) and folds the proto-JSON frames into
    an Execution. The sandbox id rides the X-Sandbox-Id header, and
    RunResult.data values are base64 (proto map<string,bytes>), decoded back to
    the kernel string."""
    import contextlib
    import struct

    from mitos.direct import DirectSandbox

    captured = {}

    def _envelope(obj, end=False):
        payload = json.dumps(obj).encode()
        flag = 0b00000010 if end else 0
        return bytes([flag]) + struct.pack(">I", len(payload)) + payload

    class _FakeResp:
        is_success = True

        def iter_bytes(self):
            yield _envelope({"stdout": b64("ok\n")})
            yield _envelope({"result": {"text": "7", "data": {"text/plain": b64("7")}}})
            yield _envelope({"exitCode": 0})
            yield _envelope({}, end=True)

    @contextlib.contextmanager
    def _fake_stream(method, url, **kwargs):
        captured["method"] = method
        captured["url"] = url
        captured["headers"] = kwargs.get("headers")
        captured["content"] = kwargs.get("content")
        yield _FakeResp()

    sb = DirectSandbox.__new__(DirectSandbox)
    sb.id = "sb1"
    sb._server_url = "http://localhost:18080"
    sb._api_key = None

    class _FakeHTTP:
        stream = staticmethod(_fake_stream)

    sb._http = _FakeHTTP()

    ex = sb.run_code("print('ok')\n7")
    assert captured["method"] == "POST"
    assert captured["url"].endswith("/sandbox.v1.Sandbox/RunCodeStream")
    assert captured["headers"]["X-Sandbox-Id"] == "sb1"
    assert b"print('ok')" in captured["content"]
    assert ex.text == "7"
    assert ex.logs["stdout"] == ["ok\n"]


def test_result_mime_accessors():
    r = Result(data={"image/png": "aGVsbG8=", "text/plain": "fig"})
    assert r.png == "aGVsbG8="
    assert r.text == "fig"
    assert r.html is None
    assert r.svg is None


def test_execution_shape():
    ex = Execution(
        text="42",
        logs={"stdout": ["hi\n"], "stderr": []},
        results=[Result(data={"text/plain": "42"})],
        error=None,
    )
    assert ex.text == "42"
    assert ex.logs["stdout"] == ["hi\n"]
    assert ex.results[0].text == "42"
    assert ex.error is None


def test_execution_error():
    err = ExecutionError(name="ValueError", value="bad", traceback=["...", "ValueError: bad"])
    ex = Execution(text=None, logs={"stdout": [], "stderr": []}, results=[], error=err)
    assert ex.error.name == "ValueError"
    assert "ValueError: bad" in ex.error.traceback
