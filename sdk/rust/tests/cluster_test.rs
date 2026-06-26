//! Integration tests for cluster mode ([`mitos::AgentRun`]) against an
//! in-process mock Kubernetes API server. Like the direct-mode `server_test.rs`,
//! the mock is a hand-rolled HTTP/1.1 listener on a loopback ephemeral port, so
//! no real cluster and no network access are needed. It serves one canned
//! response per incoming request from a queue and records every request for
//! assertions on the path, method, and body.

use std::io::{BufRead, BufReader, Read, Write};
use std::net::{TcpListener, TcpStream};
use std::sync::mpsc::{self, Receiver};
use std::sync::{Arc, Mutex};
use std::thread;

use mitos::{default_pool_name, AgentRun, CreateSandbox, SandboxPhase, ServeOptions};

#[derive(Debug, Clone)]
struct CapturedRequest {
    method: String,
    path: String,
    body: String,
}

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

struct MockApiServer {
    base_url: String,
    captured: Receiver<CapturedRequest>,
    handle: Option<thread::JoinHandle<()>>,
}

impl MockApiServer {
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

        MockApiServer {
            base_url,
            captured: rx,
            handle: Some(handle),
        }
    }

    fn next_request(&self) -> CapturedRequest {
        self.captured
            .recv_timeout(std::time::Duration::from_secs(5))
            .expect("a request should have reached the mock API server")
    }
}

impl Drop for MockApiServer {
    fn drop(&mut self) {
        let _ = TcpStream::connect(self.base_url.trim_start_matches("http://"));
        if let Some(h) = self.handle.take() {
            let _ = h.join();
        }
    }
}

fn handle_connection(stream: TcpStream, resp: &StubResponse) -> Option<CapturedRequest> {
    let mut reader = BufReader::new(stream.try_clone().expect("clone stream"));

    let mut request_line = String::new();
    if reader.read_line(&mut request_line).ok()? == 0 {
        return None;
    }
    let mut parts = request_line.split_whitespace();
    let method = parts.next()?.to_string();
    let path = parts.next()?.to_string();

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
            if k.trim().eq_ignore_ascii_case("content-length") {
                content_length = v.trim().parse().unwrap_or(0);
            }
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
        body: String::from_utf8_lossy(&body).to_string(),
    })
}

#[test]
fn default_pool_name_matches_python_byte_for_byte() {
    assert_eq!(
        default_pool_name("python:3.12"),
        "mitos-default-python-3.12"
    );
    assert_eq!(
        default_pool_name("ghcr.io/Acme/Foo-Bar:latest"),
        "mitos-default-ghcr.io-acme-foo-bar-latest"
    );
    assert_eq!(default_pool_name("busybox"), "mitos-default-busybox");
    assert_eq!(
        default_pool_name("UPPER/Case:TAG"),
        "mitos-default-upper-case-tag"
    );
    assert_eq!(
        default_pool_name(&("a".repeat(60) + ":x")),
        format!("mitos-default-{}", "a".repeat(40))
    );
    assert_eq!(
        default_pool_name("registry.io/x@sha256:abc"),
        "mitos-default-registry.io-x-sha256-abc"
    );
    assert_eq!(default_pool_name("node_18"), "mitos-default-node-18");
}

