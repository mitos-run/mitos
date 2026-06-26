import base64
import json
import struct
from unittest.mock import MagicMock, patch

import httpx
import pytest

from mitos.errors import AgentRunError
from mitos.sandbox import Sandbox, SandboxFiles
from mitos.types import SandboxPhase


TEST_TOKEN = "ab" * 32  # 64 hex chars, like the controller mints


def _token_secret_mock(token: str = TEST_TOKEN, endpoint: str = "10.0.0.5:9091") -> MagicMock:
    """Mock of CoreV1Api.read_namespaced_secret's V1Secret (base64 data)."""
    secret = MagicMock()
    secret.data = {
        "token": base64.b64encode(token.encode()).decode(),
        "endpoint": base64.b64encode(endpoint.encode()).decode(),
    }
    return secret


@pytest.fixture
def mock_api():
    return MagicMock()


@pytest.fixture
def mock_core_api():
    core_api = MagicMock()
    core_api.read_namespaced_secret.return_value = _token_secret_mock()
    return core_api


@pytest.fixture
def ready_sandbox(mock_api, mock_core_api):
    return Sandbox(
        name="test-sandbox",
        namespace="default",
        pool="test-pool",
        api=mock_api,
        core_api=mock_core_api,
        _endpoint="127.0.0.1:8080",
        _phase=SandboxPhase.READY,
    )


@pytest.fixture
def pending_sandbox(mock_api, mock_core_api):
    return Sandbox(
        name="test-sandbox",
        namespace="default",
        pool="test-pool",
        api=mock_api,
        core_api=mock_core_api,
    )


def test_sandbox_repr(ready_sandbox):
    r = repr(ready_sandbox)
    assert "test-sandbox" in r
    assert "Ready" in r


def test_sandbox_endpoint(ready_sandbox):
    assert ready_sandbox.endpoint == "127.0.0.1:8080"


def test_sandbox_phase(ready_sandbox):
    assert ready_sandbox.phase == SandboxPhase.READY


def test_pending_sandbox_phase(pending_sandbox):
    assert pending_sandbox.phase == SandboxPhase.PENDING


def test_sandbox_context_manager(mock_api, mock_core_api):
    sandbox = Sandbox(
        name="ctx-sandbox",
        namespace="default",
        pool="test-pool",
        api=mock_api,
        core_api=mock_core_api,
        _endpoint="127.0.0.1:8080",
        _phase=SandboxPhase.READY,
    )

    with sandbox as s:
        assert s.name == "ctx-sandbox"

    mock_api.delete_namespaced_custom_object.assert_called_once()


def test_sandbox_terminate(ready_sandbox, mock_api):
    ready_sandbox.terminate()

    mock_api.delete_namespaced_custom_object.assert_called_once_with(
        group="mitos.run",
        version="v1",
        namespace="default",
        plural="sandboxes",
        name="test-sandbox",
    )
    assert ready_sandbox.phase == SandboxPhase.TERMINATING


def test_sandbox_fork_creates_cr(ready_sandbox, mock_api):
    mock_api.get_namespaced_custom_object.return_value = {
        "status": {
            "readyReplicas": 2,
            "children": [
                {"name": "fork-1", "endpoint": "127.0.0.1:9001", "phase": "Ready", "sandboxID": "f1", "node": "n1"},
                {"name": "fork-2", "endpoint": "127.0.0.1:9002", "phase": "Ready", "sandboxID": "f2", "node": "n1"},
            ],
        }
    }

    forks = ready_sandbox.fork(2)

    mock_api.create_namespaced_custom_object.assert_called_once()
    call_kwargs = mock_api.create_namespaced_custom_object.call_args
    body = call_kwargs.kwargs.get("body") or call_kwargs[1].get("body")
    assert body["kind"] == "Sandbox"
    assert body["apiVersion"] == "mitos.run/v1"
    assert body["spec"]["replicas"] == 2
    assert body["spec"]["source"]["fromSandbox"]["name"] == "test-sandbox"

    assert len(forks) == 2
    assert forks[0].phase == SandboxPhase.READY
    assert forks[1].phase == SandboxPhase.READY
    assert forks[0]._sandbox_id == "f1"
    assert forks[1]._sandbox_id == "f2"


