"""Preview URLs / get_host(port) (issue #126).

get_host(port) returns a signed, expiring preview URL for a sandbox port. The
signing secret lives on the server, so the SDK asks the server to mint the URL
(POST /v1/preview) and returns the well-formed signed URL it gets back. These
tests run against an in-process fake server that mimics the /v1/preview route
shape, so the SDK's request/response wiring is exercised without a real proxy or
a public domain (the real auto-TLS proxy is the server-side half of #126).
"""

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs

import pytest

from mitos.direct import DirectSandbox


def _make_preview_server():
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def do_POST(self):
            if self.path != "/v1/preview":
                self.send_response(404)
                self.end_headers()
                return
            n = int(self.headers.get("Content-Length", 0))
            body = json.loads(self.rfile.read(n)) if n else {}
            sandbox = body.get("sandbox", "")
            port = body.get("port", 0)
            if not sandbox or not port:
                self.send_response(400)
                self.end_headers()
                self.wfile.write(b'{"error":{"code":"invalid_request","message":"bad"}}')
                return
            # Server mints the signed URL; the token here stands in for the real
            # HMAC token (the Go signer produces the real one server-side).
            url = (
                f"https://{sandbox}.preview.example.com/"
                f"?token=ZmFrZQ.c2ln&port={port}"
            )
            out = json.dumps({"url": url, "expires_unix": 1700003600}).encode()
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
def preview_server():
    server, url = _make_preview_server()
    yield url
    server.shutdown()


def _sandbox(server_url):
    return DirectSandbox(
        id="sb-1",
        template="python",
        endpoint="sb-1.local:9091",
        server_url=server_url,
        fork_time_ms=1.0,
    )


def test_get_host_returns_signed_url(preview_server):
    sb = _sandbox(preview_server)
    url = sb.get_host(8080)
    parsed = urlparse(url)
    assert parsed.scheme == "https"
    assert parsed.hostname == "sb-1.preview.example.com"
    q = parse_qs(parsed.query)
    assert q.get("token"), "preview URL must carry a token query param"
    assert q.get("port") == ["8080"]


def test_get_host_default_returns_signed_url(preview_server):
    # get_host without an explicit port should still mint a URL (whole-sandbox).
    sb = _sandbox(preview_server)
    url = sb.get_host()
    assert url.startswith("https://sb-1.preview.")
    assert "token=" in url


def test_get_host_rejects_bad_port(preview_server):
    sb = _sandbox(preview_server)
    with pytest.raises(ValueError):
        sb.get_host(0)
    with pytest.raises(ValueError):
        sb.get_host(70000)
