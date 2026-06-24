// Safe wrappers for signal-related syscalls.
//
// This is the ONLY place in the crate that calls getpid() and issues
// signal-adjacent syscalls used by the fork-correctness path.
// Every unsafe block carries a SAFETY comment.
//
// unsafe_code is permitted in this file via the #[allow(unsafe_code)] on the
// `pub mod sys;` declaration in lib.rs.

#![deny(unsafe_op_in_unsafe_fn)]

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
///   - Non-numeric directory entries: skip (not a PID dir).
///   - Kernel threads: NOT explicitly excluded. Go calls kill(pid, SIGUSR2)
///     and ignores errors. We do the same: ESRCH/EPERM from kernel threads
///     is silently discarded; only successful delivers are counted.
///
/// Returns the count of processes that received SIGUSR2.
pub fn signal_userspace_at(proc_path: &str) -> i32 {
    // SIGUSR2 is signal 12 on Linux (POSIX value; stable on amd64/arm64).
    const SIGUSR2: i32 = 12;

    let self_pid = getpid();

    let entries = match fs::read_dir(proc_path) {
        Ok(e) => e,
        Err(err) => {
            eprintln!("sandbox-agent: read {proc_path}: {err}");
            return 0;
        }
    };

    let mut signaled: i32 = 0;
    for entry in entries {
        let entry = match entry {
            Ok(e) => e,
            Err(_) => continue,
        };
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
        if crate::sys::kill(pid, SIGUSR2).is_ok() {
            signaled += 1;
        }
    }
    signaled
}

/// Sends SIGUSR2 to all userspace processes except PID 1 and the current
/// process. Returns the count of processes that received the signal.
///
/// Production entry point: walks real /proc. Mirrors signalUserspace in
/// guest/agent/notifyforked.go:299-328.
pub fn signal_userspace() -> i32 {
    signal_userspace_at("/proc")
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
    fn signal_userspace_at_nonexistent_proc_returns_zero() {
        let count = signal_userspace_at("/nonexistent/proc/path/that/does/not/exist");
        assert_eq!(count, 0);
    }

    // Host-safe SIGUSR2 test: fork a child that signals readiness via a pipe,
    // then pauses. The parent waits for the readiness byte before building a
    // synthetic /proc dir (one entry: the child PID) and calling
    // signal_userspace_at. This guarantees the child is in pause() when the
    // signal arrives, eliminating the race. No real /proc is enumerated;
    // system processes (k3s, etc.) are never touched.
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
        assert!(child_pid >= 0, "fork failed: {}", std::io::Error::last_os_error());

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

        let count = signal_userspace_at(tmp.path().to_str().expect("utf8 path"));

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
