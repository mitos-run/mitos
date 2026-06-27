// Safe wrappers for signal-related syscalls.
//
// This is the ONLY place in the crate that calls getpid() and issues
// signal-adjacent syscalls used by the fork-correctness path.
// Every unsafe block carries a SAFETY comment.
//
// unsafe_code is permitted in this file via the #[allow(unsafe_code)] on the
// `pub mod sys;` declaration in lib.rs.

#![deny(unsafe_op_in_unsafe_fn)]

use std::collections::HashSet;
use std::fs;

/// Returns the PID of the calling process (getpid(2)).
///
/// getpid() never fails and always returns a positive value. It is exposed
/// here (rather than inlined) so callers in safe modules can obtain self
/// without reaching for libc directly.
pub fn getpid() -> i32 {
    // SAFETY: getpid(2) has no preconditions, no side effects, and always
    // returns a valid pid_t. It cannot fail and does not take any pointer
    // arguments.
    unsafe {
        // SAFETY: no arguments; returns the caller's PID without any side
        // effects or pointer dereference.
        libc::getpid()
    }
}

/// Send SIGUSR2 to every userspace process visible in `proc_path` except
/// PID 1 and the current process.
///
/// This is the testable inner function: the production entry point passes
/// "/proc"; tests can pass a synthetic directory containing only the PIDs
/// they want to signal, which keeps the test host-safe (no real /proc
/// enumeration, no signals to system processes).
///
/// Mirrors the enumeration loop in signalUserspace (notifyforked.go:306-327).
/// Exclusion rules match Go exactly:
///   - pid == 1: skip (init/PID-1 guest agent).
///   - pid == self (getpid()): skip.
///   - Non-directory entries: skip (mirrors Go's !e.IsDir() guard).
///   - Non-numeric directory entries: skip (not a PID dir).
///   - Kernel threads: NOT explicitly excluded. Go calls kill(pid, SIGUSR2)
///     and ignores errors. We do the same: ESRCH/EPERM from kernel threads
///     is silently discarded; only successful delivers are counted.
///
/// read_session returns the session id of `pid` from `proc_path/<pid>/stat`
/// (field 6, 1-indexed). The comm field (field 2) is wrapped in parentheses and
/// may itself contain spaces, so we split AFTER the last ')' and count the
/// whitespace tokens of the remainder: state(1) ppid(2) pgrp(3) session(4). None
/// when the file is unreadable or malformed (the pid is then treated as having no
/// known session, so it is never excluded by accident).
pub fn read_session(proc_path: &str, pid: i32) -> Option<i32> {
    let stat = fs::read_to_string(format!("{proc_path}/{pid}/stat")).ok()?;
    let after = stat.rsplit_once(')')?.1;
    after.split_whitespace().nth(3)?.parse().ok()
}

/// process_catches_sigusr2 reports whether `pid` installed a SIGUSR2 handler,
/// read from the SigCgt (caught-signals) bitmask in `<proc_path>/<pid>/status`.
/// SIGUSR2's default disposition is terminate, so the fork broadcast signals ONLY
/// confirmed handlers (issue #467); a process that caught no SIGUSR2 could not act
/// on the reseed notification anyway, it could only die. Fail-safe: any read or
/// parse failure returns false, so a process we cannot confirm is never signaled
/// (and so never killed by the broadcast).
pub fn process_catches_sigusr2(proc_path: &str, pid: i32) -> bool {
    let status = match fs::read_to_string(format!("{proc_path}/{pid}/status")) {
        Ok(s) => s,
        Err(_) => return false,
    };
    for line in status.lines() {
        if let Some(rest) = line.strip_prefix("SigCgt:") {
            let Ok(mask) = u64::from_str_radix(rest.trim(), 16) else {
                return false;
            };
            // SigCgt bit (signo - 1) is set when a handler is installed for signo.
            let bit = 1u64 << ((libc::SIGUSR2 - 1) as u32);
            return mask & bit != 0;
        }
    }
    false
}

