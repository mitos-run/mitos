// Transport: newline-delimited JSON framing over Unix sockets (host tests) and
// AF_VSOCK (production). Mirrors handleConnection and listenVsock in
// guest/agent/main.go and handleExecStream in guest/agent/exec_stream.go.

use crate::handlers;
use crate::protocol::{ExecStreamFrame, Request, Response};
use std::collections::HashMap;
use std::io::BufReader;
use std::os::unix::net::UnixListener;
use std::sync::{Arc, Mutex};

// MaxMessageBytes matches vsock.MaxMessageBytes = 96 << 20 (96 MiB). The line
// reader enforces this cap so a client that never sends a newline cannot OOM
// PID 1 (which would crash the VM). Mirrors Go's scanner.Buffer cap.
const MAX_MESSAGE_BYTES: usize = 96 << 20;

// streamChunkBytes matches streamChunkBytes in exec_stream.go: 32 KiB per read.
const STREAM_CHUNK_BYTES: usize = 32 << 10;

// ---------------------------------------------------------------------------
// Bounded line reader: enforces MAX_MESSAGE_BYTES, mirrors Go scanner.Buffer.
// ---------------------------------------------------------------------------

/// Reads one newline-delimited line into buf. Returns Ok(0) at EOF. Returns
/// Err(InvalidData) if the line exceeds limit bytes, matching Go's
/// scanner.Buffer cap so a guest-facing socket cannot be OOM-killed by a
/// client that never sends a newline.
///
/// The function appends to buf from its current length; the caller must clear
/// buf between calls if a fresh line is wanted.
pub fn read_line_bounded<R: std::io::BufRead>(
    reader: &mut R,
    buf: &mut Vec<u8>,
    limit: usize,
) -> std::io::Result<usize> {
    let start = buf.len();
    loop {
        let available = match reader.fill_buf() {
            Ok(b) => b,
            Err(ref e) if e.kind() == std::io::ErrorKind::Interrupted => continue,
            Err(e) => return Err(e),
        };
        if available.is_empty() {
            // EOF.
            return Ok(buf.len() - start);
        }
        match available.iter().position(|&b| b == b'\n') {
            Some(i) => {
                let slice = &available[..=i];
                if (buf.len() - start) + slice.len() > limit {
                    return Err(std::io::Error::new(
                        std::io::ErrorKind::InvalidData,
                        "line exceeds MaxMessageBytes",
                    ));
                }
                buf.extend_from_slice(slice);
                let consumed = i + 1;
                reader.consume(consumed);
                return Ok(buf.len() - start);
            }
            None => {
                let slice = available;
                if (buf.len() - start) + slice.len() > limit {
                    return Err(std::io::Error::new(
                        std::io::ErrorKind::InvalidData,
                        "line exceeds MaxMessageBytes",
                    ));
                }
                buf.extend_from_slice(slice);
                let consumed = slice.len();
                reader.consume(consumed);
            }
        }
    }
}

// ---------------------------------------------------------------------------
// Unix-socket listener: test / non-KVM entry point.
// ---------------------------------------------------------------------------

/// Bind a UnixListener at path, then accept connections in a loop. Each
/// connection is handled on its own thread. This is the entry point used by
/// integration tests (and the Go fallback path when AF_VSOCK is unavailable).
///
/// Mirrors the fallback branch of listenVsock in guest/agent/main.go.
pub fn serve_unix(path: &str, env: Arc<Mutex<HashMap<String, String>>>) {
    let listener = UnixListener::bind(path).expect("bind unix socket");
    eprintln!("sandbox-agent: listening on unix socket {}", path);
    for stream in listener.incoming() {
        match stream {
            Ok(s) => {
                let env2 = Arc::clone(&env);
                std::thread::spawn(move || handle_conn(s, env2));
            }
            Err(e) => {
                eprintln!("sandbox-agent: accept error: {}", e);
            }
        }
    }
}

// ---------------------------------------------------------------------------
// AF_VSOCK listener: production entry point (Linux only).
// ---------------------------------------------------------------------------

