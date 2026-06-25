// Fork-correctness: per-fork volume mounts on the notify-forked path.
//
// Mirrors mountVolumes in guest/agent/notifyforked.go:247-276 and isMounted
// in notifyforked.go:283-296 exactly:
//   - Skips entries with an empty device or mount_path (logs warning).
//   - Skips already-mounted paths via sys::mount::is_mounted (idempotent on
//     re-fork notification).
//   - mkdir -p the mount_path before mounting.
//   - Mounts with ext4 filesystem, applying MS_RDONLY when read_only is set.
//   - Best effort per entry: logs failure and continues; a failure does not
//     crash the agent.
//   - Returns the count of volumes now mounted (including those that were
//     already mounted), matching the Go return value.
//
// Device and mount_path are configuration, not secrets, and may be logged.
// No unsafe code lives here: all syscall wrappers are in sys/.

/// Filesystem type the host formats every volume backing with. Mirrors
/// volumeFSType in notifyforked.go:236.
const VOLUME_FS_TYPE: &str = "ext4";

/// A single volume entry from the per-fork notify request.
///
/// Mirrors vsock.VolumeMountEntry in the Go guest agent.
pub struct VolumeMountEntry {
    /// Block device path inside the VM (e.g. "/dev/vdb").
    pub device: String,
    /// Absolute path at which to mount the device (e.g. "/workspace/data").
    pub mount_path: String,
    /// When true, mount read-only (MS_RDONLY). Mirrors ReadOnly in Go.
    pub read_only: bool,
}

/// Mount each volume in the per-fork volume table.
///
/// For each entry:
///   - Skip (log warning) when device or mount_path is empty.
///   - Skip (count as already mounted) when the path is already a mount point.
///   - mkdir -p the mount_path.
///   - Mount with ext4, optionally read-only.
///   - On failure: log and continue (best effort, no panic).
///
/// Returns the count of volumes now mounted at their paths.
/// Mirrors mountVolumes in guest/agent/notifyforked.go:247-276.
pub fn mount_volumes(entries: &[VolumeMountEntry]) -> i32 {
    let mut mounted: i32 = 0;
    for e in entries {
        if e.device.is_empty() || e.mount_path.is_empty() {
            eprintln!(
                "sandbox-agent: skipping volume with empty device/path: device={:?} path={:?}",
                e.device, e.mount_path,
            );
            continue;
        }
        if crate::sys::mount::is_mounted(&e.mount_path) {
            mounted += 1;
            continue;
        }
        if let Err(err) = std::fs::create_dir_all(&e.mount_path) {
            eprintln!(
                "sandbox-agent: mkdir mount path {}: {err}",
                e.mount_path,
            );
            continue;
        }
        let flags: u64 = if e.read_only { crate::sys::mount::MS_RDONLY } else { 0 };
        if let Err(err) = crate::sys::mount::mount(&e.device, &e.mount_path, VOLUME_FS_TYPE, flags) {
            eprintln!(
                "sandbox-agent: mount {} at {} (ro={}): {err}",
                e.device, e.mount_path, e.read_only,
            );
            continue;
        }
        mounted += 1;
    }
    if !entries.is_empty() {
        println!(
            "sandbox-agent: mounted {mounted}/{} volumes",
            entries.len(),
        );
    }
    mounted
}

// ---------------------------------------------------------------------------
// Tests.
// ---------------------------------------------------------------------------

#[cfg(test)]
#[allow(
    clippy::expect_used,
    clippy::unwrap_used,
    clippy::panic,
    clippy::indexing_slicing
)]
mod tests {
    use super::*;

    // -----------------------------------------------------------------------
    // Unit tests: pure logic, no syscalls, run on all platforms.
    // -----------------------------------------------------------------------

    #[test]
    fn empty_entries_returns_zero() {
        assert_eq!(mount_volumes(&[]), 0);
    }

    #[test]
    fn skip_entry_with_empty_device() {
        let e = VolumeMountEntry {
            device: "".into(),
            mount_path: "/mnt/x".into(),
            read_only: false,
        };
        assert_eq!(mount_volumes(&[e]), 0);
    }