/// select_targets enumerates the pids under `proc_path` that should receive the
/// userspace reset signal: every numeric /proc/<pid> directory except PID 1, the
/// caller, and any pid whose session id is in `exclude_sids`. The exclusion is
/// what keeps a registered serving workload (and its children, which share its
/// session) alive across a fork, since SIGUSR2 default-terminates a process that
/// does not handle it (issue #460).
pub fn select_targets(proc_path: &str, exclude_sids: &HashSet<i32>) -> Vec<i32> {
    let self_pid = getpid();
    let entries = match fs::read_dir(proc_path) {
        Ok(e) => e,
        Err(err) => {
            eprintln!("sandbox-agent: read {proc_path}: {err}");
            return Vec::new();
        }
    };
    let mut targets = Vec::new();
    for entry in entries {
        let entry = match entry {
            Ok(e) => e,
            Err(_) => continue,
        };
        // Skip non-directory entries. Mirrors Go's !e.IsDir() guard in
        // signalUserspace (notifyforked.go:309). /proc/$PID is always a
        // directory; files and symlinks at the top level are not PID entries.
        let file_type = match entry.file_type() {
            Ok(ft) => ft,
            Err(_) => continue,
        };
        if !file_type.is_dir() {
            continue;
        }
        let name = entry.file_name();
        let name_str = match name.to_str() {
            Some(s) => s,
            None => continue,
        };
        let pid: i32 = match name_str.parse() {
            Ok(p) => p,
            Err(_) => continue, // not a numeric pid entry
        };
        if pid == 1 || pid == self_pid {
            continue;
        }
        // Exclude a registered workload's whole session so its serving process
        // survives the fork. Unknown-session pids are never excluded.
        if !exclude_sids.is_empty()
            && read_session(proc_path, pid).is_some_and(|sid| exclude_sids.contains(&sid))
        {
            continue;
        }
        // Issue #467: SIGUSR2's default disposition is terminate, so signal ONLY
        // processes that installed a SIGUSR2 handler (opt-in by handler presence).
        // Fail-safe: a process whose handler we cannot confirm is skipped, never
        // killed. This is layered UNDER the session exclusion above so a registered
        // serving workload (e.g. nginx, which traps SIGUSR2 for binary upgrade) is
        // left entirely alone rather than triggered.
        if !process_catches_sigusr2(proc_path, pid) {
            continue;
        }
        targets.push(pid);
    }
    targets
}

/// Returns the count of processes that received SIGUSR2.
pub fn signal_userspace_at(proc_path: &str, exclude_sids: &HashSet<i32>) -> i32 {
    let mut signaled: i32 = 0;
    for pid in select_targets(proc_path, exclude_sids) {
        if crate::sys::kill(pid, libc::SIGUSR2).is_ok() {
            signaled += 1;
        }
    }
    signaled
}

/// Sends SIGUSR2 to all userspace processes except PID 1, the current process,
/// and any process in an excluded session (a registered serving workload).
/// Returns the count of processes that received the signal.
///
/// Production entry point: walks real /proc. Mirrors signalUserspace in
/// guest/agent/notifyforked.go:299-328.
pub fn signal_userspace(exclude_sids: &HashSet<i32>) -> i32 {
    signal_userspace_at("/proc", exclude_sids)
}

#[cfg(test)]
#[allow(clippy::unwrap_used, clippy::expect_used, clippy::panic, unsafe_code)]
mod tests {
    use super::*;

    #[test]
    fn getpid_returns_positive() {
        let pid = getpid();
        assert!(pid > 0, "getpid() must return a positive PID, got {pid}");
    }

    #[test]
    fn excludes_pids_in_an_excluded_session() {
        let dir = tempfile::tempdir().unwrap();
        // pid 100 leads session 100 (the workload), pid 101 is its child in the
        // same session, pid 200 is an unrelated process in session 200.
        for (pid, sid) in [(100, 100), (101, 100), (200, 200)] {
            let p = dir.path().join(pid.to_string());
            std::fs::create_dir(&p).unwrap();
            // /proc/<pid>/stat: "pid (comm) state ppid pgrp session ..."; a comm
            // with a space exercises the rsplit-on-')' parse.
            std::fs::write(
                p.join("stat"),
                format!("{pid} (my proc) S 1 {pid} {sid} 0 0 0 0 0 0 0"),
            )
            .unwrap();
            std::fs::write(p.join("status"), "SigCgt:\t0000000000000800\n").unwrap();
        }
        let proc = dir.path().to_str().unwrap();
        assert_eq!(read_session(proc, 200), Some(200));
        assert_eq!(read_session(proc, 101), Some(100));

        let mut exclude = HashSet::new();
        exclude.insert(100);
        let selected = select_targets(proc, &exclude);
        assert!(selected.contains(&200), "unrelated pid must be signaled");
        assert!(!selected.contains(&100), "workload leader must be excluded");
        assert!(
            !selected.contains(&101),
            "workload child (same session) must be excluded"
        );

        // With no exclusions every non-self, non-init pid is a target.
        let all = select_targets(proc, &HashSet::new());
        assert!(all.contains(&100) && all.contains(&101) && all.contains(&200));
    }

