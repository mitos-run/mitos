// Conformance test harness for the Sandbox gRPC service skeleton.
//
// Spins up the tonic SandboxService on a Unix domain socket (so tests run
// on the host without AF_VSOCK), drives it with the tonic-generated client,
// and asserts that the server accepts connections and returns well-formed
// gRPC responses (Unimplemented for all stub RPCs in this slice).
//
// Later Phase 2 tasks extend this harness by adding test cases for each RPC
// as it is implemented.

#![allow(clippy::unwrap_used, clippy::expect_used)]

use std::sync::Arc;
use tokio::sync::Mutex;
use tonic::Code;

use sandbox_agent::env::ConfiguredEnv;
use sandbox_agent::kernel::KernelManager;
use sandbox_agent::sandbox_v1::sandbox_client::SandboxClient;
use sandbox_agent::sandbox_v1::sandbox_server::SandboxServer;
use sandbox_agent::sandbox_v1::StatRequest;
use sandbox_agent::service::SandboxService;

/// Path for the Unix socket used by conformance tests.
/// Each test function must ensure this path is cleaned up before binding.
const TEST_SOCK: &str = "/tmp/agent-conformance-test.sock";

/// Build a SandboxService with empty shared state for use in tests.
fn make_service() -> SandboxService {
    SandboxService {
        env: Arc::new(ConfiguredEnv::new()),
        kernel: Arc::new(Mutex::new(KernelManager::new())),
    }
}

/// Start the Sandbox gRPC service on a Unix domain socket and return a
/// connected client. The server runs in a background tokio task; it is
/// cleaned up when the test process exits.
async fn start_server_and_client(sock_path: &str) -> SandboxClient<tonic::transport::Channel> {
    let _ = std::fs::remove_file(sock_path);
    let uds = tokio::net::UnixListener::bind(sock_path).expect("bind unix socket");
    let incoming = tokio_stream::wrappers::UnixListenerStream::new(uds);

    let service = make_service();
    tokio::spawn(async move {
        tonic::transport::Server::builder()
            .add_service(SandboxServer::new(service))
            .serve_with_incoming(incoming)
            .await
            .ok();
    });

    // Give the server a moment to accept.
    tokio::time::sleep(std::time::Duration::from_millis(50)).await;

    // Connect via the unix socket. The URI is a placeholder; the connector
    // overrides the address.
    // tonic 0.13 uses hyper's IO traits (hyper::rt::{Read, Write}) rather than
    // tokio's AsyncRead/AsyncWrite. TokioIo wraps a tokio IO type to satisfy
    // the hyper bounds that connect_with_connector requires.
    let sock_path = sock_path.to_owned();
    let channel = tonic::transport::Endpoint::from_static("http://[::]:0")
        .connect_with_connector(tower::service_fn(move |_| {
            let path = sock_path.clone();
            async move {
                let stream = tokio::net::UnixStream::connect(path).await?;
                Ok::<_, std::io::Error>(hyper_util::rt::TokioIo::new(stream))
            }
        }))
        .await
        .expect("connect to test server");

    SandboxClient::new(channel)
}

/// The Stat RPC must return a valid FileInfo for the root path.
/// This validates that the tonic service is correctly wired and the gRPC
/// framing works over a Unix socket.
#[tokio::test]
async fn stat_root_returns_dir() {
    let mut client = start_server_and_client(TEST_SOCK).await;

    let result = client
        .stat(StatRequest { path: "/".into() })
        .await;

    let fi = result.expect("Stat / must succeed");
    assert!(
        fi.into_inner().is_dir,
        "/ must be a directory",
    );
}

/// Verify that a Budget RPC also round-trips correctly as Unimplemented.
/// Budget is a unary RPC with no streaming, exercising a different code path
/// than Stat (which is also unary but uses a different message type).
#[tokio::test]
async fn budget_returns_unimplemented() {
    use sandbox_agent::sandbox_v1::BudgetRequest;

    // Use a distinct socket path to avoid racing with stat_returns_unimplemented.
    const BUDGET_SOCK: &str = "/tmp/agent-conformance-budget-test.sock";
    let mut client = start_server_and_client(BUDGET_SOCK).await;

    let result = client.budget(BudgetRequest {}).await;

    let status = result.expect_err("stub Budget must return an error");
    assert_eq!(
        status.code(),
        Code::Unimplemented,
        "stub Budget must return Code::Unimplemented, got {:?}",
        status.code()
    );
}

// ---------------------------------------------------------------------------
// Exec conformance tests (Task 2.1)
// ---------------------------------------------------------------------------