def test_sandbox_fork_rejected_is_legible(ready_sandbox, mock_api):
    # A refused fork (secret inheritance, capacity, budget) is terminal: the
    # controller sets a Rejected condition. fork() must surface it as an
    # LLM-legible error instead of waiting out the timeout (#311).
    mock_api.get_namespaced_custom_object.return_value = {
        "status": {
            "conditions": [
                {
                    "type": "Rejected",
                    "status": "True",
                    "reason": "SecretInheritanceDenied",
                    "message": "source sandbox holds secrets; recreate the fork with spec.secretInheritance=inherit to permit it",
                },
            ],
        },
    }
    with pytest.raises(AgentRunError) as ei:
        ready_sandbox.fork(2, timeout=0.5)
    assert ei.value.code == "fork_rejected"
    assert ei.value.cause == "SecretInheritanceDenied"


def test_sandbox_fork_threads_timeout(ready_sandbox):
    """fork(timeout=...) must thread the value into _wait_forks so a wide
    single-node fan-out is not capped at the 30.0s default."""
    from unittest.mock import patch

    with patch.object(
        type(ready_sandbox), "_wait_forks", return_value=[]
    ) as wait_forks:
        ready_sandbox.fork(n=2, timeout=180.0)

    wait_forks.assert_called_once()
    assert wait_forks.call_args.kwargs.get("timeout") == 180.0


def test_sandbox_fork_default_timeout_back_compat(ready_sandbox):
    """fork() with no timeout keeps the 30.0s default for back-compat."""
    from unittest.mock import patch

    with patch.object(
        type(ready_sandbox), "_wait_forks", return_value=[]
    ) as wait_forks:
        ready_sandbox.fork()

    assert wait_forks.call_args.kwargs.get("timeout") == 30.0


def test_sandbox_wait_ready_polls(pending_sandbox, mock_api):
    mock_api.get_namespaced_custom_object.side_effect = [
        {"status": {"phase": "Pending"}},
        {"status": {"phase": "Restoring"}},
        {"status": {"phase": "Ready", "endpoint": "10.0.0.5:8080"}},
    ]

    pending_sandbox._wait_ready(timeout=5.0)

    assert pending_sandbox.phase == SandboxPhase.READY
    assert pending_sandbox._endpoint == "10.0.0.5:8080"
    assert mock_api.get_namespaced_custom_object.call_count == 3


def test_sandbox_wait_ready_failed(pending_sandbox, mock_api):
    mock_api.get_namespaced_custom_object.return_value = {
        "status": {"phase": "Failed"}
    }

    with pytest.raises(AgentRunError, match="failed") as ei:
        pending_sandbox._wait_ready(timeout=1.0)
    assert ei.value.code == "sandbox_failed"


def test_wait_ready_reads_token_secret(mock_api):
    core_api = MagicMock()
    core_api.read_namespaced_secret.return_value = _token_secret_mock()
    sandbox = Sandbox(
        name="test-sandbox",
        namespace="default",
        pool="test-pool",
        api=mock_api,
        core_api=core_api,
    )
    mock_api.get_namespaced_custom_object.return_value = {
        "status": {"phase": "Ready", "endpoint": "10.0.0.5:9091", "sandboxID": "sb-1"}
    }

    sandbox._wait_ready(timeout=5.0)

    core_api.read_namespaced_secret.assert_called_once_with(
        name="test-sandbox-sandbox-token", namespace="default"
    )
    assert sandbox._token == TEST_TOKEN