// On non-Linux the vsock constants and syscall interface are absent; provide a
// clear no-op so the crate still compiles on macOS for host-side development.

/// Bind an AF_VSOCK socket at VMADDR_CID_ANY:port and accept connections in a
/// loop. Each connection is handled on its own thread. This is the production
/// entry point inside the Firecracker VM.
///
/// Mirrors listenVsock in guest/agent/main.go: socket / bind / listen / accept
/// with raw libc calls because the Rust std library does not understand AF_VSOCK.
///
/// On non-Linux platforms this function prints a "not supported" message and
/// returns immediately; serve_unix is the path for host tests on macOS.
pub fn serve_vsock(port: u32, env: Arc<Mutex<HashMap<String, String>>>) {
    #[cfg(target_os = "linux")]
    {
        serve_vsock_linux(port, env);
    }
    #[cfg(not(target_os = "linux"))]
    {
        let _ = (port, env);
        eprintln!("sandbox-agent: AF_VSOCK not supported on this platform; use serve_unix for tests");
    }
}

#[cfg(target_os = "linux")]
fn serve_vsock_linux(port: u32, env: Arc<Mutex<HashMap<String, String>>>) {
    // AF_VSOCK = 40 on Linux (not in libc stable yet; use the raw constant).
    const AF_VSOCK: libc::c_int = 40;
    // VMADDR_CID_ANY = 0xFFFFFFFF: listen on all CIDs.
    const VMADDR_CID_ANY: u32 = 0xFFFF_FFFF;

    let fd = unsafe { libc::socket(AF_VSOCK, libc::SOCK_STREAM, 0) };
    if fd < 0 {
        // AF_VSOCK not available (not inside a VM, no vsock driver). Mirror the Go
        // fallback message exactly.
        let err = std::io::Error::last_os_error();
        eprintln!(
            "sandbox-agent: vsock not available ({}), falling back to unix socket",
            err
        );
        let sock_path = format!("/tmp/sandbox-agent-{}.sock", port);
        let _ = std::fs::remove_file(&sock_path);
        serve_unix(&sock_path, env);
        return;
    }

    // sockaddr_vm layout (Linux uapi/linux/vm_sockets.h):
    //   u16  svm_family     (AF_VSOCK)
    //   u16  svm_reserved1
    //   u32  svm_port
    //   u32  svm_cid
    //   u8   svm_zero[4]
    // Total: 16 bytes. We construct it as a raw byte array to avoid depending on
    // a struct that is not in the stable libc crate yet.
    let mut addr = [0u8; 16];
    // svm_family (LE u16)
    let family_bytes = (AF_VSOCK as u16).to_ne_bytes();
    addr[0] = family_bytes[0];
    addr[1] = family_bytes[1];
    // svm_port (LE u32) at offset 4
    let port_bytes = port.to_ne_bytes();
    addr[4] = port_bytes[0];
    addr[5] = port_bytes[1];
    addr[6] = port_bytes[2];
    addr[7] = port_bytes[3];
    // svm_cid (LE u32) at offset 8
    let cid_bytes = VMADDR_CID_ANY.to_ne_bytes();
    addr[8] = cid_bytes[0];
    addr[9] = cid_bytes[1];
    addr[10] = cid_bytes[2];
    addr[11] = cid_bytes[3];

    let bind_ret = unsafe {
        libc::bind(
            fd,
            addr.as_ptr() as *const libc::sockaddr,
            addr.len() as libc::socklen_t,
        )
    };
    if bind_ret < 0 {
        let err = std::io::Error::last_os_error();
        eprintln!("sandbox-agent: vsock bind: {}", err);
        unsafe { libc::close(fd) };
        return;
    }

    let listen_ret = unsafe { libc::listen(fd, 128) };
    if listen_ret < 0 {
        let err = std::io::Error::last_os_error();
        eprintln!("sandbox-agent: vsock listen: {}", err);
        unsafe { libc::close(fd) };
        return;
    }

    eprintln!(
        "sandbox-agent: listening on vsock CID=any port={}",
        port
    );

    loop {
        let mut peer_addr = [0u8; 16];
        let mut peer_len = peer_addr.len() as libc::socklen_t;
        let conn_fd = unsafe {
            libc::accept(
                fd,
                peer_addr.as_mut_ptr() as *mut libc::sockaddr,
                &mut peer_len,
            )
        };
        if conn_fd < 0 {
            let err = std::io::Error::last_os_error();
            eprintln!("sandbox-agent: accept error: {}", err);
            continue;
        }
        // Wrap the raw fd in an OwnedFd so it is closed when the thread exits.
        let env2 = Arc::clone(&env);
        std::thread::spawn(move || {
            // Safety: conn_fd is a valid, owned fd returned by accept.
            let stream = unsafe { VsockStream::from_raw_fd(conn_fd) };
            handle_conn(stream, env2);
        });
    }
}

