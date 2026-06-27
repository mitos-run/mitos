// Serving-workload supervisor (issue #460).
//
// A pool can declare a long-running workload the template build starts AFTER
// init and keeps running while it snapshots, so a fork wakes with the app
// already listening. Two hazards make a naive `cmd &` in an init step fail:
//
//  1. The exec service (service/exec.rs) runs each command in its own process
//     group and kills that group on completion, so a child started by an exec
//     dies when the exec returns. spawn_detached escapes that by putting the
//     workload in its OWN session (setsid) before exec.
//  2. The NotifyForked handshake broadcasts SIGUSR2 to userspace, which
//     default-terminates a process that does not handle it. The WorkloadRegistry
//     records the workload's session id so sys::signal can exclude it (#460/#467).
//
// All syscall logic that needs `unsafe` (setsid in the pre-exec hook) is confined
// to spawn_detached with a SAFETY comment, mirroring how `init`/`sys` permit it.

use std::collections::HashSet;
use std::os::unix::process::CommandExt;
use std::sync::Mutex;
use std::time::Duration;

/// WorkloadRegistry records the session ids of started serving workloads so the
/// fork SIGUSR2 broadcast can exclude them (their session survives the fork).
#[derive(Default)]
pub struct WorkloadRegistry {
    sids: Mutex<HashSet<i32>>,
}

impl WorkloadRegistry {
    /// register records a workload's session id (poisoned-lock tolerant: a missed
    /// register only means the workload is not excluded, never a panic).
    pub fn register(&self, sid: i32) {
        if let Ok(mut g) = self.sids.lock() {
            g.insert(sid);
        }
    }

    /// excluded_sids returns a snapshot of the registered workload sessions.
    pub fn excluded_sids(&self) -> HashSet<i32> {
        self.sids.lock().map(|g| g.clone()).unwrap_or_default()
    }
}

/// spawn_detached starts `command` through the shell in its OWN session so it
/// outlives the exec/init that started it (the exec service kills its own process
/// group on completion). Returns the new session-leader pid, which equals the
/// workload's session id. stdio is detached to a log; env is applied as given
/// (callers pass non-secret values only).
pub fn spawn_detached(command: &str, env: &[(String, String)], cwd: &str) -> std::io::Result<i32> {
    let log = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open("/tmp/mitos-workload.log")?;
    let log2 = log.try_clone()?;
    let mut cmd = std::process::Command::new("/bin/sh");
    cmd.arg("-lc")
        .arg(command)
        .current_dir(cwd)
        .stdin(std::process::Stdio::null())
        .stdout(log)
        .stderr(log2);
    for (k, v) in env {
        cmd.env(k, v);
    }
    // SAFETY: setsid(2) is async-signal-safe and has no preconditions; the
    // pre-exec closure runs in the forked child before exec and performs only
    // setsid plus an error read, no allocation. Detaching the child into a new
    // session is exactly what lets the workload escape the exec process group.
    unsafe {
        cmd.pre_exec(|| {
            if libc::setsid() == -1 {
                return Err(std::io::Error::last_os_error());
            }
            Ok(())
        });
    }
    let child = cmd.spawn()?;
    Ok(child.id() as i32)
}

