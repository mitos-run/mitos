// PTY syscall wrappers: openpty, set_winsize, and process-group kill.
//
// This file is the ONLY place in the crate that calls the openpty(3),
// TIOCSWINSZ ioctl, pre_exec for setsid+TIOCSCTTY, O_NONBLOCK fcntl, and
// SIGKILL kill(2). Every unsafe block carries a SAFETY comment.
//
// On non-Linux platforms (macOS dev machines) all functions return an error so
// callers compile cleanly without conditional compilation at each call site.
//
// unsafe_code is permitted here via the #[allow(unsafe_code)] on the
// `pub mod sys;` declaration in lib.rs.
#![deny(unsafe_op_in_unsafe_fn)]

use std::io;
use std::os::fd::{AsRawFd, FromRawFd, IntoRawFd, OwnedFd, RawFd};

// ---------------------------------------------------------------------------
// Public types
// ---------------------------------------------------------------------------

/// A PTY master/slave file descriptor pair returned by `openpty`.
///
/// Both fds are owned and closed on drop via `OwnedFd`. The caller converts
/// the slave into stdio files via `PtyPair::slave_stdio_files` and the master
/// into a tokio async file via `master_to_async_file`.
pub struct PtyPair {
    /// The master side: the host reads output and writes input here.
    pub master: OwnedFd,
    /// The slave side: the child's terminal.
    pub slave: OwnedFd,
}

// ---------------------------------------------------------------------------
// openpty
// ---------------------------------------------------------------------------

/// Open a new PTY pair via openpty(3).
///
/// Returns the master and slave fds wrapped as `OwnedFd`. Both are
/// O_CLOEXEC on return (glibc >= 2.17, musl always).
///
/// Linux only; returns `Err(Unsupported)` on other platforms.
pub fn openpty() -> io::Result<PtyPair> {
    #[cfg(target_os = "linux")]
    {
        openpty_linux()
    }
    #[cfg(not(target_os = "linux"))]
    {
        Err(io::Error::from(io::ErrorKind::Unsupported))
    }
}

#[cfg(target_os = "linux")]
fn openpty_linux() -> io::Result<PtyPair> {
    let mut master_fd: libc::c_int = -1;
    let mut slave_fd: libc::c_int = -1;

    // SAFETY: master_fd and slave_fd are writable local variables; openpty(3)
    // fills them in with valid open fds on success. The name/termp/winp args are
    // all null, which openpty(3) documents as valid (uses kernel defaults). On
    // success we immediately wrap both fds in OwnedFd so they are closed on drop.
    let ret = unsafe {
        libc::openpty(
            &mut master_fd,
            &mut slave_fd,
            std::ptr::null_mut(),
            std::ptr::null(),
            std::ptr::null(),
        )
    };

    if ret == -1 {
        return Err(io::Error::last_os_error());
    }

    // SAFETY: openpty returned 0; both fds are valid, open, O_CLOEXEC.
    let master = unsafe { OwnedFd::from_raw_fd(master_fd) };
    let slave = unsafe { OwnedFd::from_raw_fd(slave_fd) };

    Ok(PtyPair { master, slave })
}

// ---------------------------------------------------------------------------
// set_winsize
// ---------------------------------------------------------------------------

/// Apply a window size to a PTY master fd via TIOCSWINSZ.
///
/// Cols/rows of 0 are clamped to the defaults (80x24) to match the Go
/// `setWinsize` function in pty.go:50-59.
///
/// Linux only; returns `Err(Unsupported)` on other platforms.
pub fn set_winsize(master_fd: RawFd, cols: u32, rows: u32) -> io::Result<()> {
    #[cfg(target_os = "linux")]
    {
        set_winsize_linux(master_fd, cols, rows)
    }
    #[cfg(not(target_os = "linux"))]
    {
        let _ = (master_fd, cols, rows);
        Err(io::Error::from(io::ErrorKind::Unsupported))
    }
}

