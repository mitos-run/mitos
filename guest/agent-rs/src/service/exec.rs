// Exec and PTY RPC implementation for the Sandbox gRPC service.
//
// Mirrors guest/agent/exec_stream.go (runExecStream) and guest/agent/pty.go
// (runPTY) for behavior parity. The three security invariants from those files
// are preserved here:
//
// 1. Env values are NEVER logged (keys and counts only).
// 2. The process runs in its own process group (Setpgid for non-PTY; Setsid
//    implied by setsid() in pre_exec for PTY) so kills propagate to children.
// 3. Exit-code mapping mirrors Go: timeout -> 124; signal-killed -> -1;
//    ExitError -> its code; spawn failure -> 1 with LLM-legible remediation
//    text; clean -> 0.
//
// Concurrency model (non-PTY):
//   A stdout drain task and a stderr drain task each read from the child pipe
//   and send ExecResponse::Stdout / Stderr frames. A stdin forwarder task reads
//   the client stream and writes bytes to the child stdin pipe. A wait/watchdog
//   task waits for both drain tasks to finish, a client-disconnect signal, or a
//   timeout; on any of the latter two it kills the process group immediately and
//   sends the ExecExit frame. All tasks are join-awaited before this function
//   returns: no task or fd leaks on any path.
//
// Concurrency model (PTY):
//   A reader task drains the PTY master and sends ExecResponse::Stdout frames.
//   A writer task reads the client stream and writes stdin bytes to the master;
//   resize messages call sys::pty::set_winsize via a separately duped master fd.
//   A wait/watchdog task awaits the reader task completion, a client-disconnect
//   signal, or a timeout; it kills the process group, reaps the child, and sends
//   ExecExit. All tasks are join-awaited before returning.
//
// No unsafe code in this module: all syscalls are delegated to sys/pty.rs.

use std::collections::HashMap;
use std::os::fd::AsRawFd;
use std::sync::Arc;
use std::time::{Duration, Instant};

use std::io::Read as _;
use std::io::Write as _;

use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;
use tonic::Status;

use crate::env::ConfiguredEnv;
use crate::sandbox_v1;
use crate::sandbox_v1::exec_response::Msg as RespMsg;

/// Chunk size for streaming stdout/stderr: mirrors streamChunkBytes = 32 KiB
/// from exec_stream.go:24.
const STREAM_CHUNK_BYTES: usize = 32 << 10;

/// Default exec timeout when timeout_seconds <= 0: mirrors the Go default of
/// 30 s from exec_stream.go:80-83.
const DEFAULT_TIMEOUT_SECS: u64 = 30;

// ---------------------------------------------------------------------------
// Public entry point
// ---------------------------------------------------------------------------

/// Handle the Exec bidi stream.
///
/// Reads the first ExecRequest from `stream`; it must carry an `open` message.
/// Spawns the requested command (or delegates to `exec_pty` when open.pty is
/// set), then streams stdout/stderr chunks followed by a terminal ExecExit to
/// `tx`. Returns an Err Status on protocol errors; process failures are
/// reported as ExecExit frames with non-zero exit_code.
///
/// Security invariants:
/// - env values in open.env are never logged (only key count is observable).
/// - The child runs in its own process group so kills propagate to children.
/// - On any path (normal exit, timeout, client hang-up), the process group is
///   killed and reaped before this function returns.
pub async fn exec_handler(
    env: Arc<ConfiguredEnv>,
    mut stream: tonic::Streaming<sandbox_v1::ExecRequest>,
    tx: mpsc::Sender<Result<sandbox_v1::ExecResponse, Status>>,
) -> Result<(), Status> {
    use sandbox_v1::exec_request::Msg as ReqMsg;

    // Read the mandatory first message which must carry `open`.
    let first = stream
        .message()
        .await
        .map_err(|e| Status::internal(format!("exec: recv open: {e}")))?
        .ok_or_else(|| Status::invalid_argument("exec: stream closed before open message"))?;

    let open = match first.msg {
        Some(ReqMsg::Open(o)) => o,
        _ => {
            return Err(Status::invalid_argument(
                "exec: first message must carry open",
            ));
        }
    };

    // argv exec (open.args non-empty) is not implemented in this slice,
    // matching grpc_server.go:149-151.
    if !open.args.is_empty() {
        return Err(Status::unimplemented(
            "exec: argv exec (args non-empty) is not yet implemented; use command with shell",
        ));
    }

    // Delegate to PTY path when pty options are set.
    if open.pty.is_some() {
        return exec_pty(env, open, stream, tx).await;
    }

    exec_shell(env, open, stream, tx).await
}

