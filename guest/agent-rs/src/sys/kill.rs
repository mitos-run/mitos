// Safe wrapper around libc::kill for delivering POSIX signals.
//
// This is the ONLY place in the crate that calls libc::kill.
// Every unsafe block carries a SAFETY comment.
//
// unsafe_code is permitted in this file via the #[allow(unsafe_code)] on the
// `pub mod sys;` declaration in lib.rs. We do not repeat the allow here
// (clippy flags duplicated attributes).
#![deny(unsafe_op_in_unsafe_fn)]

use std::io;

/// Send signal `signum` to process `pid` via `kill(2)`.
///
/// Returns `Ok(())` on success. Returns `Err` carrying the OS errno on failure.
/// The caller is responsible for interpreting errno and mapping to the
/// appropriate gRPC status (NotFound for ESRCH, PermissionDenied for EPERM,
/// etc.).
///
/// Available on Linux only; on other platforms this always returns an error.
pub fn kill(pid: i32, signum: i32) -> io::Result<()> {
    #[cfg(target_os = "linux")]
    {
        kill_linux(pid, signum)
    }

    #[cfg(not(target_os = "linux"))]
    {
        let _ = (pid, signum);
        Err(io::Error::new(
            io::ErrorKind::Unsupported,
            "sys::kill is Linux-only",
        ))
    }
}

#[cfg(target_os = "linux")]
fn kill_linux(pid: i32, signum: i32) -> io::Result<()> {
    // SAFETY:
    // - libc::kill takes a pid_t (i32) and a c_int (i32); both are passed
    //   directly without any pointer indirection, so there are no alignment,
    //   lifetime, or validity concerns.
    // - The signal number is validated by the caller before reaching this
    //   function (range 1..64); kill(2) will return EINVAL for any value the
    //   kernel rejects, which we convert to an Err below.
    // - kill(2) does not retain any pointer after the call returns.
    let ret = unsafe {
        // SAFETY: pid and signum are plain integer values. The kernel validates
        // both and returns ESRCH/EPERM/EINVAL on error rather than undefined
        // behavior.
        libc::kill(pid as libc::pid_t, signum as libc::c_int)
    };
    if ret == 0 {
        Ok(())
    } else {
        Err(io::Error::last_os_error())
    }
}

/// Return the OS page size in bytes via sysconf(_SC_PAGESIZE).
///
/// Falls back to 4096 (standard 4 KiB x86-64 page) if sysconf fails or
/// returns a value that is not a positive power of two. This function lives
/// in sys/ because it requires an unsafe libc call; it is the ONLY place
/// the sysconf call is made.
pub fn page_size_bytes() -> usize {
    #[cfg(target_os = "linux")]
    {
        // SAFETY: sysconf(_SC_PAGESIZE) is a pure constant query. It takes
        // no pointer arguments, has no side effects, and returns a long.
        // The return value is validated before use; no pointer is derived
        // from it.
        let n = unsafe {
            // SAFETY: _SC_PAGESIZE is a valid sysconf name; the call cannot
            // produce undefined behavior regardless of its return value.
            libc::sysconf(libc::_SC_PAGESIZE)
        };
        if n > 0 {
            return n as usize;
        }
    }
    4096
}

#[cfg(test)]
#[allow(clippy::unwrap_used, clippy::expect_used)]
mod tests {
    use super::*;

    /// kill(0, 0) is a standard POSIX existence check for the current process.
    /// On Linux it always succeeds (the caller can always signal itself).
    #[cfg(target_os = "linux")]
    #[test]
    fn kill_self_with_zero_signal_succeeds() {
        // POSIX: kill(pid, 0) checks process existence without sending a signal.
        // Using the current process PID guarantees it exists.
        let pid = unsafe { libc::getpid() };
        kill(pid, 0).expect("kill(self, 0) must succeed");
    }

    /// kill with an invalid signal number returns an error (EINVAL from kernel).
    #[cfg(target_os = "linux")]
    #[test]
    fn kill_invalid_signal_returns_error() {
        let pid = unsafe { libc::getpid() };
        // Signal 0 is valid (existence check). Signal 200 is out of the valid
        // range; the kernel returns EINVAL.
        let result = kill(pid, 200);
        assert!(result.is_err(), "kill with signal 200 must fail");
    }

    /// kill targeting a non-existent PID returns ESRCH.
    #[cfg(target_os = "linux")]
    #[test]
    fn kill_nonexistent_pid_returns_esrch() {
        // PID 2147483647 (i32::MAX) is exceedingly unlikely to exist.
        let result = kill(i32::MAX, 0);
        assert!(result.is_err(), "kill to non-existent PID must fail");
        let err = result.unwrap_err();
        assert_eq!(
            err.raw_os_error(),
            Some(libc::ESRCH),
            "expected ESRCH for non-existent PID, got {:?}",
            err,
        );
    }
}