    #[test]
    fn skip_entry_with_empty_mount_path() {
        let e = VolumeMountEntry {
            device: "/dev/vdb".into(),
            mount_path: "".into(),
            read_only: false,
        };
        assert_eq!(mount_volumes(&[e]), 0);
    }

    #[test]
    fn skip_both_empty() {
        let entries = vec![
            VolumeMountEntry { device: "".into(), mount_path: "".into(), read_only: false },
            VolumeMountEntry { device: "".into(), mount_path: "/mnt/y".into(), read_only: true },
        ];
        assert_eq!(mount_volumes(&entries), 0);
    }

    // -----------------------------------------------------------------------
    // Linux integration tests: privileged operations inside an isolated MOUNT
    // namespace so the host is not affected.
    //
    // Safety pattern:
    //   1. Skip when not root (geteuid() != 0): no mount attempted, returns.
    //   2. fork() a child; unshare(CLONE_NEWNS) inside the child.
    //   3. Immediately after unshare, mark "/" MS_REC|MS_PRIVATE so nothing
    //      can propagate back to the host mount namespace.
    //   4. Run the test body inside the child; exit 0 on success, 1 on failure.
    //   5. Parent waitpid and asserts child exited 0.
    //
    // On unshare failure: _exit(1) without mounting (fail-closed), same
    // pattern as fork/network.rs in_netns.
    // -----------------------------------------------------------------------

    #[cfg(target_os = "linux")]
    #[allow(unsafe_code)]
    mod linux {
        use super::*;

        const CLONE_NEWNS: libc::c_int = 0x0002_0000;

        // Fork, enter a private mount namespace in the child, run f, exit.
        // SAFETY: fork+unshare+_exit pattern; the child never returns to the
        // Rust runtime (uses _exit, not exit), so no destructors are run and
        // there is no double-free risk. The parent waits synchronously.
        fn in_mntns<F: FnOnce() -> bool>(test_name: &str, f: F) {
            // Skip when not root: unshare(CLONE_NEWNS) requires CAP_SYS_ADMIN.
            // SAFETY: geteuid() has no side effects.
            if unsafe { libc::geteuid() } != 0 {
                eprintln!("skipping {test_name}: requires root/CAP_SYS_ADMIN");
                return;
            }
            // SAFETY: fork() duplicates the process; the child path calls only
            // async-signal-safe functions and _exit before the Rust runtime tears
            // down anything. No mutexes are held at this point (single-threaded
            // test child).
            let pid = unsafe { libc::fork() };
            if pid < 0 {
                panic!("{test_name}: fork failed: {}", std::io::Error::last_os_error());
            }
            if pid == 0 {
                // Child: enter an isolated mount namespace.
                // SAFETY: CLONE_NEWNS is a valid flag; no pointer args.
                let r = unsafe { libc::unshare(CLONE_NEWNS) };
                if r != 0 {
                    eprintln!(
                        "{test_name}: unshare(CLONE_NEWNS) failed: {}",
                        std::io::Error::last_os_error(),
                    );
                    // SAFETY: _exit is always safe; terminates the child.
                    unsafe { libc::_exit(1) };
                }
                // Mark the entire mount tree private so no event propagates to
                // the host namespace. This is the host-safety gate.
                use crate::sys::mount::{MS_PRIVATE, MS_REC};
                if let Err(e) = crate::sys::mount::mount("none", "/", "", MS_REC | MS_PRIVATE) {
                    eprintln!("{test_name}: mount --make-rprivate /: {e}");
                    // SAFETY: see above.
                    unsafe { libc::_exit(1) };
                }
                let ok = f();
                // SAFETY: see above.
                unsafe { libc::_exit(if ok { 0 } else { 1 }) };
            }
            // Parent: wait for child.
            let mut status: libc::c_int = 0;
            // SAFETY: pid > 0 (child returned by fork); status is a valid i32.
            unsafe { libc::waitpid(pid, &mut status, 0) };
            let exited_ok = libc::WIFEXITED(status) && libc::WEXITSTATUS(status) == 0;
            assert!(exited_ok, "{test_name}: child process failed (status={status})");
        }