    #[test]
    fn process_catches_sigusr2_reads_sigcgt() {
        let dir = tempfile::tempdir().unwrap();
        // bit (SIGUSR2 - 1) = bit 11 = 0x800.
        let with_handler = dir.path().join("10");
        std::fs::create_dir(&with_handler).unwrap();
        std::fs::write(
            with_handler.join("status"),
            "Name:\tapp\nState:\tS\nSigCgt:\t0000000000000800\n",
        )
        .unwrap();
        let no_handler = dir.path().join("11");
        std::fs::create_dir(&no_handler).unwrap();
        std::fs::write(
            no_handler.join("status"),
            "Name:\tapp\nState:\tS\nSigCgt:\t0000000000000000\n",
        )
        .unwrap();
        let malformed = dir.path().join("12");
        std::fs::create_dir(&malformed).unwrap();
        std::fs::write(malformed.join("status"), "SigCgt:\tnothex\n").unwrap();

        let proc = dir.path().to_str().unwrap();
        assert!(
            process_catches_sigusr2(proc, 10),
            "SIGUSR2 bit set => handler"
        );
        assert!(!process_catches_sigusr2(proc, 11), "no bit => no handler");
        assert!(
            !process_catches_sigusr2(proc, 12),
            "malformed => fail-safe false"
        );
        assert!(
            !process_catches_sigusr2(proc, 99),
            "missing file => fail-safe false"
        );
    }

    #[test]
    fn targets_only_processes_that_catch_sigusr2() {
        let dir = tempfile::tempdir().unwrap();
        // pid 300 installs a SIGUSR2 handler (SigCgt bit 11 set); pid 301 does not.
        for (pid, sigcgt) in [(300, "0000000000000800"), (301, "0000000000000000")] {
            let p = dir.path().join(pid.to_string());
            std::fs::create_dir(&p).unwrap();
            std::fs::write(p.join("stat"), format!("{pid} (app) S 1 {pid} {pid} 0 0")).unwrap();
            std::fs::write(p.join("status"), format!("SigCgt:\t{sigcgt}\n")).unwrap();
        }
        let proc = dir.path().to_str().unwrap();
        let selected = select_targets(proc, &HashSet::new());
        assert!(
            selected.contains(&300),
            "a SIGUSR2 handler must be a target"
        );
        assert!(
            !selected.contains(&301),
            "a non-handler must never be a target"
        );
    }

    #[test]
    fn signal_userspace_at_nonexistent_proc_returns_zero() {
        let count = signal_userspace_at(
            "/nonexistent/proc/path/that/does/not/exist",
            &HashSet::new(),
        );
        assert_eq!(count, 0);
    }

