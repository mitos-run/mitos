use crate::protocol::{
    ConfigureRequest, ExecRequest, FileEntry, ListDirRequest, MkdirRequest, NotifyForkedRequest,
    NotifyForkedResponse, PingResponse, ReadFileRequest, RemoveRequest, Request, Response,
    WriteFileRequest,
};
use std::collections::HashMap;
use std::io;
use std::os::unix::fs::PermissionsExt;
use std::sync::Mutex;
use std::time::{Duration, Instant, UNIX_EPOCH};

// START_TIME is captured once at agent startup so ping can report uptime.
use std::sync::OnceLock;
static START_TIME: OnceLock<Instant> = OnceLock::new();

/// Return the agent start time, initializing it on the first call.
pub fn agent_start_time() -> Instant {
    *START_TIME.get_or_init(Instant::now)
}

// ---------------------------------------------------------------------------
// Public dispatch entry point consumed by Task 1.5 (transport).
// ---------------------------------------------------------------------------

/// Dispatch a one-shot request and return a Response.
/// Streaming types (exec_stream, pty, run_code, tunnel) are NOT handled here;
/// they own their connection and are dispatched in the transport layer (Task 1.5).
pub fn dispatch(req: &Request, env: &Mutex<HashMap<String, String>>) -> Response {
    match req.r#type.as_str() {
        "ping" => handle_ping(),
        "exec" => match &req.exec {
            None => Response {
                ok: false,
                error: "exec request is nil".into(),
                ..Default::default()
            },
            Some(r) => handle_exec(r, env),
        },
        "read_file" => match &req.read_file {
            None => Response {
                ok: false,
                error: "read_file request is nil".into(),
                ..Default::default()
            },
            Some(r) => handle_read_file(r),
        },
        "write_file" => match &req.write_file {
            None => Response {
                ok: false,
                error: "write_file request is nil".into(),
                ..Default::default()
            },
            Some(r) => handle_write_file(r),
        },
        "list_dir" => match &req.list_dir {
            None => Response {
                ok: false,
                error: "list_dir request is nil".into(),
                ..Default::default()
            },
            Some(r) => handle_list_dir(r),
        },
        "mkdir" => match &req.mkdir {
            None => Response {
                ok: false,
                error: "mkdir request is nil".into(),
                ..Default::default()
            },
            Some(r) => handle_mkdir(r),
        },
        "remove" => match &req.remove {
            None => Response {
                ok: false,
                error: "remove request is nil".into(),
                ..Default::default()
            },
            Some(r) => handle_remove(r),
        },
        "configure" => match &req.configure {
            None => Response {
                ok: false,
                error: "configure request is nil".into(),
                ..Default::default()
            },
            Some(r) => handle_configure(r, env),
        },
        "notify_forked" => match &req.notify_forked {
            None => Response {
                ok: false,
                error: "notify_forked request is nil".into(),
                ..Default::default()
            },
            Some(r) => handle_notify_forked(r),
        },
        other => Response {
            ok: false,
            error: format!("{} not implemented in spike agent", other),
            ..Default::default()
        },
    }
}

// ---------------------------------------------------------------------------
// ping
// ---------------------------------------------------------------------------

pub fn handle_ping() -> Response {
    let uptime = agent_start_time().elapsed().as_secs_f64();
    Response {
        ok: true,
        ping: Some(PingResponse {
            uptime_seconds: uptime,
        }),
        ..Default::default()
    }
}

// ---------------------------------------------------------------------------
// exec
// ---------------------------------------------------------------------------

