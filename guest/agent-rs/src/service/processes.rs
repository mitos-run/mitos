// Processes and Signal RPC implementations (Task 2.5).
//
// Processes: reads the in-guest process table from /proc by iterating every
// numeric pid directory and parsing /proc/<pid>/stat. Only the program NAME
// (comm, the parenthesized field 2 of /proc/<pid>/stat) and resource counters
// are included. Command-line ARGUMENTS (/proc/<pid>/cmdline) and ENVIRONMENT
// (/proc/<pid>/environ) are NEVER read or logged: both can carry secrets
// (API keys, tokens, connection strings). This matches the Go agent's explicit
// exclusion of argv/environ in grpc_runtime.go.
//
// Signal: delivers a POSIX signal to a process via sys::kill. pid 1 (the
// in-VM control plane) and pids <= 0 are rejected. Signal numbers outside
// 1..64 are rejected. errno is mapped to gRPC status codes.
//
// cpu_percent is computed over a short two-snapshot window (100 ms), mirroring
// the Go agent's processCPUSampleWindow. The aggregate /proc/stat total jiffies
// are read at each snapshot so cpu_percent is a share of wall CPU across the
// window, not a lifetime average.

use std::collections::HashMap;
use std::time::Duration;
use tonic::{Request, Response, Status};

use crate::error::AgentError;
use crate::sandbox_v1::{ProcessInfo, ProcessList, ProcessesRequest, SignalRequest, SignalResponse};

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

/// The two-snapshot window over which per-process CPU jiffies are sampled.
/// Mirrors processCPUSampleWindow = 100ms in the Go agent.
const CPU_SAMPLE_WINDOW: Duration = Duration::from_millis(100);

/// Maximum valid POSIX signal number.
/// Linux signal space runs 1..64 (_NSIG-1: standard + SIGRT range).
/// Mirrors maxSignal = 64 in the Go agent.
const MAX_SIGNAL: i32 = 64;

/// procfs mount path. Always "/proc" in production.
fn proc_root() -> &'static str {
    "/proc"
}

// ---------------------------------------------------------------------------
// Parsed pid stat entry (Rust mirror of guestvitals.PidStat).
// ---------------------------------------------------------------------------

/// A parsed /proc/<pid>/stat entry with the fields needed for ProcessInfo.
///
/// comm is the bare program name from the parenthesized field 2. It is NOT
/// the full command line; /proc/<pid>/cmdline is never read.
struct PidStat {
    pid: i32,
    ppid: i32,
    comm: String,
    state: String,
    /// user-mode jiffies (field 14 of /proc/<pid>/stat).
    utime: u64,
    /// kernel-mode jiffies (field 15 of /proc/<pid>/stat).
    stime: u64,
    /// resident set size in pages (field 24 of /proc/<pid>/stat).
    rss_pages: u64,
}