// ---------------------------------------------------------------------------
// VsockStream: thin wrapper around a raw fd that implements Read + Write.
// Used only on Linux for the AF_VSOCK accept path. The fd is closed on drop.
// ---------------------------------------------------------------------------

#[cfg(target_os = "linux")]
struct VsockStream {
    fd: libc::c_int,
}

#[cfg(target_os = "linux")]
impl VsockStream {
    /// Take ownership of a raw fd. Safety: fd must be valid and not shared.
    unsafe fn from_raw_fd(fd: libc::c_int) -> Self {
        VsockStream { fd }
    }
}

#[cfg(target_os = "linux")]
impl std::io::Read for VsockStream {
    fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
        let n = unsafe { libc::read(self.fd, buf.as_mut_ptr() as *mut libc::c_void, buf.len()) };
        if n < 0 {
            Err(std::io::Error::last_os_error())
        } else {
            Ok(n as usize)
        }
    }
}

#[cfg(target_os = "linux")]
impl std::io::Write for VsockStream {
    fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
        let n = unsafe { libc::write(self.fd, buf.as_ptr() as *const libc::c_void, buf.len()) };
        if n < 0 {
            Err(std::io::Error::last_os_error())
        } else {
            Ok(n as usize)
        }
    }

    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

#[cfg(target_os = "linux")]
impl Drop for VsockStream {
    fn drop(&mut self) {
        unsafe { libc::close(self.fd) };
    }
}

// ---------------------------------------------------------------------------
// Per-connection handler: shared by both Unix and vsock paths.
// ---------------------------------------------------------------------------