/// The Exec RPC must stream stdout bytes then send ExecExit with exit_code=0
/// for a simple printf command. This is the primary conformance gate for the
/// non-PTY exec path.
#[tokio::test]
async fn exec_echo_returns_stdout() {
    use sandbox_agent::sandbox_v1::{self, exec_response};
    use tokio_stream::StreamExt;

    const EXEC_ECHO_SOCK: &str = "/tmp/agent-conformance-exec-echo.sock";
    let mut client = start_server_and_client(EXEC_ECHO_SOCK).await;

    let open = sandbox_v1::ExecRequest {
        msg: Some(sandbox_v1::exec_request::Msg::Open(sandbox_v1::ExecOpen {
            command: "printf 'hello'".into(),
            cwd: "/tmp".into(),
            ..Default::default()
        })),
    };

    let (tx, rx) = tokio::sync::mpsc::channel(10);
    tx.send(open).await.unwrap();
    drop(tx); // no more stdin

    let stream = client
        .exec(tokio_stream::wrappers::ReceiverStream::new(rx))
        .await
        .unwrap()
        .into_inner();

    let mut out = String::new();
    let mut exit_code = -1i32;
    tokio::pin!(stream);
    while let Some(msg) = stream.next().await {
        match msg.unwrap().msg.unwrap() {
            exec_response::Msg::Stdout(b) => out.push_str(&String::from_utf8_lossy(&b)),
            exec_response::Msg::Exit(e) => {
                exit_code = e.exit_code;
                break;
            }
            _ => {}
        }
    }
    assert_eq!(out, "hello");
    assert_eq!(exit_code, 0);
}

/// The Exec RPC must propagate non-zero exit codes from the child process.
#[tokio::test]
async fn exec_nonzero_exit_code_propagated() {
    use sandbox_agent::sandbox_v1::{self, exec_response};
    use tokio_stream::StreamExt;

    const SOCK: &str = "/tmp/agent-conformance-exec-exit.sock";
    let mut client = start_server_and_client(SOCK).await;

    let open = sandbox_v1::ExecRequest {
        msg: Some(sandbox_v1::exec_request::Msg::Open(sandbox_v1::ExecOpen {
            command: "exit 42".into(),
            cwd: "/tmp".into(),
            ..Default::default()
        })),
    };

    let (tx, rx) = tokio::sync::mpsc::channel(4);
    tx.send(open).await.unwrap();
    drop(tx);

    let stream = client
        .exec(tokio_stream::wrappers::ReceiverStream::new(rx))
        .await
        .unwrap()
        .into_inner();

    let mut exit_code = -1i32;
    tokio::pin!(stream);
    while let Some(msg) = stream.next().await {
        if let exec_response::Msg::Exit(e) = msg.unwrap().msg.unwrap() {
            exit_code = e.exit_code;
            break;
        }
    }
    assert_eq!(exit_code, 42);
}

/// The Exec RPC must reject a stream with args set (argv exec not implemented).
#[tokio::test]
async fn exec_args_returns_unimplemented() {
    use sandbox_agent::sandbox_v1;

    const SOCK: &str = "/tmp/agent-conformance-exec-args.sock";
    let mut client = start_server_and_client(SOCK).await;

    let open = sandbox_v1::ExecRequest {
        msg: Some(sandbox_v1::exec_request::Msg::Open(sandbox_v1::ExecOpen {
            command: "echo".into(),
            args: vec!["hello".into()],
            ..Default::default()
        })),
    };

    let (tx, rx) = tokio::sync::mpsc::channel(4);
    tx.send(open).await.unwrap();
    drop(tx);

    let stream = client
        .exec(tokio_stream::wrappers::ReceiverStream::new(rx))
        .await;

    // The RPC must either return an initial error or deliver an error message.
    // In practice tonic returns the Unimplemented status as an error on the
    // stream itself (the handler sends it after reading the first message).
    // Accept either form.
    match stream {
        Err(status) => {
            assert_eq!(status.code(), Code::Unimplemented);
        }
        Ok(response) => {
            use tokio_stream::StreamExt;
            let stream = response.into_inner();
            tokio::pin!(stream);
            let first = stream.next().await.expect("at least one message");
            let status = first.expect_err("must be an error");
            assert_eq!(status.code(), Code::Unimplemented);
        }
    }
}