#[cfg(target_os = "linux")]
fn set_winsize_linux(master_fd: RawFd, cols: u32, rows: u32) -> io::Result<()> {
    let cols = if cols == 0 { 80 } else { cols };
    let rows = if rows == 0 { 24 } else { rows };

    let ws = libc::winsize {
        ws_col: cols as libc::c_ushort,
        ws_row: rows as libc::c_ushort,
        ws_xpixel: 0,
        ws_ypixel: 0,
    };

    // SAFETY: master_fd is a valid open PTY master fd provided by the caller.
    // &ws points to a fully initialised winsize on the stack, alive for the
    // ioctl call. TIOCSWINSZ (0x5414) is the standard Linux ioctl for this.
    // The kernel copies the winsize from user space and does not retain the ptr.
    let ret = unsafe { libc::ioctl(master_fd, libc::TIOCSWINSZ, &ws) };
    if ret == -1 {
        Err(io::Error::last_os_error())
    } else {
        Ok(())
    }
}

// ---------------------------------------------------------------------------
// slave_stdio_files
// ---------------------------------------------------------------------------

/// Convert the slave `OwnedFd` into three `std::fs::File` values for use as
/// stdin, stdout, and stderr of the child process.
///
/// The original slave fd and two dups of it are consumed; `OwnedFd` ensures
/// all three are closed when the returned Files are dropped.
///
/// Linux only; returns `Err(Unsupported)` on other platforms.
pub fn slave_stdio_files(
    slave: OwnedFd,
) -> io::Result<(std::fs::File, std::fs::File, std::fs::File)> {
    #[cfg(target_os = "linux")]
    {
        slave_stdio_files_linux(slave)
    }
    #[cfg(not(target_os = "linux"))]
    {
        let _ = slave;
        Err(io::Error::from(io::ErrorKind::Unsupported))
    }
}

#[cfg(target_os = "linux")]
fn slave_stdio_files_linux(
    slave: OwnedFd,
) -> io::Result<(std::fs::File, std::fs::File, std::fs::File)> {
    let slave_raw = slave.as_raw_fd();

    // SAFETY: slave_raw is a valid open fd from openpty. dup(2) creates a new
    // fd referring to the same open file description; we wrap each result in
    // OwnedFd immediately so errors in subsequent dups close the prior ones.
    let dup1 = {
        let fd = unsafe { libc::dup(slave_raw) };
        if fd == -1 {
            return Err(io::Error::last_os_error());
        }
        // SAFETY: fd is a valid freshly-duped fd.
        unsafe { OwnedFd::from_raw_fd(fd) }
    };
    let dup2 = {
        let fd = unsafe { libc::dup(slave_raw) };
        if fd == -1 {
            return Err(io::Error::last_os_error());
        }
        // SAFETY: fd is a valid freshly-duped fd.
        unsafe { OwnedFd::from_raw_fd(fd) }
    };

    // Consume each OwnedFd into a std::fs::File.
    // SAFETY: into_raw_fd releases ownership from OwnedFd (preventing double-
    // close); from_raw_fd gives exclusive ownership to File so File closes the
    // fd on drop.
    let f0 = unsafe { std::fs::File::from_raw_fd(slave.into_raw_fd()) };
    let f1 = unsafe { std::fs::File::from_raw_fd(dup1.into_raw_fd()) };
    let f2 = unsafe { std::fs::File::from_raw_fd(dup2.into_raw_fd()) };

    Ok((f0, f1, f2))
}

// ---------------------------------------------------------------------------
// master_to_async_file + dup_for_resize
// ---------------------------------------------------------------------------

/// Duplicate the PTY master fd for use as a resize ioctl target. Returns a
/// raw fd integer (caller wraps it in OwnedFd inside its task for cleanup).
///
/// Linux only; returns `Err(Unsupported)` on other platforms.
pub fn dup_master_for_resize(master: &OwnedFd) -> io::Result<OwnedFd> {
    #[cfg(target_os = "linux")]
    {
        dup_fd_linux(master.as_raw_fd())
    }
    #[cfg(not(target_os = "linux"))]
    {
        let _ = master;
        Err(io::Error::from(io::ErrorKind::Unsupported))
    }
}

