//! Integration tests for the direct-mode `SandboxServer`. They run against an
//! in-process stub HTTP server (a hand-rolled HTTP/1.1 listener on a loopback
//! ephemeral port) so no network access is needed. The stub mirrors the
//! sandbox-server wire shapes, the same way the TypeScript `test/server.test.ts`
//! and the Ruby WEBrick stub do.

use std::io::{BufRead, BufReader, Read, Write};
use std::net::{TcpListener, TcpStream};
use std::sync::mpsc::{self, Receiver};
use std::sync::{Arc, Mutex};
use std::thread;

use mitos::{MitosError, SandboxServer};

/// A captured request as seen by the stub server.
#[derive(Debug, Clone)]
struct CapturedRequest {
    method: String,
    path: String,
    headers: Vec<(String, String)>,
    body: String,
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

/// A canned response the stub returns for the next request.
#[derive(Clone)]
struct StubResponse {
    status: u16,
    reason: String,
    body: String,
}

impl StubResponse {
    fn ok(body: &str) -> Self {
        StubResponse {
            status: 200,
            reason: "OK".to_string(),
            body: body.to_string(),
        }
    }

    fn status(status: u16, reason: &str, body: &str) -> Self {
        StubResponse {
            status,
            reason: reason.to_string(),
            body: body.to_string(),
        }
    }
}

/// An in-process HTTP/1.1 stub. It serves one canned response per incoming
/// request from a queue and records every request for assertions.
struct StubServer {
    base_url: String,
    captured: Receiver<CapturedRequest>,
    handle: Option<thread::JoinHandle<()>>,
}

impl StubServer {
    /// Starts the stub bound to 127.0.0.1:0, serving `responses` in order.
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
                    // Out of canned responses: stop accepting. This is the
                    // teardown signal sent by the shutdown poke below.
                    None => break,
                };
                if let Some(req) = handle_connection(stream, &resp) {
                    let _ = tx.send(req);
                }
            }
        });

        StubServer {
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

impl Drop for StubServer {
    fn drop(&mut self) {
        // Poke the listener so the accept loop wakes and exits if it is still
        // waiting (the queue may be drained or not).
        let _ = TcpStream::connect(self.base_url.trim_start_matches("http://"));
        if let Some(h) = self.handle.take() {
            let _ = h.join();
        }
    }
}

/// Parses one HTTP/1.1 request off the stream, writes `resp`, and returns the
/// captured request. Returns `None` if nothing readable arrived (a shutdown
/// poke).
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
    let payload = format!(
        "HTTP/1.1 {} {}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
        resp.status,
        resp.reason,
        resp.body.len(),
        resp.body,
    );
    let _ = out.write_all(payload.as_bytes());
    let _ = out.flush();

    Some(CapturedRequest {
        method,
        path,
        headers,
        body: String::from_utf8_lossy(&body).to_string(),
    })
}

/// Builds a client pointed at the stub with no token.
fn client_for(base_url: &str) -> SandboxServer {
    SandboxServer::builder().base_url(base_url).build()
}

#[test]
fn create_template_returns_id_and_ready() {
    let stub = StubServer::start(vec![StubResponse::ok(
        r#"{"id":"python","ready":true,"created_at":"2026-06-21T00:00:00Z","creation_time_ms":12.5}"#,
    )]);
    let server = client_for(&stub.base_url);

    let tmpl = server.create_template("python").expect("create_template");
    assert_eq!(tmpl.id, "python");
    assert!(tmpl.ready);

    let req = stub.next_request();
    assert_eq!(req.method, "POST");
    assert_eq!(req.path, "/v1/templates");
    assert!(req.body.contains("\"id\":\"python\""));
    assert!(req.body.contains("\"init_wait_seconds\":5"));
    // create_template sends a fresh Idempotency-Key.
    assert!(req.header("Idempotency-Key").is_some());
}

#[test]
fn list_templates_maps_array() {
    let stub = StubServer::start(vec![StubResponse::ok(
        r#"[{"id":"python","ready":true,"created_at":"","creation_time_ms":0},
            {"id":"node","ready":false,"created_at":"","creation_time_ms":0}]"#,
    )]);
    let server = client_for(&stub.base_url);

    let templates = server.list_templates().expect("list_templates");
    assert_eq!(templates.len(), 2);
    assert_eq!(templates[0].id, "python");
    assert!(templates[0].ready);
    assert_eq!(templates[1].id, "node");
    assert!(!templates[1].ready);

    let req = stub.next_request();
    assert_eq!(req.method, "GET");
    assert_eq!(req.path, "/v1/templates");
}

#[test]
fn list_templates_null_body_is_empty() {
    let stub = StubServer::start(vec![StubResponse::ok("null")]);
    let server = client_for(&stub.base_url);
    let templates = server.list_templates().expect("list_templates");
    assert!(templates.is_empty());
}

