// Transport: newline-delimited JSON framing over Unix sockets (host tests) and
// AF_VSOCK (production). Mirrors handleConnection and listenVsock in
// guest/agent/main.go and handleExecStream in guest/agent/exec_stream.go.

use crate::handlers;
use crate::protocol::{ExecStreamFrame, Request, Response};
use std::collections::HashMap;
use std::io::{BufRead, BufReader};
use std::os::unix::net::UnixListener;
use std::sync::{Arc, Mutex};

// MaxMessageBytes matches vsock.MaxMessageBytes = 96 << 20 (96 MiB). The line
// reader must hold at least this many bytes for tar/untar messages.
const MAX_MESSAGE_BYTES: usize = 96 << 20;

// streamChunkBytes matches streamChunkBytes in exec_stream.go: 32 KiB per read.
const STREAM_CHUNK_BYTES: usize = 32 << 10;

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
            self.0.lock().unwrap().read(buf)
        }
    }

    let read_half = ReadHalf(Arc::clone(&shared));
    let mut reader = BufReader::with_capacity(64 * 1024, read_half);
    // Set a large line buffer matching MaxMessageBytes. BufReader's capacity
    // controls the internal I/O buffer; read_line grows its String as needed, so
    // there is no separate size limit to enforce here beyond what the OS provides.
    // For the tar/untar path a full 96 MiB line is expected; read_line handles it
    // as long as memory is available. We do not impose a hard per-line cap in this
    // spike: the Go agent caps via scanner.Buffer, but the spike uses read_line
    // which grows dynamically. A production implementation would enforce the cap.
    let _ = MAX_MESSAGE_BYTES; // constant kept for documentation; see above.

    loop {
        let mut line = String::new();
        let n = match reader.read_line(&mut line) {
            Ok(n) => n,
            Err(e) => {
                eprintln!("sandbox-agent: read error: {}", e);
                return;
            }
        };
        if n == 0 {
            // EOF: client closed the connection.
            return;
        }

        let req: Request = match serde_json::from_str(line.trim_end_matches('\n')) {
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
    let mut guard = shared.lock().unwrap();
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

    let mut child = match std::process::Command::new("/bin/sh")
        .arg("-c")
        .arg(&req.command)
        .current_dir(&working_dir)
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped())
        .spawn()
    {
        Err(e) => {
            let frame = ExecStreamFrame {
                kind: "exit".into(),
                exit_code: 1,
                error: format!("start: {}", e),
                exec_time_ms: start.elapsed().as_micros() as f64 / 1000.0,
                ..Default::default()
            };
            let mut guard = shared.lock().unwrap();
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

    // Poll for child exit or timeout. On timeout, kill and reap.
    let poll_interval = std::time::Duration::from_millis(10);
    let deadline = Instant::now() + timeout;
    let timed_out = loop {
        match child.try_wait() {
            Ok(Some(_)) => break false,
            Ok(None) => {
                if Instant::now() >= deadline {
                    let _ = child.kill();
                    let _ = child.wait();
                    break true;
                }
                std::thread::sleep(poll_interval);
            }
            Err(_) => break false,
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
    let mut guard = shared.lock().unwrap();
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
        let mut guard = shared.lock().unwrap();
        write_frame_locked(&mut *guard, &frame);
    }
}