#[cfg(target_os = "linux")]
fn dup_fd_linux(fd: RawFd) -> io::Result<OwnedFd> {
    // SAFETY: fd is a valid open fd. dup(2) creates a new fd; we wrap it in
    // OwnedFd immediately.
    let new_fd = unsafe { libc::dup(fd) };
    if new_fd == -1 {
        return Err(io::Error::last_os_error());
    }
    // SAFETY: new_fd is a valid freshly-duped fd.
    Ok(unsafe { OwnedFd::from_raw_fd(new_fd) })
}

/// Set O_NONBLOCK on the master fd and return a `tokio::fs::File` that wraps
/// it. The `OwnedFd` is consumed; the returned tokio File owns the fd.
///
/// tokio requires files registered with the async runtime to be non-blocking;
/// this function sets that flag before handing off to tokio.
///
/// Linux only; returns `Err(Unsupported)` on other platforms.
pub fn master_to_async_file(master: OwnedFd) -> io::Result<tokio::fs::File> {
    #[cfg(target_os = "linux")]
    {
        master_to_async_file_linux(master)
    }
    #[cfg(not(target_os = "linux"))]
    {
        let _ = master;
        Err(io::Error::from(io::ErrorKind::Unsupported))
    }
}

#[cfg(target_os = "linux")]
fn master_to_async_file_linux(master: OwnedFd) -> io::Result<tokio::fs::File> {
    let raw = master.as_raw_fd();

    // SAFETY: fcntl(F_GETFL) reads the file status flags; it does not modify
    // any state and is safe to call on any valid open fd.
    let flags = unsafe { libc::fcntl(raw, libc::F_GETFL) };
    if flags == -1 {
        return Err(io::Error::last_os_error());
    }

    // SAFETY: fcntl(F_SETFL, O_NONBLOCK) sets the non-blocking flag on the
    // open file description. master (via raw) is a valid open PTY master fd.
    // The call does not transfer or close the fd; OwnedFd retains ownership.
    let ret = unsafe { libc::fcntl(raw, libc::F_SETFL, flags | libc::O_NONBLOCK) };
    if ret == -1 {
        return Err(io::Error::last_os_error());
    }

    // SAFETY: master.into_raw_fd() releases ownership from OwnedFd (preventing
    // double-close); from_raw_fd gives ownership to std::fs::File which closes
    // it when dropped. The fd has O_NONBLOCK set, satisfying tokio's requirement.
    let std_file = unsafe { std::fs::File::from_raw_fd(master.into_raw_fd()) };
    Ok(tokio::fs::File::from(std_file))
}

// ---------------------------------------------------------------------------
// apply_pty_session_leader (pre_exec helper)
// ---------------------------------------------------------------------------

/// Register a `pre_exec` hook on a `std::process::Command` that calls
/// `setsid()` followed by `ioctl(TIOCSCTTY)` on fd 0 (stdin) in the child.
///
/// This mirrors `cmd.SysProcAttr = {Setsid: true, Setctty: true, Ctty: 0}`
/// from pty.go:143-147. It makes the child process a session leader and sets
/// the slave PTY (wired to fd 0 by `Command::stdin`) as its controlling
/// terminal.
///
/// `pre_exec` is inherently `unsafe` because the closure runs between `fork`
/// and `exec` in the child process, where only async-signal-safe operations
/// are safe. `setsid(2)` and `ioctl(TIOCSCTTY)` are both async-signal-safe
/// on Linux.
///
/// Linux only; on other platforms this function does nothing (no pre_exec hook
/// is registered) and returns `Ok(())`.
pub fn apply_pty_session_leader(cmd: &mut std::process::Command) -> io::Result<()> {
    #[cfg(target_os = "linux")]
    {
        apply_pty_session_leader_linux(cmd)
    }
    #[cfg(not(target_os = "linux"))]
    {
        let _ = cmd;
        Ok(())
    }
}