#[test]
fn sandbox_get_or_creates_pool_then_creates_sandbox() {
    // sandbox("python") on a missing pool: GET pool (404), POST pool, POST
    // sandbox. The pool is named mitos-default-python.
    let mock = MockApiServer::start(vec![
        StubResponse::status(
            404,
            "Not Found",
            r#"{"kind":"Status","reason":"NotFound","message":"sandboxpools.mitos.run \"mitos-default-python\" not found"}"#,
        ),
        StubResponse::status(
            201,
            "Created",
            r#"{"metadata":{"name":"mitos-default-python"}}"#,
        ),
        StubResponse::status(
            201,
            "Created",
            r#"{"metadata":{"name":"sandbox-abcd1234"}}"#,
        ),
    ]);
    let client = AgentRun::for_testing(&mock.base_url, "default");

    let sandbox = client.sandbox("python").expect("sandbox()");
    assert_eq!(sandbox.pool, "mitos-default-python");
    assert_eq!(sandbox.phase(), SandboxPhase::Pending);

    // 1) GET the default pool.
    let get_pool = mock.next_request();
    assert_eq!(get_pool.method, "GET");
    assert_eq!(
        get_pool.path,
        "/apis/mitos.run/v1/namespaces/default/sandboxpools/mitos-default-python"
    );

    // 2) POST the pool with an inline template image and replicas: 1.
    let post_pool = mock.next_request();
    assert_eq!(post_pool.method, "POST");
    assert_eq!(
        post_pool.path,
        "/apis/mitos.run/v1/namespaces/default/sandboxpools"
    );
    assert!(post_pool.body.contains("\"kind\":\"SandboxPool\""));
    assert!(post_pool.body.contains("\"image\":\"python\""));
    assert!(post_pool.body.contains("\"replicas\":1"));

    // 3) POST the sandbox with the poolRef.
    let post_sandbox = mock.next_request();
    assert_eq!(post_sandbox.method, "POST");
    assert_eq!(
        post_sandbox.path,
        "/apis/mitos.run/v1/namespaces/default/sandboxes"
    );
    assert!(post_sandbox.body.contains("\"kind\":\"Sandbox\""));
    assert!(post_sandbox
        .body
        .contains("\"poolRef\":{\"name\":\"mitos-default-python\"}"));
}

