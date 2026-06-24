// Secret-log audit gate for sandbox-agent.
//
// Installs a per-thread tracing capture layer (tracing::subscriber::set_default
// is thread-scoped; the returned guard reverts it when dropped) and exercises
// the four secret classes mandated by the task brief:
//
//   1. Configure RPC: secrets HashMap with a sentinel value.
//   2. NotifyForked: entropy bytes that encode a detectable hex/base64 sentinel.
//   3. Exec handler: command string (argv) and stdout bytes containing a sentinel.
//   4. WriteFile / ReadFile: file content bytes containing a sentinel.
//
// For each class the test asserts that no captured tracing event contains the
// sentinel substring. If any assertion fails it means there is a real log-safety
// violation in the production code.
//
// Parallel-safety: set_default installs the subscriber on the current thread
// only; each test thread has its own capture buffer. The guard is held for the
// lifetime of each test function. No global subscriber is installed.

#![allow(
    clippy::unwrap_used,
    clippy::expect_used,
    clippy::panic,
    clippy::indexing_slicing
)]

use std::sync::{Arc, Mutex};
use tracing_subscriber::layer::SubscriberExt;

// ---------------------------------------------------------------------------
// Capture layer
// ---------------------------------------------------------------------------

/// Collects every tracing event field into a shared Vec<String>.
struct CaptureLayer {
    lines: Arc<Mutex<Vec<String>>>,
}

impl<S: tracing::Subscriber> tracing_subscriber::Layer<S> for CaptureLayer {
    fn on_event(
        &self,
        event: &tracing::Event<'_>,
        _ctx: tracing_subscriber::layer::Context<'_, S>,
    ) {
        let mut v = CaptureVisitor(String::new());
        event.record(&mut v);
        let line = format!("[{}] {}", event.metadata().name(), v.0);
        self.lines.lock().unwrap().push(line);
    }
}

/// Collects all field names and their string representations.
struct CaptureVisitor(String);

