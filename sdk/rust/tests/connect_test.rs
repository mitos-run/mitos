//! Integration tests for the direct-mode `Sandbox::exec`, which speaks the
//! Connect `sandbox.v1.Sandbox/ExecStream` runtime protocol (issue #358/#24)
//! over the SDK's existing `ureq` transport.
//!
//! Each stub is a hand-rolled HTTP/1.1 listener that serves a queue of canned
//! responses (raw bytes, arbitrary content type) and records every request. A
//! test forks first (the stub answers the fork POST with a JSON reply), then
//! calls exec (the stub answers with an `application/connect+json` body of
//! ENVELOPED frames: a stdout frame, an exit frame, an end-stream frame). The
//! test asserts the SDK hit the ExecStream path with the right headers and an
//! enveloped request body, and that an error end-stream surfaces as a typed
//! `MitosError`. No network access is needed.

use std::io::{BufRead, BufReader, Read, Write};
use std::net::{TcpListener, TcpStream};
use std::sync::mpsc::{self, Receiver};
use std::sync::{Arc, Mutex};
use std::thread;

use mitos::SandboxServer;

/// A captured request as seen by the stub.
#[derive(Debug, Clone)]
struct CapturedRequest {
    method: String,
    path: String,
    headers: Vec<(String, String)>,
    /// The raw request body bytes (for exec, the enveloped ExecStream request).
    body: Vec<u8>,
}

impl CapturedRequest {
    fn header(&self, name: &str) -> Option<&str> {
        let lname = name.to_ascii_lowercase();
        self.headers
            .iter()
            .find(|(k, _)| k.to_ascii_lowercase() == lname)
            .map(|(_, v)| v.as_str())
    }
}

/// A canned response: a status, a content type, and a raw body written verbatim.
#[derive(Clone)]
struct StubResponse {
    status: u16,
    reason: String,
    content_type: String,
    body: Vec<u8>,
}

impl StubResponse {
    /// A JSON fork reply for `id`, used as the first queued response so a test
    /// can mint a `Sandbox` bound to this stub before driving exec.
    fn fork_reply(id: &str) -> Self {
        StubResponse {
            status: 200,
            reason: "OK".to_string(),
            content_type: "application/json".to_string(),
            body: format!(
                r#"{{"id":"{id}","template_id":"python","endpoint":"e","fork_time_ms":1.0}}"#
            )
            .into_bytes(),
        }
    }

    /// A Connect streaming reply (`application/connect+json`) carrying `body`.
    fn connect_stream(body: Vec<u8>) -> Self {
        StubResponse {
            status: 200,
            reason: "OK".to_string(),
            content_type: "application/connect+json".to_string(),
            body,
        }
    }

    /// A non-2xx Connect error envelope returned before any frame.
    fn connect_error(status: u16, reason: &str, body: &str) -> Self {
        StubResponse {
            status,
            reason: reason.to_string(),
            content_type: "application/json".to_string(),
            body: body.as_bytes().to_vec(),
        }
    }
}

const FLAG_DATA: u8 = 0x00;
const FLAG_END_STREAM: u8 = 0x02;

/// Wraps one payload in the Connect 5-byte envelope prefix (1 flag byte + 4-byte
/// big-endian length + payload).
fn frame(flag: u8, payload: &[u8]) -> Vec<u8> {
    let mut out = Vec::with_capacity(5 + payload.len());
    out.push(flag);
    out.extend_from_slice(&(payload.len() as u32).to_be_bytes());
    out.extend_from_slice(payload);
    out
}