/// Parse one /proc/<pid>/stat line into a PidStat.
///
/// The comm field (field 2 in proc(5) 1-based numbering) is wrapped in
/// parentheses and may contain spaces and nested parentheses. It is delimited
/// by the FIRST '(' and the LAST ')' in the line, matching the Go parser in
/// internal/guestvitals/proctable.go. Parsing is tolerant of disappearing
/// pids: callers skip on any Err.
///
/// Fields extracted (1-based proc(5) numbering, 0-based index in `rest` after
/// the closing ')'):
///   rest[0]  = state        (field 3)
///   rest[1]  = ppid         (field 4)
///   rest[11] = utime        (field 14)
///   rest[12] = stime        (field 15)
///   rest[21] = rss in pages (field 24)
fn parse_pid_stat(line: &[u8]) -> Result<PidStat, AgentError> {
    let s = std::str::from_utf8(line)
        .map_err(|_| AgentError::Internal("pid stat: non-UTF8 content".into()))?;

    let open = s
        .find('(')
        .ok_or_else(|| AgentError::Internal("pid stat: no opening paren".into()))?;
    let close = s
        .rfind(')')
        .ok_or_else(|| AgentError::Internal("pid stat: no closing paren".into()))?;
    if close <= open {
        return Err(AgentError::Internal(
            "pid stat: closing paren before opening paren".into(),
        ));
    }

    let pid_str = s[..open].trim();
    let pid: i32 = pid_str
        .parse()
        .map_err(|_| AgentError::Internal(format!("pid stat: pid field {pid_str:?} not an int")))?;

    // comm: text between the first '(' and last ')'. Never a secret.
    let comm = s[open + 1..close].to_owned();

    // Fields after the closing ')': space-split.
    let rest: Vec<&str> = s[close + 1..].split_whitespace().collect();
    // Need at least 22 fields: rest[0..=21].
    if rest.len() < 22 {
        return Err(AgentError::Internal(format!(
            "pid stat: only {} post-comm fields, need >= 22",
            rest.len()
        )));
    }

    // Use .get() to avoid indexing panics (clippy::indexing_slicing).
    // The len >= 22 guard above guarantees all indices are in bounds; .get()
    // returning None is unreachable but gracefully returns an Internal error.
    let state = rest
        .first()
        .ok_or_else(|| AgentError::Internal("pid stat: missing state field".into()))?
        .to_string();

    let ppid_str = rest
        .get(1)
        .ok_or_else(|| AgentError::Internal("pid stat: missing ppid field".into()))?;
    let ppid: i32 = ppid_str
        .parse()
        .map_err(|_| AgentError::Internal(format!("pid stat: ppid field {ppid_str:?} not an int")))?;

    let utime_str = rest
        .get(11)
        .ok_or_else(|| AgentError::Internal("pid stat: missing utime field".into()))?;
    let utime: u64 = utime_str
        .parse()
        .map_err(|_| AgentError::Internal(format!("pid stat: utime field {utime_str:?} not u64")))?;

    let stime_str = rest
        .get(12)
        .ok_or_else(|| AgentError::Internal("pid stat: missing stime field".into()))?;
    let stime: u64 = stime_str
        .parse()
        .map_err(|_| AgentError::Internal(format!("pid stat: stime field {stime_str:?} not u64")))?;

    let rss_str = rest
        .get(21)
        .ok_or_else(|| AgentError::Internal("pid stat: missing rss field".into()))?;
    let rss_pages: u64 = rss_str
        .parse()
        .map_err(|_| AgentError::Internal(format!("pid stat: rss field {rss_str:?} not u64")))?;

    Ok(PidStat {
        pid,
        ppid,
        comm,
        state,
        utime,
        stime,
        rss_pages,
    })
}

// ---------------------------------------------------------------------------
// /proc/stat aggregate total jiffies
// ---------------------------------------------------------------------------

/// Parse the aggregate "cpu " line of /proc/stat and return the total jiffies
/// and the steal jiffies.
///
/// Returns (0, 0) on any parse error; a zero total_delta causes cpu_percent to
/// be 0 for all processes (conservative, not wrong).
///
/// Shared with service::vitals for steal-fraction computation.
pub(crate) fn read_total_and_steal_jiffies(proc: &str) -> (u64, u64) {
    let path = format!("{proc}/stat");
    let data = match std::fs::read_to_string(&path) {
        Ok(d) => d,
        Err(_) => return (0, 0),
    };
    for line in data.lines() {
        if !line.starts_with("cpu ") && !line.starts_with("cpu\t") {
            continue;
        }
        let fields: Vec<&str> = line.split_whitespace().collect();
        // fields[0] = "cpu"; user nice system idle iowait irq softirq steal ...
        // We need at least 9 fields (0-indexed: cpu + 8 jiffy columns).
        if fields.len() < 9 {
            return (0, 0);
        }
        let total: u64 = fields
            .iter()
            .skip(1)
            .take(8)
            .filter_map(|f| f.parse::<u64>().ok())
            .sum();
        // steal is the 8th column (0-indexed: fields[8]).
        let steal: u64 = fields.get(8).and_then(|f| f.parse().ok()).unwrap_or(0);
        return (total, steal);
    }
    (0, 0)
}