def test_wait_ready_tolerates_missing_token_secret(mock_api):
    from kubernetes.client.rest import ApiException

    core_api = MagicMock()
    core_api.read_namespaced_secret.side_effect = ApiException(status=404)
    sandbox = Sandbox(
        name="test-sandbox",
        namespace="default",
        pool="test-pool",
        api=mock_api,
        core_api=core_api,
    )
    mock_api.get_namespaced_custom_object.return_value = {
        "status": {"phase": "Ready", "endpoint": "10.0.0.5:9091"}
    }

    sandbox._wait_ready(timeout=5.0)

    assert sandbox._token is None


def _ready_http_sandbox(transport: httpx.MockTransport, token: str | None = TEST_TOKEN) -> Sandbox:
    sandbox = Sandbox(
        name="claim-1",
        namespace="default",
        pool="pool-1",
        api=MagicMock(),  # k8s API unused when endpoint/phase/sandbox_id pre-seeded
        core_api=MagicMock(),
        _endpoint="10.0.3.7:9091",
        _phase=SandboxPhase.READY,
    )
    sandbox._sandbox_id = "sb-claim-1"
    sandbox._token = token
    sandbox._http = httpx.Client(transport=transport)
    return sandbox


def _connect_frame(payload: bytes, end: bool = False) -> bytes:
    """One Connect enveloped frame: a 1-byte flag (0x02 on end-stream), a 4-byte
    big-endian length, then the JSON payload."""
    flag = 0b00000010 if end else 0
    return bytes([flag]) + struct.pack(">I", len(payload)) + payload


def _decode_connect_request(body: bytes) -> list[dict]:
    """Decode the buffered Connect request body (one enveloped frame per
    message) back to the proto-JSON messages the SDK sent."""
    out, i = [], 0
    while i + 5 <= len(body):
        length = struct.unpack(">I", body[i + 1 : i + 5])[0]
        payload = body[i + 5 : i + 5 + length]
        i += 5 + length
        if payload:
            out.append(json.loads(payload))
    return out


def _exec_stream_response(
    exit_code: int = 0, stdout: bytes = b"", stderr: bytes = b"", exec_time_ms: float = 1.0
) -> httpx.Response:
    """A Connect ExecStream server-stream reply: optional stdout/stderr chunk
    frames, the terminal ExecExit, then a clean end-stream frame."""
    frames = b""
    if stdout:
        frames += _connect_frame(
            json.dumps({"stdout": base64.b64encode(stdout).decode()}).encode()
        )
    if stderr:
        frames += _connect_frame(
            json.dumps({"stderr": base64.b64encode(stderr).decode()}).encode()
        )
    frames += _connect_frame(
        json.dumps({"exit": {"exitCode": exit_code, "execTimeMs": exec_time_ms}}).encode()
    )
    frames += _connect_frame(json.dumps({}).encode(), end=True)
    return httpx.Response(
        200, content=frames, headers={"content-type": "application/connect+json"}
    )


def test_exec_targets_connect_and_sends_sandbox_id():
    """k8s Sandbox.exec drives the Connect ExecStream server-stream
    (/sandbox.v1.Sandbox/ExecStream), routes by the X-Sandbox-Id header, and
    sends the proto-JSON ExecStreamRequest (command + timeoutSeconds)."""
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["url"] = str(request.url)
        seen["sandbox_id"] = request.headers.get("x-sandbox-id")
        seen["content_type"] = request.headers.get("content-type")
        seen["msgs"] = _decode_connect_request(request.content)
        return _exec_stream_response(exit_code=0, stdout=b"hi\n")

    result = _ready_http_sandbox(httpx.MockTransport(handler)).exec("echo hi")

    assert result.stdout == "hi\n"
    assert result.exit_code == 0
    assert seen["url"].endswith("/sandbox.v1.Sandbox/ExecStream")
    assert seen["sandbox_id"] == "sb-claim-1"
    assert seen["content_type"] == "application/connect+json"
    assert seen["msgs"][0]["command"] == "echo hi"
    assert "timeoutSeconds" in seen["msgs"][0]