#[test]
fn sandbox_reuses_existing_pool() {
    // A pre-existing pool with a matching inline image is reused untouched: GET
    // returns 200, so no POST pool happens, only the POST sandbox.
    let mock = MockApiServer::start(vec![
        StubResponse::ok(
            r#"{"metadata":{"name":"mitos-default-python"},"spec":{"template":{"image":"python"},"replicas":1}}"#,
        ),
        StubResponse::status(201, "Created", r#"{"metadata":{"name":"sandbox-xyz"}}"#),
    ]);
    let client = AgentRun::for_testing(&mock.base_url, "default");

    let sandbox = client.sandbox("python").expect("sandbox()");
    assert_eq!(sandbox.pool, "mitos-default-python");

    let get_pool = mock.next_request();
    assert_eq!(get_pool.method, "GET");
    let post_sandbox = mock.next_request();
    assert_eq!(post_sandbox.method, "POST");
    assert_eq!(
        post_sandbox.path,
        "/apis/mitos.run/v1/namespaces/default/sandboxes"
    );
}

#[test]
fn sandbox_rejects_pool_image_mismatch() {
    // A slug collision: the existing pool runs a different image. Reuse fails
    // closed with pool_image_mismatch and no sandbox is created.
    let mock = MockApiServer::start(vec![StubResponse::ok(
        r#"{"metadata":{"name":"mitos-default-python-3.11"},"spec":{"template":{"image":"python:3.11"}}}"#,
    )]);
    let client = AgentRun::for_testing(&mock.base_url, "default");

    let err = client
        .sandbox("python-3.11")
        .expect_err("mismatched image must be rejected");
    assert_eq!(err.code, "pool_image_mismatch");
    let _ = mock.next_request(); // the GET happened
}

#[test]
fn create_builds_sandbox_with_poolref_env_secret_ttl_and_workspace() {
    let mock = MockApiServer::start(vec![StubResponse::status(
        201,
        "Created",
        r#"{"metadata":{"name":"my-box"}}"#,
    )]);
    let client = AgentRun::for_testing(&mock.base_url, "team-a");

    let opts = CreateSandbox::new()
        .name("my-box")
        .env("FOO", "bar")
        .secret("TOKEN", "creds", "token")
        .ttl("30m")
        .workspace("ws1");
    let sandbox = client.create("prod-pool", opts).expect("create");
    assert_eq!(sandbox.name, "my-box");
    assert_eq!(sandbox.pool, "prod-pool");

    let req = mock.next_request();
    assert_eq!(req.method, "POST");
    assert_eq!(req.path, "/apis/mitos.run/v1/namespaces/team-a/sandboxes");
    assert!(req.body.contains("\"poolRef\":{\"name\":\"prod-pool\"}"));
    assert!(req.body.contains("\"name\":\"FOO\",\"value\":\"bar\""));
    // The secret entry carries the env var name and the secretRef name/key.
    assert!(req.body.contains("\"secretRef\""));
    assert!(req.body.contains("\"creds\""));
    assert!(req.body.contains("\"key\":\"token\""));
    assert!(req.body.contains("\"envVar\":\"TOKEN\""));
    assert!(req.body.contains("\"ttl\":\"30m\""));
    assert!(req.body.contains("\"workspaceRef\":{\"name\":\"ws1\"}"));
}

#[test]
fn create_tolerates_409_is_not_used_but_pool_create_409_is() {
    // ensure_default_pool tolerates a 409 on the pool POST (a concurrent
    // creator won the race): GET 404, POST pool 409, POST sandbox 201.
    let mock = MockApiServer::start(vec![
        StubResponse::status(
            404,
            "Not Found",
            r#"{"kind":"Status","reason":"NotFound","message":"not found"}"#,
        ),
        StubResponse::status(
            409,
            "Conflict",
            r#"{"kind":"Status","reason":"AlreadyExists","message":"sandboxpools.mitos.run \"mitos-default-busybox\" already exists"}"#,
        ),
        StubResponse::status(201, "Created", r#"{"metadata":{"name":"sandbox-1"}}"#),
    ]);
    let client = AgentRun::for_testing(&mock.base_url, "default");

    let sandbox = client
        .sandbox("busybox")
        .expect("409 on pool create is tolerated");
    assert_eq!(sandbox.pool, "mitos-default-busybox");

    let _get = mock.next_request();
    let post_pool = mock.next_request();
    assert_eq!(post_pool.method, "POST");
    let post_sandbox = mock.next_request();
    assert_eq!(post_sandbox.method, "POST");
    assert_eq!(
        post_sandbox.path,
        "/apis/mitos.run/v1/namespaces/default/sandboxes"
    );
}

#[test]
fn get_reads_poolref_and_phase() {
    // A Ready sandbox: get() reads the poolRef and phase, then loads the token
    // Secret (which we 404 to keep it tokenless without error).
    let mock = MockApiServer::start(vec![
        StubResponse::ok(
            r#"{"metadata":{"name":"box1"},"spec":{"source":{"poolRef":{"name":"p1"}}},"status":{"phase":"Ready","endpoint":"10.0.0.1:9091"}}"#,
        ),
        StubResponse::status(
            404,
            "Not Found",
            r#"{"kind":"Status","reason":"NotFound","message":"secrets \"box1-sandbox-token\" not found"}"#,
        ),
    ]);
    let client = AgentRun::for_testing(&mock.base_url, "default");

    let sandbox = client.get("box1").expect("get");
    assert_eq!(sandbox.pool, "p1");
    assert_eq!(sandbox.phase(), SandboxPhase::Ready);
    assert_eq!(sandbox.endpoint(), Some("10.0.0.1:9091"));

    let get_sb = mock.next_request();
    assert_eq!(get_sb.method, "GET");
    assert_eq!(
        get_sb.path,
        "/apis/mitos.run/v1/namespaces/default/sandboxes/box1"
    );
    // A Ready sandbox triggers a token Secret read.
    let get_secret = mock.next_request();
    assert_eq!(get_secret.method, "GET");
    assert_eq!(
        get_secret.path,
        "/api/v1/namespaces/default/secrets/box1-sandbox-token"
    );
}

#[test]
fn list_filters_by_pool_and_reads_poolref() {
    let mock = MockApiServer::start(vec![StubResponse::ok(
        r#"{"items":[
            {"metadata":{"name":"a"},"spec":{"source":{"poolRef":{"name":"p1"}}},"status":{"phase":"Pending"}},
            {"metadata":{"name":"b"},"spec":{"source":{"poolRef":{"name":"p2"}}},"status":{"phase":"Pending"}}
        ]}"#,
    )]);
    let client = AgentRun::for_testing(&mock.base_url, "default");

    let all = client.list(None).expect("list all");
    assert_eq!(all.len(), 2);
    assert_eq!(all[0].pool, "p1");
    assert_eq!(all[1].pool, "p2");

    let req = mock.next_request();
    assert_eq!(req.method, "GET");
    assert_eq!(req.path, "/apis/mitos.run/v1/namespaces/default/sandboxes");

    // Now filter by pool: only the p2 sandbox should remain.
    let mock2 = MockApiServer::start(vec![StubResponse::ok(
        r#"{"items":[
            {"metadata":{"name":"a"},"spec":{"source":{"poolRef":{"name":"p1"}}},"status":{"phase":"Pending"}},
            {"metadata":{"name":"b"},"spec":{"source":{"poolRef":{"name":"p2"}}},"status":{"phase":"Pending"}}
        ]}"#,
    )]);
    let client2 = AgentRun::for_testing(&mock2.base_url, "default");
    let filtered = client2.list(Some("p2")).expect("list filtered");
    assert_eq!(filtered.len(), 1);
    assert_eq!(filtered[0].name, "b");
    let _ = mock2.next_request();
}

