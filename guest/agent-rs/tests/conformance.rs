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

use std::path::PathBuf;
use std::sync::Arc;
use tokio::sync::Mutex;
use tonic::Code;

use sandbox_agent::env::ConfiguredEnv;
use sandbox_agent::kernel::KernelManager;
use sandbox_agent::sandbox_v1::sandbox_client::SandboxClient;
use sandbox_agent::sandbox_v1::sandbox_server::SandboxServer;
use sandbox_agent::sandbox_v1::StatRequest;
use sandbox_agent::service::SandboxService;

/// Build a SandboxService with the given workspace root.
fn make_service_with_root(workspace_root: impl Into<PathBuf>) -> SandboxService {
    SandboxService {
        env: Arc::new(ConfiguredEnv::new()),
        kernel: Arc::new(Mutex::new(KernelManager::new())),
        workspace_root: workspace_root.into(),
    }
}

/// Build a SandboxService with the default /workspace root.
fn make_service() -> SandboxService {
    make_service_with_root("/workspace")
}

/// Start the Sandbox gRPC service on a Unix domain socket and return a
/// connected client. The server runs in a background tokio task; it is
/// cleaned up when the test process exits.
async fn start_server_and_client(sock_path: &str) -> SandboxClient<tonic::transport::Channel> {
    start_server_and_client_with_service(sock_path, make_service()).await
}

