// Functions and statics are pub for Task 1.5 (transport). Allow dead_code until
// main.rs wires them in (same pattern as protocol.rs transient allow).
#![allow(dead_code)]

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

    // Default to /workspace when no directory is specified (matches Go behavior).
    // If /workspace does not exist (e.g., on a dev machine outside a VM),
    // fall back to the system temp directory so unit tests can run cross-platform.
    let working_dir = if req.working_dir.is_empty() {
        let ws = std::path::Path::new("/workspace");
        if ws.exists() {
            "/workspace".to_string()
        } else {
            std::env::temp_dir().to_string_lossy().into_owned()
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

    // Spawn the child; use a thread + channel pattern to enforce the timeout
    // on macOS/Linux without relying on linux-only prctl tricks.
    let command = req.command.clone();
    let (tx, rx) = std::sync::mpsc::channel();

    std::thread::spawn(move || {
        let result = std::process::Command::new("/bin/sh")
            .arg("-c")
            .arg(&command)
            .current_dir(&working_dir)
            .env_clear()
            .envs(merged.iter().filter_map(|kv| {
                kv.find('=').map(|eq| (&kv[..eq], &kv[eq + 1..]))
            }))
            .output();
        let _ = tx.send(result);
    });

    let exit_code: i32;
    let stdout_str: String;
    let stderr_str: String;

    match rx.recv_timeout(timeout) {
        Ok(Ok(output)) => {
            exit_code = output.status.code().unwrap_or(1);
            stdout_str = String::from_utf8_lossy(&output.stdout).into_owned();
            stderr_str = String::from_utf8_lossy(&output.stderr).into_owned();
        }
        Ok(Err(e)) => {
            exit_code = 1;
            stdout_str = String::new();
            stderr_str = e.to_string();
        }
        Err(_) => {
            // Timeout: exit code 124 (matches Go's ctx.Err() == DeadlineExceeded path).
            exit_code = 124;
            stdout_str = String::new();
            stderr_str = String::new();
        }
    }

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
// notify_forked (stub; real RNG-reseed/clock-step belongs to Task 1.4)
// ---------------------------------------------------------------------------

pub fn handle_notify_forked(_req: &NotifyForkedRequest) -> Response {
    // Task 1.4 will implement the actual RNG reseed and clock step.
    // Return a well-formed response with zero values so the type is wired.
    Response {
        ok: true,
        notify_forked: Some(NotifyForkedResponse {
            applied_clock_step_nanos: 0,
            reseeded_rng: false,
            signaled_processes: 0,
        }),
        ..Default::default()
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
    fn configure_values_are_not_returned_or_logged() {
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
}