#[test]
fn pool_status_reads_status_fields() {
    let mock = MockApiServer::start(vec![StubResponse::ok(
        r#"{"metadata":{"name":"p1"},"spec":{"replicas":5},"status":{"readySnapshots":3,"nodeDistribution":{"node-a":2,"node-b":1}}}"#,
    )]);
    let client = AgentRun::for_testing(&mock.base_url, "default");

    let status = client.pool_status("p1").expect("pool_status");
    assert_eq!(status.name, "p1");
    assert_eq!(status.ready_snapshots, 3);
    assert_eq!(status.desired, 5);
    let mut dist = status.node_distribution.clone();
    dist.sort();
    assert_eq!(
        dist,
        vec![("node-a".to_string(), 2), ("node-b".to_string(), 1)]
    );

    let req = mock.next_request();
    assert_eq!(req.method, "GET");
    assert_eq!(
        req.path,
        "/apis/mitos.run/v1/namespaces/default/sandboxpools/p1"
    );
}

#[test]
fn from_name_is_get_alias() {
    let mock = MockApiServer::start(vec![StubResponse::ok(
        r#"{"metadata":{"name":"box9"},"spec":{"source":{"poolRef":{"name":"p9"}}},"status":{"phase":"Pending"}}"#,
    )]);
    let client = AgentRun::for_testing(&mock.base_url, "default");

    let sandbox = client.from_name("box9").expect("from_name");
    assert_eq!(sandbox.pool, "p9");

    let req = mock.next_request();
    assert_eq!(
        req.path,
        "/apis/mitos.run/v1/namespaces/default/sandboxes/box9"
    );
}