/// Handle one connection. Reads newline-delimited JSON requests, dispatches
/// one-shot types via handlers::dispatch, and for exec_stream takes over the
/// connection to stream output frames followed by exactly one exit frame.
///
/// Mirrors handleConnection in guest/agent/main.go.
pub fn handle_conn<S: std::io::Read + std::io::Write + Send + 'static>(stream: S, env: Arc<Mutex<HashMap<String, String>>>) {
    // Split into independent read and write halves. For exec_stream we need to
    // write to the stream while reading is done; we achieve this by giving the
    // write half to the frame writer and consuming the read half with BufReader.
    // For one-shot types we read one line, write one response, then loop.
    //
    // Because BufReader takes ownership of the stream, we use try_clone-like
    // approaches for the exec_stream path. The cleanest cross-platform approach:
    // wrap the stream in Arc<Mutex<...>> so both the read and write sides share
    // the same underlying I/O object under a lock. The BufReader wraps the
    // Arc<Mutex<...>> through a thin adapter.

    let shared = Arc::new(Mutex::new(stream));

    // A thin newtype that implements Read by locking the shared stream.
    struct ReadHalf<S>(Arc<Mutex<S>>);
    impl<S: std::io::Read> std::io::Read for ReadHalf<S> {
        fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
            self.0.lock().unwrap_or_else(|p| p.into_inner()).read(buf)
        }
    }

    let read_half = ReadHalf(Arc::clone(&shared));
    let mut reader = BufReader::with_capacity(64 * 1024, read_half);

    loop {
        let mut raw = Vec::new();
        let n = match read_line_bounded(&mut reader, &mut raw, MAX_MESSAGE_BYTES) {
            Ok(n) => n,
            Err(e) => {
                eprintln!("sandbox-agent: read error: {}; closing connection", e);
                return;
            }
        };
        if n == 0 {
            // EOF: client closed the connection.
            return;
        }

        // Trim trailing newline then parse as UTF-8 JSON.
        let trimmed = match std::str::from_utf8(&raw) {
            Ok(s) => s.trim_end_matches('\n'),
            Err(e) => {
                eprintln!("sandbox-agent: invalid UTF-8: {}; closing connection", e);
                return;
            }
        };

        let req: Request = match serde_json::from_str(trimmed) {
            Ok(r) => r,
            Err(e) => {
                let resp = Response {
                    ok: false,
                    error: format!("invalid request: {}", e),
                    ..Default::default()
                };
                write_response(&shared, &resp);
                continue;
            }
        };

        match req.r#type.as_str() {
            "exec_stream" => {
                // exec_stream takes over the connection. After this call returns
                // the connection is done; we close by returning from handle_conn.
                match &req.exec_stream {
                    None => {
                        let resp = Response {
                            ok: false,
                            error: "exec_stream request is nil".into(),
                            ..Default::default()
                        };
                        write_response(&shared, &resp);
                    }
                    Some(exec_req) => {
                        handle_exec_stream(&shared, exec_req);
                    }
                }
                return;
            }
            "run_code" | "pty" | "tunnel" => {
                // Out of scope for the spike. Return an error response and close
                // the connection (these types own their connection in Go too).
                let resp = Response {
                    ok: false,
                    error: format!("{} not implemented in spike agent", req.r#type),
                    ..Default::default()
                };
                write_response(&shared, &resp);
                return;
            }
            _ => {
                let resp = handlers::dispatch(&req, &env);
                write_response(&shared, &resp);
            }
        }
    }
}

// Write a one-shot Response as a newline-delimited JSON line.
fn write_response<S: std::io::Write>(shared: &Arc<Mutex<S>>, resp: &Response) {
    let mut buf = match serde_json::to_vec(resp) {
        Ok(b) => b,
        Err(_) => return,
    };
    buf.push(b'\n');
    // Use unwrap_or_else so a poisoned lock (from a panicked pump thread) does
    // not prevent the response from being written. Parity with finding 2.
    let mut guard = shared.lock().unwrap_or_else(|p| p.into_inner());
    let _ = guard.write_all(&buf);
}

// Write one ExecStreamFrame as a newline-delimited JSON line. The caller must
// hold the write lock (or pass the locked guard).
fn write_frame_locked<S: std::io::Write>(guard: &mut S, frame: &ExecStreamFrame) {
    let mut buf = match serde_json::to_vec(frame) {
        Ok(b) => b,
        Err(_) => return,
    };
    buf.push(b'\n');
    let _ = guard.write_all(&buf);
}

// ---------------------------------------------------------------------------
// exec_stream takeover: mirrors handleExecStream in guest/agent/exec_stream.go.
// ---------------------------------------------------------------------------