#[test]
fn fork_returns_sandbox_with_id_and_sends_idempotency_key() {
    let stub = StubServer::start(vec![StubResponse::ok(
        r#"{"id":"my-box","template_id":"python","endpoint":"127.0.0.1:9091","fork_time_ms":3.0}"#,
    )]);
    let server = client_for(&stub.base_url);

    let sandbox = server.fork_as("python", "my-box").expect("fork");
    assert_eq!(sandbox.id, "my-box");
    assert_eq!(sandbox.template_id, "python");

    let req = stub.next_request();
    assert_eq!(req.method, "POST");
    assert_eq!(req.path, "/v1/fork");
    assert!(req.body.contains("\"template\":\"python\""));
    assert!(req.body.contains("\"id\":\"my-box\""));
    // fork sends a fresh Idempotency-Key.
    let key = req
        .header("Idempotency-Key")
        .expect("Idempotency-Key header");
    assert!(!key.is_empty());
}

#[test]
fn fork_generates_sandbox_hex_id_when_none_given() {
    // The server echoes back an empty id, so the SDK must keep its generated id.
    let stub = StubServer::start(vec![StubResponse::ok(
        r#"{"id":"","template_id":"python","endpoint":"127.0.0.1:9091","fork_time_ms":3.0}"#,
    )]);
    let server = client_for(&stub.base_url);

    let sandbox = server.fork("python").expect("fork");
    assert!(
        sandbox.id.starts_with("sandbox-"),
        "generated id was {:?}",
        sandbox.id
    );
    // "sandbox-" + 8 hex chars.
    assert_eq!(sandbox.id.len(), "sandbox-".len() + 8);

    let req = stub.next_request();
    // The generated id is what was sent on the wire.
    assert!(req.body.contains(&format!("\"id\":\"{}\"", sandbox.id)));
}

#[test]
fn fork_rejects_invalid_id_before_any_request() {
    // No canned responses: if the SDK sent a request, the stub would have none
    // to serve and the test would still pass via the early-return path, so we
    // assert no request was captured.
    let stub = StubServer::start(vec![]);
    let server = client_for(&stub.base_url);

    let err = server
        .fork_as("python", "bad id with spaces")
        .expect_err("invalid id must be rejected");
    assert_eq!(err.code, "invalid_sandbox_id");
    assert_eq!(err.status, 0);

    // Nothing should have reached the stub.
    assert!(stub
        .captured
        .recv_timeout(std::time::Duration::from_millis(200))
        .is_err());
}

#[test]
fn fork_rejects_path_traversal_id() {
    let stub = StubServer::start(vec![]);
    let server = client_for(&stub.base_url);
    let err = server
        .fork_as("python", "../escape")
        .expect_err("must reject");
    assert_eq!(err.code, "invalid_sandbox_id");
    drop(stub);
}

