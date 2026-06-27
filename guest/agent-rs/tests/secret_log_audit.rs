// Secret-log audit gate for sandbox-agent.
//
// Design: a GLOBAL subscriber captures log events from ALL threads (including
// tokio worker threads). Because set_global_default can only be called once per
// process, a single OnceCell installs it on first use. All audit classes share
// one process-wide Arc<Mutex<Vec<String>>> capture buffer and run SEQUENTIALLY
// inside a single #[tokio::test] function protected by a Mutex so parallel
// cargo test runs in other binaries are unaffected (each test binary is its own
// process). The buffer is snapshotted and cleared between classes so each class
// sees only the events it produced.
//
// Positive controls: after every real handler call the test asserts the
// capture buffer is NON-EMPTY and contains an expected non-secret marker
// logged by that handler. An empty buffer is a test-infrastructure failure
// and is treated as a hard failure (the gate cannot give security assurance
// over an empty buffer).
//
// The four secret classes covered:
//   1. Configure RPC: secret values via the REAL ControlService gRPC handler.
//   2. NotifyForked: entropy raw bytes (hex + base64).
//   3. Exec handler: command argv and stdout bytes.
//   4. WriteFile / ReadFile: file content bytes.

#![allow(
    clippy::unwrap_used,
    clippy::expect_used,
    clippy::panic,
    clippy::indexing_slicing,
    // The audit_lock() Mutex guard is intentionally held across .await points
    // to serialize test classes and prevent cross-test buffer contamination.
    // This is a test-only pattern; production code must not hold std::sync::Mutex
    // across await points.
    clippy::await_holding_lock
)]

use std::sync::{Arc, Mutex, OnceLock};

use tracing_subscriber::layer::SubscriberExt;

// ---------------------------------------------------------------------------
// Global capture layer: installs a SINGLE subscriber for the whole process.
// Captures events emitted on ANY thread, including tokio worker threads.
// ---------------------------------------------------------------------------

static GLOBAL_LINES: OnceLock<Arc<Mutex<Vec<String>>>> = OnceLock::new();

/// Retrieve (and lazily install) the global capture buffer.
///
/// On first call this registers the global subscriber. Subsequent calls
/// return the same Arc so all test code shares one buffer.
fn global_lines() -> Arc<Mutex<Vec<String>>> {
    GLOBAL_LINES
        .get_or_init(|| {
            let lines: Arc<Mutex<Vec<String>>> = Arc::new(Mutex::new(Vec::new()));
            let layer = CaptureLayer {
                lines: Arc::clone(&lines),
            };
            // LevelFilter::DEBUG ensures all log levels (debug and above) are
            // captured. Without this explicit filter, registry()'s max_level_hint
            // may default to INFO and silently drop debug-level handler events
            // (e.g. files.rs logs paths at debug level). We stop at DEBUG to
            // avoid capturing trace-level h2 internal events that would pollute
            // the buffer and slow down sentinel scans.
            use tracing_subscriber::filter::LevelFilter;
            let sub = tracing_subscriber::registry()
                .with(LevelFilter::DEBUG)
                .with(layer);
            // set_global_default registers this subscriber for ALL threads.
            // It can be called at most once per process; OnceLock ensures that.
            tracing::subscriber::set_global_default(sub)
                .expect("global tracing subscriber already set");
            lines
        })
        .clone()
}

/// Collect every tracing event field into the shared buffer.
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
// Buffer helpers
// ---------------------------------------------------------------------------

/// Drain and return all captured lines since the last snapshot.
fn take_lines() -> Vec<String> {
    global_lines().lock().unwrap().drain(..).collect()
}

/// Assert the capture is non-empty (positive control: proves we saw events).
fn assert_nonempty(lines: &[String], marker: &str, context: &str) {
    assert!(
        !lines.is_empty(),
        "capture buffer is EMPTY for {context}: global subscriber did not capture \
         any events. This means the audit cannot give security assurance. \
         Expected to see marker: {marker:?}"
    );
}

/// Assert at least one line contains the expected non-secret marker.
fn assert_contains_marker(lines: &[String], marker: &str, context: &str) {
    let found = lines.iter().any(|l| l.contains(marker));
    assert!(
        found,
        "positive control FAILED for {context}: expected non-secret marker \
         {marker:?} was not found in captured lines:\n{lines:#?}"
    );
}