/// Spawn /bin/sh -c command, stream stdout/stderr as base64 chunk frames, then
/// emit exactly one exit frame. The connection is owned for the duration.
///
/// Concurrency design:
///   - Two reader threads drain stdout and stderr independently so the child
///     cannot block on a full pipe buffer (same hazard solved in handlers.rs
///     handle_exec).
///   - Both threads share a write lock (Arc<Mutex<S>>) so frames are written
///     atomically: no two partial frame bytes can interleave on the wire.
///   - We wait for both reader threads to finish (wg.Wait() equivalent) before
///     writing the exit frame, so the exit frame is always last.
///   - The child is reaped via child.wait() so no zombie is left.
///   - The child is started in its own process group (process_group(0)) and
///     the entire group is killed on timeout via killpg, matching Go's
///     Setpgid + killpg in handleExecStream in exec_stream.go. This prevents
///     orphaned grandchildren accumulating in PID 1 after a timeout.
fn handle_exec_stream<S: std::io::Read + std::io::Write + Send + 'static>(
    shared: &Arc<Mutex<S>>,
    req: &crate::protocol::ExecRequest,
) {
    use std::time::Instant;

    let start = Instant::now();

    let timeout_secs = if req.timeout == 0 { 30 } else { req.timeout };
    let timeout = std::time::Duration::from_secs(timeout_secs as u64);

    // Determine working directory (same defaulting as Go: /workspace, with a
    // macOS dev fallback if that path does not exist).
    let working_dir = if req.working_dir.is_empty() {
        #[cfg(target_os = "linux")]
        {
            "/workspace".to_string()
        }
        #[cfg(not(target_os = "linux"))]
        {
            let ws = std::path::Path::new("/workspace");
            if ws.exists() {
                "/workspace".to_string()
            } else {
                std::env::temp_dir().to_string_lossy().into_owned()
            }
        }
    } else {
        req.working_dir.clone()
    };

    // Build the command. On Unix, put the child in its own process group so
    // that a timeout kill via killpg reaches all grandchildren too. This
    // mirrors Go's SysProcAttr{Setpgid: true} in exec_stream.go.
    let mut cmd = std::process::Command::new("/bin/sh");
    cmd.arg("-c")
        .arg(&req.command)
        .current_dir(&working_dir)
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped());

    #[cfg(unix)]
    {
        // process_group(0) means: use the child's own PID as its PGID, so the
        // child is the leader of a new process group. Stable since Rust 1.64.
        use std::os::unix::process::CommandExt;
        cmd.process_group(0);
    }

    let mut child = match cmd.spawn() {
        Err(e) => {
            let frame = ExecStreamFrame {
                kind: "exit".into(),
                exit_code: 1,
                error: format!("start: {}", e),
                exec_time_ms: start.elapsed().as_micros() as f64 / 1000.0,
                ..Default::default()
            };
            // Use unwrap_or_else so a poisoned lock does not strand the exit
            // frame. Mirrors finding 2.
            let mut guard = shared.lock().unwrap_or_else(|p| p.into_inner());
            write_frame_locked(&mut *guard, &frame);
            return;
        }
        Ok(c) => c,
    };

    let stdout_pipe = child.stdout.take().expect("stdout was piped");
    let stderr_pipe = child.stderr.take().expect("stderr was piped");

    // Each reader thread writes chunk frames to the shared connection under the
    // write lock so no two frames interleave.
    let shared_out = Arc::clone(shared);
    let shared_err = Arc::clone(shared);

    let stdout_thread = std::thread::spawn(move || {
        pump_stream(stdout_pipe, "stdout", &shared_out);
    });
    let stderr_thread = std::thread::spawn(move || {
        pump_stream(stderr_pipe, "stderr", &shared_err);
    });

    // Poll for child exit or timeout. On timeout, kill the whole process group
    // (mirrors Go's killpg in exec_stream.go) so grandchildren are not
    // orphaned inside PID 1.
    let poll_interval = std::time::Duration::from_millis(10);
    let deadline = Instant::now() + timeout;
    let timed_out = loop {
        match child.try_wait() {
            Ok(Some(_)) => break false,
            Ok(None) => {
                if Instant::now() >= deadline {
                    // Kill the entire process group. The child's PID equals its
                    // PGID because we used process_group(0) above. On non-Unix
                    // builds fall back to killing just the child process.
                    #[cfg(unix)]
                    {
                        let pgid = child.id() as libc::pid_t;
                        unsafe { libc::killpg(pgid, libc::SIGKILL) };
                    }
                    #[cfg(not(unix))]
                    {
                        let _ = child.kill();
                    }
                    let _ = child.wait();
                    break true;
                }
                std::thread::sleep(poll_interval);
            }
            Err(e) => {
                // Pathological OS error from try_wait (e.g. ECHILD from a
                // rogue waitpid call). Treat as exited to avoid looping
                // forever; the exit_code will be 1 via the wait() below.
                eprintln!("sandbox-agent: try_wait error: {}; treating as exited", e);
                break false;
            }
        }
    };

    // Wait for both pump threads to finish before reading the exit status.
    // This ensures all chunk frames are written before the exit frame.
    let _ = stdout_thread.join();
    let _ = stderr_thread.join();

    let exit_code: i32;
    let exec_time_ms: f64;
    if timed_out {
        exit_code = 124;
        exec_time_ms = start.elapsed().as_micros() as f64 / 1000.0;
    } else {
        // Child already exited (poll loop saw Some); call wait() to reap and
        // get the exit status.
        let status = child.wait().ok();
        exit_code = status
            .and_then(|s| s.code())
            .unwrap_or(1);
        exec_time_ms = start.elapsed().as_micros() as f64 / 1000.0;
    }

    let frame = ExecStreamFrame {
        kind: "exit".into(),
        exit_code,
        exec_time_ms,
        ..Default::default()
    };
    // Use unwrap_or_else so a poisoned lock (from a panicked pump thread) does
    // not prevent the exit frame from being written, avoiding a hung host client.
    let mut guard = shared.lock().unwrap_or_else(|p| p.into_inner());
    write_frame_locked(&mut *guard, &frame);
}