/// Parse the aggregate "cpu " line of /proc/stat and return the total jiffies.
///
/// Returns 0 on any parse error; a zero totalDelta causes cpu_percent to be 0
/// for all processes (conservative, not wrong).
fn read_total_jiffies(proc: &str) -> u64 {
    let path = format!("{proc}/stat");
    let data = match std::fs::read_to_string(&path) {
        Ok(d) => d,
        Err(_) => return 0,
    };
    for line in data.lines() {
        if !line.starts_with("cpu ") && !line.starts_with("cpu\t") {
            continue;
        }
        let fields: Vec<&str> = line.split_whitespace().collect();
        // fields[0] = "cpu"; 8 jiffy columns follow.
        if fields.len() < 9 {
            return 0;
        }
        let total: u64 = fields
            .iter()
            .skip(1)
            .take(8)
            .filter_map(|f| f.parse::<u64>().ok())
            .sum();
        return total;
    }
    0
}

// ---------------------------------------------------------------------------
// Snapshot: one pass over /proc at an instant
// ---------------------------------------------------------------------------

/// One process-table snapshot: map of pid -> PidStat plus the aggregate
/// /proc/stat total jiffies at the same instant.
struct Snapshot {
    procs: HashMap<i32, PidStat>,
    total_jiffies: u64,
}

/// Walk /proc, read /proc/<pid>/stat for each numeric directory, return a
/// Snapshot. Pids that vanish mid-walk are silently skipped; the table is
/// inherently racy.
fn snapshot(proc: &str) -> Result<Snapshot, AgentError> {
    let total_jiffies = read_total_jiffies(proc);

    let entries = std::fs::read_dir(proc)
        .map_err(|e| AgentError::Internal(format!("read_dir {proc}: {e}")))?;

    let mut procs: HashMap<i32, PidStat> = HashMap::new();

    for entry in entries {
        let entry = match entry {
            Ok(e) => e,
            Err(_) => continue,
        };
        let name = entry.file_name();
        let name_str = name.to_string_lossy();

        // Skip non-numeric entries (not a pid directory).
        if name_str.parse::<u32>().is_err() {
            continue;
        }

        let stat_path = format!("{proc}/{name_str}/stat");
        let data = match std::fs::read(&stat_path) {
            Ok(d) => d,
            Err(_) => continue, // pid exited mid-walk
        };

        match parse_pid_stat(&data) {
            Ok(p) => {
                procs.insert(p.pid, p);
            }
            Err(_) => continue, // malformed stat; skip
        }
    }

    Ok(Snapshot {
        procs,
        total_jiffies,
    })
}

// ---------------------------------------------------------------------------
// Shared process-count helper for vitals
// ---------------------------------------------------------------------------

/// Count the number of live processes by walking /proc numeric directories.
///
/// Used by service::vitals to populate GuestVitals.process_count without
/// duplicating the /proc walk logic. Pids that vanish mid-walk are silently
/// skipped. Returns 0 on error (conservative fallback).
///
/// NEVER reads /proc/<pid>/cmdline or /proc/<pid>/environ: argv and environ
/// can carry secrets and are excluded by the secret-hygiene policy.
pub(crate) fn count_processes(proc: &str) -> i32 {
    let entries = match std::fs::read_dir(proc) {
        Ok(e) => e,
        Err(_) => return 0,
    };
    let mut count = 0i32;
    for entry in entries.flatten() {
        let name = entry.file_name();
        if name.to_string_lossy().parse::<u32>().is_ok() {
            count += 1;
        }
    }
    count
}

// ---------------------------------------------------------------------------
// Processes RPC
// ---------------------------------------------------------------------------