/// The Exec RPC must reject a stream whose first message is not `open`.
#[tokio::test]
async fn exec_missing_open_returns_invalid_argument() {
    use sandbox_agent::sandbox_v1;

    const SOCK: &str = "/tmp/agent-conformance-exec-noopen.sock";
    let mut client = start_server_and_client(SOCK).await;

    // Send a stdin message as the first message (not open).
    let bad_first = sandbox_v1::ExecRequest {
        msg: Some(sandbox_v1::exec_request::Msg::Stdin(b"hello".to_vec())),
    };

    let (tx, rx) = tokio::sync::mpsc::channel(4);
    tx.send(bad_first).await.unwrap();
    drop(tx);

    let stream = client
        .exec(tokio_stream::wrappers::ReceiverStream::new(rx))
        .await;

    match stream {
        Err(status) => {
            assert_eq!(status.code(), Code::InvalidArgument);
        }
        Ok(response) => {
            use tokio_stream::StreamExt;
            let stream = response.into_inner();
            tokio::pin!(stream);
            let first = stream.next().await.expect("at least one message");
            let status = first.expect_err("must be an error");
            assert_eq!(status.code(), Code::InvalidArgument);
        }
    }
}

/// The Exec RPC with PTY mode must allocate a PTY, run the command through it,
/// stream the output as Stdout frames, and send ExecExit with exit_code=0.
/// This exercises exec_pty, apply_pty_session_leader, and the reader/writer
/// task concurrency end to end.
///
/// Only runs on Linux where openpty is available. On other platforms the server
/// returns Unimplemented which we accept gracefully.
#[tokio::test]
async fn exec_pty_echo_returns_stdout_and_exit_zero() {
    use sandbox_agent::sandbox_v1::{self, exec_response};
    use tokio_stream::StreamExt;

    const SOCK: &str = "/tmp/agent-conformance-exec-pty-echo.sock";
    let mut client = start_server_and_client(SOCK).await;

    let open = sandbox_v1::ExecRequest {
        msg: Some(sandbox_v1::exec_request::Msg::Open(sandbox_v1::ExecOpen {
            // Use printf via /bin/sh -c so we get a deterministic marker with
            // no trailing newline complications; the PTY may echo input but the
            // marker string is present in output regardless.
            command: "printf 'PTYMARKER'".into(),
            cwd: "/tmp".into(),
            pty: Some(sandbox_v1::PtyOptions {
                term: "xterm-256color".into(),
                size: Some(sandbox_v1::WindowSize {
                    cols: 80,
                    rows: 24,
                }),
            }),
            timeout_seconds: 10,
            ..Default::default()
        })),
    };

    let (tx, rx) = tokio::sync::mpsc::channel(10);
    tx.send(open).await.unwrap();
    drop(tx); // no further stdin; shell will exit after printf

    let stream = client
        .exec(tokio_stream::wrappers::ReceiverStream::new(rx))
        .await;

    // On non-Linux platforms the server returns Unimplemented; accept that.
    let stream = match stream {
        Ok(r) => r.into_inner(),
        Err(status) if status.code() == Code::Unimplemented => {
            // PTY not available on this platform; test passes vacuously.
            return;
        }
        Err(e) => panic!("exec rpc failed unexpectedly: {e}"),
    };

    let mut all_stdout = String::new();
    let mut exit_code: Option<i32> = None;
    tokio::pin!(stream);
    while let Some(msg) = stream.next().await {
        match msg.unwrap().msg.unwrap() {
            exec_response::Msg::Stdout(b) => {
                all_stdout.push_str(&String::from_utf8_lossy(&b));
            }
            exec_response::Msg::Exit(e) => {
                exit_code = Some(e.exit_code);
                break;
            }
            _ => {}
        }
    }

    assert!(
        all_stdout.contains("PTYMARKER"),
        "expected PTYMARKER in PTY output, got: {:?}",
        all_stdout,
    );
    assert_eq!(
        exit_code,
        Some(0),
        "expected exit_code=0 for PTY exec, got: {:?}",
        exit_code,
    );
}

// ---------------------------------------------------------------------------
// File RPC conformance tests (Task 2.2)
// ---------------------------------------------------------------------------