#[test]
fn terminate_returns_workspace_ref() {
    // The request order is: create POST, then terminate's GET (to read the
    // workspaceRef) and DELETE.
    let mock = MockApiServer::start(vec![
        StubResponse::status(201, "Created", r#"{"metadata":{"name":"box1"}}"#),
        StubResponse::ok(
            r#"{"metadata":{"name":"box1"},"spec":{"source":{"poolRef":{"name":"p1"}},"workspaceRef":{"name":"ws7"}}}"#,
        ),
        StubResponse::status(200, "OK", r#"{"kind":"Status","status":"Success"}"#),
    ]);
    let client = AgentRun::for_testing(&mock.base_url, "default");

    let mut sandbox = client
        .create("p1", CreateSandbox::new().name("box1"))
        .expect("create");
    let create_req = mock.next_request();
    assert_eq!(create_req.method, "POST");

    let ws = sandbox.terminate().expect("terminate");
    assert_eq!(ws.as_deref(), Some("ws7"));

    let get_req = mock.next_request();
    assert_eq!(get_req.method, "GET");
    let del_req = mock.next_request();
    assert_eq!(del_req.method, "DELETE");
    assert_eq!(
        del_req.path,
        "/apis/mitos.run/v1/namespaces/default/sandboxes/box1"
    );
}

// --- Workspace::serve tests --------------------------------------------------

#[test]
fn serve_creates_sandbox_with_expose_and_returns_url() {
    // Sequence: POST sandbox (201), GET sandbox (Ready).
    let mock = MockApiServer::start(vec![
        StubResponse::status(201, "Created", r#"{"metadata":{"name":"sandbox-aa01"}}"#),
        StubResponse::ok(r#"{"metadata":{"name":"sandbox-aa01"},"status":{"phase":"Ready"}}"#),
    ]);
    let client = AgentRun::for_testing(&mock.base_url, "default");
    let ws = client.workspace("ws1");

    let result = ws
        .serve(
            ServeOptions::new()
                .pool("my-pool")
                .label("mybot")
                .expose_domain("mitos.app"),
        )
        .expect("serve");

    assert_eq!(result.url, "https://mybot.mitos.app/");
    assert_eq!(result.label, "mybot");
    assert_eq!(result.sharing, "private");

    // Verify the POST body contains spec.expose and workspaceRef.
    let post_req = mock.next_request();
    assert_eq!(post_req.method, "POST");
    assert_eq!(
        post_req.path,
        "/apis/mitos.run/v1/namespaces/default/sandboxes"
    );
    assert!(post_req
        .body
        .contains("\"workspaceRef\":{\"name\":\"ws1\"}"));
    assert!(post_req.body.contains("\"expose\""));
    assert!(post_req.body.contains("\"label\":\"mybot\""));
    assert!(post_req.body.contains("\"sharing\":\"private\""));
    assert!(post_req.body.contains("\"port\":8080"));
    assert!(post_req.body.contains("\"poolRef\":{\"name\":\"my-pool\"}"));

    // Verify the poll GET happened.
    let get_req = mock.next_request();
    assert_eq!(get_req.method, "GET");
    assert!(get_req
        .path
        .starts_with("/apis/mitos.run/v1/namespaces/default/sandboxes/"));
}

#[test]
fn serve_label_defaults_to_sandbox_name() {
    // No explicit label: the sandbox name (generated) becomes the label.
    let mock = MockApiServer::start(vec![
        StubResponse::status(201, "Created", r#"{"metadata":{"name":"sandbox-bb02"}}"#),
        StubResponse::ok(r#"{"metadata":{"name":"sandbox-bb02"},"status":{"phase":"Ready"}}"#),
    ]);
    let client = AgentRun::for_testing(&mock.base_url, "default");
    let ws = client.workspace("ws2");

    let result = ws
        .serve(ServeOptions::new().pool("p1").expose_domain("mitos.app"))
        .expect("serve without explicit label");

    // The URL label must match the returned label, and the label must look like
    // a sandbox- name (which is a valid DNS label: all lowercase hex).
    assert!(result.url.starts_with("https://sandbox-"));
    assert!(result.url.ends_with(".mitos.app/"));
    assert_eq!(result.label, result.sandbox_name);

    let post_req = mock.next_request();
    // The body label must match what the URL uses.
    let label_fragment = format!("\"label\":\"{}\"", result.label);
    assert!(
        post_req.body.contains(&label_fragment),
        "expected {:?} in body {:?}",
        label_fragment,
        post_req.body
    );
    let _ = mock.next_request();
}

#[test]
fn serve_respects_port_and_sharing_options() {
    let mock = MockApiServer::start(vec![
        StubResponse::status(201, "Created", r#"{"metadata":{"name":"sandbox-cc03"}}"#),
        StubResponse::ok(r#"{"metadata":{"name":"sandbox-cc03"},"status":{"phase":"Ready"}}"#),
    ]);
    let client = AgentRun::for_testing(&mock.base_url, "default");
    let ws = client.workspace("ws3");

    let result = ws
        .serve(
            ServeOptions::new()
                .pool("p1")
                .label("toolbot")
                .port(3000)
                .sharing("link")
                .expose_domain("mitos.app"),
        )
        .expect("serve with custom port and sharing");

    assert_eq!(result.sharing, "link");
    assert_eq!(result.url, "https://toolbot.mitos.app/");

    let post_req = mock.next_request();
    assert!(post_req.body.contains("\"port\":3000"));
    assert!(post_req.body.contains("\"sharing\":\"link\""));
    let _ = mock.next_request();
}

#[test]
fn serve_polls_until_ready() {
    // First GET returns Pending, second returns Ready.
    let mock = MockApiServer::start(vec![
        StubResponse::status(201, "Created", r#"{"metadata":{"name":"sandbox-dd04"}}"#),
        StubResponse::ok(r#"{"metadata":{"name":"sandbox-dd04"},"status":{"phase":"Pending"}}"#),
        StubResponse::ok(r#"{"metadata":{"name":"sandbox-dd04"},"status":{"phase":"Ready"}}"#),
    ]);
    let client = AgentRun::for_testing(&mock.base_url, "default");
    let ws = client.workspace("ws4");

    let result = ws
        .serve(
            ServeOptions::new()
                .pool("p1")
                .label("polltest")
                .expose_domain("mitos.app"),
        )
        .expect("serve with polling");

    assert_eq!(result.url, "https://polltest.mitos.app/");

    let post_req = mock.next_request();
    assert_eq!(post_req.method, "POST");
    let get1 = mock.next_request();
    assert_eq!(get1.method, "GET"); // Pending
    let get2 = mock.next_request();
    assert_eq!(get2.method, "GET"); // Ready
}

#[test]
fn serve_returns_error_on_failed_sandbox() {
    // GET returns Failed: serve must surface sandbox_failed.
    let mock = MockApiServer::start(vec![
        StubResponse::status(201, "Created", r#"{"metadata":{"name":"sandbox-ee05"}}"#),
        StubResponse::ok(r#"{"metadata":{"name":"sandbox-ee05"},"status":{"phase":"Failed"}}"#),
    ]);
    let client = AgentRun::for_testing(&mock.base_url, "default");
    let ws = client.workspace("ws5");

    let err = ws
        .serve(
            ServeOptions::new()
                .pool("p1")
                .label("failbot")
                .expose_domain("mitos.app"),
        )
        .expect_err("should surface sandbox_failed");

    assert_eq!(err.code, "sandbox_failed");
    let _ = mock.next_request();
    let _ = mock.next_request();
}

#[test]
fn serve_error_missing_pool() {
    let client = AgentRun::for_testing("http://127.0.0.1:1", "default");
    let ws = client.workspace("ws6");

    let err = ws
        .serve(ServeOptions::new().expose_domain("mitos.app"))
        .expect_err("pool is required");
    assert_eq!(err.code, "missing_serve_pool");
}

#[test]
fn serve_error_missing_expose_domain() {
    // Clear the env var so the fallback also fails.
    std::env::remove_var("MITOS_EXPOSE_DOMAIN");
    let client = AgentRun::for_testing("http://127.0.0.1:1", "default");
    let ws = client.workspace("ws7");

    let err = ws
        .serve(ServeOptions::new().pool("p1").label("mybot"))
        .expect_err("expose domain is required");
    assert_eq!(err.code, "missing_expose_domain");
}

#[test]
fn serve_error_invalid_port_zero() {
    let client = AgentRun::for_testing("http://127.0.0.1:1", "default");
    let ws = client.workspace("ws8");

    let err = ws
        .serve(
            ServeOptions::new()
                .pool("p1")
                .label("mybot")
                .port(0)
                .expose_domain("mitos.app"),
        )
        .expect_err("port 0 is invalid");
    assert_eq!(err.code, "invalid_serve_port");
}

#[test]
fn serve_error_reserved_label() {
    let client = AgentRun::for_testing("http://127.0.0.1:1", "default");
    let ws = client.workspace("ws9");

    for reserved in &["www", "api", "console", "admin", "status"] {
        let err = ws
            .serve(
                ServeOptions::new()
                    .pool("p1")
                    .label(*reserved)
                    .expose_domain("mitos.app"),
            )
            .expect_err(&format!("reserved label {reserved:?} must be rejected"));
        assert_eq!(
            err.code, "reserved_expose_label",
            "expected reserved_expose_label for {:?}",
            reserved
        );
    }
}

#[test]
fn serve_error_invalid_label_hyphen_start() {
    let client = AgentRun::for_testing("http://127.0.0.1:1", "default");
    let ws = client.workspace("ws10");

    let err = ws
        .serve(
            ServeOptions::new()
                .pool("p1")
                .label("-badlabel")
                .expose_domain("mitos.app"),
        )
        .expect_err("label starting with hyphen is invalid");
    assert_eq!(err.code, "invalid_expose_label");
}

#[test]
fn serve_label_is_lowercased() {
    // An uppercase label must be lowercased before validation and use.
    let mock = MockApiServer::start(vec![
        StubResponse::status(201, "Created", r#"{"metadata":{"name":"sandbox-ff06"}}"#),
        StubResponse::ok(r#"{"metadata":{"name":"sandbox-ff06"},"status":{"phase":"Ready"}}"#),
    ]);
    let client = AgentRun::for_testing(&mock.base_url, "default");
    let ws = client.workspace("ws11");

    let result = ws
        .serve(
            ServeOptions::new()
                .pool("p1")
                .label("MyBot")
                .expose_domain("mitos.app"),
        )
        .expect("uppercase label is lowercased");

    assert_eq!(result.label, "mybot");
    assert_eq!(result.url, "https://mybot.mitos.app/");
    let _ = mock.next_request();
    let _ = mock.next_request();
}

#[test]
fn serve_error_label_too_long() {
    let client = AgentRun::for_testing("http://127.0.0.1:1", "default");
    let ws = client.workspace("ws12");
    let long_label: String = "a".repeat(64);

    let err = ws
        .serve(
            ServeOptions::new()
                .pool("p1")
                .label(long_label)
                .expose_domain("mitos.app"),
        )
        .expect_err("label exceeding 63 chars is invalid");
    assert_eq!(err.code, "invalid_expose_label");
}

#[test]
fn serve_env_var_expose_domain_fallback() {
    // When expose_domain is not passed as an option, MITOS_EXPOSE_DOMAIN is used.
    let mock = MockApiServer::start(vec![
        StubResponse::status(201, "Created", r#"{"metadata":{"name":"sandbox-gg07"}}"#),
        StubResponse::ok(r#"{"metadata":{"name":"sandbox-gg07"},"status":{"phase":"Ready"}}"#),
    ]);
    let client = AgentRun::for_testing(&mock.base_url, "default");
    let ws = client.workspace("ws13");

    std::env::set_var("MITOS_EXPOSE_DOMAIN", "env.example.com");
    let result = ws
        .serve(ServeOptions::new().pool("p1").label("envbot"))
        .expect("serve with env-var domain");
    std::env::remove_var("MITOS_EXPOSE_DOMAIN");

    assert_eq!(result.url, "https://envbot.env.example.com/");
    let _ = mock.next_request();
    let _ = mock.next_request();
}