        #[test]
        fn mount_tmpfs_succeeds_and_is_counted() {
            in_mntns("mount_tmpfs_succeeds_and_is_counted", || {
                let dir = tempfile::tempdir().unwrap();
                let mp = dir.path().join("vol0");
                std::fs::create_dir_all(&mp).unwrap();
                let mp_str = mp.to_str().unwrap();

                // Use tmpfs so no backing device is needed on the test host.
                // We call sys::mount directly here to exercise the plumbing.
                // The volumes module always uses ext4, but for the namespace-
                // isolation proof we just need to confirm mount + unmount work
                // without leaking to the host.
                let r = crate::sys::mount::mount("tmpfs", mp_str, "tmpfs", 0);
                if let Err(ref e) = r {
                    eprintln!("mount tmpfs: {e}");
                    return false;
                }
                let mounted = crate::sys::mount::is_mounted(mp_str);
                if !mounted {
                    eprintln!("is_mounted returned false after successful mount");
                    return false;
                }
                // Unmount so the tempdir cleanup does not fail.
                // SAFETY: mp_str is a valid NUL-free path; umount2 does not retain the pointer.
                let mp_c = std::ffi::CString::new(mp_str).unwrap();
                unsafe { libc::umount2(mp_c.as_ptr(), 0) };
                true
            });
        }

        #[test]
        fn is_mounted_false_for_plain_dir() {
            in_mntns("is_mounted_false_for_plain_dir", || {
                let dir = tempfile::tempdir().unwrap();
                let plain = dir.path().join("not_a_mount");
                std::fs::create_dir_all(&plain).unwrap();
                let plain_str = plain.to_str().unwrap();
                !crate::sys::mount::is_mounted(plain_str)
            });
        }

        #[test]
        fn already_mounted_counts_without_double_mount() {
            in_mntns("already_mounted_counts_without_double_mount", || {
                let dir = tempfile::tempdir().unwrap();
                let mp = dir.path().join("vol-idem");
                std::fs::create_dir_all(&mp).unwrap();
                let mp_str = mp.to_str().unwrap();

                // Mount a tmpfs at mp_str directly (bypassing mount_volumes
                // because mount_volumes uses ext4, not available without a device).
                let r = crate::sys::mount::mount("tmpfs", mp_str, "tmpfs", 0);
                if r.is_err() {
                    return false;
                }
                // Now call mount_volumes with the same path. is_mounted should
                // return true, so it counts it without attempting a second mount.
                let e = VolumeMountEntry {
                    device: "tmpfs".into(),
                    mount_path: mp_str.to_string(),
                    read_only: false,
                };
                let count = mount_volumes(&[e]);
                let mp_c = std::ffi::CString::new(mp_str).unwrap();
                unsafe { libc::umount2(mp_c.as_ptr(), 0) };
                count == 1
            });
        }

        #[test]
        fn read_only_flag_is_applied() {
            in_mntns("read_only_flag_is_applied", || {
                let dir = tempfile::tempdir().unwrap();
                let mp = dir.path().join("vol-ro");
                std::fs::create_dir_all(&mp).unwrap();
                let mp_str = mp.to_str().unwrap();

                // Mount tmpfs read-only via sys::mount to test the MS_RDONLY flag.
                use crate::sys::mount::MS_RDONLY;
                let r = crate::sys::mount::mount("tmpfs", mp_str, "tmpfs", MS_RDONLY);
                if r.is_err() {
                    return false;
                }
                // Attempt to create a file; should fail because mount is read-only.
                let test_file = mp.join("canary");
                let write_ok = std::fs::write(&test_file, b"x").is_err();
                let mp_c = std::ffi::CString::new(mp_str).unwrap();
                unsafe { libc::umount2(mp_c.as_ptr(), 0) };
                write_ok
            });
        }
    }
}