/// WriteFile followed by ReadFile must round-trip the exact bytes written.
#[tokio::test]
async fn write_then_read_file_roundtrips() {
    use sandbox_agent::sandbox_v1;
    use tokio_stream::StreamExt;

    const SOCK: &str = "/tmp/agent-conformance-write-read.sock";
    let mut client = start_server_and_client(SOCK).await;
    let path = "/tmp/agent-rs-conformance-test.txt";

    let open_msg = sandbox_v1::WriteFileRequest {
        msg: Some(sandbox_v1::write_file_request::Msg::Open(sandbox_v1::WriteFileOpen {
            path: path.into(),
            mode: 0o644,
        })),
    };
    let data_msg = sandbox_v1::WriteFileRequest {
        msg: Some(sandbox_v1::write_file_request::Msg::Data(b"hello world".to_vec())),
    };
    let (tx, rx) = tokio::sync::mpsc::channel(10);
    tx.send(open_msg).await.unwrap();
    tx.send(data_msg).await.unwrap();
    drop(tx);

    let result = client
        .write_file(tokio_stream::wrappers::ReceiverStream::new(rx))
        .await
        .unwrap()
        .into_inner();
    assert_eq!(result.bytes_written, 11);

    let stream = client
        .read_file(sandbox_v1::ReadFileRequest { path: path.into() })
        .await
        .unwrap()
        .into_inner();
    let mut content = Vec::new();
    tokio::pin!(stream);
    while let Some(chunk) = stream.next().await {
        let c = chunk.unwrap();
        content.extend_from_slice(&c.data);
        if c.eof {
            break;
        }
    }
    assert_eq!(content, b"hello world");
}

/// Stat on /tmp must return is_dir=true and path=/tmp.
#[tokio::test]
async fn stat_tmp_is_dir() {
    use sandbox_agent::sandbox_v1;

    const SOCK: &str = "/tmp/agent-conformance-stat-tmp.sock";
    let mut client = start_server_and_client(SOCK).await;

    let fi = client
        .stat(sandbox_v1::StatRequest { path: "/tmp".into() })
        .await
        .unwrap()
        .into_inner();
    assert!(fi.is_dir, "expected /tmp to be a directory");
    assert_eq!(fi.path, "/tmp");
}

/// Mkdir creates a directory; List then shows the entry in the parent.
#[tokio::test]
async fn mkdir_then_list_sees_entry() {
    use sandbox_agent::sandbox_v1;

    const SOCK: &str = "/tmp/agent-conformance-mkdir-list.sock";
    let mut client = start_server_and_client(SOCK).await;
    let dir = "/tmp/agent-rs-mkdir-test";

    client
        .mkdir(sandbox_v1::MkdirRequest { path: dir.into() })
        .await
        .unwrap();

    let resp = client
        .list(sandbox_v1::ListRequest {
            parent: "/tmp".into(),
            ..Default::default()
        })
        .await
        .unwrap()
        .into_inner();
    let names: Vec<&str> = resp.entries.iter().map(|e| e.name.as_str()).collect();
    assert!(
        names.contains(&"agent-rs-mkdir-test"),
        "expected agent-rs-mkdir-test in /tmp listing, got: {:?}",
        names,
    );
}

/// Stat on a missing path must return NotFound.
#[tokio::test]
async fn stat_missing_path_returns_not_found() {
    use sandbox_agent::sandbox_v1;

    const SOCK: &str = "/tmp/agent-conformance-stat-notfound.sock";
    let mut client = start_server_and_client(SOCK).await;

    let err = client
        .stat(sandbox_v1::StatRequest {
            path: "/tmp/agent-rs-does-not-exist-xyz".into(),
        })
        .await
        .unwrap_err();
    assert_eq!(err.code(), tonic::Code::NotFound);
}

/// Remove deletes a file created by WriteFile.
#[tokio::test]
async fn remove_deletes_file() {
    use sandbox_agent::sandbox_v1;

    const SOCK: &str = "/tmp/agent-conformance-remove.sock";
    let mut client = start_server_and_client(SOCK).await;
    let path = "/tmp/agent-rs-remove-test.txt";

    // Write the file first.
    let open_msg = sandbox_v1::WriteFileRequest {
        msg: Some(sandbox_v1::write_file_request::Msg::Open(sandbox_v1::WriteFileOpen {
            path: path.into(),
            mode: 0o644,
        })),
    };
    let (tx, rx) = tokio::sync::mpsc::channel(4);
    tx.send(open_msg).await.unwrap();
    drop(tx);
    client
        .write_file(tokio_stream::wrappers::ReceiverStream::new(rx))
        .await
        .unwrap();

    // Remove it.
    client
        .remove(sandbox_v1::RemoveRequest {
            path: path.into(),
            recursive: false,
        })
        .await
        .unwrap();

    // Stat must now return NotFound.
    let err = client
        .stat(sandbox_v1::StatRequest { path: path.into() })
        .await
        .unwrap_err();
    assert_eq!(err.code(), tonic::Code::NotFound);
}