/// Implement the Processes RPC: return the guest process table.
///
/// cpu_percent is computed over CPU_SAMPLE_WINDOW via two snapshots.
/// rss_bytes converts rss_pages using the OS page size (4096 fallback).
/// comm is sourced from /proc/<pid>/stat field 2 only; cmdline and environ
/// are NEVER read.
pub async fn processes(
    _request: Request<ProcessesRequest>,
) -> Result<Response<ProcessList>, Status> {
    let proc = proc_root();

    // First snapshot.
    let first = snapshot(proc).map_err(|e| -> Status { e.into() })?;

    // Sleep for the CPU sampling window (async-friendly).
    tokio::time::sleep(CPU_SAMPLE_WINDOW).await;

    // Second snapshot.
    let second = snapshot(proc).map_err(|e| -> Status { e.into() })?;

    let total_delta = second
        .total_jiffies
        .saturating_sub(first.total_jiffies) as f64;

    let page_size = page_size_bytes();
    let page_bytes = page_size as i64;

    let mut process_list: Vec<ProcessInfo> = Vec::with_capacity(second.procs.len());

    for (pid, p) in &second.procs {
        let cpu_percent = if let Some(prev) = first.procs.get(pid) {
            if total_delta > 0.0 {
                let proc_delta = ((p.utime + p.stime).saturating_sub(prev.utime + prev.stime)) as f64;
                if proc_delta > 0.0 {
                    proc_delta / total_delta * 100.0
                } else {
                    0.0
                }
            } else {
                0.0
            }
        } else {
            0.0
        };

        process_list.push(ProcessInfo {
            pid: p.pid,
            ppid: p.ppid,
            command: p.comm.clone(),
            state: p.state.clone(),
            cpu_percent,
            rss_bytes: p.rss_pages as i64 * page_bytes,
        });
    }

    Ok(Response::new(ProcessList {
        processes: process_list,
    }))
}

/// Return the OS page size in bytes. Falls back to 4096 (standard 4 KiB page).
///
/// Delegates to the sys::kill module helper which is the only place in the
/// crate allowed to hold the unsafe sysconf call.
fn page_size_bytes() -> usize {
    crate::sys::kill::page_size_bytes()
}

// ---------------------------------------------------------------------------
// Signal RPC
// ---------------------------------------------------------------------------

/// Implement the Signal RPC: deliver a POSIX signal to a process in the guest.
///
/// Security: pid 1 (the in-VM control plane) and pids <= 0 (process groups)
/// are rejected with InvalidArgument. Signal numbers outside 1..MAX_SIGNAL
/// (64) are also rejected. errno ESRCH maps to NotFound; EPERM maps to
/// PermissionDenied. This matches the Go agent's Signal handler exactly.
pub async fn signal(
    request: Request<SignalRequest>,
) -> Result<Response<SignalResponse>, Status> {
    let req = request.into_inner();
    let pid = req.pid;
    let signum = req.signal;

    // Reject pid 1 (the guest control plane) and any non-positive pid.
    if pid <= 1 {
        return Err(AgentError::InvalidArgument(format!(
            "signal: refusing to signal pid {pid}: pid 1 is the guest control plane and pids <= 1 are not addressable"
        ))
        .into());
    }

    // Validate the signal number.
    if !(1..=MAX_SIGNAL).contains(&signum) {
        return Err(AgentError::InvalidArgument(format!(
            "signal: signal number {signum} out of range 1..{MAX_SIGNAL}"
        ))
        .into());
    }

    match crate::sys::kill(pid, signum) {
        Ok(()) => Ok(Response::new(SignalResponse {})),
        Err(e) => {
            match e.raw_os_error() {
                Some(libc::ESRCH) => Err(AgentError::NotFound(format!(
                    "signal: no such process {pid}"
                ))
                .into()),
                Some(libc::EPERM) => Err(AgentError::PermissionDenied(format!(
                    "signal: not permitted to signal pid {pid}"
                ))
                .into()),
                _ => Err(AgentError::Internal(format!(
                    "signal: kill({pid}, {signum}): {e}"
                ))
                .into()),
            }
        }
    }
}

// ---------------------------------------------------------------------------
// Unit tests
// ---------------------------------------------------------------------------

#[cfg(test)]
#[allow(clippy::unwrap_used, clippy::expect_used)]
mod tests {
    use super::*;

    // --- parse_pid_stat ---