/// Drain a pipe (stdout or stderr), emitting one chunk frame per read.
/// Frames are written atomically under the shared write lock.
fn pump_stream<R: std::io::Read, S: std::io::Write>(
    mut pipe: R,
    stream_name: &str,
    shared: &Arc<Mutex<S>>,
) {
    let mut buf = vec![0u8; STREAM_CHUNK_BYTES];
    loop {
        let n = match pipe.read(&mut buf) {
            Ok(0) => break,         // EOF
            Ok(n) => n,
            Err(_) => break,
        };
        let frame = ExecStreamFrame {
            kind: "chunk".into(),
            stream: stream_name.to_string(),
            data: buf[..n].to_vec(),
            ..Default::default()
        };
        // Use unwrap_or_else so a poisoned lock does not stall the pump thread.
        // Mirrors finding 2: the exit frame must still be writable after any
        // pump thread panic.
        let mut guard = shared.lock().unwrap_or_else(|p| p.into_inner());
        write_frame_locked(&mut *guard, &frame);
    }
}

// ---------------------------------------------------------------------------
// Unit tests.
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Cursor;

    #[test]
    fn read_line_bounded_rejects_oversized_line() {
        // Simulate a line of 6 bytes with no newline and a limit of 5.
        // The helper must return an InvalidData error rather than buffering
        // the entire line, proving the OOM-DoS cap is enforced.
        let data = b"AAAAAA"; // 6 bytes, no newline
        let mut reader = std::io::BufReader::new(Cursor::new(data));
        let mut buf = Vec::new();
        let result = read_line_bounded(&mut reader, &mut buf, 5);
        assert!(result.is_err());
        let err = result.unwrap_err();
        assert_eq!(err.kind(), std::io::ErrorKind::InvalidData);
        assert!(err.to_string().contains("MaxMessageBytes"));
    }

    #[test]
    fn read_line_bounded_accepts_line_within_limit() {
        let data = b"hello\nworld\n";
        let mut reader = std::io::BufReader::new(Cursor::new(data));
        let mut buf = Vec::new();
        let n = read_line_bounded(&mut reader, &mut buf, 1024).unwrap();
        assert_eq!(n, 6); // "hello\n"
        assert_eq!(&buf, b"hello\n");
    }

    #[test]
    fn read_line_bounded_eof_returns_zero() {
        let data: &[u8] = b"";
        let mut reader = std::io::BufReader::new(Cursor::new(data));
        let mut buf = Vec::new();
        let n = read_line_bounded(&mut reader, &mut buf, 1024).unwrap();
        assert_eq!(n, 0);
    }

    #[test]
    fn read_line_bounded_accepts_line_exactly_at_limit() {
        // Line is exactly limit bytes including the newline.
        let data = b"AAAA\n"; // 5 bytes
        let mut reader = std::io::BufReader::new(Cursor::new(data));
        let mut buf = Vec::new();
        let n = read_line_bounded(&mut reader, &mut buf, 5).unwrap();
        assert_eq!(n, 5);
    }
}