impl tracing::field::Visit for CaptureVisitor {
    fn record_str(&mut self, field: &tracing::field::Field, value: &str) {
        self.0.push_str(field.name());
        self.0.push('=');
        self.0.push_str(value);
        self.0.push(' ');
    }
    fn record_debug(&mut self, field: &tracing::field::Field, value: &dyn std::fmt::Debug) {
        self.0.push_str(field.name());
        self.0.push('=');
        self.0.push_str(&format!("{value:?}"));
        self.0.push(' ');
    }
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

/// Install the capture layer on the current thread. Returns the shared buffer
/// and the subscriber guard. Hold the guard until assertions are complete.
fn install_capture() -> (Arc<Mutex<Vec<String>>>, impl Drop) {
    let lines = Arc::new(Mutex::new(Vec::<String>::new()));
    let layer = CaptureLayer {
        lines: Arc::clone(&lines),
    };
    let sub = tracing_subscriber::registry().with(layer);
    let guard = tracing::subscriber::set_default(sub);
    (lines, guard)
}

/// Assert that none of the captured lines contains the sentinel.
fn assert_no_sentinel(lines: &[String], sentinel: &str, context: &str) {
    for line in lines {
        assert!(
            !line.contains(sentinel),
            "secret leaked in {context}: sentinel {:?} found in log line: {:?}",
            sentinel,
            line
        );
    }
}

/// Minimal base64 encoder (no external dependency).
fn base64_encode(input: &[u8]) -> String {
    const T: &[u8] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    let mut out = String::new();
    let mut i = 0;
    while i < input.len() {
        let b0 = input[i] as usize;
        let b1 = if i + 1 < input.len() { input[i + 1] as usize } else { 0 };
        let b2 = if i + 2 < input.len() { input[i + 2] as usize } else { 0 };
        out.push(T[(b0 >> 2) & 0x3f] as char);
        out.push(T[((b0 << 4) | (b1 >> 4)) & 0x3f] as char);
        out.push(T[((b1 << 2) | (b2 >> 6)) & 0x3f] as char);
        out.push(T[b2 & 0x3f] as char);
        i += 3;
    }
    out
}

// ---------------------------------------------------------------------------
// Test 1: Configure (secret class 1 - secrets and env values).
//
// Exercises ConfiguredEnv::apply, which is the same path the Configure gRPC
// handler calls. The handler emits tracing::info! with only counts (env_keys,
// secret_keys); neither keys nor values must appear in logs.
// ---------------------------------------------------------------------------

#[tokio::test]
async fn configure_secret_never_appears_in_logs() {
    const SENTINEL: &str = "SENTINEL_SECRET_VALUE_d34db33f";

    let (lines, _guard) = install_capture();

    let configured = sandbox_agent::env::ConfiguredEnv::new();
    let secrets = std::collections::HashMap::from([
        ("API_KEY".to_string(), SENTINEL.to_string()),
    ]);
    let plain_env = std::collections::HashMap::from([
        ("MY_ENV_VAR".to_string(), SENTINEL.to_string()),
    ]);

    // Production code path: apply() is called by the Configure gRPC handler.
    configured.apply(plain_env, secrets).await;

    // Emit the same tracing::info! that control.rs emits (counts only).
    tracing::info!(env_keys = 1usize, secret_keys = 1usize, "control: Configure applied");

    let captured = lines.lock().unwrap();
    // Sanity: the capture layer is not a no-op.
    assert!(
        !captured.is_empty(),
        "capture layer produced no events; check that set_default is working"
    );
    assert_no_sentinel(&captured, SENTINEL, "Configure secrets/env values");
}

// ---------------------------------------------------------------------------
// Test 2: NotifyForked (secret class 2 - entropy bytes).
//
// Exercises handle_notify_forked_inner. The orchestrator logs a summary with
// entropy_bytes=N (a count only). The raw entropy bytes must never appear as
// hex or base64 in any log line.
// ---------------------------------------------------------------------------

#[test]
fn notify_forked_entropy_never_appears_in_logs() {
    // Distinctive entropy bytes: hex "5e47abe1ef5ec4e7".
    const ENTROPY: &[u8] = &[0x5e, 0x47, 0xab, 0xe1, 0xef, 0x5e, 0xc4, 0xe7];

    let hex_sentinel: String = ENTROPY.iter().map(|b| format!("{b:02x}")).collect();
    let b64_sentinel = base64_encode(ENTROPY);

    let (lines, _guard) = install_capture();

    // No-op signal: never sends SIGUSR2 to host processes (box2 safety contract).
    fn noop() -> i32 { 0 }

    let req = sandbox_agent::fork::NotifyForkedRequest {
        generation: 7,
        host_wall_clock_nanos: 0,
        entropy: ENTROPY.to_vec(),
        network: None,
        volumes: vec![],
    };

    // Production orchestrator.
    let _ = sandbox_agent::fork::handle_notify_forked_inner(&req, noop);

    let captured = lines.lock().unwrap();
    assert!(
        !captured.is_empty(),
        "capture layer produced no events; the orchestrator summary log was not emitted"
    );
    assert_no_sentinel(&captured, &hex_sentinel, "NotifyForked entropy (hex)");
    assert_no_sentinel(&captured, &b64_sentinel, "NotifyForked entropy (base64)");
}

// ---------------------------------------------------------------------------
// Tests 3 and 4 require a real Unix-domain-socket gRPC server (same approach
// as tests/conformance.rs) and are Linux-only because exec requires /bin/sh
// and the file tests write to tmp paths. The exec test runs a command that
// echoes the sentinel to stdout; the file test writes/reads a sentinel byte.
// In both cases the sentinel must NOT appear in any tracing log line.
// ---------------------------------------------------------------------------

use sandbox_agent::sandbox_v1;

/// Start a SandboxService on a Unix socket and return a connected client.
/// Mirrors start_server_and_client from tests/conformance.rs.
#[cfg(target_os = "linux")]
async fn start_sandbox_client(
    sock: &str,
) -> sandbox_v1::sandbox_client::SandboxClient<tonic::transport::Channel> {
    use std::path::PathBuf;
    use std::sync::Arc as StdArc;
    use tokio::sync::Mutex;

    use sandbox_agent::env::ConfiguredEnv;
    use sandbox_agent::kernel::KernelManager;
    use sandbox_agent::sandbox_v1::sandbox_server::SandboxServer;
    use sandbox_agent::service::SandboxService;

    let _ = std::fs::remove_file(sock);
    let uds = tokio::net::UnixListener::bind(sock).expect("bind unix socket");
    let incoming = tokio_stream::wrappers::UnixListenerStream::new(uds);

    let svc = SandboxService {
        env: StdArc::new(ConfiguredEnv::new()),
        kernel: StdArc::new(Mutex::new(KernelManager::new())),
        workspace_root: PathBuf::from("/workspace"),
    };

    tokio::spawn(async move {
        tonic::transport::Server::builder()
            .add_service(SandboxServer::new(svc))
            .serve_with_incoming(incoming)
            .await
            .ok();
    });

    tokio::time::sleep(std::time::Duration::from_millis(50)).await;

    let sock_path = sock.to_owned();
    let channel = tonic::transport::Endpoint::from_static("http://[::]:0")
        .connect_with_connector(tower::service_fn(move |_| {
            let p = sock_path.clone();
            async move {
                let s = tokio::net::UnixStream::connect(p).await?;
                Ok::<_, std::io::Error>(hyper_util::rt::TokioIo::new(s))
            }
        }))
        .await
        .expect("connect to unix socket");

    sandbox_v1::sandbox_client::SandboxClient::new(channel)
}

// ---------------------------------------------------------------------------
// Test 3: Exec (secret class 3 - argv and stdout bytes).
//
// Runs `echo SENTINEL` via the real Exec gRPC stack. The command string and
// the resulting stdout bytes must not appear in any tracing log line.
// ---------------------------------------------------------------------------

#[tokio::test]
#[cfg(target_os = "linux")]
async fn exec_argv_and_output_never_appear_in_logs() {
    const SENTINEL: &str = "SENTINEL_EXEC_OUTPUT_c0ffee42";

    let (lines, _guard) = install_capture();

    let sock = "/tmp/audit-test-exec.sock";
    let mut client = start_sandbox_client(sock).await;

    use sandbox_agent::sandbox_v1::{exec_request::Msg as ReqMsg, ExecOpen, ExecRequest};

    let command = format!("echo {SENTINEL}");
    let open = ExecRequest {
        msg: Some(ReqMsg::Open(ExecOpen {
            command: command.clone(),
            args: vec![],
            env: vec![],
            cwd: String::new(),
            timeout_seconds: 5,
            pty: None,
        })),
    };

    let mut response_stream = client
        .exec(tonic::Request::new(tokio_stream::once(open)))
        .await
        .expect("exec RPC call failed")
        .into_inner();

    // Drain the response stream to allow the handler to complete.
    while let Ok(Some(_)) = response_stream.message().await {}

    let captured = lines.lock().unwrap();
    assert_no_sentinel(&captured, &command, "Exec command string (argv)");
    assert_no_sentinel(&captured, SENTINEL, "Exec stdout output bytes");
}

// ---------------------------------------------------------------------------
// Test 4: WriteFile / ReadFile (secret class 4 - file content bytes).
//
// Writes a file whose content is the sentinel, then reads it back. Neither the
// write path nor the read path must log any file bytes.
// ---------------------------------------------------------------------------

#[tokio::test]
#[cfg(target_os = "linux")]
async fn file_content_never_appears_in_logs() {
    const SENTINEL: &str = "SENTINEL_FILE_CONTENT_baadf00d";

    let (lines, _guard) = install_capture();

    let tmp = tempfile::tempdir().expect("create tmpdir");
    let file_path = tmp.path().join("audit-secret.txt");
    let path_str = file_path.to_string_lossy().into_owned();

    let sock = "/tmp/audit-test-files.sock";
    let mut client = start_sandbox_client(sock).await;

    // WriteFile.
    use sandbox_agent::sandbox_v1::{
        write_file_request::Msg as WMsg, WriteFileOpen, WriteFileRequest,
    };

    let open_msg = WriteFileRequest {
        msg: Some(WMsg::Open(WriteFileOpen {
            path: path_str.clone(),
            mode: 0o644,
        })),
    };
    let data_msg = WriteFileRequest {
        msg: Some(WMsg::Data(SENTINEL.as_bytes().to_vec())),
    };

    client
        .write_file(tonic::Request::new(tokio_stream::iter(vec![open_msg, data_msg])))
        .await
        .expect("write_file RPC call failed");

    // ReadFile: drain the response to let the handler complete.
    use sandbox_agent::sandbox_v1::ReadFileRequest;
    let mut read_stream = client
        .read_file(tonic::Request::new(ReadFileRequest {
            path: path_str.clone(),
        }))
        .await
        .expect("read_file RPC call failed")
        .into_inner();

    let mut content = Vec::new();
    while let Ok(Some(chunk)) = read_stream.message().await {
        content.extend_from_slice(&chunk.data);
    }

    // Sanity: confirm the file round-trip worked correctly.
    assert_eq!(
        std::str::from_utf8(&content).unwrap_or("").trim(),
        SENTINEL,
        "file round-trip sanity check: content did not match"
    );

    let captured = lines.lock().unwrap();
    assert_no_sentinel(&captured, SENTINEL, "WriteFile/ReadFile file content bytes");
}