#[test]
fn exec_round_trips_stdout_and_exit_code() {
    let stub = StubServer::start(vec![
        StubResponse::ok(
            r#"{"id":"box","template_id":"python","endpoint":"e","fork_time_ms":1.0}"#,
        ),
        StubResponse::ok(r#"{"exit_code":0,"stdout":"hello\n","stderr":"","exec_time_ms":4.2}"#),
    ]);
    let server = client_for(&stub.base_url);

    let sandbox = server.fork_as("python", "box").expect("fork");
    let _fork_req = stub.next_request();

    let result = sandbox.exec("echo hello").expect("exec");
    assert_eq!(result.exit_code, 0);
    assert_eq!(result.stdout, "hello\n");
    assert!(result.success());

    let req = stub.next_request();
    assert_eq!(req.method, "POST");
    assert_eq!(req.path, "/v1/exec");
    assert!(req.body.contains("\"sandbox\":\"box\""));
    assert!(req.body.contains("\"command\":\"echo hello\""));
    assert!(req.body.contains("\"timeout\":30"));
}

#[test]
fn terminate_issues_delete() {
    let stub = StubServer::start(vec![
        StubResponse::ok(
            r#"{"id":"box","template_id":"python","endpoint":"e","fork_time_ms":1.0}"#,
        ),
        StubResponse::status(200, "OK", ""),
    ]);
    let server = client_for(&stub.base_url);

    let sandbox = server.fork_as("python", "box").expect("fork");
    let _fork_req = stub.next_request();

    sandbox.terminate().expect("terminate");

    let req = stub.next_request();
    assert_eq!(req.method, "DELETE");
    assert_eq!(req.path, "/v1/sandboxes/box");
}

// The auth and base-URL resolution tests mutate process-global environment
// variables (MITOS_API_KEY, MITOS_BASE_URL, MITOS_CONFIG_DIR). Cargo runs tests
// in parallel threads of one process, so they are folded into a single
// sequential test guarded by a static mutex to keep them from racing with each
// other and with any other env access. The mutex also makes the snapshot /
// restore around the whole block atomic.
static ENV_LOCK: Mutex<()> = Mutex::new(());

#[test]
fn auth_and_base_url_resolution() {
    let _guard = ENV_LOCK.lock().unwrap_or_else(|p| p.into_inner());

    let prev_base = std::env::var("MITOS_BASE_URL").ok();
    let prev_key = std::env::var("MITOS_API_KEY").ok();
    let prev_cfg = std::env::var("MITOS_CONFIG_DIR").ok();

    let tmp = std::env::temp_dir().join(format!("mitos-rust-auth-{}", std::process::id()));
    std::fs::create_dir_all(&tmp).unwrap();

    // 1) Base URL defaults to the hosted endpoint when nothing is set, and an
    //    empty (credential-less) config dir means tokenless. Point the credfile
    //    lookup at the empty temp dir so no real credential file interferes.
    std::env::remove_var("MITOS_BASE_URL");
    std::env::remove_var("MITOS_API_KEY");
    std::env::set_var("MITOS_CONFIG_DIR", &tmp);
    let server = SandboxServer::new();
    assert_eq!(server.url(), "https://mitos.run");

    // 2) The bearer falls back to the credential file's "token" field.
    std::fs::write(
        tmp.join("credentials.json"),
        r#"{"token":"sk-from-file","email":"a@b.c","default_org":"o"}"#,
    )
    .unwrap();
    {
        let stub = StubServer::start(vec![StubResponse::ok("[]")]);
        let server = SandboxServer::builder().base_url(&stub.base_url).build();
        let _ = server.list_templates().expect("list");
        assert_eq!(
            stub.next_request().header("Authorization"),
            Some("Bearer sk-from-file")
        );
    }

    // 3) Precedence: arg > env > file. Set env and keep the file present.
    std::env::set_var("MITOS_API_KEY", "sk-env");
    {
        // Explicit arg wins over both env and file.
        let stub = StubServer::start(vec![StubResponse::ok("[]")]);
        let server = SandboxServer::builder()
            .base_url(&stub.base_url)
            .api_key("sk-arg")
            .build();
        let _ = server.list_templates().expect("list");
        assert_eq!(
            stub.next_request().header("Authorization"),
            Some("Bearer sk-arg")
        );
    }
    {
        // With no arg, env wins over the file.
        let stub = StubServer::start(vec![StubResponse::ok("[]")]);
        let server = SandboxServer::builder().base_url(&stub.base_url).build();
        let _ = server.list_templates().expect("list");
        assert_eq!(
            stub.next_request().header("Authorization"),
            Some("Bearer sk-env")
        );
    }

    // 4) MITOS_BASE_URL overrides the default when no arg is given.
    std::env::set_var("MITOS_BASE_URL", "http://example.invalid:1234/");
    let server = SandboxServer::new();
    // Trailing slash is trimmed.
    assert_eq!(server.url(), "http://example.invalid:1234");

    restore("MITOS_BASE_URL", prev_base);
    restore("MITOS_API_KEY", prev_key);
    restore("MITOS_CONFIG_DIR", prev_cfg);
    let _ = std::fs::remove_dir_all(&tmp);
}

#[test]
fn non_2xx_envelope_yields_typed_error() {
    let stub = StubServer::start(vec![StubResponse::status(
        404,
        "Not Found",
        r#"{"error":{"code":"not_found","message":"sandbox not found","cause":"no such sandbox box","remediation":"Confirm the sandbox id exists and is Ready before calling."}}"#,
    )]);
    let server = client_for(&stub.base_url);

    let err = server.list_sandboxes().expect_err("404 must be an error");
    assert_eq!(err.code, "not_found");
    assert_eq!(err.status, 404);
    assert_eq!(err.message, "sandbox not found");
    assert_eq!(err.cause, "no such sandbox box");
    drop(stub);
}

#[test]
fn non_2xx_without_envelope_falls_back_to_status_code() {
    let stub = StubServer::start(vec![StubResponse::status(
        503,
        "Service Unavailable",
        "upstream down",
    )]);
    let server = client_for(&stub.base_url);
    let err = server.list_sandboxes().expect_err("503 must be an error");
    assert_eq!(err.code, "unavailable");
    assert_eq!(err.status, 503);
    drop(stub);
}

#[test]
fn api_key_value_never_appears_in_error_string() {
    // The server echoes the token in the body; the SDK must redact it before it
    // becomes the error cause, and it must never appear in the Display string.
    let secret = "sk-super-secret-value";
    let body = format!(
        r#"{{"error":{{"code":"unauthorized","message":"bad token {secret}","cause":"token {secret} rejected","remediation":"rotate {secret}"}}}}"#
    );
    let stub = StubServer::start(vec![StubResponse::status(401, "Unauthorized", &body)]);
    let server = SandboxServer::builder()
        .base_url(&stub.base_url)
        .api_key(secret)
        .build();

    let err: MitosError = server.list_sandboxes().expect_err("401 must be an error");
    assert_eq!(err.code, "unauthorized");
    let rendered = format!("{err}");
    assert!(
        !rendered.contains(secret),
        "the api key leaked into the error: {rendered}"
    );
    assert!(!err.cause.contains(secret));
    assert!(!err.message.contains(secret));
    assert!(!err.remediation.contains(secret));
    drop(stub);
}

/// Restores an environment variable to its prior value or removes it.
fn restore(key: &str, prev: Option<String>) {
    match prev {
        Some(v) => std::env::set_var(key, v),
        None => std::env::remove_var(key),
    }
}