/// Assert that NO captured line contains the sentinel.
fn assert_no_sentinel(lines: &[String], sentinel: &str, context: &str) {
    for line in lines {
        assert!(
            !line.contains(sentinel),
            "SECRET LEAKED in {context}: sentinel {sentinel:?} found in log line: {line:?}"
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
// Unix-socket gRPC server helpers
// ---------------------------------------------------------------------------

use sandbox_agent::sandbox_v1;

/// Start a SandboxService on a unique Unix socket and return a connected client.
#[cfg(target_os = "linux")]
async fn start_sandbox_client(
    sock: &str,
) -> sandbox_v1::sandbox_client::SandboxClient<tonic::transport::Channel> {
    use std::path::PathBuf;
    use std::sync::Arc as StdArc;
    use tokio::sync::Mutex as TokioMutex;

    use sandbox_agent::env::ConfiguredEnv;
    use sandbox_agent::kernel::KernelManager;
    use sandbox_agent::sandbox_v1::sandbox_server::SandboxServer;
    use sandbox_agent::service::SandboxService;

    let _ = std::fs::remove_file(sock);
    let uds = tokio::net::UnixListener::bind(sock).expect("bind unix socket");
    let incoming = tokio_stream::wrappers::UnixListenerStream::new(uds);

    let svc = SandboxService {
        env: StdArc::new(ConfiguredEnv::new()),
        kernel: StdArc::new(TokioMutex::new(KernelManager::new())),
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

/// Start a ControlService on a unique Unix socket and return a connected client.
///
/// The ControlService is the REAL production handler for Configure and
/// NotifyForked. Tests drive it via a Unix-domain gRPC connection so the
/// handler code (including its log emissions) runs on tokio worker threads,
/// proving the global subscriber captures cross-thread events.
#[cfg(target_os = "linux")]
async fn start_control_client(
    sock: &str,
) -> sandbox_agent::control_v1::control_client::ControlClient<tonic::transport::Channel> {
    use std::sync::Arc as StdArc;
    use std::time::Instant;

    use sandbox_agent::control_v1::control_server::ControlServer;
    use sandbox_agent::env::ConfiguredEnv;
    use sandbox_agent::service::control::ControlService;

    let _ = std::fs::remove_file(sock);
    let uds = tokio::net::UnixListener::bind(sock).expect("bind unix socket for ControlService");
    let incoming = tokio_stream::wrappers::UnixListenerStream::new(uds);

    let svc = ControlService {
        start_time: Instant::now(),
        env: StdArc::new(ConfiguredEnv::new()),
        signal_fn: |_| 0, // no-op: must not broadcast SIGUSR2 on box2
        workload: StdArc::new(sandbox_agent::service::workload::WorkloadRegistry::default()),
    };

    tokio::spawn(async move {
        tonic::transport::Server::builder()
            .add_service(ControlServer::new(svc))
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
        .expect("connect to control unix socket");

    sandbox_agent::control_v1::control_client::ControlClient::new(channel)
}

// ---------------------------------------------------------------------------
// Mutex serializing all audit classes in this binary.
//
// cargo test may run tests inside one binary in parallel on separate threads.
// The global capture buffer is shared, so classes must run one at a time
// and snapshot/clear the buffer atomically between them. A single Mutex
// around a unit token (()) enforces this. Note: each test binary is its own
// process, so this Mutex does NOT interact with other test binaries.
// ---------------------------------------------------------------------------

static AUDIT_LOCK: OnceLock<Mutex<()>> = OnceLock::new();

fn audit_lock() -> &'static Mutex<()> {
    AUDIT_LOCK.get_or_init(|| Mutex::new(()))
}

// ---------------------------------------------------------------------------
// Cross-thread positive-control: prove global subscriber captures worker events.
//
// This test spawns a tokio task (which runs on a worker thread) and emits a
// tracing event there. If the global subscriber does NOT capture it, the
// positive-control assertion catches the empty buffer. This guards the entire
// audit against silently regressing to a no-op capture.
// ---------------------------------------------------------------------------

#[tokio::test]
async fn global_subscriber_captures_worker_thread_events() {
    let _guard = audit_lock().lock().unwrap();
    // Initialize the global subscriber (idempotent after first call).
    let _ = global_lines();
    // Clear any noise from earlier tests.
    take_lines();

    const WORKER_MARKER: &str = "audit_cross_thread_positive_control_7a3f";

    // Emit the marker from a tokio worker thread.
    tokio::spawn(async move {
        tracing::info!(marker = WORKER_MARKER, "cross-thread positive control");
    })
    .await
    .expect("worker task panicked");

    // Give the subscriber a moment to flush (it is synchronous, but the
    // spawn schedules on a worker; await above ensures it completed).
    let lines = take_lines();

    assert_nonempty(&lines, WORKER_MARKER, "cross-thread positive control");
    assert_contains_marker(&lines, WORKER_MARKER, "cross-thread positive control");
}

// ---------------------------------------------------------------------------
// Class 1: Configure (real ControlService gRPC handler).
//
// Drives the production configure() handler via a Unix-domain gRPC server.
// The handler emits tracing::info! with only counts (env_keys, secret_keys).
// Neither keys nor values must appear in logs.
// ---------------------------------------------------------------------------

#[tokio::test]
#[cfg(target_os = "linux")]
async fn configure_secret_never_appears_in_logs() {
    let _guard = audit_lock().lock().unwrap();
    let _ = global_lines();
    take_lines();

    const SENTINEL: &str = "SENTINEL_SECRET_VALUE_d34db33f";

    // Unique socket path avoids collisions with parallel test binaries.
    let sock = format!(
        "/tmp/audit-configure-{}.sock",
        std::process::id()
    );
    let mut client = start_control_client(&sock).await;

    use sandbox_agent::control_v1::ConfigureRequest;

    let req = ConfigureRequest {
        env: std::collections::HashMap::from([
            ("MY_ENV_VAR".to_string(), SENTINEL.to_string()),
        ]),
        secrets: std::collections::HashMap::from([
            ("API_KEY".to_string(), SENTINEL.to_string()),
        ]),
    };

    // Call the REAL production Configure gRPC handler.
    client
        .configure(tonic::Request::new(req))
        .await
        .expect("Configure RPC failed");

    // Small wait: handler runs on a tokio worker thread; await above ensures
    // the future completed but the tracing event is emitted synchronously
    // inside the handler, so no additional sleep is needed.
    let lines = take_lines();

    // Positive control: the handler MUST have logged "Configure applied".
    assert_nonempty(&lines, "Configure applied", "Configure");
    assert_contains_marker(&lines, "Configure applied", "Configure");

    // Security gate: sentinel must not appear anywhere in logs.
    assert_no_sentinel(&lines, SENTINEL, "Configure secrets/env values");
}

// ---------------------------------------------------------------------------
// Class 2: NotifyForked (entropy bytes as raw, hex, base64).
//
// Exercises handle_notify_forked_inner directly (it runs synchronously on the
// calling thread). The orchestrator logs a summary with entropy_bytes=N (a
// count only). The raw bytes must never appear as hex or base64.
// ---------------------------------------------------------------------------

#[test]
fn notify_forked_entropy_never_appears_in_logs() {
    let _guard = audit_lock().lock().unwrap();
    let _ = global_lines();
    take_lines();

    // Distinctive entropy bytes: hex "5e47abe1ef5ec4e7".
    const ENTROPY: &[u8] = &[0x5e, 0x47, 0xab, 0xe1, 0xef, 0x5e, 0xc4, 0xe7];

    let hex_sentinel: String = ENTROPY.iter().map(|b| format!("{b:02x}")).collect();
    let b64_sentinel = base64_encode(ENTROPY);

    // No-op signal: never sends SIGUSR2 to host processes (box2 safety contract).
    fn noop() -> i32 { 0 }

    let req = sandbox_agent::fork::NotifyForkedRequest {
        generation: 7,
        host_wall_clock_nanos: 0,
        entropy: ENTROPY.to_vec(),
        network: None,
        volumes: vec![],
    };

    // Production orchestrator (synchronous; runs on this thread).
    let _ = sandbox_agent::fork::handle_notify_forked_inner(&req, noop);

    let lines = take_lines();

    // Positive control: the orchestrator MUST have logged entropy_bytes=N.
    assert_nonempty(&lines, "entropy_bytes", "NotifyForked");
    assert_contains_marker(&lines, "entropy_bytes", "NotifyForked");

    // Security gate.
    assert_no_sentinel(&lines, &hex_sentinel, "NotifyForked entropy (hex)");
    assert_no_sentinel(&lines, &b64_sentinel, "NotifyForked entropy (base64)");
}

// ---------------------------------------------------------------------------
// Class 3: Exec (argv and stdout bytes via real SandboxService gRPC handler).
//
// Runs `echo SENTINEL` via the Exec RPC. The command string and stdout bytes
// must not appear in any tracing log line.
// ---------------------------------------------------------------------------

#[tokio::test]
#[cfg(target_os = "linux")]
async fn exec_argv_and_output_never_appear_in_logs() {
    let _guard = audit_lock().lock().unwrap();
    let _ = global_lines();
    take_lines();

    const SENTINEL: &str = "SENTINEL_EXEC_OUTPUT_c0ffee42";

    let sock = format!(
        "/tmp/audit-exec-{}.sock",
        std::process::id()
    );
    let mut client = start_sandbox_client(&sock).await;

    use sandbox_agent::sandbox_v1::{exec_request::Msg as ReqMsg, ExecOpen, ExecRequest};

    let command = format!("echo {SENTINEL}");
    let open = ExecRequest {
        msg: Some(ReqMsg::Open(ExecOpen {
            command: command.clone(),
            args: vec![],
            env: vec![],
            // Use /tmp as cwd: /workspace may not exist outside the VM.
            cwd: "/tmp".to_string(),
            timeout_seconds: 5,
            pty: None,
        })),
    };

    let mut response_stream = client
        .exec(tonic::Request::new(tokio_stream::once(open)))
        .await
        .expect("exec RPC call failed")
        .into_inner();

    // Drain the response stream: waits for the handler to complete and emit logs.
    while let Ok(Some(_)) = response_stream.message().await {}

    // Small sleep: ensure the handler's deferred tracing events (emitted on
    // the worker after the stream drains) are flushed into the global buffer.
    tokio::time::sleep(std::time::Duration::from_millis(20)).await;

    let lines = take_lines();

    // Positive control: exec handler MUST log "exec: process exited".
    // This proves the global subscriber captured events from the tokio worker thread.
    assert_nonempty(&lines, "exec: process exited", "Exec");
    assert_contains_marker(&lines, "exec: process exited", "Exec");

    // Security gate: command string and output bytes must not appear.
    assert_no_sentinel(&lines, &command, "Exec command string (argv)");
    assert_no_sentinel(&lines, SENTINEL, "Exec stdout output bytes");
}

// ---------------------------------------------------------------------------
// Class 4: WriteFile / ReadFile (file content bytes).
//
// Writes a file whose content is the sentinel, then reads it back. Neither
// the write path nor the read path must log any file bytes.
// ---------------------------------------------------------------------------

#[tokio::test]
#[cfg(target_os = "linux")]
async fn file_content_never_appears_in_logs() {
    let _guard = audit_lock().lock().unwrap();
    let _ = global_lines();
    take_lines();

    const SENTINEL: &str = "SENTINEL_FILE_CONTENT_baadf00d";

    let tmp = tempfile::tempdir().expect("create tmpdir");
    let file_path = tmp.path().join("audit-secret.txt");
    let path_str = file_path.to_string_lossy().into_owned();

    let sock = format!(
        "/tmp/audit-files-{}.sock",
        std::process::id()
    );
    let mut client = start_sandbox_client(&sock).await;

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

    tokio::time::sleep(std::time::Duration::from_millis(20)).await;

    let lines = take_lines();

    // Positive control: write/read handlers MUST log the file path (non-secret).
    assert_nonempty(&lines, &path_str, "WriteFile/ReadFile");
    assert_contains_marker(&lines, &path_str, "WriteFile/ReadFile");

    // Security gate: file content bytes must not appear.
    assert_no_sentinel(&lines, SENTINEL, "WriteFile/ReadFile file content bytes");
}