/// A successful ExecStream response: a stdout frame, an exit frame, then a clean
/// end-stream frame. `stdout_b64` is the base64 of the stdout bytes.
fn exec_ok_body(stdout_b64: &str, exit_code: i64, exec_time_ms: f64) -> Vec<u8> {
    let mut body = Vec::new();
    body.extend_from_slice(&frame(
        FLAG_DATA,
        format!(r#"{{"stdout":"{stdout_b64}"}}"#).as_bytes(),
    ));
    body.extend_from_slice(&frame(
        FLAG_DATA,
        format!(r#"{{"exit":{{"exitCode":{exit_code},"execTimeMs":{exec_time_ms}}}}}"#).as_bytes(),
    ));
    // A clean end-stream frame: empty trailers object.
    body.extend_from_slice(&frame(FLAG_END_STREAM, b"{}"));
    body
}

/// An in-process HTTP/1.1 stub serving a queue of canned responses in order and
/// recording each request.
struct ConnectStub {
    base_url: String,
    captured: Receiver<CapturedRequest>,
    handle: Option<thread::JoinHandle<()>>,
}

impl ConnectStub {
    fn start(responses: Vec<StubResponse>) -> Self {
        let listener = TcpListener::bind("127.0.0.1:0").expect("bind loopback");
        let addr = listener.local_addr().expect("local addr");
        let base_url = format!("http://{addr}");

        let (tx, rx) = mpsc::channel();
        let queue = Arc::new(Mutex::new(responses.into_iter()));

        let handle = thread::spawn(move || {
            for stream in listener.incoming() {
                let stream = match stream {
                    Ok(s) => s,
                    Err(_) => break,
                };
                let next = queue.lock().unwrap().next();
                let resp = match next {
                    Some(r) => r,
                    None => break,
                };
                if let Some(req) = handle_connection(stream, &resp) {
                    let _ = tx.send(req);
                }
            }
        });

        ConnectStub {
            base_url,
            captured: rx,
            handle: Some(handle),
        }
    }

    fn next_request(&self) -> CapturedRequest {
        self.captured
            .recv_timeout(std::time::Duration::from_secs(5))
            .expect("a request should have reached the stub")
    }
}

impl Drop for ConnectStub {
    fn drop(&mut self) {
        let _ = TcpStream::connect(self.base_url.trim_start_matches("http://"));
        if let Some(h) = self.handle.take() {
            let _ = h.join();
        }
    }
}

/// Reads one HTTP/1.1 request (with a Content-Length body of raw bytes), writes
/// the canned response, and returns the captured request.
fn handle_connection(stream: TcpStream, resp: &StubResponse) -> Option<CapturedRequest> {
    let mut reader = BufReader::new(stream.try_clone().expect("clone stream"));

    let mut request_line = String::new();
    if reader.read_line(&mut request_line).ok()? == 0 {
        return None;
    }
    let mut parts = request_line.split_whitespace();
    let method = parts.next()?.to_string();
    let path = parts.next()?.to_string();

    let mut headers = Vec::new();
    let mut content_length = 0usize;
    loop {
        let mut line = String::new();
        if reader.read_line(&mut line).ok()? == 0 {
            break;
        }
        let trimmed = line.trim_end_matches(['\r', '\n']);
        if trimmed.is_empty() {
            break;
        }
        if let Some((k, v)) = trimmed.split_once(':') {
            let k = k.trim().to_string();
            let v = v.trim().to_string();
            if k.eq_ignore_ascii_case("content-length") {
                content_length = v.parse().unwrap_or(0);
            }
            headers.push((k, v));
        }
    }

    let mut body = vec![0u8; content_length];
    if content_length > 0 {
        reader.read_exact(&mut body).ok()?;
    }

    let mut out = stream;
    let header = format!(
        "HTTP/1.1 {} {}\r\nContent-Type: {}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
        resp.status,
        resp.reason,
        resp.content_type,
        resp.body.len(),
    );
    let _ = out.write_all(header.as_bytes());
    let _ = out.write_all(&resp.body);
    let _ = out.flush();

    Some(CapturedRequest {
        method,
        path,
        headers,
        body,
    })
}

/// Builds a client pointed at the stub with no token.
fn client_for(base_url: &str) -> SandboxServer {
    SandboxServer::builder().base_url(base_url).build()
}

/// Parses the captured enveloped request body into (flag, json-payload-string).
fn parse_one_frame(body: &[u8]) -> (u8, String) {
    assert!(body.len() >= 5, "request body is too short for a frame");
    let flag = body[0];
    let len = u32::from_be_bytes([body[1], body[2], body[3], body[4]]) as usize;
    assert_eq!(body.len(), 5 + len, "request body length mismatch");
    (flag, String::from_utf8_lossy(&body[5..]).to_string())
}

#[test]
fn exec_round_trips_stdout_and_exit_code_over_connect() {
    // "hello\n" base64 == "aGVsbG8K".
    let stub = ConnectStub::start(vec![
        StubResponse::fork_reply("box"),
        StubResponse::connect_stream(exec_ok_body("aGVsbG8K", 0, 4.2)),
    ]);
    let server = client_for(&stub.base_url);

    let sandbox = server.fork_as("python", "box").expect("fork");
    let _fork_req = stub.next_request();

    let result = sandbox.exec("echo hello").expect("exec");
    assert_eq!(result.exit_code, 0);
    assert_eq!(result.stdout, "hello\n");
    assert_eq!(result.exec_time_ms, 4.2);
    assert!(result.success());

    let req = stub.next_request();
    assert_eq!(req.method, "POST");
    assert_eq!(req.path, "/sandbox.v1.Sandbox/ExecStream");
    assert_eq!(req.header("Content-Type"), Some("application/connect+json"));
    assert_eq!(req.header("Connect-Protocol-Version"), Some("1"));
    assert_eq!(req.header("X-Sandbox-Id"), Some("box"));
    // Tokenless client: no Authorization header.
    assert!(req.header("Authorization").is_none());

    let (flag, payload) = parse_one_frame(&req.body);
    assert_eq!(flag, FLAG_DATA, "request frame must be a plain data frame");
    assert!(
        payload.contains("\"command\":\"echo hello\""),
        "payload was {payload}"
    );
    // The default 30s timeout is sent as proto-JSON timeoutSeconds.
    assert!(
        payload.contains("\"timeoutSeconds\":30"),
        "payload was {payload}"
    );
}

#[test]
fn exec_captures_stderr_and_nonzero_exit() {
    // stdout "" , stderr "boom\n" base64 == "Ym9vbQo=", exit 2.
    let mut body = Vec::new();
    body.extend_from_slice(&frame(FLAG_DATA, br#"{"stderr":"Ym9vbQo="}"#));
    body.extend_from_slice(&frame(
        FLAG_DATA,
        br#"{"exit":{"exitCode":2,"execTimeMs":7.0}}"#,
    ));
    body.extend_from_slice(&frame(FLAG_END_STREAM, b"{}"));

    let stub = ConnectStub::start(vec![
        StubResponse::fork_reply("box"),
        StubResponse::connect_stream(body),
    ]);
    let server = client_for(&stub.base_url);
    let sandbox = server.fork_as("python", "box").expect("fork");
    let _ = stub.next_request();

    let result = sandbox.exec("false").expect("exec");
    assert_eq!(result.exit_code, 2);
    assert_eq!(result.stderr, "boom\n");
    assert!(!result.success());
    let _ = stub.next_request();
}

#[test]
fn exec_error_end_stream_yields_typed_error() {
    let mut body = Vec::new();
    // A stdout frame before the error proves partial output does not mask the
    // terminal error.
    body.extend_from_slice(&frame(FLAG_DATA, br#"{"stdout":"cGFydGlhbA=="}"#));
    body.extend_from_slice(&frame(
        FLAG_END_STREAM,
        br#"{"error":{"code":"not_found","message":"sandbox not ready"}}"#,
    ));
    let stub = ConnectStub::start(vec![
        StubResponse::fork_reply("box"),
        StubResponse::connect_stream(body),
    ]);
    let server = client_for(&stub.base_url);
    let sandbox = server.fork_as("python", "box").expect("fork");
    let _ = stub.next_request();

    let err = sandbox
        .exec("echo hi")
        .expect_err("error end-stream must fail");
    assert_eq!(err.code, "not_found");
    assert_eq!(err.status, 404);
    let _ = stub.next_request();
}

#[test]
fn exec_non_2xx_connect_envelope_yields_typed_error() {
    // A streaming RPC that fails before the first frame returns a normal HTTP
    // error body (the Connect error envelope), not an end-stream frame.
    let stub = ConnectStub::start(vec![
        StubResponse::fork_reply("box"),
        StubResponse::connect_error(
            429,
            "Too Many Requests",
            r#"{"code":"resource_exhausted","message":"too many concurrent streams"}"#,
        ),
    ]);
    let server = client_for(&stub.base_url);
    let sandbox = server.fork_as("python", "box").expect("fork");
    let _ = stub.next_request();

    let err = sandbox.exec("echo hi").expect_err("429 must fail");
    assert_eq!(err.code, "resource_exhausted");
    assert_eq!(err.status, 429);
    let _ = stub.next_request();
}

#[test]
fn exec_sends_authorization_when_token_set() {
    let stub = ConnectStub::start(vec![
        StubResponse::fork_reply("box"),
        StubResponse::connect_stream(exec_ok_body("", 0, 1.0)),
    ]);
    let server = SandboxServer::builder()
        .base_url(&stub.base_url)
        .api_key("sk-secret")
        .build();
    let sandbox = server.fork_as("python", "box").expect("fork");
    let _ = stub.next_request();

    let _ = sandbox.exec("true").expect("exec");
    let req = stub.next_request();
    assert_eq!(req.header("Authorization"), Some("Bearer sk-secret"));
    // The token value must never leak into the recorded path or any header key.
    assert_eq!(req.path, "/sandbox.v1.Sandbox/ExecStream");
}