    #[test]
    fn parse_pid_stat_basic() {
        // A representative /proc/1/stat line from a Linux system:
        // 1 (systemd) S 0 1 1 0 -1 ... utime stime ... rss ...
        // Fields: pid comm state ppid pgrp session tty_nr tpgid flags
        //         minflt cminflt majflt cmajflt utime stime cutime cstime
        //         priority nice num_threads itrealvalue starttime vsize rss
        // We need: pid=1, comm=systemd, state=S, ppid=0, utime=rest[11], stime=rest[12], rss=rest[21]
        // Construct a synthetic stat line with exactly 22+ post-comm fields:
        // state ppid pgrp session tty tpgid flags minflt cminflt majflt cmajflt utime stime cutime cstime prio nice nth itrealvalue start vsize rss
        let line = b"1 (systemd) S 0 1 1 0 -1 4194560 1234 0 5 0 42 10 0 0 20 0 1 0 100 12345678 512 18446744073709551615 1 1 0 0 0 0 671173123 4096 1260 1 0 0 17 0 0 0 0 0 0 0 0 0 0 0 0 0 0";
        let p = parse_pid_stat(line).expect("must parse");
        assert_eq!(p.pid, 1);
        assert_eq!(p.comm, "systemd");
        assert_eq!(p.state, "S");
        assert_eq!(p.ppid, 0);
        assert_eq!(p.utime, 42);
        assert_eq!(p.stime, 10);
        assert_eq!(p.rss_pages, 512);
    }

    #[test]
    fn parse_pid_stat_comm_with_spaces() {
        // comm may contain spaces; the parser must delimit by first '(' and last ')'.
        let line = b"42 (my process) R 1 42 42 0 -1 4194560 0 0 0 0 100 50 0 0 20 0 1 0 200 99887766 256 18446744073709551615 1 1 0 0 0 0 0 0 0 1 0 0 17 0 0 0 0 0 0 0 0 0 0 0 0 0 0";
        let p = parse_pid_stat(line).expect("must parse");
        assert_eq!(p.pid, 42);
        assert_eq!(p.comm, "my process");
        assert_eq!(p.state, "R");
        assert_eq!(p.ppid, 1);
        assert_eq!(p.utime, 100);
        assert_eq!(p.stime, 50);
        assert_eq!(p.rss_pages, 256);
    }

    #[test]
    fn parse_pid_stat_comm_with_parens() {
        // comm may itself contain parentheses: rfirst '(' and rlast ')' are correct.
        let line = b"99 (bash(1)) S 2 99 99 0 -1 4194560 0 0 0 0 200 100 0 0 20 0 1 0 300 12345 128 18446744073709551615 1 1 0 0 0 0 0 0 0 1 0 0 17 0 0 0 0 0 0 0 0 0 0 0 0 0 0";
        let p = parse_pid_stat(line).expect("must parse");
        assert_eq!(p.comm, "bash(1)");
        assert_eq!(p.pid, 99);
    }

    #[test]
    fn parse_pid_stat_missing_parens_returns_error() {
        let line = b"1 systemd S 0 1 1 0 -1 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0";
        assert!(parse_pid_stat(line).is_err());
    }

    #[test]
    fn parse_pid_stat_too_few_fields_returns_error() {
        // Only 5 post-comm fields (need 22).
        let line = b"1 (init) S 0 1";
        assert!(parse_pid_stat(line).is_err());
    }

    // --- ProcessInfo field source guarantee ---

    /// Verify that ProcessInfo carries no argv or environ field.
    /// This is a compile-time assertion: if ProcessInfo ever grows cmdline or
    /// environ fields, this test will fail to compile (field doesn't exist).
    /// The intent is to document and enforce the secret-hygiene constraint.
    #[test]
    fn process_info_has_no_argv_or_environ_field() {
        let info = ProcessInfo {
            pid: 1,
            ppid: 0,
            command: "init".into(),
            state: "S".into(),
            cpu_percent: 0.0,
            rss_bytes: 0,
        };
        // These are all the fields; if cmdline/argv/environ were added the
        // struct literal above would produce a compile error (missing field).
        let _ = info;
    }
}