// ---------------------------------------------------------------------------
// Env merge helper
// ---------------------------------------------------------------------------

fn build_merged_env(
    env: &HashMap<String, String>,
    open_env: &[sandbox_v1::EnvVar],
) -> Vec<String> {
    let base: Vec<String> = std::env::vars().map(|(k, v)| format!("{k}={v}")).collect();
    let req_env: HashMap<String, String> = open_env
        .iter()
        .map(|ev| (ev.key.clone(), ev.value.clone()))
        .collect();
    crate::env::merge(&base, env, &req_env)
}

// ---------------------------------------------------------------------------
// Non-PTY shell exec
// ---------------------------------------------------------------------------

async fn exec_shell(
    env: Arc<ConfiguredEnv>,
    open: sandbox_v1::ExecOpen,
    stream: tonic::Streaming<sandbox_v1::ExecRequest>,
    tx: mpsc::Sender<Result<sandbox_v1::ExecResponse, Status>>,
) -> Result<(), Status> {
    let start = Instant::now();

    let timeout_secs = if open.timeout_seconds > 0 {
        open.timeout_seconds as u64
    } else {
        DEFAULT_TIMEOUT_SECS
    };
    let timeout = Duration::from_secs(timeout_secs);

    // Working directory: mirrors exec_stream.go:88-90.
    let cwd = if open.cwd.is_empty() {
        "/workspace".to_string()
    } else {
        open.cwd.clone()
    };

    // Environment merge: base OS env < configured < request.
    // Env values are never logged.
    let configured = env.snapshot().await;
    let merged_env = build_merged_env(&configured, &open.env);

    // Spawn /bin/sh -c <command> in its own process group so SIGKILL kills
    // the whole child tree. process_group(0) is equivalent to Setpgid=true.
    let mut child = tokio::process::Command::new("/bin/sh")
        .arg("-c")
        .arg(&open.command)
        .current_dir(&cwd)
        .envs(merged_env.iter().filter_map(|kv| {
            kv.split_once('=').map(|(k, v)| (k.to_string(), v.to_string()))
        }))
        .stdin(std::process::Stdio::piped())
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped())
        .process_group(0)
        .spawn()
        .map_err(|e| {
            // Best-effort: send a spawn-failure exit frame. Ignore if channel full.
            let _ = tx.try_send(Ok(exit_frame(1, 0.0, format!("start: {e}"))));
            Status::internal(format!("exec: spawn: {e}"))
        })?;

    let child_pid = child.id().unwrap_or(0);
    let stdin_pipe = child.stdin.take();
    let stdout_pipe = child.stdout.take();
    let stderr_pipe = child.stderr.take();

    // Signal channel: drain tasks signal the watchdog when both pipes are empty.
    let (drain_done_tx, drain_done_rx) = tokio::sync::oneshot::channel::<()>();

    // CancellationToken: set by the stdin forwarder when the client disconnects.
    // The wait/watchdog selects on this as a third arm so it kills the child
    // immediately rather than waiting for the full timeout.
    let client_gone = CancellationToken::new();

    // Stdout drain task.
    let tx_out = tx.clone();
    let stdout_task = tokio::spawn(async move {
        if let Some(mut pipe) = stdout_pipe {
            let mut buf = vec![0u8; STREAM_CHUNK_BYTES];
            loop {
                match pipe.read(&mut buf).await {
                    Ok(0) | Err(_) => break,
                    Ok(n) => {
                        // get(..n) avoids indexing_slicing: n comes from read()
                        // which guarantees n <= buf.len().
                        if let Some(chunk) = buf.get(..n) {
                            let chunk = chunk.to_vec();
                            if tx_out
                                .send(Ok(sandbox_v1::ExecResponse {
                                    msg: Some(RespMsg::Stdout(chunk)),
                                }))
                                .await
                                .is_err()
                            {
                                break;
                            }
                        }
                    }
                }
            }
        }
    });

    // Stderr drain task.
    let tx_err = tx.clone();
    let stderr_task = tokio::spawn(async move {
        if let Some(mut pipe) = stderr_pipe {
            let mut buf = vec![0u8; STREAM_CHUNK_BYTES];
            loop {
                match pipe.read(&mut buf).await {
                    Ok(0) | Err(_) => break,
                    Ok(n) => {
                        if let Some(chunk) = buf.get(..n) {
                            let chunk = chunk.to_vec();
                            if tx_err
                                .send(Ok(sandbox_v1::ExecResponse {
                                    msg: Some(RespMsg::Stderr(chunk)),
                                }))
                                .await
                                .is_err()
                            {
                                break;
                            }
                        }
                    }
                }
            }
        }
    });

    // Drain joiner: waits for both drain tasks and fires the done signal.
    let drain_join = tokio::spawn(async move {
        let _ = tokio::join!(stdout_task, stderr_task);
        let _ = drain_done_tx.send(());
    });

    // Stdin forwarder task: reads the client stream and writes to the child.
    // Cancels the client_gone token ONLY on a stream error (true disconnect).
    // A clean Ok(None) (half-close, client done sending stdin) is normal and
    // does NOT cancel: the child may keep producing output. This mirrors Go's
    // stream.Context() which cancels only on connection drop, not on a clean
    // stdin close.
    let client_gone_stdin = client_gone.clone();
    let stdin_fwd_task = tokio::spawn(async move {
        use sandbox_v1::exec_request::Msg as ReqMsg;
        let mut pipe = stdin_pipe;
        let mut stream = stream;
        loop {
            match stream.message().await {
                Ok(Some(msg)) => match msg.msg {
                    Some(ReqMsg::Stdin(bytes)) => {
                        if let Some(ref mut p) = pipe
                            && p.write_all(&bytes).await.is_err()
                        {
                            break;
                        }
                    }
                    Some(ReqMsg::StdinClose(true)) => {
                        // Close stdin pipe so the child sees EOF on its stdin.
                        pipe.take();
                    }
                    Some(ReqMsg::StdinClose(false))
                    | Some(ReqMsg::Resize(_))
                    | Some(ReqMsg::Open(_))
                    | None => {}
                },
                // Ok(None): client cleanly closed their send half. Normal EOF
                // on stdin; do not treat as a disconnect.
                Ok(None) => {
                    break;
                }
                // Err: transport error - the client is truly gone.
                Err(_) => {
                    client_gone_stdin.cancel();
                    break;
                }
            }
        }
        // Dropping pipe here sends EOF to the child stdin.
        drop(pipe);
    });

    // Kill watchdog: waits for drain done, client disconnect, OR timeout.
    // Client disconnect and timeout both result in an immediate SIGKILL of the
    // process group; this matches Go's behavior where stream.Context() cancels
    // and kills the child as soon as the client disconnects.
    let tx_exit = tx.clone();
    let wait_task = tokio::spawn(async move {
        let timed_out;
        tokio::select! {
            _ = drain_done_rx => {
                timed_out = false;
            }
            _ = client_gone.cancelled() => {
                // Client disconnected: kill immediately.
                timed_out = false;
                crate::sys::pty::kill_pgroup(child_pid);
                // Brief pause to let drain tasks see the EOF from the killed child.
                tokio::time::sleep(Duration::from_millis(200)).await;
            }
            _ = tokio::time::sleep(timeout) => {
                timed_out = true;
                crate::sys::pty::kill_pgroup(child_pid);
                // Brief pause to let drain tasks see the EOF from the killed child.
                tokio::time::sleep(Duration::from_millis(200)).await;
            }
        }

        let exit_code = match child.wait().await {
            Ok(status) => {
                if timed_out {
                    124
                } else {
                    // Signal-killed processes report no code; mirror Go's -1.
                    status.code().unwrap_or(-1)
                }
            }
            Err(_) => -1,
        };

        let exec_time_ms = start.elapsed().as_micros() as f64 / 1000.0;
        // Log only the exit code and timing; never the command or output.
        tracing::info!(exit_code, exec_time_ms, "exec: process exited");
        let _ = tx_exit
            .send(Ok(exit_frame(exit_code, exec_time_ms, String::new())))
            .await;
    });

    let _ = tokio::join!(wait_task, drain_join, stdin_fwd_task);
    Ok(())
}