    // Host-safe SIGUSR2 test: fork a child that signals readiness via a pipe,
    // then pauses. The parent waits for the readiness byte before building a
    // synthetic /proc dir (one entry: the child PID) and calling
    // signal_userspace_at. The pipe close() on the child side after the write
    // happens before the child enters pause(), so the race window is reduced
    // to a single close() syscall. No real /proc is enumerated; system
    // processes (k3s, etc.) are never touched.
    //
    // Linux-only (fork/pause/sigaction); skips on non-Linux.
    #[cfg(target_os = "linux")]
    #[test]
    fn sigusr2_delivered_to_child_via_synthetic_proc() {
        extern "C" fn sigusr2_handler(_: libc::c_int) {
            // handler: no-op. The signal wakes pause(); the child exits 0.
        }

        // Create a pipe for child -> parent readiness notification.
        // SAFETY: pipe2 with valid fd array; the returned fds are owned by this
        // scope and closed explicitly.
        let mut pipe_fds: [libc::c_int; 2] = [-1, -1];
        let r = unsafe {
            // SAFETY: pipe_fds is a valid [c_int; 2].
            libc::pipe(pipe_fds.as_mut_ptr())
        };
        assert_eq!(r, 0, "pipe() failed: {}", std::io::Error::last_os_error());
        let read_fd = pipe_fds[0];
        let write_fd = pipe_fds[1];

        // Install SIGUSR2 handler before fork so the child inherits it.
        // SA_RESTART is intentionally NOT set: pause(2) must return EINTR
        // after the signal is delivered (with SA_RESTART, pause() would
        // be restarted and the child would never exit).
        // SAFETY: sigaction is always safe to call; the handler is a valid
        // function pointer and only performs async-signal-safe operations
        // (it is a no-op).
        let old_action = unsafe {
            let mut sa: libc::sigaction = std::mem::zeroed();
            sa.sa_sigaction = sigusr2_handler as *const () as libc::sighandler_t;
            libc::sigemptyset(&mut sa.sa_mask);
            sa.sa_flags = 0;
            let mut old: libc::sigaction = std::mem::zeroed();
            libc::sigaction(libc::SIGUSR2, &sa, &mut old);
            old
        };

        // Fork the child. The child runs in the inherited multi-threaded state
        // but calls only async-signal-safe syscalls and _exit, which is safe.
        // SAFETY: fork() is always safe to call; the child path is restricted
        // to async-signal-safe syscalls (write, pause) followed by _exit.
        let child_pid = unsafe { libc::fork() };
        assert!(
            child_pid >= 0,
            "fork failed: {}",
            std::io::Error::last_os_error()
        );

        if child_pid == 0 {
            // Child: close the read end (we only write).
            // SAFETY: close(2) on a valid fd.
            unsafe { libc::close(read_fd) };
            // Signal parent we are ready by writing one byte.
            let ready: u8 = 1;
            // SAFETY: write(2) with a 1-byte buffer; fd is valid.
            unsafe { libc::write(write_fd, &raw const ready as *const libc::c_void, 1) };
            // SAFETY: close(2) on a valid fd; close after write so parent
            // can detect EOF if we exit early.
            unsafe { libc::close(write_fd) };
            // Wait for SIGUSR2 (or any signal). pause() returns when a
            // signal is delivered and the handler returns.
            // SAFETY: pause() is always safe.
            unsafe { libc::pause() };
            // SAFETY: _exit(0) is always safe.
            unsafe { libc::_exit(0) };
        }

        // Parent: close the write end, then read the readiness byte from child.
        // SAFETY: close(2) on valid fd.
        unsafe { libc::close(write_fd) };
        let mut buf: u8 = 0;
        // SAFETY: read(2) into a valid 1-byte buffer; read_fd is open.
        let n = unsafe { libc::read(read_fd, &raw mut buf as *mut libc::c_void, 1) };
        // SAFETY: close(2) on valid fd.
        unsafe { libc::close(read_fd) };
        assert_eq!(n, 1, "did not receive readiness byte from child");

        // Child is now in pause(). Build a synthetic /proc with one entry
        // named after the child PID only. No other PIDs are enumerated;
        // this confines signal delivery to our child and is HOST-SAFE.
        let tmp = tempfile::tempdir().expect("tempdir");
        let child_dir = tmp.path().join(child_pid.to_string());
        std::fs::create_dir_all(&child_dir).expect("create synthetic pid dir");
        // The child installs a SIGUSR2 handler; expose it via SigCgt so the
        // handler-detection gate (issue #467) signals it.
        std::fs::write(child_dir.join("status"), "SigCgt:\t0000000000000800\n")
            .expect("write synthetic status");

        let count = signal_userspace_at(tmp.path().to_str().expect("utf8 path"), &HashSet::new());

        // Reap the child.
        let mut status: libc::c_int = 0;
        // SAFETY: child_pid > 0; status is a valid i32.
        unsafe { libc::waitpid(child_pid, &mut status, 0) };

        // Restore original SIGUSR2 disposition.
        // SAFETY: restoring a previously saved sigaction.
        unsafe { libc::sigaction(libc::SIGUSR2, &old_action, std::ptr::null_mut()) };

        assert_eq!(count, 1, "expected 1 process signaled, got {count}");
        assert!(
            libc::WIFEXITED(status) && libc::WEXITSTATUS(status) == 0,
            "child must exit 0 after SIGUSR2 delivery (status={status})"
        );
    }
}
