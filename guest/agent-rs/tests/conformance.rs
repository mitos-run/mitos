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

// ---------------------------------------------------------------------------
// Archive + Upload conformance tests (Task 2.3)
// ---------------------------------------------------------------------------

/// Archive with UNTAR direction must return InvalidArgument (mirrors Go grpc_server.go:414-415).
#[tokio::test]
async fn archive_untar_direction_returns_invalid_argument() {
    use sandbox_agent::sandbox_v1::{self, archive_request};
    use tokio_stream::StreamExt;

    const SOCK: &str = "/tmp/agent-conformance-archive-untar-dir.sock";
    let mut client = start_server_and_client(SOCK).await;

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
    let mut client = start_server_and_client(SOCK).await;

    // Create a small directory tree under /tmp/agent-rs-archive-src.
    let src = "/tmp/agent-rs-archive-src";
    let _ = std::fs::remove_dir_all(src);
    std::fs::create_dir_all(src).unwrap();
    std::fs::write(format!("{src}/hello.txt"), b"hello from archive test").unwrap();

    // Override the workspace root to /tmp so the allowlist check passes.
    sandbox_agent::service::archive::set_workspace_root_for_test("/tmp");

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

    // Note: workspace root is intentionally NOT reset here; all archive/upload
    // tests use /tmp as the root, and /etc is rejected by any root != /etc.
}

/// Upload extracts a tar sent as Chunk bytes into a destination directory. The
/// UploadResult must report bytes_written > 0 and the extracted file must exist.
#[tokio::test]
async fn upload_extracts_tar_and_returns_bytes_written() {
    use sandbox_agent::sandbox_v1;
    const SOCK: &str = "/tmp/agent-conformance-upload-extract.sock";
    let mut client = start_server_and_client(SOCK).await;

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

    // Override workspace root so /tmp passes the allowlist.
    sandbox_agent::service::archive::set_workspace_root_for_test("/tmp");

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

    // Note: workspace root is intentionally NOT reset; see set_workspace_root_for_test.
}

/// Archive -> Upload roundtrip: archive a directory, stream the tar bytes, then
/// Upload them back into a new destination, and confirm the files match.
#[tokio::test]
async fn archive_upload_roundtrip() {
    use sandbox_agent::sandbox_v1::{self, archive_request};
    use tokio_stream::StreamExt;

    const SOCK: &str = "/tmp/agent-conformance-archive-upload-roundtrip.sock";
    let mut client = start_server_and_client(SOCK).await;

    // Override workspace root.
    sandbox_agent::service::archive::set_workspace_root_for_test("/tmp");

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

    // Note: workspace root is intentionally NOT reset; see set_workspace_root_for_test.
}

/// Upload of a tar containing a path-traversal entry ("../escape.txt") must be
/// rejected with PermissionDenied; no file must be written outside dest.
#[tokio::test]
async fn upload_path_traversal_rejected() {
    use sandbox_agent::sandbox_v1;

    const SOCK: &str = "/tmp/agent-conformance-upload-traversal.sock";
    let mut client = start_server_and_client(SOCK).await;

    // Override workspace root so /tmp passes the allowlist.
    sandbox_agent::service::archive::set_workspace_root_for_test("/tmp");

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

    // Note: workspace root is intentionally NOT reset; see set_workspace_root_for_test.
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
    let mut client = start_server_and_client(SOCK).await;

    let dir = "/tmp/agent-rs-watch-test";
    std::fs::create_dir_all(dir).unwrap();
    // Remove any leftover file from a prior run.
    let _ = std::fs::remove_file(format!("{dir}/hello.txt"));

    // Override workspace root so /tmp passes the allowlist check.
    sandbox_agent::service::archive::set_workspace_root_for_test("/tmp");

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
    let mut client = start_server_and_client(SOCK).await;

    // Override workspace root so /tmp passes the allowlist check.
    sandbox_agent::service::archive::set_workspace_root_for_test("/tmp");

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
    let mut client = start_server_and_client(SOCK).await;

    // Do NOT override workspace root: /workspace is the default, so /etc is denied.
    // However, the archive tests may have overridden it. Reset to /workspace here.
    sandbox_agent::service::archive::set_workspace_root_for_test("/workspace");

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
    let mut client = start_server_and_client(SOCK).await;

    let dir = "/tmp/agent-rs-watch-rename-test";
    std::fs::create_dir_all(dir).unwrap();
    let src = format!("{dir}/old.txt");
    let dst = format!("{dir}/new.txt");
    let _ = std::fs::remove_file(&src);
    let _ = std::fs::remove_file(&dst);
    std::fs::write(&src, b"rename me").unwrap();

    sandbox_agent::service::archive::set_workspace_root_for_test("/tmp");

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
    let mut client = start_server_and_client(SOCK).await;

    let dir = "/tmp/agent-rs-watch-delete-test";
    std::fs::create_dir_all(dir).unwrap();
    let file = format!("{dir}/to-delete.txt");
    std::fs::write(&file, b"delete me").unwrap();

    sandbox_agent::service::archive::set_workspace_root_for_test("/tmp");

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
    let mut client = start_server_and_client(SOCK).await;

    let dir = "/tmp/agent-rs-watch-recursive-test";
    let subdir = format!("{dir}/sub");
    std::fs::create_dir_all(&subdir).unwrap();
    let file = format!("{subdir}/deep.txt");
    let _ = std::fs::remove_file(&file);

    sandbox_agent::service::archive::set_workspace_root_for_test("/tmp");

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