/// await_http_ready polls 127.0.0.1:port until a request to `path` returns the
/// expected status (0 means 200), or `timeout` elapses. This is the gate the
/// build waits on before snapshotting, so the snapshot captures a listening app.
pub async fn await_http_ready(
    port: u16,
    path: &str,
    expect: u16,
    timeout: Duration,
) -> Result<(), String> {
    let expect = if expect == 0 { 200 } else { expect };
    let path = if path.is_empty() { "/" } else { path };
    // HTTP/1.1, not 1.0: some servers (for example Chromium's DevTools endpoint,
    // the readiness target for a headless-browser workload) reject HTTP/1.0 with
    // "Cannot handle request with protocol: HTTP/1.0" and never return 200, so an
    // HTTP/1.0 probe would time out against a workload that is in fact listening.
    // Connection: close keeps the single-read response handling below valid.
    let req = format!("GET {path} HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n");
    let want = format!(" {expect} ");
    let deadline = tokio::time::Instant::now() + timeout;
    loop {
        if try_http(port, &req)
            .await
            .is_ok_and(|resp| resp.contains(&want))
        {
            return Ok(());
        }
        if tokio::time::Instant::now() >= deadline {
            return Err(format!(
                "workload not ready on 127.0.0.1:{port}{path} (want status {expect}) within {timeout:?}"
            ));
        }
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
}

/// try_http opens a TCP connection, writes the request, and returns the first
/// bytes of the response (status line + start of headers), which is enough to
/// match the expected status. Any connect/IO error bubbles up so the caller
/// keeps polling until the workload is listening.
async fn try_http(port: u16, req: &str) -> std::io::Result<String> {
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    let mut stream = tokio::net::TcpStream::connect(("127.0.0.1", port)).await?;
    stream.write_all(req.as_bytes()).await?;
    let mut tmp = [0u8; 1024];
    let n = stream.read(&mut tmp).await?;
    Ok(String::from_utf8_lossy(tmp.get(..n).unwrap_or(&[])).into_owned())
}

#[cfg(test)]
#[allow(clippy::unwrap_used, clippy::expect_used, clippy::panic, clippy::indexing_slicing)]
mod tests {
    use super::*;

    #[test]
    fn registry_round_trips_sessions() {
        let reg = WorkloadRegistry::default();
        assert!(reg.excluded_sids().is_empty());
        reg.register(4242);
        reg.register(4243);
        let s = reg.excluded_sids();
        assert!(s.contains(&4242) && s.contains(&4243));
    }

    #[cfg(target_os = "linux")]
    #[tokio::test]
    async fn spawned_workload_survives_and_leads_its_session() {
        // A sleep that outlives this test; spawn_detached must put it in its own
        // session so it escapes any caller process group.
        let pid = spawn_detached("sleep 30", &[], "/tmp").expect("spawn");
        let sid = crate::sys::signal::read_session("/proc", pid).expect("session");
        assert_eq!(sid, pid, "a detached workload must be its own session leader");
        // Clean up so the test runner does not leak the sleep.
        let _ = crate::sys::kill(pid, libc::SIGKILL);
    }

    #[tokio::test]
    async fn await_http_ready_succeeds_once_a_server_listens() {
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let port = listener.local_addr().unwrap().port();
        std::thread::spawn(move || {
            for s in listener.incoming() {
                use std::io::{Read, Write};
                let mut s = s.unwrap();
                let mut scratch = [0u8; 256];
                let _ = s.read(&mut scratch);
                let _ = s.write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n");
            }
        });
        let r = await_http_ready(port, "/", 200, Duration::from_secs(5)).await;
        assert!(r.is_ok(), "ready gate should pass once the server listens: {r:?}");
    }

    #[tokio::test]
    async fn await_http_ready_probes_with_http_1_1() {
        // The probe must speak HTTP/1.1: servers like Chromium's DevTools endpoint
        // reject HTTP/1.0 and never return 200, so an HTTP/1.0 probe would time out
        // against a workload that is genuinely listening. This server returns 200
        // only for an HTTP/1.1 request line and closes (mimicking that contract).
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let port = listener.local_addr().unwrap().port();
        std::thread::spawn(move || {
            for s in listener.incoming() {
                use std::io::{Read, Write};
                let mut s = s.unwrap();
                let mut scratch = [0u8; 256];
                let n = s.read(&mut scratch).unwrap_or(0);
                let req = String::from_utf8_lossy(&scratch[..n]);
                if req.contains("HTTP/1.1") {
                    let _ = s.write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n");
                } else {
                    let _ = s.write_all(b"HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n");
                }
            }
        });
        let r = await_http_ready(port, "/", 200, Duration::from_secs(5)).await;
        assert!(r.is_ok(), "ready gate must use HTTP/1.1 so a 1.1-only server returns 200: {r:?}");
    }

    #[tokio::test]
    async fn await_http_ready_times_out_when_nothing_listens() {
        // Port 1 needs privileges and is not listening in the test sandbox.
        let r = await_http_ready(1, "/", 200, Duration::from_millis(300)).await;
        assert!(r.is_err(), "ready gate must fail when nothing listens");
    }
}