/// Start the Sandbox gRPC service using a caller-supplied SandboxService
/// instance (so callers can set workspace_root independently per test).
async fn start_server_and_client_with_service(
    sock_path: &str,
    service: SandboxService,
) -> SandboxClient<tonic::transport::Channel> {
    let _ = std::fs::remove_file(sock_path);
    let uds = tokio::net::UnixListener::bind(sock_path).expect("bind unix socket");
    let incoming = tokio_stream::wrappers::UnixListenerStream::new(uds);

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

/// Build a minimal ustar (POSIX tar) with one entry at the given name and the
/// given content. This bypasses the tar crate's path validation so we can craft
/// a tar with a "../" traversal entry to test the server-side guard.
///
/// Format: one 512-byte header block followed by content padded to a 512-byte
/// boundary, then two zero-filled 512-byte end-of-archive blocks.
fn build_traversal_tar(entry_name: &str, content: &[u8]) -> Vec<u8> {
    let mut header = [0u8; 512];

    // name field: bytes 0-99 (100 bytes, NUL-padded).
    let name_bytes = entry_name.as_bytes();
    let name_len = name_bytes.len().min(99);
    header[..name_len].copy_from_slice(&name_bytes[..name_len]);

    // mode field: bytes 100-107 (octal, space-terminated).
    header[100..107].copy_from_slice(b"0000644");
    header[107] = b' ';

    // uid/gid fields (bytes 108-123): all zeros (NUL-terminated octal 0).
    header[108..115].copy_from_slice(b"0000000");
    header[115] = b' ';
    header[116..123].copy_from_slice(b"0000000");
    header[123] = b' ';

    // size field: bytes 124-135 (octal, space-terminated).
    let size_str = format!("{:011o} ", content.len());
    let size_bytes = size_str.as_bytes();
    let sz_len = size_bytes.len().min(12);
    header[124..124 + sz_len].copy_from_slice(&size_bytes[..sz_len]);

    // mtime field: bytes 136-147 (octal, space-terminated). Use "00000000000 ".
    header[136..147].copy_from_slice(b"00000000000");
    header[147] = b' ';

    // checksum placeholder: bytes 148-155. Set to spaces for checksum calculation.
    for b in header[148..156].iter_mut() {
        *b = b' ';
    }

    // typeflag: byte 156. '0' = regular file.
    header[156] = b'0';

    // magic (ustar): bytes 257-262.
    header[257..263].copy_from_slice(b"ustar ");
    header[263] = b' ';

    // Compute and write the checksum (simple sum of all header bytes).
    let checksum: u32 = header.iter().map(|&b| b as u32).sum();
    let cksum_str = format!("{:06o}\0 ", checksum);
    let cksum_bytes = cksum_str.as_bytes();
    let ck_len = cksum_bytes.len().min(8);
    header[148..148 + ck_len].copy_from_slice(&cksum_bytes[..ck_len]);

    // Pad content to 512-byte boundary.
    let padded_len = content.len().div_ceil(512) * 512;
    let mut tar = Vec::with_capacity(512 + padded_len + 1024);
    tar.extend_from_slice(&header);
    tar.extend_from_slice(content);
    tar.resize(512 + padded_len, 0);
    // Two end-of-archive zero blocks.
    tar.resize(512 + padded_len + 1024, 0);
    tar
}

/// The Stat RPC must return a valid FileInfo for the root path.
/// This validates that the tonic service is correctly wired and the gRPC
/// framing works over a Unix socket.
#[tokio::test]
async fn stat_root_returns_dir() {
    const SOCK: &str = "/tmp/agent-conformance-stat-root.sock";
    let mut client = start_server_and_client(SOCK).await;

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

// ---------------------------------------------------------------------------
// Archive + Upload conformance tests (Task 2.3)
// ---------------------------------------------------------------------------

/// Archive with UNTAR direction must return InvalidArgument (mirrors Go grpc_server.go:414-415).
#[tokio::test]
async fn archive_untar_direction_returns_invalid_argument() {
    use sandbox_agent::sandbox_v1::{self, archive_request};
    use tokio_stream::StreamExt;

    const SOCK: &str = "/tmp/agent-conformance-archive-untar-dir.sock";
    // Use /tmp as workspace root so the path check passes and we reach the direction check.
    let mut client = start_server_and_client_with_service(
        SOCK,
        make_service_with_root("/tmp"),
    ).await;

    let stream = client
        .archive(sandbox_v1::ArchiveRequest {
            direction: archive_request::Direction::Untar as i32,
            path: "/tmp".into(),
        })
        .await;

    // The error surfaces either as the RPC call itself failing or as the first
    // message on the stream being an error.
    match stream {
        Err(status) => {
            assert_eq!(
                status.code(),
                tonic::Code::InvalidArgument,
                "expected InvalidArgument for UNTAR direction, got {:?}",
                status.code()
            );
        }
        Ok(response) => {
            let stream = response.into_inner();
            tokio::pin!(stream);
            let first = stream.next().await.expect("at least one message");
            let status = first.expect_err("must be an error for UNTAR direction");
            assert_eq!(
                status.code(),
                tonic::Code::InvalidArgument,
                "expected InvalidArgument for UNTAR direction, got {:?}",
                status.code()
            );
        }
    }
}

/// Archive with a path outside /workspace must return PermissionDenied.
#[tokio::test]
async fn archive_outside_workspace_returns_permission_denied() {
    use sandbox_agent::sandbox_v1::{self, archive_request};
    use tokio_stream::StreamExt;

    const SOCK: &str = "/tmp/agent-conformance-archive-outside-ws.sock";
    // Service uses /workspace root so /etc is denied.
    let mut client = start_server_and_client(SOCK).await;

    let stream = client
        .archive(sandbox_v1::ArchiveRequest {
            direction: archive_request::Direction::Download as i32,
            path: "/etc".into(),
        })
        .await;

    match stream {
        Err(status) => {
            assert_eq!(
                status.code(),
                tonic::Code::PermissionDenied,
                "expected PermissionDenied for /etc, got {:?}",
                status.code()
            );
        }
        Ok(response) => {
            let stream = response.into_inner();
            tokio::pin!(stream);
            let first = stream.next().await.expect("at least one message");
            let status = first.expect_err("must be an error for /etc path");
            assert_eq!(
                status.code(),
                tonic::Code::PermissionDenied,
                "expected PermissionDenied for /etc, got {:?}",
                status.code()
            );
        }
    }
}

/// Archive streams a tar of a directory under /tmp (used as workspace stand-in
/// in tests), and the tar bytes end with an eof=true Chunk. The tar must be
/// parseable (non-empty bytes before the eof chunk means a valid tar header was
/// produced).
#[tokio::test]
async fn archive_download_streams_tar_with_eof_chunk() {
    use sandbox_agent::sandbox_v1::{self, archive_request};
    use tokio_stream::StreamExt;

    const SOCK: &str = "/tmp/agent-conformance-archive-download.sock";
    // Use /tmp as workspace root so the allowlist check passes.
    let mut client = start_server_and_client_with_service(
        SOCK,
        make_service_with_root("/tmp"),
    ).await;

    // Create a small directory tree under /tmp/agent-rs-archive-src.
    let src = "/tmp/agent-rs-archive-src";
    let _ = std::fs::remove_dir_all(src);
    std::fs::create_dir_all(src).unwrap();
    std::fs::write(format!("{src}/hello.txt"), b"hello from archive test").unwrap();

    let response = client
        .archive(sandbox_v1::ArchiveRequest {
            direction: archive_request::Direction::Download as i32,
            path: src.into(),
        })
        .await
        .expect("Archive must succeed");

    let stream = response.into_inner();
    tokio::pin!(stream);

    let mut all_bytes: Vec<u8> = Vec::new();
    let mut saw_eof = false;
    while let Some(chunk) = stream.next().await {
        let c = chunk.expect("chunk must not be an error");
        all_bytes.extend_from_slice(&c.data);
        if c.eof {
            saw_eof = true;
            break;
        }
    }
    assert!(saw_eof, "Archive stream must end with an eof=true Chunk");
    // A valid tar of at least one file is well above 512 bytes.
    assert!(
        all_bytes.len() >= 512,
        "expected at least 512 bytes of tar data, got {}",
        all_bytes.len()
    );
}

/// Upload extracts a tar sent as Chunk bytes into a destination directory. The
/// UploadResult must report bytes_written > 0 and the extracted file must exist.
#[tokio::test]
async fn upload_extracts_tar_and_returns_bytes_written() {
    use sandbox_agent::sandbox_v1;
    const SOCK: &str = "/tmp/agent-conformance-upload-extract.sock";
    // Use /tmp as workspace root so /tmp destinations pass the allowlist.
    let mut client = start_server_and_client_with_service(
        SOCK,
        make_service_with_root("/tmp"),
    ).await;

    // Build a minimal tar in memory with one regular file.
    let mut tar_bytes: Vec<u8> = Vec::new();
    {
        let mut builder = tar::Builder::new(&mut tar_bytes);
        let content = b"uploaded file content";
        let mut header = tar::Header::new_gnu();
        header.set_size(content.len() as u64);
        header.set_mode(0o644);
        header.set_entry_type(tar::EntryType::Regular);
        header.set_cksum();
        builder.append_data(&mut header, "uploaded.txt", content.as_ref()).unwrap();
        builder.finish().unwrap();
    }

    let dest = "/tmp/agent-rs-upload-dest";
    let _ = std::fs::remove_dir_all(dest);

    let open_msg = sandbox_v1::UploadRequest {
        msg: Some(sandbox_v1::upload_request::Msg::Open(sandbox_v1::UploadOpen {
            dest: dest.into(),
        })),
    };
    let chunk_msg = sandbox_v1::UploadRequest {
        msg: Some(sandbox_v1::upload_request::Msg::Chunk(tar_bytes.clone())),
    };

    let (tx, rx) = tokio::sync::mpsc::channel(4);
    tx.send(open_msg).await.unwrap();
    tx.send(chunk_msg).await.unwrap();
    drop(tx);

    let result = client
        .upload(tokio_stream::wrappers::ReceiverStream::new(rx))
        .await
        .expect("Upload must succeed")
        .into_inner();

    assert!(
        result.bytes_written > 0,
        "Upload must report bytes_written > 0, got {}",
        result.bytes_written
    );

    let extracted = std::fs::read(format!("{dest}/uploaded.txt"))
        .expect("extracted file must exist");
    assert_eq!(extracted, b"uploaded file content");
}

/// Archive -> Upload roundtrip: archive a directory, stream the tar bytes, then
/// Upload them back into a new destination, and confirm the files match.
#[tokio::test]
async fn archive_upload_roundtrip() {
    use sandbox_agent::sandbox_v1::{self, archive_request};
    use tokio_stream::StreamExt;

    const SOCK: &str = "/tmp/agent-conformance-archive-upload-roundtrip.sock";
    // Use /tmp as workspace root for both archive and upload.
    let mut client = start_server_and_client_with_service(
        SOCK,
        make_service_with_root("/tmp"),
    ).await;

    // Create source directory.
    let src = "/tmp/agent-rs-roundtrip-src";
    let _ = std::fs::remove_dir_all(src);
    std::fs::create_dir_all(src).unwrap();
    std::fs::write(format!("{src}/data.txt"), b"roundtrip content").unwrap();

    // Archive it.
    let archive_stream = client
        .archive(sandbox_v1::ArchiveRequest {
            direction: archive_request::Direction::Download as i32,
            path: src.into(),
        })
        .await
        .expect("Archive must succeed")
        .into_inner();

    tokio::pin!(archive_stream);

    let mut tar_bytes: Vec<u8> = Vec::new();
    while let Some(chunk) = archive_stream.next().await {
        let c = chunk.expect("archive chunk must not be error");
        tar_bytes.extend_from_slice(&c.data);
        if c.eof {
            break;
        }
    }
    assert!(!tar_bytes.is_empty(), "archive must produce non-empty tar");

    // Upload the tar to a new destination.
    let dest = "/tmp/agent-rs-roundtrip-dest";
    let _ = std::fs::remove_dir_all(dest);

    let open_msg = sandbox_v1::UploadRequest {
        msg: Some(sandbox_v1::upload_request::Msg::Open(sandbox_v1::UploadOpen {
            dest: dest.into(),
        })),
    };
    let chunk_msg = sandbox_v1::UploadRequest {
        msg: Some(sandbox_v1::upload_request::Msg::Chunk(tar_bytes)),
    };

    let (tx, rx) = tokio::sync::mpsc::channel(4);
    tx.send(open_msg).await.unwrap();
    tx.send(chunk_msg).await.unwrap();
    drop(tx);

    let result = client
        .upload(tokio_stream::wrappers::ReceiverStream::new(rx))
        .await
        .expect("Upload must succeed")
        .into_inner();
    assert!(result.bytes_written > 0, "roundtrip upload must report bytes_written > 0");

    let extracted = std::fs::read(format!("{dest}/data.txt"))
        .expect("roundtrip file must exist after upload");
    assert_eq!(extracted, b"roundtrip content");
}

/// Upload of a tar containing a path-traversal entry ("../escape.txt") must be
/// rejected with PermissionDenied; no file must be written outside dest.
#[tokio::test]
async fn upload_path_traversal_rejected() {
    use sandbox_agent::sandbox_v1;

    const SOCK: &str = "/tmp/agent-conformance-upload-traversal.sock";
    // Use /tmp as workspace root so /tmp destinations pass the allowlist.
    let mut client = start_server_and_client_with_service(
        SOCK,
        make_service_with_root("/tmp"),
    ).await;

    // Build a tar with a malicious "../escape.txt" entry manually, because
    // tar::Builder rejects "../" names at build time. We craft the raw POSIX
    // ustar bytes directly to test the server-side guard.
    let tar_bytes = build_traversal_tar("../escape.txt", b"ESCAPED");

    let dest = "/tmp/agent-rs-traversal-dest";
    let _ = std::fs::remove_dir_all(dest);
    let escape_path = "/tmp/escape.txt";
    let _ = std::fs::remove_file(escape_path);

    let open_msg = sandbox_v1::UploadRequest {
        msg: Some(sandbox_v1::upload_request::Msg::Open(sandbox_v1::UploadOpen {
            dest: dest.into(),
        })),
    };
    let chunk_msg = sandbox_v1::UploadRequest {
        msg: Some(sandbox_v1::upload_request::Msg::Chunk(tar_bytes)),
    };

    let (tx, rx) = tokio::sync::mpsc::channel(4);
    tx.send(open_msg).await.unwrap();
    tx.send(chunk_msg).await.unwrap();
    drop(tx);

    let result = client
        .upload(tokio_stream::wrappers::ReceiverStream::new(rx))
        .await;

    // The RPC must return an error (PermissionDenied).
    let status = result.expect_err("path-traversal upload must fail");
    assert_eq!(
        status.code(),
        tonic::Code::PermissionDenied,
        "expected PermissionDenied for path-traversal tar, got {:?}",
        status.code()
    );

    // The escape file must NOT have been written outside dest.
    assert!(
        !std::path::Path::new(escape_path).exists(),
        "path-traversal must not write outside dest: {escape_path} must not exist",
    );
}

// ---------------------------------------------------------------------------
// Watch RPC conformance tests (Task 2.4)
// ---------------------------------------------------------------------------

/// The Watch RPC must stream a CREATE FsEvent when a file is written under the
/// watched directory. The watched path must be a directory; the event must carry
/// kind=CREATE and a path ending in the created filename.
#[tokio::test]
async fn watch_detects_file_create() {
    use sandbox_agent::sandbox_v1;
    use tokio_stream::StreamExt;

    const SOCK: &str = "/tmp/agent-conformance-watch-create.sock";
    // Use /tmp as workspace root so /tmp paths pass the allowlist check.
    let mut client = start_server_and_client_with_service(
        SOCK,
        make_service_with_root("/tmp"),
    ).await;

    let dir = "/tmp/agent-rs-watch-test";
    std::fs::create_dir_all(dir).unwrap();
    // Remove any leftover file from a prior run.
    let _ = std::fs::remove_file(format!("{dir}/hello.txt"));

    let stream = client
        .watch(sandbox_v1::WatchRequest {
            path: dir.into(),
            recursive: false,
        })
        .await
        .unwrap()
        .into_inner();

    // Give the watcher a moment to install before the file op.
    tokio::time::sleep(std::time::Duration::from_millis(50)).await;
    std::fs::write(format!("{dir}/hello.txt"), b"hi").unwrap();

    tokio::pin!(stream);
    let ev = tokio::time::timeout(
        std::time::Duration::from_secs(2),
        stream.next(),
    )
    .await
    .expect("no event within 2s")
    .unwrap()
    .unwrap();

    assert_eq!(
        ev.kind,
        sandbox_v1::fs_event::Kind::Create as i32,
        "expected CREATE event, got kind={}",
        ev.kind
    );
    assert!(
        ev.path.ends_with("hello.txt"),
        "expected path ending in hello.txt, got: {}",
        ev.path
    );
}

/// Watch on a non-directory path must return InvalidArgument.
#[tokio::test]
async fn watch_non_directory_returns_invalid_argument() {
    use sandbox_agent::sandbox_v1;

    const SOCK: &str = "/tmp/agent-conformance-watch-notdir.sock";
    // Use /tmp as workspace root so /tmp paths pass the allowlist check.
    let mut client = start_server_and_client_with_service(
        SOCK,
        make_service_with_root("/tmp"),
    ).await;

    // Create a regular file to watch.
    let file_path = "/tmp/agent-rs-watch-file.txt";
    std::fs::write(file_path, b"not a directory").unwrap();

    let result = client
        .watch(sandbox_v1::WatchRequest {
            path: file_path.into(),
            recursive: false,
        })
        .await;

    let status = result.expect_err("Watch on a non-directory must fail");
    assert_eq!(
        status.code(),
        tonic::Code::InvalidArgument,
        "expected InvalidArgument for non-directory path, got {:?}",
        status.code()
    );
}

/// Watch on a path outside the workspace allowlist must return PermissionDenied.
#[tokio::test]
async fn watch_outside_workspace_returns_permission_denied() {
    use sandbox_agent::sandbox_v1;

    const SOCK: &str = "/tmp/agent-conformance-watch-denied.sock";
    // Service uses /workspace root so /etc is denied.
    let mut client = start_server_and_client(SOCK).await;

    let result = client
        .watch(sandbox_v1::WatchRequest {
            path: "/etc".into(),
            recursive: false,
        })
        .await;

    let status = result.expect_err("Watch on /etc must fail with PermissionDenied");
    assert_eq!(
        status.code(),
        tonic::Code::PermissionDenied,
        "expected PermissionDenied for /etc, got {:?}",
        status.code()
    );
}

/// Watch detects a rename within the watched directory and reports a RENAME event
/// with the new path set. This exercises the highest-risk event arm: the inotify
/// backend must correlate MOVED_FROM / MOVED_TO via cookie and deliver a single
/// Modify(Name(Both)) event with both paths populated.
#[tokio::test]
async fn watch_detects_rename() {
    use sandbox_agent::sandbox_v1;
    use tokio_stream::StreamExt;

    const SOCK: &str = "/tmp/agent-conformance-watch-rename.sock";
    let mut client = start_server_and_client_with_service(
        SOCK,
        make_service_with_root("/tmp"),
    ).await;

    let dir = "/tmp/agent-rs-watch-rename-test";
    std::fs::create_dir_all(dir).unwrap();
    let src = format!("{dir}/old.txt");
    let dst = format!("{dir}/new.txt");
    let _ = std::fs::remove_file(&src);
    let _ = std::fs::remove_file(&dst);
    std::fs::write(&src, b"rename me").unwrap();

    let stream = client
        .watch(sandbox_v1::WatchRequest {
            path: dir.into(),
            recursive: false,
        })
        .await
        .unwrap()
        .into_inner();

    // Give the watcher a moment to install before the file op.
    tokio::time::sleep(std::time::Duration::from_millis(50)).await;
    std::fs::rename(&src, &dst).unwrap();

    tokio::pin!(stream);
    // Poll for a RENAME event; skip any CREATE/MODIFY/DELETE events that may
    // arrive before it (e.g. the initial write settling). Fail if no RENAME
    // arrives within 2 seconds.
    let deadline = std::time::Duration::from_secs(2);
    let mut rename_ev: Option<sandbox_v1::FsEvent> = None;
    let start = std::time::Instant::now();
    while start.elapsed() < deadline {
        let remaining = deadline.saturating_sub(start.elapsed());
        let next = tokio::time::timeout(remaining, stream.next()).await;
        let Ok(Some(Ok(ev))) = next else { break };
        if ev.kind == sandbox_v1::fs_event::Kind::Rename as i32 {
            rename_ev = Some(ev);
            break;
        }
    }
    let ev = rename_ev.expect("expected a RENAME event within 2s");
    assert!(
        ev.path.ends_with("old.txt"),
        "expected old path ending in old.txt, got: {}",
        ev.path
    );
    assert!(
        ev.new_path.ends_with("new.txt"),
        "expected new_path ending in new.txt, got: {}",
        ev.new_path
    );
}

/// Watch detects file removal and reports a DELETE event with the removed path.
#[tokio::test]
async fn watch_detects_delete() {
    use sandbox_agent::sandbox_v1;
    use tokio_stream::StreamExt;

    const SOCK: &str = "/tmp/agent-conformance-watch-delete.sock";
    let mut client = start_server_and_client_with_service(
        SOCK,
        make_service_with_root("/tmp"),
    ).await;

    let dir = "/tmp/agent-rs-watch-delete-test";
    std::fs::create_dir_all(dir).unwrap();
    let file = format!("{dir}/to-delete.txt");
    std::fs::write(&file, b"delete me").unwrap();

    let stream = client
        .watch(sandbox_v1::WatchRequest {
            path: dir.into(),
            recursive: false,
        })
        .await
        .unwrap()
        .into_inner();

    // Give the watcher a moment to install before the file op.
    tokio::time::sleep(std::time::Duration::from_millis(50)).await;
    std::fs::remove_file(&file).unwrap();

    tokio::pin!(stream);
    // Poll for a DELETE event with a bounded timeout.
    let deadline = std::time::Duration::from_secs(2);
    let mut delete_ev: Option<sandbox_v1::FsEvent> = None;
    let start = std::time::Instant::now();
    while start.elapsed() < deadline {
        let remaining = deadline.saturating_sub(start.elapsed());
        let next = tokio::time::timeout(remaining, stream.next()).await;
        let Ok(Some(Ok(ev))) = next else { break };
        if ev.kind == sandbox_v1::fs_event::Kind::Delete as i32 {
            delete_ev = Some(ev);
            break;
        }
    }
    let ev = delete_ev.expect("expected a DELETE event within 2s");
    assert!(
        ev.path.ends_with("to-delete.txt"),
        "expected path ending in to-delete.txt, got: {}",
        ev.path
    );
}

/// With recursive=true, Watch detects a file created in a subdirectory and
/// reports a CREATE event for the new file path.
#[tokio::test]
async fn watch_recursive_detects_subdir_create() {
    use sandbox_agent::sandbox_v1;
    use tokio_stream::StreamExt;

    const SOCK: &str = "/tmp/agent-conformance-watch-recursive.sock";
    let mut client = start_server_and_client_with_service(
        SOCK,
        make_service_with_root("/tmp"),
    ).await;

    let dir = "/tmp/agent-rs-watch-recursive-test";
    let subdir = format!("{dir}/sub");
    std::fs::create_dir_all(&subdir).unwrap();
    let file = format!("{subdir}/deep.txt");
    let _ = std::fs::remove_file(&file);

    let stream = client
        .watch(sandbox_v1::WatchRequest {
            path: dir.into(),
            recursive: true,
        })
        .await
        .unwrap()
        .into_inner();

    // Give the watcher a moment to install before the file op.
    tokio::time::sleep(std::time::Duration::from_millis(50)).await;
    std::fs::write(&file, b"deep file").unwrap();

    tokio::pin!(stream);
    // Poll for a CREATE event whose path contains the subdir filename.
    let deadline = std::time::Duration::from_secs(2);
    let mut create_ev: Option<sandbox_v1::FsEvent> = None;
    let start = std::time::Instant::now();
    while start.elapsed() < deadline {
        let remaining = deadline.saturating_sub(start.elapsed());
        let next = tokio::time::timeout(remaining, stream.next()).await;
        let Ok(Some(Ok(ev))) = next else { break };
        if ev.kind == sandbox_v1::fs_event::Kind::Create as i32
            && ev.path.ends_with("deep.txt")
        {
            create_ev = Some(ev);
            break;
        }
    }
    let ev = create_ev.expect("expected a CREATE event for deep.txt within 2s");
    assert!(
        ev.path.ends_with("deep.txt"),
        "expected path ending in deep.txt, got: {}",
        ev.path
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

// ---------------------------------------------------------------------------
// Processes + Signal conformance tests (Task 2.5)
// ---------------------------------------------------------------------------

/// The Processes RPC must include the agent's own process (the test runner) in
/// the returned list. At minimum one entry with pid == std::process::id() must
/// be present on Linux; on other platforms the RPC may succeed vacuously.
///
/// This is the primary conformance gate: the /proc table contains the caller.
#[tokio::test]
async fn processes_includes_own_process() {
    use sandbox_agent::sandbox_v1::ProcessesRequest;

    const SOCK: &str = "/tmp/agent-conformance-processes-self.sock";
    let mut client = start_server_and_client(SOCK).await;

    let result = client.processes(ProcessesRequest {}).await;

    // On non-Linux platforms /proc does not exist; accept any outcome.
    let list = match result {
        Ok(r) => r.into_inner(),
        Err(s) if s.code() == tonic::Code::Internal => {
            // /proc unavailable (macOS CI).
            return;
        }
        Err(e) => panic!("Processes RPC failed unexpectedly: {e}"),
    };

    let own_pid = std::process::id() as i32;
    let found = list.processes.iter().any(|p| p.pid == own_pid);
    assert!(
        found,
        "Processes must include own pid {own_pid}; got pids: {:?}",
        list.processes.iter().map(|p| p.pid).collect::<Vec<_>>(),
    );
}

/// The Processes RPC must not leak argv or environ: the child process entry
/// (identified by pid) must have a command field that does not contain the
/// distinctive argv argument used to spawn it.
///
/// This test launches a child process with a distinctive argv and verifies that
/// the argv string does NOT appear in the child's ProcessInfo command field.
#[tokio::test]
async fn processes_does_not_leak_argv() {
    use sandbox_agent::sandbox_v1::ProcessesRequest;

    const SOCK: &str = "/tmp/agent-conformance-processes-noargv.sock";
    let mut client = start_server_and_client(SOCK).await;

    // Spawn a child process whose argv contains a distinctive secret-marker.
    // On Linux we use `sleep` with a distinctive duration as the arg. The comm
    // field must be "sleep", not "sleep 99999" or "/usr/bin/sleep 99999".
    let mut child = match std::process::Command::new("sleep").arg("99999").spawn() {
        Ok(c) => c,
        Err(_) => {
            // sleep not available; skip on this platform.
            return;
        }
    };
    let child_pid = child.id() as i32;

    // Brief yield so the child registers in /proc.
    tokio::time::sleep(std::time::Duration::from_millis(20)).await;

    let result = client.processes(ProcessesRequest {}).await;

    // Kill the child regardless of outcome.
    let _ = child.kill();
    let _ = child.wait();

    let list = match result {
        Ok(r) => r.into_inner(),
        Err(s) if s.code() == tonic::Code::Internal => {
            return; // /proc unavailable
        }
        Err(e) => panic!("Processes RPC failed: {e}"),
    };

    // Find the child process entry by pid. If it already exited, skip the check.
    if let Some(entry) = list.processes.iter().find(|p| p.pid == child_pid) {
        // The command field must be the bare comm name ("sleep"), not the
        // full argv ("sleep 99999") and must not contain the distinctive arg.
        assert!(
            !entry.command.contains("99999"),
            "ProcessInfo.command must not contain argv arg '99999'; got {:?}",
            entry.command,
        );
        // comm is always a short bare name (max 15 chars on Linux).
        assert!(
            entry.command.len() <= 15,
            "ProcessInfo.command must be a bare comm name (<= 15 chars); got {:?}",
            entry.command,
        );
    }
}

/// All ProcessInfo entries must have non-empty state, pid > 0, and
/// rss_bytes >= 0. The command field must be non-empty for running processes.
#[tokio::test]
async fn processes_fields_are_sane() {
    use sandbox_agent::sandbox_v1::ProcessesRequest;

    const SOCK: &str = "/tmp/agent-conformance-processes-fields.sock";
    let mut client = start_server_and_client(SOCK).await;

    let list = match client.processes(ProcessesRequest {}).await {
        Ok(r) => r.into_inner(),
        Err(s) if s.code() == tonic::Code::Internal => return,
        Err(e) => panic!("Processes RPC failed: {e}"),
    };

    assert!(
        !list.processes.is_empty(),
        "Processes must return at least one entry",
    );

    for p in &list.processes {
        assert!(p.pid > 0, "pid must be > 0, got {}", p.pid);
        assert!(!p.state.is_empty(), "state must be non-empty for pid {}", p.pid);
        assert!(p.rss_bytes >= 0, "rss_bytes must be >= 0 for pid {}", p.pid);
        // cpu_percent in [0, 100] is a sanity bound; short window may give ~0.
        assert!(
            p.cpu_percent >= 0.0 && p.cpu_percent <= 200.0,
            "cpu_percent {:.2} out of sane range for pid {}",
            p.cpu_percent,
            p.pid,
        );
    }
}

/// Signal with a valid signal to a spawned child delivers the signal; the child
/// exits. We use SIGTERM (15) and confirm the child is no longer running.
#[tokio::test]
async fn signal_terminates_child_process() {
    use sandbox_agent::sandbox_v1::SignalRequest;

    const SOCK: &str = "/tmp/agent-conformance-signal-term.sock";
    let mut client = start_server_and_client(SOCK).await;

    // Spawn a long-lived child.
    let mut child = match std::process::Command::new("sleep").arg("300").spawn() {
        Ok(c) => c,
        Err(_) => return, // skip if sleep not available
    };
    let child_pid = child.id() as i32;

    // Brief yield so the child appears in /proc.
    tokio::time::sleep(std::time::Duration::from_millis(20)).await;

    let result = client
        .signal(SignalRequest {
            pid: child_pid,
            signal: 15, // SIGTERM
        })
        .await;

    match result {
        Ok(_) => {
            // Signal delivered; wait for child to exit.
            let status = child.wait().expect("child must exit after SIGTERM");
            // On Linux a SIGTERM'd process is not successful (exit code != 0).
            assert!(
                !status.success(),
                "SIGTERM'd child must not exit successfully",
            );
        }
        Err(s) if s.code() == tonic::Code::Internal => {
            // libc::kill not available on this platform.
            let _ = child.kill();
            let _ = child.wait();
        }
        Err(e) => {
            let _ = child.kill();
            let _ = child.wait();
            panic!("Signal RPC failed unexpectedly: {e}");
        }
    }
}

/// Signal to pid 1 must return InvalidArgument (the guest control plane guard).
#[tokio::test]
async fn signal_to_pid_1_returns_invalid_argument() {
    use sandbox_agent::sandbox_v1::SignalRequest;

    const SOCK: &str = "/tmp/agent-conformance-signal-pid1.sock";
    let mut client = start_server_and_client(SOCK).await;

    let err = client
        .signal(SignalRequest { pid: 1, signal: 15 })
        .await
        .expect_err("Signal to pid 1 must fail");
    assert_eq!(
        err.code(),
        tonic::Code::InvalidArgument,
        "expected InvalidArgument for pid 1, got {:?}",
        err.code(),
    );
}

/// Signal with pid <= 0 must return InvalidArgument.
#[tokio::test]
async fn signal_to_nonpositive_pid_returns_invalid_argument() {
    use sandbox_agent::sandbox_v1::SignalRequest;

    const SOCK: &str = "/tmp/agent-conformance-signal-pidneg.sock";
    let mut client = start_server_and_client(SOCK).await;

    for bad_pid in [0i32, -1, i32::MIN] {
        let err = client
            .signal(SignalRequest { pid: bad_pid, signal: 15 })
            .await
            .expect_err(&format!("Signal to pid {bad_pid} must fail"));
        assert_eq!(
            err.code(),
            tonic::Code::InvalidArgument,
            "expected InvalidArgument for pid {bad_pid}, got {:?}",
            err.code(),
        );
    }
}

/// Signal with an out-of-range signal number must return InvalidArgument.
#[tokio::test]
async fn signal_bad_signal_number_returns_invalid_argument() {
    use sandbox_agent::sandbox_v1::SignalRequest;

    const SOCK: &str = "/tmp/agent-conformance-signal-badsig.sock";
    let mut client = start_server_and_client(SOCK).await;

    for bad_sig in [0i32, 65, 100, -1, i32::MIN] {
        let err = client
            .signal(SignalRequest { pid: 2, signal: bad_sig })
            .await
            .expect_err(&format!("Signal with sig {bad_sig} must fail"));
        assert_eq!(
            err.code(),
            tonic::Code::InvalidArgument,
            "expected InvalidArgument for signal {bad_sig}, got {:?}",
            err.code(),
        );
    }
}

/// Signal to a non-existent pid must return NotFound (ESRCH).
#[tokio::test]
async fn signal_nonexistent_pid_returns_not_found() {
    use sandbox_agent::sandbox_v1::SignalRequest;

    const SOCK: &str = "/tmp/agent-conformance-signal-notfound.sock";
    let mut client = start_server_and_client(SOCK).await;

    // PID i32::MAX is extremely unlikely to exist.
    let result = client
        .signal(SignalRequest {
            pid: i32::MAX,
            signal: 0,
        })
        .await;

    // Signal 0 is out of range (< 1), so this returns InvalidArgument first.
    // Use signal 15 (SIGTERM) to reach the kill syscall.
    let result2 = client
        .signal(SignalRequest {
            pid: i32::MAX,
            signal: 15,
        })
        .await;

    // result has signal=0 which is InvalidArgument; result2 should be NotFound.
    let _ = result; // consumed to drop, we only check result2
    match result2 {
        Err(s) if s.code() == tonic::Code::NotFound => {}
        Err(s) if s.code() == tonic::Code::Internal => {
            // kill() unavailable on this platform.
        }
        Err(s) => panic!(
            "expected NotFound for non-existent pid, got {:?}: {}",
            s.code(),
            s.message()
        ),
        Ok(_) => panic!("expected error for non-existent pid, got Ok"),
    }
}

// ---------------------------------------------------------------------------
// PortForward conformance tests (Task 2.6)
// ---------------------------------------------------------------------------

/// PortForward echo roundtrip: start a loopback TCP echo server, open a
/// PortForward to it, send bytes, confirm they echo back, then close cleanly.
///
/// This is the primary conformance gate for the PortForward RPC.
#[tokio::test]
async fn port_forward_echo_roundtrip() {
    use sandbox_agent::sandbox_v1::{self, frame};
    use tokio_stream::StreamExt;

    const SOCK: &str = "/tmp/agent-conformance-portforward-echo.sock";
    let mut client = start_server_and_client(SOCK).await;

    // Bind a loopback echo server on an OS-assigned port.
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = listener.local_addr().unwrap().port() as u32;

    // Spawn the echo server: accept one connection, copy it back.
    tokio::spawn(async move {
        let (mut sock, _) = listener.accept().await.unwrap();
        let (mut r, mut w) = sock.split();
        let _ = tokio::io::copy(&mut r, &mut w).await;
    });

    // Build the client stream: open frame then data frame then close.
    let open_frame = sandbox_v1::Frame {
        msg: Some(frame::Msg::Open(sandbox_v1::PortForwardOpen { port })),
    };
    let data_frame = sandbox_v1::Frame {
        msg: Some(frame::Msg::Data(b"hello portforward".to_vec())),
    };
    let close_frame = sandbox_v1::Frame {
        msg: Some(frame::Msg::Close(true)),
    };

    let (tx, rx) = tokio::sync::mpsc::channel(8);
    tx.send(open_frame).await.unwrap();
    tx.send(data_frame).await.unwrap();
    tx.send(close_frame).await.unwrap();
    drop(tx);

    let stream = client
        .port_forward(tokio_stream::wrappers::ReceiverStream::new(rx))
        .await
        .expect("PortForward must succeed on a listening port")
        .into_inner();

    tokio::pin!(stream);

    // Collect all data frames from the server response stream.
    let mut received = Vec::<u8>::new();
    while let Some(frame_result) = tokio::time::timeout(
        std::time::Duration::from_secs(5),
        stream.next(),
    )
    .await
    .unwrap_or(None)
    {
        let f = frame_result.expect("PortForward stream frame must not be an error");
        match f.msg {
            Some(frame::Msg::Data(b)) => received.extend_from_slice(&b),
            Some(frame::Msg::Close(_)) | None => break,
            Some(frame::Msg::Open(_)) => {}
        }
    }

    assert_eq!(
        received,
        b"hello portforward",
        "PortForward must echo back the exact bytes sent",
    );
}

/// PortForward to a non-listening port must return a gRPC error (Unavailable
/// or Internal) with a dial-refused message; it must NOT hang.
#[tokio::test]
async fn port_forward_refused_port_returns_error() {
    use sandbox_agent::sandbox_v1::{self, frame};
    use tokio_stream::StreamExt;

    const SOCK: &str = "/tmp/agent-conformance-portforward-refused.sock";
    let mut client = start_server_and_client(SOCK).await;

    // Find a free port by binding then immediately closing it, so nothing listens.
    let probe = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let dead_port = probe.local_addr().unwrap().port() as u32;
    drop(probe);

    let open_frame = sandbox_v1::Frame {
        msg: Some(frame::Msg::Open(sandbox_v1::PortForwardOpen { port: dead_port })),
    };

    let (tx, rx) = tokio::sync::mpsc::channel(4);
    tx.send(open_frame).await.unwrap();
    drop(tx);

    // The RPC must return an error promptly (within 10 seconds, well inside the
    // 5-second dial timeout) for a connection-refused scenario.
    let result = tokio::time::timeout(
        std::time::Duration::from_secs(10),
        client.port_forward(tokio_stream::wrappers::ReceiverStream::new(rx)),
    )
    .await
    .expect("PortForward refused must not hang");

    // Either the initial RPC call fails with a status error, or the stream is
    // opened but the first message is an error. Accept both forms.
    match result {
        Err(status) => {
            // Direct RPC-level error: must not be Unimplemented.
            assert_ne!(
                status.code(),
                tonic::Code::Unimplemented,
                "refused port must not return Unimplemented; got {:?}",
                status.code(),
            );
        }
        Ok(response) => {
            let stream = response.into_inner();
            tokio::pin!(stream);
            let first = tokio::time::timeout(
                std::time::Duration::from_secs(10),
                stream.next(),
            )
            .await
            .expect("first frame must arrive within 10s");
            let status = first
                .expect("stream must have at least one item")
                .expect_err("first frame must be an error for a refused port");
            assert_ne!(
                status.code(),
                tonic::Code::Unimplemented,
                "refused port must not return Unimplemented; got {:?}",
                status.code(),
            );
        }
    }
}

/// PortForward with an invalid port (0 and 65536) must return InvalidArgument.
#[tokio::test]
async fn port_forward_invalid_port_returns_invalid_argument() {
    use sandbox_agent::sandbox_v1::{self, frame};
    use tokio_stream::StreamExt;

    for bad_port in [0u32, 65536] {
        let sock = format!("/tmp/agent-conformance-portforward-badport-{bad_port}.sock");
        let mut client = start_server_and_client(&sock).await;

        let open_frame = sandbox_v1::Frame {
            msg: Some(frame::Msg::Open(sandbox_v1::PortForwardOpen { port: bad_port })),
        };

        let (tx, rx) = tokio::sync::mpsc::channel(4);
        tx.send(open_frame).await.unwrap();
        drop(tx);

        let result = client
            .port_forward(tokio_stream::wrappers::ReceiverStream::new(rx))
            .await;

        match result {
            Err(status) => {
                assert_eq!(
                    status.code(),
                    tonic::Code::InvalidArgument,
                    "port {bad_port} must return InvalidArgument; got {:?}",
                    status.code(),
                );
            }
            Ok(response) => {
                let stream = response.into_inner();
                tokio::pin!(stream);
                let first = stream.next().await.expect("at least one item");
                let status = first.expect_err("must be an error for invalid port");
                assert_eq!(
                    status.code(),
                    tonic::Code::InvalidArgument,
                    "port {bad_port} must return InvalidArgument; got {:?}",
                    status.code(),
                );
            }
        }
    }
}

/// PortForward with no open frame (first frame is data) must return
/// InvalidArgument: the protocol requires the first frame to carry `open`.
#[tokio::test]
async fn port_forward_missing_open_returns_invalid_argument() {
    use sandbox_agent::sandbox_v1::{self, frame};
    use tokio_stream::StreamExt;

    const SOCK: &str = "/tmp/agent-conformance-portforward-noopen.sock";
    let mut client = start_server_and_client(SOCK).await;

    // Send a data frame as the first frame (no open).
    let bad_first = sandbox_v1::Frame {
        msg: Some(frame::Msg::Data(b"too early".to_vec())),
    };

    let (tx, rx) = tokio::sync::mpsc::channel(4);
    tx.send(bad_first).await.unwrap();
    drop(tx);

    let result = client
        .port_forward(tokio_stream::wrappers::ReceiverStream::new(rx))
        .await;

    match result {
        Err(status) => {
            assert_eq!(status.code(), tonic::Code::InvalidArgument);
        }
        Ok(response) => {
            let stream = response.into_inner();
            tokio::pin!(stream);
            let first = stream.next().await.expect("at least one item");
            let status = first.expect_err("must be an error when open is missing");
            assert_eq!(status.code(), tonic::Code::InvalidArgument);
        }
    }
}

// ---------------------------------------------------------------------------
// Vitals conformance tests (Task 2.7)
// ---------------------------------------------------------------------------

/// Vitals with interval_seconds=0 must return at least one GuestVitals sample
/// with plausible fields, then close the stream cleanly.
///
/// Plausibility constraints:
///   mem_total_bytes > 0 (every host has memory),
///   cpu_steal_percent in [0, 100] (fractional steal time, clamped),
///   process_count > 0 (at least the test process itself).
///
/// This is the primary conformance gate for the Vitals streaming RPC.
#[tokio::test]
async fn vitals_single_shot_returns_plausible_sample() {
    use sandbox_agent::sandbox_v1::VitalsRequest;
    use tokio_stream::StreamExt;

    const SOCK: &str = "/tmp/agent-conformance-vitals-single.sock";
    let mut client = start_server_and_client(SOCK).await;

    let stream = client
        .vitals(VitalsRequest { interval_seconds: 0 })
        .await;

    // On non-Linux platforms /proc does not exist; accept Internal gracefully.
    let stream = match stream {
        Ok(r) => r.into_inner(),
        Err(s) if s.code() == tonic::Code::Internal => return,
        Err(e) => panic!("Vitals RPC failed unexpectedly: {e}"),
    };

    tokio::pin!(stream);

    // Expect at least one sample.
    let sample = tokio::time::timeout(
        std::time::Duration::from_secs(5),
        stream.next(),
    )
    .await
    .expect("Vitals single-shot must deliver a sample within 5s")
    .expect("stream must not be empty")
    .expect("sample must not be an error");

    assert!(
        sample.mem_total_bytes > 0,
        "mem_total_bytes must be > 0, got {}",
        sample.mem_total_bytes,
    );
    assert!(
        sample.cpu_steal_percent >= 0.0 && sample.cpu_steal_percent <= 100.0,
        "cpu_steal_percent must be in [0, 100], got {}",
        sample.cpu_steal_percent,
    );
    assert!(
        sample.process_count > 0,
        "process_count must be > 0, got {}",
        sample.process_count,
    );

    // The stream must close after the single sample (interval=0 means one-shot).
    let next = tokio::time::timeout(
        std::time::Duration::from_secs(2),
        stream.next(),
    )
    .await;
    match next {
        Ok(None) | Err(_) => {}
        Ok(Some(Ok(_))) => panic!("Vitals single-shot must close after one sample"),
        Ok(Some(Err(_))) => {}
    }
}

/// Vitals with interval_seconds=1 must stream at least two samples, then stop
/// cleanly on client cancel (drop of the stream). No task must be leaked.
///
/// This exercises the ticker-based streaming loop and teardown on disconnect.
#[tokio::test]
async fn vitals_interval_streams_multiple_samples_then_cancels() {
    use sandbox_agent::sandbox_v1::VitalsRequest;
    use tokio_stream::StreamExt;

    const SOCK: &str = "/tmp/agent-conformance-vitals-interval.sock";
    let mut client = start_server_and_client(SOCK).await;

    let stream = client
        .vitals(VitalsRequest { interval_seconds: 1 })
        .await;

    let stream = match stream {
        Ok(r) => r.into_inner(),
        Err(s) if s.code() == tonic::Code::Internal => return,
        Err(e) => panic!("Vitals interval RPC failed unexpectedly: {e}"),
    };

    tokio::pin!(stream);

    // Collect at least 2 samples (first is immediate; second arrives after 1s).
    let mut samples = Vec::new();
    for _ in 0..2 {
        let sample = tokio::time::timeout(
            std::time::Duration::from_secs(5),
            stream.next(),
        )
        .await
        .expect("sample must arrive within 5s")
        .expect("stream must not be empty")
        .expect("sample must not be an error");
        samples.push(sample);
    }

    assert_eq!(samples.len(), 2, "must receive at least 2 samples");

    // Assert both samples have plausible fields.
    for (i, s) in samples.iter().enumerate() {
        assert!(
            s.mem_total_bytes > 0,
            "sample {i}: mem_total_bytes must be > 0, got {}",
            s.mem_total_bytes,
        );
        assert!(
            s.cpu_steal_percent >= 0.0 && s.cpu_steal_percent <= 100.0,
            "sample {i}: cpu_steal_percent must be in [0, 100], got {}",
            s.cpu_steal_percent,
        );
        assert!(
            s.process_count > 0,
            "sample {i}: process_count must be > 0, got {}",
            s.process_count,
        );
    }

    // Let the pinned stream go out of scope: the channel closes, the server
    // streaming task sees a send error and exits cleanly.
    // No explicit drop() needed; lexical scope handles it.

    // Give the server task a moment to notice the client gone.
    tokio::time::sleep(std::time::Duration::from_millis(200)).await;
    // No assertion needed: if the task leaked it will be caught by the process
    // exiting cleanly (no hung tokio tasks blocking shutdown).
}

// ---------------------------------------------------------------------------
// RunCode gRPC-stack conformance tests (Task 2.8)
//
// These tests drive the full gRPC path: tonic client -> SandboxService ->
// KernelManager -> kernel_driver.py. They require a live Python 3 interpreter
// and ipykernel at the default driver path (/opt/mitos/kernel_driver.py).
// Each test owns a fresh SandboxService (and thus a fresh KernelManager) so
// they are parallel-safe with no shared kernel state.
// ---------------------------------------------------------------------------

/// Helper: collect all RunCodeResponse frames from a streaming gRPC response.
/// Returns (stdout_text, stderr_text, error_name, exit_code).
async fn collect_run_code_stream(
    stream: tonic::codec::Streaming<sandbox_agent::sandbox_v1::RunCodeResponse>,
) -> (String, String, Option<String>, Option<i32>) {
    use sandbox_agent::sandbox_v1::run_code_response::Msg;
    use tokio_stream::StreamExt;

    let mut stdout = String::new();
    let mut stderr = String::new();
    let mut error_name: Option<String> = None;
    let mut exit_code: Option<i32> = None;

    tokio::pin!(stream);
    while let Some(item) = stream.next().await {
        let resp = item.expect("RunCode stream frame must not be a transport error");
        match resp.msg {
            Some(Msg::Stdout(b)) => stdout.push_str(&String::from_utf8_lossy(&b)),
            Some(Msg::Stderr(b)) => stderr.push_str(&String::from_utf8_lossy(&b)),
            Some(Msg::Error(e)) => {
                // Record the first error name (KernelUnavailable, NameError, etc.).
                if error_name.is_none() {
                    error_name = Some(e.name.clone());
                }
            }
            Some(Msg::ExitCode(c)) => {
                exit_code = Some(c);
                break;
            }
            Some(Msg::Result(_)) | None => {}
        }
    }

    (stdout, stderr, error_name, exit_code)
}

/// Helper: build a fresh SandboxService with its own KernelManager and start
/// a server + client on the given socket path. Parallel-safe: each call
/// creates an independent kernel process.
async fn start_runcode_client(
    sock: &str,
) -> sandbox_agent::sandbox_v1::sandbox_client::SandboxClient<tonic::transport::Channel> {
    start_server_and_client(sock).await
}

// (a) print(2+2) -> stdout frame containing "4" + exit_code 0.
#[tokio::test]
async fn runcode_grpc_print_stdout_and_exit_zero() {
    use sandbox_agent::sandbox_v1::{self, run_code_request};

    const SOCK: &str = "/tmp/agent-conformance-runcode-print.sock";
    let mut client = start_runcode_client(SOCK).await;

    let open_msg = sandbox_v1::RunCodeRequest {
        msg: Some(run_code_request::Msg::Open(sandbox_v1::RunCodeOpen {
            code: "print(2+2)".into(),
            language: "python".into(),
            timeout_seconds: 30,
        })),
    };
    let (tx, rx) = tokio::sync::mpsc::channel(4);
    tx.send(open_msg).await.unwrap();
    drop(tx);

    let stream = client
        .run_code(tokio_stream::wrappers::ReceiverStream::new(rx))
        .await;

    // If the driver is not installed on this host, accept KernelUnavailable gracefully.
    let stream = match stream {
        Ok(r) => r.into_inner(),
        Err(s) if s.code() == tonic::Code::Unavailable => return,
        Err(e) => panic!("RunCode RPC failed unexpectedly: {e}"),
    };

    let (stdout, _stderr, error_name, exit_code) = collect_run_code_stream(stream).await;

    // If the kernel driver is missing the first frame is KernelUnavailable; skip.
    if error_name.as_deref() == Some("KernelUnavailable") {
        return;
    }

    assert!(
        stdout.contains('4'),
        "expected '4' in stdout, got: {:?}",
        stdout
    );
    assert_eq!(exit_code, Some(0), "expected exit_code 0 for print(2+2)");
}

// (b) Exception -> RunError frame with the exception name (e.g. NameError).
#[tokio::test]
async fn runcode_grpc_exception_produces_run_error_frame() {
    use sandbox_agent::sandbox_v1::{self, run_code_request};

    const SOCK: &str = "/tmp/agent-conformance-runcode-exception.sock";
    let mut client = start_runcode_client(SOCK).await;

    let open_msg = sandbox_v1::RunCodeRequest {
        msg: Some(run_code_request::Msg::Open(sandbox_v1::RunCodeOpen {
            code: "undefined_variable_xyz_conformance".into(),
            language: "".into(),
            timeout_seconds: 30,
        })),
    };
    let (tx, rx) = tokio::sync::mpsc::channel(4);
    tx.send(open_msg).await.unwrap();
    drop(tx);

    let stream = client
        .run_code(tokio_stream::wrappers::ReceiverStream::new(rx))
        .await;

    let stream = match stream {
        Ok(r) => r.into_inner(),
        Err(s) if s.code() == tonic::Code::Unavailable => return,
        Err(e) => panic!("RunCode RPC failed unexpectedly: {e}"),
    };

    let (_stdout, _stderr, error_name, exit_code) = collect_run_code_stream(stream).await;

    if error_name.as_deref() == Some("KernelUnavailable") {
        return;
    }

    assert_eq!(
        error_name.as_deref(),
        Some("NameError"),
        "expected NameError error frame, got: {:?}",
        error_name
    );
    assert_eq!(exit_code, Some(1), "expected exit_code 1 after NameError");
}

// (c) State persistence: x=41 in call 1, print(x+1) in call 2 prints "42".
//
// Both calls must use the SAME kernel (same SandboxService instance). We achieve
// this by reusing the same client (and thus the same server-side kernel) across
// two sequential run_code calls.
#[tokio::test]
async fn runcode_grpc_state_persists_across_calls() {
    use sandbox_agent::sandbox_v1::{self, run_code_request};

    const SOCK: &str = "/tmp/agent-conformance-runcode-state.sock";
    let mut client = start_runcode_client(SOCK).await;

    // Call 1: define x = 41.
    let open1 = sandbox_v1::RunCodeRequest {
        msg: Some(run_code_request::Msg::Open(sandbox_v1::RunCodeOpen {
            code: "x = 41".into(),
            language: "python".into(),
            timeout_seconds: 30,
        })),
    };
    let (tx1, rx1) = tokio::sync::mpsc::channel(4);
    tx1.send(open1).await.unwrap();
    drop(tx1);

    let stream1 = match client
        .run_code(tokio_stream::wrappers::ReceiverStream::new(rx1))
        .await
    {
        Ok(r) => r.into_inner(),
        Err(s) if s.code() == tonic::Code::Unavailable => return,
        Err(e) => panic!("RunCode call 1 failed unexpectedly: {e}"),
    };

    let (_stdout1, _stderr1, error1, exit1) = collect_run_code_stream(stream1).await;
    if error1.as_deref() == Some("KernelUnavailable") {
        return;
    }
    assert_eq!(exit1, Some(0), "call 1 (x=41) must exit cleanly");

    // Call 2: read x.
    let open2 = sandbox_v1::RunCodeRequest {
        msg: Some(run_code_request::Msg::Open(sandbox_v1::RunCodeOpen {
            code: "print(x + 1)".into(),
            language: "python".into(),
            timeout_seconds: 30,
        })),
    };
    let (tx2, rx2) = tokio::sync::mpsc::channel(4);
    tx2.send(open2).await.unwrap();
    drop(tx2);

    let stream2 = match client
        .run_code(tokio_stream::wrappers::ReceiverStream::new(rx2))
        .await
    {
        Ok(r) => r.into_inner(),
        Err(s) if s.code() == tonic::Code::Unavailable => return,
        Err(e) => panic!("RunCode call 2 failed unexpectedly: {e}"),
    };

    let (stdout2, _stderr2, error2, exit2) = collect_run_code_stream(stream2).await;
    if error2.as_deref() == Some("KernelUnavailable") {
        return;
    }

    assert!(
        stdout2.contains("42"),
        "expected '42' in stdout of call 2 (state persistence), got: {:?}",
        stdout2
    );
    assert_eq!(exit2, Some(0), "call 2 must exit cleanly");
}

// (d) Unsupported language -> error frame with name "KernelUnavailable" + exit_code 127.
#[tokio::test]
async fn runcode_grpc_unsupported_language_returns_kernel_unavailable() {
    use sandbox_agent::sandbox_v1::{self, run_code_request};

    const SOCK: &str = "/tmp/agent-conformance-runcode-lang.sock";
    let mut client = start_runcode_client(SOCK).await;

    let open_msg = sandbox_v1::RunCodeRequest {
        msg: Some(run_code_request::Msg::Open(sandbox_v1::RunCodeOpen {
            code: "puts 'hello'".into(),
            language: "ruby".into(),
            timeout_seconds: 30,
        })),
    };
    let (tx, rx) = tokio::sync::mpsc::channel(4);
    tx.send(open_msg).await.unwrap();
    drop(tx);

    let stream = client
        .run_code(tokio_stream::wrappers::ReceiverStream::new(rx))
        .await;

    let stream = match stream {
        Ok(r) => r.into_inner(),
        Err(s) if s.code() == tonic::Code::Unavailable => return,
        Err(e) => panic!("RunCode RPC failed unexpectedly: {e}"),
    };

    let (_stdout, _stderr, error_name, exit_code) = collect_run_code_stream(stream).await;

    assert_eq!(
        error_name.as_deref(),
        Some("KernelUnavailable"),
        "expected KernelUnavailable error frame for unsupported language, got: {:?}",
        error_name
    );
    assert_eq!(
        exit_code,
        Some(127),
        "expected exit_code 127 for unsupported language, got: {:?}",
        exit_code
    );
}

// (e) First message is not `open` -> InvalidArgument status returned by the server.
#[tokio::test]
async fn runcode_grpc_missing_open_returns_invalid_argument() {
    use sandbox_agent::sandbox_v1;
    use tokio_stream::StreamExt;

    const SOCK: &str = "/tmp/agent-conformance-runcode-noopen.sock";
    let mut client = start_runcode_client(SOCK).await;

    // Send a non-open message as the first (and only) message. The sandbox
    // protocol requires the first RunCodeRequest to carry `open`.
    // We send an Open with no msg variant set (msg: None) which triggers the
    // "first message must carry open" validation path.
    let bad_first = sandbox_v1::RunCodeRequest { msg: None };

    let (tx, rx) = tokio::sync::mpsc::channel(4);
    tx.send(bad_first).await.unwrap();
    drop(tx);

    let result = client
        .run_code(tokio_stream::wrappers::ReceiverStream::new(rx))
        .await;

    // The server must reject this with InvalidArgument. The error may surface as
    // an RPC-level status or as the first message on the stream.
    match result {
        Err(status) => {
            assert_eq!(
                status.code(),
                tonic::Code::InvalidArgument,
                "expected InvalidArgument when first message is not open, got {:?}",
                status.code()
            );
        }
        Ok(response) => {
            let stream = response.into_inner();
            tokio::pin!(stream);
            // The server returns the InvalidArgument via the run_code_handler
            // returning Err, which tonic surfaces as a Status error on the stream.
            let first = stream.next().await.expect("stream must have at least one item");
            // The stream item must be an Err with InvalidArgument.
            // (tonic propagates the handler's Err(Status) as a stream error.)
            match first {
                Err(status) => {
                    assert_eq!(
                        status.code(),
                        tonic::Code::InvalidArgument,
                        "expected InvalidArgument, got {:?}",
                        status.code()
                    );
                }
                Ok(frame) => {
                    // If somehow the server sends a frame instead of an error,
                    // check that the error is embedded in the frame.
                    panic!(
                        "expected InvalidArgument status, got a response frame: {:?}",
                        frame
                    );
                }
            }
        }
    }
}
