"""Live-state fork request shape for the flat SDK (issue #596).

DirectSandbox.fork(n) forks the RUNNING sandbox, carrying its live memory and
on-disk filesystem to each child, instead of re-forking the cold template. On
the wire that means POSTing to /v1/sandboxes/<parent-id>/fork (the standalone
server's live-fork endpoint), not the old /v1/fork template-claim route. These
tests run against an in-process fake server that records the path, body, and
headers each fork sends, so the SDK's request wiring is proven without a real
engine or KVM.
"""

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

from mitos.direct import DirectSandbox


class _Recorder:
    def __init__(self):
        self.requests = []


def _make_fork_server(recorder: _Recorder):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def do_POST(self):
            n = int(self.headers.get("Content-Length", 0))
            body = json.loads(self.rfile.read(n)) if n else {}
            recorder.requests.append(
                {
                    "path": self.path,
                    "body": body,
                    "idempotency_key": self.headers.get("Idempotency-Key"),
                }
            )
            # Echo a sandboxInfo shape: the standalone server returns the child's
            # id, its inherited template id, endpoint, and measured fork time.
            child_id = body.get("id") or "auto-child"
            out = json.dumps(
                {
                    "id": child_id,
                    "template_id": "python",
                    "endpoint": f"{child_id}.local:9091",
                    "fork_time_ms": 4.2,
                }
            ).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(out)))
            self.end_headers()
            self.wfile.write(out)

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    t = threading.Thread(target=server.serve_forever, daemon=True)
    t.start()
    return server, f"http://127.0.0.1:{server.server_address[1]}"


@pytest.fixture()
def fork_server():
    rec = _Recorder()
    server, url = _make_fork_server(rec)
    yield url, rec
    server.shutdown()


def _sandbox(server_url):
    return DirectSandbox(
        id="parent-1",
        template="python",
        endpoint="parent-1.local:9091",
        server_url=server_url,
        fork_time_ms=1.0,
    )


def test_fork_posts_to_live_endpoint_with_parent_id(fork_server):
    url, rec = _sandbox_and_fork(fork_server)
    assert len(rec.requests) == 1
    req = rec.requests[0]
    # A live fork targets the PARENT sandbox by id, not the /v1/fork template
    # route: the child descends from the running parent.
    assert req["path"] == "/v1/sandboxes/parent-1/fork", req["path"]
    # It pauses the source across the checkpoint so memory and disk are
    # consistent.
    assert req["body"].get("pause_source") is True
    # Every auto-retried fork carries an idempotency key so it never
    # double-creates a sibling (issue #22).
    assert req["idempotency_key"], "fork must send an Idempotency-Key"


def test_fork_returns_ready_child(fork_server):
    url, rec = fork_server
    parent = _sandbox(url)
    child = parent.fork()[0]
    assert child.id == rec.requests[0]["body"]["id"]
    assert child.template == "python"
    assert child.fork_time_ms == 4.2


def test_fork_n_creates_n_distinct_children(fork_server):
    url, rec = fork_server
    parent = _sandbox(url)
    kids = parent.fork(3, id="batch")
    assert len(kids) == 3
    ids = {k.id for k in kids}
    assert len(ids) == 3, ids
    # Every request went to the live endpoint for the SAME parent.
    assert all(r["path"] == "/v1/sandboxes/parent-1/fork" for r in rec.requests)


def _sandbox_and_fork(fork_server):
    url, rec = fork_server
    parent = _sandbox(url)
    parent.fork()
    return url, rec