// ---------------------------------------------------------------------------
// PTY exec
// ---------------------------------------------------------------------------

async fn exec_pty(
    env: Arc<ConfiguredEnv>,
    open: sandbox_v1::ExecOpen,
    stream: tonic::Streaming<sandbox_v1::ExecRequest>,
    tx: mpsc::Sender<Result<sandbox_v1::ExecResponse, Status>>,
) -> Result<(), Status> {
    use crate::sys::pty as pty_sys;

    // openpty is Linux-only; sys::pty::openpty returns Err(Unsupported) on
    // other platforms and we convert that to an Unimplemented status so tests
    // on macOS compile and the error is clear.
    let pty_pair = pty_sys::openpty().map_err(|e| {
        if e.kind() == std::io::ErrorKind::Unsupported {
            Status::unimplemented("exec: PTY exec is only available on Linux")
        } else {
            Status::internal(format!("exec: openpty: {e}"))
        }
    })?;

    let start = Instant::now();

    let timeout_secs = if open.timeout_seconds > 0 {
        open.timeout_seconds as u64
    } else {
        DEFAULT_TIMEOUT_SECS
    };
    let timeout = Duration::from_secs(timeout_secs);

    let cwd = if open.cwd.is_empty() {
        "/workspace".to_string()
    } else {
        open.cwd.clone()
    };

    // Environment merge. Filter out any inherited TERM then set it from
    // PtyOptions.term (or default), matching pty.go:155.
    let configured = env.snapshot().await;
    let mut merged_env = build_merged_env(&configured, &open.env);
    merged_env.retain(|kv| !kv.starts_with("TERM="));
    let term_val = open
        .pty
        .as_ref()
        .filter(|p| !p.term.is_empty())
        .map(|p| p.term.as_str())
        .unwrap_or("xterm-256color");
    merged_env.push(format!("TERM={term_val}"));

    // Initial window size from PtyOptions.size.
    let (init_cols, init_rows) = open
        .pty
        .as_ref()
        .and_then(|p| p.size.as_ref())
        .map(|s| (s.cols, s.rows))
        .unwrap_or((80, 24));

    // Apply initial window size to the PTY master.
    pty_sys::set_winsize(pty_pair.master.as_raw_fd(), init_cols, init_rows)
        .map_err(|e| Status::internal(format!("exec: set_winsize: {e}")))?;

    // Dup the master fd for the resize ioctl path (the main master fd will be
    // consumed into a tokio::fs::File which does not expose AsRawFd for ioctls).
    let resize_fd = pty_sys::dup_master_for_resize(&pty_pair.master)
        .map_err(|e| Status::internal(format!("exec: dup master for resize: {e}")))?;

    // Split the slave into three std::fs::File values for stdin/stdout/stderr.
    let (slave_stdin, slave_stdout, slave_stderr) = pty_sys::slave_stdio_files(pty_pair.slave)
        .map_err(|e| Status::internal(format!("exec: slave_stdio_files: {e}")))?;

    // Shell: use open.command if set, else /bin/sh (pty.go:124-127).
    let shell_cmd = if open.command.is_empty() {
        "/bin/sh".to_string()
    } else {
        open.command.clone()
    };

    // Build the command. We use std::process::Command so we can register
    // pre_exec via CommandExt; tokio::process::Command::from converts it.
    let mut std_cmd = std::process::Command::new("/bin/sh");
    std_cmd
        .arg("-c")
        .arg(&shell_cmd)
        .current_dir(&cwd)
        .envs(merged_env.iter().filter_map(|kv| {
            kv.split_once('=').map(|(k, v)| (k.to_string(), v.to_string()))
        }))
        .stdin(slave_stdin)
        .stdout(slave_stdout)
        .stderr(slave_stderr);

    // Register setsid + TIOCSCTTY in pre_exec (see sys/pty.rs for SAFETY).
    pty_sys::apply_pty_session_leader(&mut std_cmd)
        .map_err(|e| Status::internal(format!("exec: apply_pty_session_leader: {e}")))?;

    // Convert the master to an AsyncFd for epoll-driven IO. tokio::fs::File
    // uses spawn_blocking and does not work correctly with O_NONBLOCK on a PTY
    // master character device: it would return EAGAIN immediately on reads
    // before data arrives. AsyncFd uses epoll readiness so the read is only
    // attempted when the kernel reports data available, which is correct for
    // character devices and sockets. master_to_async_file sets O_NONBLOCK and
    // moves the fd into a std::fs::File; we recover the fd via into_std +
    // into_raw_fd and wrap it in AsyncFd.
    let master_async = pty_sys::master_to_async_file(pty_pair.master)
        .map_err(|e| Status::internal(format!("exec: master_to_async_file: {e}")))?;
    // into_std() converts the tokio async file back to a std::fs::File.
    let master_std = master_async
        .into_std()
        .await;
    let master_afd = tokio::io::unix::AsyncFd::new(master_std)
        .map_err(|e| Status::internal(format!("exec: AsyncFd::new: {e}")))?;
    let master_afd = Arc::new(master_afd);
    let master_write_afd = Arc::clone(&master_afd);

    let mut child = tokio::process::Command::from(std_cmd)
        .spawn()
        .map_err(|e| {
            let _ = tx.try_send(Ok(exit_frame(1, 0.0, format!("start shell: {e}"))));
            Status::internal(format!("exec: spawn pty shell: {e}"))
        })?;

    let child_pid = child.id().unwrap_or(0);

    // CancellationToken: set by the writer task when the client disconnects.
    // The wait/watchdog selects on this as a third arm for immediate kill.
    let client_gone = CancellationToken::new();

    // PTY reader task: drain master output -> ExecResponse::Stdout.
    // Uses AsyncFd::readable() for epoll-driven readiness, then a non-blocking
    // std::io::Read to pull available data. This is correct for PTY master
    // character devices: tokio::fs::File uses spawn_blocking and would return
    // EAGAIN immediately in non-blocking mode before any data arrives.
    // AsyncFd registers the fd with epoll so readable() only wakes when the
    // kernel reports data available (EPOLLIN).
    // Declared mut so the wait_task can take &mut reader_task in select!.
    let tx_pty = tx.clone();
    let master_read_afd = Arc::clone(&master_afd);
    let mut reader_task = tokio::spawn(async move {
        let mut buf = vec![0u8; STREAM_CHUNK_BYTES];
        loop {
            // Wait for the fd to be readable (epoll-driven, no busy wait).
            let mut guard = match master_read_afd.readable().await {
                Ok(g) => g,
                Err(_) => break,
            };
            // Perform a non-blocking read now that epoll says data is ready.
            match guard.get_inner().read(&mut buf) {
                Ok(0) => {
                    // 0-byte read: treat as EOF (should not normally occur on PTY).
                    break;
                }
                Ok(n) => {
                    if let Some(chunk) = buf.get(..n) {
                        let chunk = chunk.to_vec();
                        if tx_pty
                            .send(Ok(sandbox_v1::ExecResponse {
                                msg: Some(RespMsg::Stdout(chunk)),
                            }))
                            .await
                            .is_err()
                        {
                            break;
                        }
                    }
                    // Guard drops here, re-arming epoll for the next iteration.
                }
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                    // EAGAIN: must clear the readiness flag before looping so
                    // epoll is re-armed and we wait properly on the next call.
                    guard.clear_ready();
                }
                Err(_) => {
                    // EIO when all slave fds close (shell exited), or other error.
                    break;
                }
            }
        }
    });

    // PTY writer task: forward stdin bytes and resize events from the client.
    // Cancels the client_gone token ONLY on a stream error (true disconnect).
    // Ok(None) is a clean half-close (client done sending); the shell may still
    // be running and producing output, so we do not treat it as a disconnect.
    // Writes go through the AsyncFd's inner std::fs::File directly (PTY writes
    // are typically small and do not block for long; no writable wait needed).
    let client_gone_writer = client_gone.clone();
    let writer_task = tokio::spawn(async move {
        use sandbox_v1::exec_request::Msg as ReqMsg;
        let mut stream = stream;
        // resize_fd is owned here so it is closed when this task exits.
        let resize_fd = resize_fd;
        loop {
            match stream.message().await {
                Ok(Some(msg)) => match msg.msg {
                    Some(ReqMsg::Stdin(bytes)) => {
                        if master_write_afd.get_ref().write_all(&bytes).is_err() {
                            client_gone_writer.cancel();
                            break;
                        }
                    }
                    Some(ReqMsg::Resize(ws)) => {
                        // Apply window resize via TIOCSWINSZ. Errors are non-fatal.
                        let _ = crate::sys::pty::set_winsize(
                            resize_fd.as_raw_fd(),
                            ws.cols,
                            ws.rows,
                        );
                    }
                    Some(ReqMsg::StdinClose(_)) | Some(ReqMsg::Open(_)) | None => {}
                },
                // Ok(None): clean half-close; not a disconnect.
                Ok(None) => {
                    break;
                }
                // Err: transport error - the client is truly gone.
                Err(_) => {
                    client_gone_writer.cancel();
                    break;
                }
            }
        }
        drop(resize_fd);
    });

    // Wait/watchdog task: awaits reader completion, client disconnect, OR
    // timeout. Client disconnect and timeout both kill the child immediately.
    // reader_task is aborted on the cancel/timeout arms so no task is left
    // detached: every exit arm either awaits or aborts reader_task.
    let tx_exit = tx.clone();
    let wait_task = tokio::spawn(async move {
        let timed_out;
        tokio::select! {
            _ = &mut reader_task => {
                // Normal completion: the shell exited and closed the PTY.
                timed_out = false;
            }
            _ = client_gone.cancelled() => {
                // Client disconnected: kill immediately, then abort reader.
                timed_out = false;
                crate::sys::pty::kill_pgroup(child_pid);
                reader_task.abort();
                let _ = reader_task.await;
                tokio::time::sleep(Duration::from_millis(200)).await;
            }
            _ = tokio::time::sleep(timeout) => {
                timed_out = true;
                crate::sys::pty::kill_pgroup(child_pid);
                reader_task.abort();
                let _ = reader_task.await;
                tokio::time::sleep(Duration::from_millis(200)).await;
            }
        }

        let exit_code = match child.wait().await {
            Ok(status) => {
                if timed_out {
                    124
                } else {
                    // Signal-killed processes report no code; mirror Go's -1.
                    status.code().unwrap_or(-1)
                }
            }
            Err(_) => -1,
        };

        let exec_time_ms = start.elapsed().as_micros() as f64 / 1000.0;
        let _ = tx_exit
            .send(Ok(exit_frame(exit_code, exec_time_ms, String::new())))
            .await;
    });

    let _ = tokio::join!(wait_task, writer_task);
    Ok(())
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

/// Build an ExecResponse carrying an ExecExit terminal frame.
fn exit_frame(exit_code: i32, exec_time_ms: f64, error: String) -> sandbox_v1::ExecResponse {
    sandbox_v1::ExecResponse {
        msg: Some(RespMsg::Exit(sandbox_v1::ExecExit {
            exit_code,
            exec_time_ms,
            error,
        })),
    }
}