def test_files_read_targets_connect_and_sends_sandbox_id():
    """k8s Sandbox.files.read drives the Connect ReadFile server-stream
    (/sandbox.v1.Sandbox/ReadFile), routes by the X-Sandbox-Id header, and
    concatenates the base64 Chunk data."""
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["url"] = str(request.url)
        seen["sandbox_id"] = request.headers.get("x-sandbox-id")
        seen["content_type"] = request.headers.get("content-type")
        body = (
            _connect_frame(json.dumps({"data": base64.b64encode(b"data").decode(), "eof": True}).encode())
            + _connect_frame(json.dumps({}).encode(), end=True)
        )
        return httpx.Response(200, content=body,
                              headers={"content-type": "application/connect+json"})

    content = _ready_http_sandbox(httpx.MockTransport(handler)).files.read("/workspace/x")
    assert content == "data"
    assert seen["url"].endswith("/sandbox.v1.Sandbox/ReadFile")
    assert seen["sandbox_id"] == "sb-claim-1"
    assert seen["content_type"] == "application/connect+json"


def test_exec_sends_bearer_token():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["auth"] = request.headers.get("authorization")
        return _exec_stream_response()

    _ready_http_sandbox(httpx.MockTransport(handler)).exec("true")

    assert seen["auth"] == f"Bearer {TEST_TOKEN}"


def test_all_file_calls_send_bearer_token():
    """Every Connect file RPC (ReadFile, WriteFile, List, Mkdir, Remove) carries
    the per-sandbox bearer, whether it is a server-stream, a client-stream, or a
    unary call."""
    auths = []

    def handler(request: httpx.Request) -> httpx.Response:
        auths.append(request.headers.get("authorization"))
        method = str(request.url).rsplit("/", 1)[-1]
        if method == "ReadFile":
            body = (
                _connect_frame(json.dumps({"data": "", "eof": True}).encode())
                + _connect_frame(json.dumps({}).encode(), end=True)
            )
            return httpx.Response(200, content=body,
                                  headers={"content-type": "application/connect+json"})
        if method == "WriteFile":
            body = (
                _connect_frame(json.dumps({"bytesWritten": 4}).encode())
                + _connect_frame(json.dumps({}).encode(), end=True)
            )
            return httpx.Response(200, content=body,
                                  headers={"content-type": "application/connect+json"})
        # List, Mkdir, Remove are unary application/json.
        return httpx.Response(200, json={"entries": []})

    files = _ready_http_sandbox(httpx.MockTransport(handler)).files
    files.read("/x")
    files.write("/x", "data")
    files.list("/")
    files.mkdir("/d")
    files.remove("/x")

    assert auths == [f"Bearer {TEST_TOKEN}"] * 5


def test_no_token_sends_no_auth_header():
    seen = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen["auth"] = request.headers.get("authorization")
        return _exec_stream_response()

    _ready_http_sandbox(httpx.MockTransport(handler), token=None).exec("true")

    assert seen["auth"] is None


def test_wait_forks_loads_each_fork_token(mock_api):
    core_api = MagicMock()
    core_api.read_namespaced_secret.return_value = _token_secret_mock()
    sandbox = Sandbox(
        name="test-sandbox",
        namespace="default",
        pool="test-pool",
        api=mock_api,
        core_api=core_api,
        _endpoint="127.0.0.1:8080",
        _phase=SandboxPhase.READY,
    )
    mock_api.get_namespaced_custom_object.return_value = {
        "status": {
            "readyReplicas": 1,
            "children": [
                {"name": "fork-1", "endpoint": "127.0.0.1:9001", "phase": "Ready", "sandboxID": "f1", "node": "n1"},
            ],
        }
    }

    forks = sandbox.fork(1)

    assert forks[0]._token == TEST_TOKEN
    core_api.read_namespaced_secret.assert_called_once_with(
        name="fork-1-sandbox-token", namespace="default"
    )