/// Merge environments: base < configured < request (later wins, same as guestenv.Merge).
fn merge_env(
    base: &[String],
    configured: &HashMap<String, String>,
    request: &HashMap<String, String>,
) -> Vec<String> {
    // Track insertion order to maintain stable env ordering.
    let mut merged: HashMap<String, String> = HashMap::new();
    let mut order: Vec<String> = Vec::new();
    let mut verbatim: Vec<String> = Vec::new();

    let mut set = |k: String, v: String| {
        if !merged.contains_key(&k) {
            order.push(k.clone());
        }
        merged.insert(k, v);
    };

    // Base entries without '=' pass through verbatim (matches Go behavior).
    for kv in base {
        if let Some(eq) = kv.find('=') {
            let k = kv[..eq].to_string();
            let v = kv[eq + 1..].to_string();
            set(k, v);
        } else {
            verbatim.push(kv.clone());
        }
    }
    for (k, v) in configured {
        set(k.clone(), v.clone());
    }
    for (k, v) in request {
        set(k.clone(), v.clone());
    }

    let mut out = verbatim;
    for k in &order {
        out.push(format!("{}={}", k, merged[k]));
    }
    out
}

pub fn handle_exec(req: &ExecRequest, env: &Mutex<HashMap<String, String>>) -> Response {
    let start = Instant::now();

    let timeout_secs = if req.timeout == 0 { 30 } else { req.timeout };
    let timeout = Duration::from_secs(timeout_secs as u64);

    // Default to /workspace when no directory is specified. On Linux (the real
    // target) /workspace is used unconditionally, matching Go: a missing dir
    // yields exit_code 1 rather than silently succeeding in a temp dir.
    // On macOS (dev-only, outside a VM) fall back to temp_dir so unit tests run.
    let working_dir = if req.working_dir.is_empty() {
        #[cfg(target_os = "linux")]
        {
            "/workspace".to_string()
        }
        #[cfg(not(target_os = "linux"))]
        {
            // Dev-only: /workspace does not exist on macOS outside a VM. On Linux
            // (the real target) /workspace is used unconditionally, matching Go:
            // a missing dir yields exit_code 1 rather than silently succeeding.
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

    // Snapshot the configured env under the lock, then release.
    let configured_snapshot: HashMap<String, String> = {
        let guard = env.lock().unwrap();
        guard.clone()
    };

    let base_env: Vec<String> = std::env::vars().map(|(k, v)| format!("{}={}", k, v)).collect();
    let empty_req_env = HashMap::new();
    let req_env = req.env.as_ref().unwrap_or(&empty_req_env);
    let merged = merge_env(&base_env, &configured_snapshot, req_env);

    // Spawn the child with piped stdout/stderr. Capture the pid BEFORE moving
    // the child into the waiter thread (needed for SIGKILL on timeout).
    let mut child = match std::process::Command::new("/bin/sh")
        .arg("-c")
        .arg(&req.command)
        .current_dir(&working_dir)
        .env_clear()
        .envs(merged.iter().filter_map(|kv| {
            kv.find('=').map(|eq| (&kv[..eq], &kv[eq + 1..]))
        }))
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped())
        .spawn()
    {
        Err(e) => {
            return Response {
                ok: true,
                exec: Some(crate::protocol::ExecResponse {
                    exit_code: 1,
                    stdout: String::new(),
                    stderr: e.to_string(),
                    exec_time_ms: start.elapsed().as_micros() as f64 / 1000.0,
                }),
                ..Default::default()
            };
        }
        Ok(c) => c,
    };

    // Capture pid before moving child into the waiter thread.
    let pid = child.id();

    // Take the pipes; spawn one drain thread per pipe so the child never blocks
    // on a full pipe buffer. Each thread reads to EOF and returns the bytes.
    let stdout_pipe = child.stdout.take().expect("stdout was piped");
    let stderr_pipe = child.stderr.take().expect("stderr was piped");

    let drain_stdout = std::thread::spawn(move || {
        use std::io::Read;
        let mut buf = Vec::new();
        let mut pipe = stdout_pipe;
        let _ = pipe.read_to_end(&mut buf);
        buf
    });
    let drain_stderr = std::thread::spawn(move || {
        use std::io::Read;
        let mut buf = Vec::new();
        let mut pipe = stderr_pipe;
        let _ = pipe.read_to_end(&mut buf);
        buf
    });

    // Move the child into a waiter thread that blocks on child.wait() and
    // sends the ExitStatus over a channel. This avoids any poll interval,
    // eliminating the up-to-10ms latency that was added to every exec-rt
    // measurement (the key metric of this spike).
    let (tx, rx) = std::sync::mpsc::channel();
    std::thread::spawn(move || {
        let _ = tx.send(child.wait());
    });

    // Wait up to timeout for the child to exit, then SIGKILL via pid.
    // One-shot exec deliberately does NOT use a process group (no Setpgid),
    // matching Go's handleExec which also does not set Setpgid.
    let exit_code: i32;
    let stdout_bytes: Vec<u8>;
    let stderr_bytes: Vec<u8>;

    use std::sync::mpsc::RecvTimeoutError;
    match rx.recv_timeout(timeout) {
        Ok(Ok(status)) => {
            // Child exited normally; join drain threads to collect all output.
            stdout_bytes = drain_stdout.join().unwrap_or_default();
            stderr_bytes = drain_stderr.join().unwrap_or_default();
            exit_code = status.code().unwrap_or(1);
        }
        Ok(Err(_)) => {
            // wait() failed; drain threads may still hold partial output.
            stdout_bytes = drain_stdout.join().unwrap_or_default();
            stderr_bytes = drain_stderr.join().unwrap_or_default();
            exit_code = 1;
        }
        Err(RecvTimeoutError::Timeout) => {
            // Timeout: kill via pid (single process, no process group, matching Go).
            unsafe { libc::kill(pid as libc::pid_t, libc::SIGKILL) };
            // Reap the child by waiting for the waiter thread to finish.
            let _ = rx.recv();
            // Drain threads finish quickly now that the pipes are closed by the killed child.
            stdout_bytes = drain_stdout.join().unwrap_or_default();
            stderr_bytes = drain_stderr.join().unwrap_or_default();
            exit_code = 124;
        }
        Err(RecvTimeoutError::Disconnected) => {
            stdout_bytes = drain_stdout.join().unwrap_or_default();
            stderr_bytes = drain_stderr.join().unwrap_or_default();
            exit_code = 1;
        }
    }

    // from_utf8_lossy replaces invalid UTF-8 sequences with U+FFFD, a minor
    // divergence from Go which returns raw bytes; acceptable for the spike's
    // text commands.
    let stdout_str = String::from_utf8_lossy(&stdout_bytes).into_owned();
    let stderr_str = String::from_utf8_lossy(&stderr_bytes).into_owned();

    let elapsed_ms = start.elapsed().as_micros() as f64 / 1000.0;

    Response {
        ok: true,
        exec: Some(crate::protocol::ExecResponse {
            exit_code,
            stdout: stdout_str,
            stderr: stderr_str,
            exec_time_ms: elapsed_ms,
        }),
        ..Default::default()
    }
}

// ---------------------------------------------------------------------------
// read_file
// ---------------------------------------------------------------------------

pub fn handle_read_file(req: &ReadFileRequest) -> Response {
    match std::fs::read(&req.path) {
        Err(e) => Response {
            ok: false,
            error: e.to_string(),
            ..Default::default()
        },
        Ok(data) => {
            let size = std::fs::metadata(&req.path)
                .map(|m| m.len() as i64)
                .unwrap_or(data.len() as i64);
            Response {
                ok: true,
                read_file: Some(crate::protocol::ReadFileResponse { content: data, size }),
                ..Default::default()
            }
        }
    }
}

// ---------------------------------------------------------------------------
// write_file
// ---------------------------------------------------------------------------

pub fn handle_write_file(req: &WriteFileRequest) -> Response {
    let mode = if req.mode == 0 { 0o644 } else { req.mode };

    // mkdir -p the parent directory.
    if let Some(parent) = std::path::Path::new(&req.path).parent() {
        if let Err(e) = std::fs::create_dir_all(parent) {
            return Response {
                ok: false,
                error: e.to_string(),
                ..Default::default()
            };
        }
    }

    if let Err(e) = std::fs::write(&req.path, &req.content) {
        return Response {
            ok: false,
            error: e.to_string(),
            ..Default::default()
        };
    }

    // Apply the file mode.
    if let Err(e) = std::fs::set_permissions(
        &req.path,
        std::fs::Permissions::from_mode(mode),
    ) {
        return Response {
            ok: false,
            error: e.to_string(),
            ..Default::default()
        };
    }

    Response {
        ok: true,
        ..Default::default()
    }
}

// ---------------------------------------------------------------------------
// list_dir
// ---------------------------------------------------------------------------

pub fn handle_list_dir(req: &ListDirRequest) -> Response {
    let read_result = std::fs::read_dir(&req.path);
    match read_result {
        Err(e) => Response {
            ok: false,
            error: e.to_string(),
            ..Default::default()
        },
        Ok(iter) => {
            let mut entries: Vec<FileEntry> = Vec::new();
            for entry_result in iter {
                let entry = match entry_result {
                    Ok(e) => e,
                    Err(_) => continue,
                };
                let meta = match entry.metadata() {
                    Ok(m) => m,
                    Err(_) => continue,
                };
                let modified_at = meta
                    .modified()
                    .ok()
                    .and_then(|t| t.duration_since(UNIX_EPOCH).ok())
                    .map(|d| d.as_secs() as i64)
                    .unwrap_or(0);
                entries.push(FileEntry {
                    name: entry.file_name().to_string_lossy().into_owned(),
                    is_dir: meta.is_dir(),
                    size: meta.len() as i64,
                    mode: meta.permissions().mode(),
                    modified_at,
                });
            }
            Response {
                ok: true,
                list_dir: Some(crate::protocol::ListDirResponse { entries }),
                ..Default::default()
            }
        }
    }
}

// ---------------------------------------------------------------------------
// mkdir / remove
// ---------------------------------------------------------------------------

pub fn handle_mkdir(req: &MkdirRequest) -> Response {
    match std::fs::create_dir_all(&req.path) {
        Ok(_) => Response {
            ok: true,
            ..Default::default()
        },
        Err(e) => Response {
            ok: false,
            error: e.to_string(),
            ..Default::default()
        },
    }
}

pub fn handle_remove(req: &RemoveRequest) -> Response {
    match remove_all(&req.path) {
        Ok(_) => Response {
            ok: true,
            ..Default::default()
        },
        Err(e) => Response {
            ok: false,
            error: e.to_string(),
            ..Default::default()
        },
    }
}

/// Remove a path; if it is a directory remove it recursively. Mirrors os.RemoveAll.
fn remove_all(path: &str) -> io::Result<()> {
    let p = std::path::Path::new(path);
    if p.is_dir() {
        std::fs::remove_dir_all(p)
    } else {
        std::fs::remove_file(p)
    }
}

// ---------------------------------------------------------------------------
// configure
// ---------------------------------------------------------------------------

pub fn handle_configure(req: &ConfigureRequest, env: &Mutex<HashMap<String, String>>) -> Response {
    // Merge additively: new keys are added, existing keys are overwritten, no key is removed.
    // Secret VALUES are never logged, never echoed in the response.
    let mut guard = env.lock().unwrap();
    if let Some(env_map) = &req.env {
        for (k, v) in env_map {
            guard.insert(k.clone(), v.clone());
        }
    }
    if let Some(secrets_map) = &req.secrets {
        for (k, v) in secrets_map {
            guard.insert(k.clone(), v.clone());
        }
    }
    let count = guard.len();
    drop(guard);

    // Log key count only, never any value.
    eprintln!("sandbox-agent: configured {} environment variables", count);

    Response {
        ok: true,
        ..Default::default()
    }
}

// ---------------------------------------------------------------------------
// notify_forked: fork-correctness actions (RNG reseed + clock step).
// See docs/fork-correctness.md sections 1 and 2 for the rationale.
// ---------------------------------------------------------------------------

// CLOCK_STEP_THRESHOLD_NANOS mirrors Go's clockStepThresholdNanos: 500ms.
// Drifts within this window are left alone to avoid fighting in-guest NTP.
const CLOCK_STEP_THRESHOLD_NANOS: i64 = 500 * 1_000_000;

pub fn handle_notify_forked(req: &NotifyForkedRequest) -> Response {
    // Cross-reference: docs/fork-correctness.md sections 1 (RNG) and 2 (clock).

    // Log only the byte count, never the entropy value itself.
    eprintln!("sandbox-agent: notify_forked entropy_bytes={}", req.entropy.len());

    let reseeded_rng = reseed_rng(&req.entropy);
    let applied_clock_step_nanos = step_clock(req.host_wall_clock_nanos);
    // signaled_processes: signal userspace so language runtimes reseed their
    // own PRNGs. This spike reads /proc for the pid list on Linux (where /proc
    // is always present) and skips it on non-Linux (no /proc on macOS). A
    // count of 0 is a faithful minimal value for the spike on the dev host.
    let signaled_processes = signal_userspace();

    Response {
        ok: true,
        notify_forked: Some(NotifyForkedResponse {
            applied_clock_step_nanos,
            reseeded_rng,
            signaled_processes,
        }),
        ..Default::default()
    }
}

// reseed_rng injects the host-supplied entropy into the kernel RNG after a fork.
//
// The entropy argument is the per-fork divergence material from the host; it is
// what makes each fork diverge after restoring the same snapshot. Mirroring Go's
// reseedCRNG: if entropy is empty, return false immediately (nothing to inject).
//
// On Linux: writes the host-supplied bytes to /dev/urandom. Linux (mode 0666)
// allows any process to write; the write mixes bytes into the input pool. This
// matches the spike intent described in the task brief. The production Go agent
// uses RNDADDENTROPY (credited injection, requires a fd open O_RDWR and
// CAP_SYS_ADMIN on some kernels) and fails closed; this spike uses the plain
// write path which is sufficient for the fork-correctness demonstration.
// See docs/fork-correctness.md section 1.
//
// On macOS: the kernel refuses writes to /dev/urandom with EPERM even though
// the device is mode 0666. reseed_rng() returns false on macOS because no
// real reseed occurred. reseeded_rng == true MUST mean a real reseed happened;
// returning anything else would be a false claim. The guest agent only runs in
// Linux VMs in production so a false return on the dev host is correct.
fn reseed_rng(entropy: &[u8]) -> bool {
    // Mirror Go: return false immediately when entropy is empty (nothing to inject).
    if entropy.is_empty() {
        return false;
    }
    write_entropy_bytes(entropy)
}

// write_entropy_bytes mixes entropy bytes into the kernel RNG pool via
// /dev/urandom. Returns true when the write succeeds, false otherwise.
// On Linux /dev/urandom is world-writable (mode 0666); a write mixes the
// bytes into the input pool without requiring any privilege.
// On macOS the kernel rejects the write with EPERM; this function returns
// false there, and reseed_rng() propagates that honest false.
fn write_entropy_bytes(entropy: &[u8]) -> bool {
    use std::io::Write;
    match std::fs::OpenOptions::new().write(true).open("/dev/urandom") {
        Ok(mut f) => f.write_all(entropy).is_ok(),
        Err(_) => false,
    }
}

// step_clock applies a CLOCK_REALTIME step toward the host-provided wall time.
// Returns the signed step applied in nanoseconds, or 0 when:
//   - host_wall_clock_nanos is zero (no host time provided), or
//   - drift is within CLOCK_STEP_THRESHOLD_NANOS, or
//   - clock_gettime / clock_settime fails (no CAP_SYS_TIME on this host).
//
// Cross-reference: docs/fork-correctness.md section 2. CLOCK_MONOTONIC is
// deliberately not touched (Linux rejects clock_settime(CLOCK_MONOTONIC) with
// EINVAL); see the rationale in docs/fork-correctness.md and notifyforked.go.
fn step_clock(host_wall_clock_nanos: i64) -> i64 {
    if host_wall_clock_nanos == 0 {
        return 0;
    }

    // Read current CLOCK_REALTIME via libc clock_gettime.
    let guest_nanos = match get_realtime_nanos() {
        Some(n) => n,
        None => return 0,
    };

    let drift = host_wall_clock_nanos - guest_nanos;
    // Check abs(drift) > threshold without overflow risk.
    if (-CLOCK_STEP_THRESHOLD_NANOS..=CLOCK_STEP_THRESHOLD_NANOS).contains(&drift) {
        return 0;
    }

    // Attempt clock_settime(CLOCK_REALTIME). Fails without CAP_SYS_TIME.
    if set_realtime_nanos(host_wall_clock_nanos) {
        drift
    } else {
        0
    }
}

// get_realtime_nanos returns the current CLOCK_REALTIME in nanoseconds,
// or None if clock_gettime fails.
fn get_realtime_nanos() -> Option<i64> {
    unsafe {
        let mut ts = libc::timespec { tv_sec: 0, tv_nsec: 0 };
        if libc::clock_gettime(libc::CLOCK_REALTIME, &mut ts) == 0 {
            Some(ts.tv_sec * 1_000_000_000 + ts.tv_nsec)
        } else {
            None
        }
    }
}

// set_realtime_nanos calls clock_settime(CLOCK_REALTIME) to step the wall
// clock. Returns true on success. Requires CAP_SYS_TIME; fails silently
// without it (the caller reports 0 step, not an error).
fn set_realtime_nanos(nanos: i64) -> bool {
    unsafe {
        let ts = libc::timespec {
            tv_sec: (nanos / 1_000_000_000) as libc::time_t,
            tv_nsec: (nanos % 1_000_000_000) as libc::c_long,
        };
        libc::clock_settime(libc::CLOCK_REALTIME, &ts) == 0
    }
}

// signal_userspace sends SIGUSR2 to every userspace process except PID 1
// and the agent itself, prompting language runtimes to reseed their PRNGs.
// On Linux reads /proc for the pid list. On non-Linux (no /proc) returns 0.
// Mirrors Go's signalUserspace in guest/agent/notifyforked.go.
fn signal_userspace() -> i32 {
    #[cfg(target_os = "linux")]
    {
        let self_pid = unsafe { libc::getpid() };
        let entries = match std::fs::read_dir("/proc") {
            Ok(e) => e,
            Err(_) => return 0,
        };
        let mut signaled: i32 = 0;
        for entry in entries.flatten() {
            let name = entry.file_name();
            let name_str = name.to_string_lossy();
            let pid: libc::pid_t = match name_str.parse() {
                Ok(p) => p,
                Err(_) => continue,
            };
            if pid == 1 || pid == self_pid {
                continue;
            }
            let ret = unsafe { libc::kill(pid, libc::SIGUSR2) };
            if ret == 0 {
                signaled += 1;
            }
        }
        signaled
    }
    #[cfg(not(target_os = "linux"))]
    {
        // /proc is not present on macOS; no userspace signaling on the dev host.
        0
    }
}

// ---------------------------------------------------------------------------
// Tests (TDD: these were written first, before the implementation above).
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;
    use std::sync::Mutex;

    #[test]
    fn exec_returns_stdout_and_exit_zero() {
        let env = Mutex::new(HashMap::new());
        let req = serde_json::from_str(r#"{"type":"exec","exec":{"command":"printf hello","timeout":5}}"#).unwrap();
        let resp = dispatch(&req, &env);
        assert!(resp.ok);
        let e = resp.exec.unwrap();
        assert_eq!(e.exit_code, 0);
        assert_eq!(e.stdout, "hello");
    }

    #[test]
    fn write_then_read_file_roundtrips() {
        let env = Mutex::new(HashMap::new());
        let dir = std::env::temp_dir().join("agentrs_test");
        let path = dir.join("f.txt");
        let p = path.to_str().unwrap();
        let w: super::super::protocol::Request =
            serde_json::from_str(&format!(r#"{{"type":"write_file","write_file":{{"path":"{p}","content":"aGk=","mode":420}}}}"#)).unwrap();
        assert!(dispatch(&w, &env).ok);
        let r: super::super::protocol::Request =
            serde_json::from_str(&format!(r#"{{"type":"read_file","read_file":{{"path":"{p}"}}}}"#)).unwrap();
        let resp = dispatch(&r, &env);
        assert_eq!(resp.read_file.unwrap().content, b"hi");
    }

    #[test]
    fn configure_secret_not_echoed_in_response() {
        let env = Mutex::new(HashMap::new());
        let req = serde_json::from_str(r#"{"type":"configure","configure":{"secrets":{"TOKEN":"s3cret"}}}"#).unwrap();
        let resp = dispatch(&req, &env);
        assert!(resp.ok);
        assert_eq!(env.lock().unwrap().get("TOKEN").map(String::as_str), Some("s3cret"));
        // the response carries no echo of the secret value
        assert!(!serde_json::to_string(&resp).unwrap().contains("s3cret"));
    }

    #[test]
    fn out_of_scope_type_is_a_clear_error() {
        let env = Mutex::new(HashMap::new());
        let req = serde_json::from_str(r#"{"type":"vitals"}"#).unwrap();
        let resp = dispatch(&req, &env);
        assert!(!resp.ok);
        assert!(resp.error.contains("not implemented in spike agent"));
    }

    #[test]
    fn notify_forked_reports_reseed() {
        // Asserts the response is well-formed and that a zero host wall-clock
        // yields applied_clock_step_nanos == 0 (no CAP_SYS_TIME needed).
        // The strict reseeded_rng == true assertion is Linux-only: on macOS the
        // kernel rejects writes to /dev/urandom with EPERM so reseed_rng() honestly
        // returns false. The firecracker-test CI job runs on Linux and exercises
        // the true-reseed path.
        //
        // entropy field: base64-encoded bytes ("AAECBA==" == [0,1,2,4]).
        // This matches the Go wire format where NotifyForkedRequest.Entropy is []byte
        // serialized as a base64 string (json tag: "entropy").
        let env = std::sync::Mutex::new(std::collections::HashMap::new());
        let req = serde_json::from_str(
            r#"{"type":"notify_forked","notify_forked":{"host_wall_clock_nanos":0,"entropy":"AAECBA=="}}"#,
        ).unwrap();
        let resp = dispatch(&req, &env);
        assert!(resp.ok);
        let n = resp.notify_forked.unwrap();
        assert_eq!(n.applied_clock_step_nanos, 0); // 0 host time => no step
        // On Linux the write to /dev/urandom succeeds (mode 0666, no caps needed).
        // On macOS the kernel rejects it with EPERM; reseed_rng honestly returns false.
        #[cfg(target_os = "linux")]
        assert!(n.reseeded_rng, "Linux: write to /dev/urandom must succeed (mode 0666, no caps needed)");
        #[cfg(not(target_os = "linux"))]
        let _ = n.reseeded_rng; // EPERM on macOS; confirm field exists but do not assert true
    }

    #[test]
    fn notify_forked_empty_entropy_returns_false() {
        // Mirror Go's reseedCRNG: empty entropy => return false on all platforms.
        // No host-supplied divergence material means no reseed should be claimed.
        let env = std::sync::Mutex::new(std::collections::HashMap::new());
        let req = serde_json::from_str(
            r#"{"type":"notify_forked","notify_forked":{"host_wall_clock_nanos":0}}"#,
        ).unwrap();
        let resp = dispatch(&req, &env);
        assert!(resp.ok);
        let n = resp.notify_forked.unwrap();
        assert_eq!(n.applied_clock_step_nanos, 0);
        assert!(!n.reseeded_rng, "empty entropy must yield reseeded_rng=false on all platforms");
    }

    // Verify that a timed-out exec returns exit_code 124 and does not hang.
    // Mirrors Go's exec.CommandContext kill-on-deadline: exit 124 on timeout.
    #[test]
    fn exec_timeout_returns_124_and_does_not_hang() {
        let env = Mutex::new(HashMap::new());
        // sleep 60 would block forever; timeout of 1 s kills it and returns 124.
        let req = serde_json::from_str(
            r#"{"type":"exec","exec":{"command":"sleep 60","timeout":1}}"#,
        )
        .unwrap();
        let resp = dispatch(&req, &env);
        assert!(resp.ok);
        assert_eq!(resp.exec.unwrap().exit_code, 124);
    }
}
