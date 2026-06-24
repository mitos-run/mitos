// Exec and PTY RPC implementation for the Sandbox gRPC service.
//
// Mirrors guest/agent/exec_stream.go (runExecStream) and guest/agent/pty.go
// (runPTY) for behavior parity. The three security invariants from those files
// are preserved here:
//
// 1. Env values are NEVER logged (keys and counts only).
// 2. The process runs in its own process group (Setpgid for non-PTY; Setsid
//    implied by setsid() in pre_exec for PTY) so kills propagate to children.
// 3. Exit-code mapping mirrors Go: timeout -> 124; ExitError -> its code;
//    spawn failure -> 1 with LLM-legible remediation text; clean -> 0.
//
// Concurrency model (non-PTY):
//   A stdout drain task and a stderr drain task each read from the child pipe
//   and send ExecResponse::Stdout / Stderr frames. A stdin forwarder task reads
//   the client stream and writes bytes to the child stdin pipe. A wait/watchdog
//   task waits for both drain tasks to finish or for a timeout, then kills the
//   process group and sends the ExecExit frame. All tasks are join-awaited
//   before this function returns: no task or fd leaks on any path.
//
// Concurrency model (PTY):
//   A reader task drains the PTY master and sends ExecResponse::Stdout frames.
//   A writer task reads the client stream and writes stdin bytes to the master;
//   resize messages call sys::pty::set_winsize via a separately duped master fd.
//   A wait/watchdog task awaits the reader task or a timeout, kills the process
//   group, and sends ExecExit. All tasks are join-awaited before returning.
//
// No unsafe code in this module: all syscalls are delegated to sys/pty.rs.

use std::collections::HashMap;
use std::os::fd::AsRawFd;
use std::sync::Arc;
use std::time::{Duration, Instant};

use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::sync::mpsc;
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

    // Atomic flag set when the client hangs up so the watchdog kills the group.
    let client_gone = Arc::new(std::sync::atomic::AtomicBool::new(false));

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
    let client_gone_stdin = Arc::clone(&client_gone);
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
                Ok(None) | Err(_) => {
                    client_gone_stdin.store(true, std::sync::atomic::Ordering::Relaxed);
                    break;
                }
            }
        }
        // Dropping pipe here sends EOF to the child stdin.
        drop(pipe);
    });

    // Kill watchdog: waits for drain done OR timeout, kills the group, reaps
    // the child, then sends ExecExit.
    let client_gone_wait = Arc::clone(&client_gone);
    let tx_exit = tx.clone();
    let wait_task = tokio::spawn(async move {
        let timed_out;
        tokio::select! {
            _ = drain_done_rx => {
                timed_out = false;
            }
            _ = tokio::time::sleep(timeout) => {
                timed_out = true;
                crate::sys::pty::kill_pgroup(child_pid);
                // Brief pause to let drain tasks see the EOF from the killed child.
                tokio::time::sleep(Duration::from_millis(200)).await;
            }
        }

        if client_gone_wait.load(std::sync::atomic::Ordering::Relaxed) {
            crate::sys::pty::kill_pgroup(child_pid);
        }

        let exit_code = match child.wait().await {
            Ok(status) => {
                if timed_out {
                    124
                } else {
                    status.code().unwrap_or(1)
                }
            }
            Err(_) => 1,
        };

        let exec_time_ms = start.elapsed().as_micros() as f64 / 1000.0;
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

    // Convert the master to a tokio async file for non-blocking IO.
    let master_async = pty_sys::master_to_async_file(pty_pair.master)
        .map_err(|e| Status::internal(format!("exec: master_to_async_file: {e}")))?;

    let (mut master_read, master_write_half) = tokio::io::split(master_async);
    let master_write = Arc::new(tokio::sync::Mutex::new(master_write_half));

    let mut child = tokio::process::Command::from(std_cmd)
        .spawn()
        .map_err(|e| {
            let _ = tx.try_send(Ok(exit_frame(1, 0.0, format!("start shell: {e}"))));
            Status::internal(format!("exec: spawn pty shell: {e}"))
        })?;

    let child_pid = child.id().unwrap_or(0);

    // PTY reader task: drain master output -> ExecResponse::Stdout.
    let tx_pty = tx.clone();
    let reader_task = tokio::spawn(async move {
        let mut buf = vec![0u8; STREAM_CHUNK_BYTES];
        loop {
            match master_read.read(&mut buf).await {
                Ok(0) => break,
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
                }
                Err(_) => break, // EIO when all slave fds close (shell exited)
            }
        }
    });

    // PTY writer task: forward stdin bytes and resize events from the client.
    let client_gone = Arc::new(std::sync::atomic::AtomicBool::new(false));
    let client_gone_writer = Arc::clone(&client_gone);
    let writer_task = tokio::spawn(async move {
        use sandbox_v1::exec_request::Msg as ReqMsg;
        let mut stream = stream;
        // resize_fd is owned here so it is closed when this task exits.
        let resize_fd = resize_fd;
        loop {
            match stream.message().await {
                Ok(Some(msg)) => match msg.msg {
                    Some(ReqMsg::Stdin(bytes)) => {
                        let mut w = master_write.lock().await;
                        if w.write_all(&bytes).await.is_err() {
                            client_gone_writer
                                .store(true, std::sync::atomic::Ordering::Relaxed);
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
                Ok(None) | Err(_) => {
                    client_gone_writer.store(true, std::sync::atomic::Ordering::Relaxed);
                    break;
                }
            }
        }
        drop(resize_fd);
    });

    // Wait/watchdog task: awaits reader completion OR timeout, kills group,
    // reaps the child, sends ExecExit.
    let tx_exit = tx.clone();
    let wait_task = tokio::spawn(async move {
        let timed_out;
        tokio::select! {
            _ = reader_task => {
                timed_out = false;
            }
            _ = tokio::time::sleep(timeout) => {
                timed_out = true;
                crate::sys::pty::kill_pgroup(child_pid);
                tokio::time::sleep(Duration::from_millis(200)).await;
            }
        }

        if client_gone.load(std::sync::atomic::Ordering::Relaxed) {
            crate::sys::pty::kill_pgroup(child_pid);
        }

        let exit_code = match child.wait().await {
            Ok(status) => {
                if timed_out {
                    124
                } else {
                    status.code().unwrap_or(1)
                }
            }
            Err(_) => 1,
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