/// Remove on a missing path must succeed (mirrors Go's os.RemoveAll no-op).
#[tokio::test]
async fn remove_missing_path_is_ok() {
    use sandbox_agent::sandbox_v1;

    const SOCK: &str = "/tmp/agent-conformance-remove-missing.sock";
    let mut client = start_server_and_client(SOCK).await;

    client
        .remove(sandbox_v1::RemoveRequest {
            path: "/tmp/agent-rs-never-existed-xyz".into(),
            recursive: false,
        })
        .await
        .unwrap();
}

/// WriteFile must set the file mode atomically: the file's mode bits must equal
/// the requested mode (0o600) immediately after the RPC returns, with no
/// intermediate window at a umask-dependent mode.
#[tokio::test]
async fn write_file_mode_is_set_atomically() {
    use sandbox_agent::sandbox_v1;
    use std::os::unix::fs::PermissionsExt as _;

    const SOCK: &str = "/tmp/agent-conformance-writefile-mode.sock";
    let mut client = start_server_and_client(SOCK).await;
    let path = "/tmp/agent-rs-mode-test.txt";

    // Remove any leftover from a prior run.
    let _ = std::fs::remove_file(path);

    let open_msg = sandbox_v1::WriteFileRequest {
        msg: Some(sandbox_v1::write_file_request::Msg::Open(
            sandbox_v1::WriteFileOpen {
                path: path.into(),
                mode: 0o600,
            },
        )),
    };
    let data_msg = sandbox_v1::WriteFileRequest {
        msg: Some(sandbox_v1::write_file_request::Msg::Data(b"secret".to_vec())),
    };
    let (tx, rx) = tokio::sync::mpsc::channel(4);
    tx.send(open_msg).await.unwrap();
    tx.send(data_msg).await.unwrap();
    drop(tx);

    client
        .write_file(tokio_stream::wrappers::ReceiverStream::new(rx))
        .await
        .unwrap();

    let meta = std::fs::metadata(path).expect("file must exist after WriteFile");
    let mode = meta.permissions().mode() & 0o777;
    assert_eq!(
        mode, 0o600,
        "expected file mode 0o600, got 0o{mode:o}",
    );
}

/// Mkdir must create the directory tree with mode 0o755, not a umask-dependent
/// mode, matching Go's os.MkdirAll(path, 0o755).
#[tokio::test]
async fn mkdir_sets_explicit_0o755_mode() {
    use sandbox_agent::sandbox_v1;
    use std::os::unix::fs::PermissionsExt as _;

    const SOCK: &str = "/tmp/agent-conformance-mkdir-mode.sock";
    let mut client = start_server_and_client(SOCK).await;
    let dir = "/tmp/agent-rs-mkdir-mode-test";

    // Remove any leftover from a prior run.
    let _ = std::fs::remove_dir_all(dir);

    client
        .mkdir(sandbox_v1::MkdirRequest { path: dir.into() })
        .await
        .unwrap();

    let meta = std::fs::metadata(dir).expect("directory must exist after Mkdir");
    let mode = meta.permissions().mode() & 0o777;
    assert_eq!(
        mode, 0o755,
        "expected directory mode 0o755, got 0o{mode:o}",
    );
}

/// The Exec RPC must stream stderr bytes for commands that write to stderr.
#[tokio::test]
async fn exec_stderr_returned() {
    use sandbox_agent::sandbox_v1::{self, exec_response};
    use tokio_stream::StreamExt;

    const SOCK: &str = "/tmp/agent-conformance-exec-stderr.sock";
    let mut client = start_server_and_client(SOCK).await;

    let open = sandbox_v1::ExecRequest {
        msg: Some(sandbox_v1::exec_request::Msg::Open(sandbox_v1::ExecOpen {
            command: "printf 'err' >&2".into(),
            cwd: "/tmp".into(),
            ..Default::default()
        })),
    };

    let (tx, rx) = tokio::sync::mpsc::channel(10);
    tx.send(open).await.unwrap();
    drop(tx);

    let stream = client
        .exec(tokio_stream::wrappers::ReceiverStream::new(rx))
        .await
        .unwrap()
        .into_inner();

    let mut stderr_out = String::new();
    let mut exit_code = -1i32;
    tokio::pin!(stream);
    while let Some(msg) = stream.next().await {
        match msg.unwrap().msg.unwrap() {
            exec_response::Msg::Stderr(b) => {
                stderr_out.push_str(&String::from_utf8_lossy(&b));
            }
            exec_response::Msg::Exit(e) => {
                exit_code = e.exit_code;
                break;
            }
            _ => {}
        }
    }
    assert_eq!(stderr_out, "err");
    assert_eq!(exit_code, 0);
}