#[cfg(target_os = "linux")]
fn apply_pty_session_leader_linux(cmd: &mut std::process::Command) -> io::Result<()> {
    use std::os::unix::process::CommandExt;

    // SAFETY: This pre_exec closure runs in the child after fork, before exec.
    // The only operations performed are:
    //   1. setsid(2): creates a new session; the child becomes the session
    //      leader. Async-signal-safe on Linux (only a syscall).
    //   2. ioctl(0, TIOCSCTTY, 0): sets fd 0 (stdin = slave PTY, wired by
    //      Command::stdin before spawn) as the controlling terminal of the new
    //      session. Async-signal-safe on Linux (only a syscall with an i32 arg).
    // No heap allocation, no libc state, no Rust runtime calls are made.
    unsafe {
        cmd.pre_exec(|| {
            if libc::setsid() == -1 {
                return Err(io::Error::last_os_error());
            }
            // arg 0: do not steal the terminal from another session leader.
            if libc::ioctl(0, libc::TIOCSCTTY, 0i32) == -1 {
                return Err(io::Error::last_os_error());
            }
            Ok(())
        });
    }
    Ok(())
}

// ---------------------------------------------------------------------------
// kill_pgroup
// ---------------------------------------------------------------------------

/// Send SIGKILL to the process group whose PGID equals `pid`.
///
/// Used by the exec watchdog to kill the whole child tree on timeout or
/// client hang-up. Mirrors `unix.Kill(-cmd.Process.Pid, unix.SIGKILL)` from
/// exec_stream.go:98 and `unix.Kill(-cmd.Process.Pid, unix.SIGKILL)` from
/// pty.go:169.
///
/// No-op if `pid` is 0 (unknown child pid). Errors from the syscall are
/// silently ignored (the process may have already exited).
pub fn kill_pgroup(pid: u32) {
    if pid == 0 {
        return;
    }
    // SAFETY: kill(2) with a negative first argument sends the signal to the
    // process group with PGID == |pid|. SIGKILL cannot be caught or ignored;
    // the kernel delivers it to every process in the group. This is async-
    // signal-safe and does not access any Rust-owned memory.
    unsafe {
        libc::kill(-(pid as libc::pid_t), libc::SIGKILL);
    }
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
#[allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]
mod tests {
    use super::*;

    #[test]
    fn openpty_on_non_linux_returns_unsupported() {
        #[cfg(not(target_os = "linux"))]
        {
            let err = openpty().unwrap_err();
            assert_eq!(err.kind(), io::ErrorKind::Unsupported);
        }
        #[cfg(target_os = "linux")]
        {
            let pair = openpty().expect("openpty must succeed on Linux");
            assert!(pair.master.as_raw_fd() >= 0);
            assert!(pair.slave.as_raw_fd() >= 0);
        }
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn set_winsize_on_master_succeeds() {
        let pair = openpty().expect("openpty");
        set_winsize(pair.master.as_raw_fd(), 120, 40).expect("set_winsize");
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn set_winsize_clamps_zero_cols_rows() {
        let pair = openpty().expect("openpty");
        // 0 cols/rows should not return an error (clamped to 80x24 internally).
        set_winsize(pair.master.as_raw_fd(), 0, 0).expect("set_winsize with zeros");
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn slave_stdio_files_returns_three_open_files() {
        let pair = openpty().expect("openpty");
        let (f0, f1, f2) = slave_stdio_files(pair.slave).expect("slave_stdio_files");
        // Check all three fds are distinct and valid.
        use std::os::unix::io::AsRawFd;
        let fd0 = f0.as_raw_fd();
        let fd1 = f1.as_raw_fd();
        let fd2 = f2.as_raw_fd();
        assert!(fd0 >= 0);
        assert!(fd1 >= 0);
        assert!(fd2 >= 0);
        assert_ne!(fd0, fd1);
        assert_ne!(fd1, fd2);
        assert_ne!(fd0, fd2);
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn dup_master_for_resize_returns_distinct_fd() {
        let pair = openpty().expect("openpty");
        let original_raw = pair.master.as_raw_fd();
        let duped = dup_master_for_resize(&pair.master).expect("dup_master_for_resize");
        assert_ne!(duped.as_raw_fd(), original_raw);
    }

    #[cfg(target_os = "linux")]
    #[tokio::test]
    async fn master_to_async_file_returns_writable_file() {
        let pair = openpty().expect("openpty");
        let async_file = master_to_async_file(pair.master).expect("master_to_async_file");
        // If we can get here without error the fd is open and O_NONBLOCK.
        drop(async_file);
    }

    #[test]
    fn kill_pgroup_with_zero_pid_is_noop() {
        // pid 0 must be a safe no-op.
        kill_pgroup(0);
    }
}
