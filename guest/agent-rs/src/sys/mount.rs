// PID-1 mount and sethostname syscall wrappers.
//
// These wrappers are Linux-only. On non-Linux hosts (macOS dev machines) the
// functions always return Ok(()), letting init_system() be called from unit
// tests without syscall failures.
//
// unsafe_code is permitted in this file via the #[allow(unsafe_code)] on the
// `pub mod sys;` declaration in lib.rs.
#![deny(unsafe_op_in_unsafe_fn)]

use std::io;

/// MS_RDONLY: mount the filesystem read-only. Mirrors unix.MS_RDONLY in the Go
/// agent (golang.org/x/sys/unix). Value is 1 on Linux (all architectures).
/// Hardcoded to avoid the libc crate's Linux-only constant on non-Linux hosts.
pub const MS_RDONLY: u64 = 1;

/// MS_REC: recursive mount flag. Used with MS_PRIVATE to mark every mount in
/// the subtree private so propagation is confined to the namespace.
/// Linux stable value: 1<<14 = 16384.
pub const MS_REC: u64 = 16384;

/// MS_PRIVATE: do not propagate mount events to or from this mount point.
/// Combined with MS_REC after unshare(CLONE_NEWNS) to create a fully isolated
/// mount namespace for host-safe mount tests.
/// Linux stable value: 1<<18 = 262144.
pub const MS_PRIVATE: u64 = 262144;

/// Check whether a path is currently a mount point by scanning /proc/mounts.
///
/// Returns true when the path matches field 2 (the mount target) in any line.
/// Returns false on any read error so the caller retries the mount (a redundant
/// mount fails loudly rather than silently skipping). This mirrors isMounted in
/// guest/agent/notifyforked.go:283-296.
///
/// On non-Linux platforms always returns false (mount is a no-op there).
pub fn is_mounted(mount_path: &str) -> bool {
    #[cfg(target_os = "linux")]
    {
        is_mounted_linux(mount_path)
    }
    #[cfg(not(target_os = "linux"))]
    {
        let _ = mount_path;
        false
    }
}

#[cfg(target_os = "linux")]
fn is_mounted_linux(mount_path: &str) -> bool {
    mounted_fstype(mount_path).is_some()
}

/// Return the filesystem type mounted at `mount_path`, or None when nothing
/// is mounted there (or /proc/mounts cannot be read, so the caller falls
/// through to a mount attempt that fails loudly rather than silently skipping).
///
/// PID-1 init uses this to detect targets the kernel already mounted before
/// the agent ran: with CONFIG_DEVTMPFS_MOUNT the kernel automounts devtmpfs
/// on /dev, so init's own devtmpfs mount would return EBUSY (issue #668).
///
/// On non-Linux platforms always returns None (mount is a no-op there).
pub fn mounted_fstype(mount_path: &str) -> Option<String> {
    let content = read_proc_mounts()?;
    fstype_from_mounts(&content, mount_path)
}

#[cfg(target_os = "linux")]
fn read_proc_mounts() -> Option<String> {
    std::fs::read_to_string("/proc/mounts").ok()
}

#[cfg(not(target_os = "linux"))]
fn read_proc_mounts() -> Option<String> {
    None
}

/// Parse /proc/mounts content: return the fstype (field 3) of the line whose
/// mount target (field 2) equals `mount_path` exactly. When several lines
/// match (overmounts), the LAST one wins: /proc/mounts is ordered oldest
/// first and the most recent mount is the visible one. Separated from the
/// /proc/mounts read so it is unit-testable on any host.
fn fstype_from_mounts(content: &str, mount_path: &str) -> Option<String> {
    let mut found = None;
    for line in content.lines() {
        let mut fields = line.split_ascii_whitespace();
        // Field 0: device, field 1: mount target, field 2: fstype.
        let _device = fields.next();
        if let Some(target) = fields.next()
            && target == mount_path
        {
            found = fields.next().map(str::to_string);
        }
    }
    found
}

/// Mount a filesystem.
///
/// Wraps the Linux mount(2) syscall. Flags is passed directly (0 = no flags,
/// matching the Go agent). On non-Linux platforms this is a no-op returning Ok(()).
pub fn mount(source: &str, target: &str, fstype: &str, flags: u64) -> io::Result<()> {
    #[cfg(target_os = "linux")]
    {
        mount_linux(source, target, fstype, flags)
    }
    #[cfg(not(target_os = "linux"))]
    {
        let _ = (source, target, fstype, flags);
        Ok(())
    }
}

/// Set the system hostname.
///
/// Wraps the Linux sethostname(2) syscall. On non-Linux platforms this is a
/// no-op returning Ok(()).
pub fn sethostname(name: &str) -> io::Result<()> {
    #[cfg(target_os = "linux")]
    {
        sethostname_linux(name)
    }
    #[cfg(not(target_os = "linux"))]
    {
        let _ = name;
        Ok(())
    }
}

#[cfg(target_os = "linux")]
fn mount_linux(source: &str, target: &str, fstype: &str, flags: u64) -> io::Result<()> {
    use std::ffi::CString;

    let source = CString::new(source)
        .map_err(|e| io::Error::new(io::ErrorKind::InvalidInput, e))?;
    let target = CString::new(target)
        .map_err(|e| io::Error::new(io::ErrorKind::InvalidInput, e))?;
    let fstype = CString::new(fstype)
        .map_err(|e| io::Error::new(io::ErrorKind::InvalidInput, e))?;

    // SAFETY:
    // - source, target, fstype are valid NUL-terminated C strings owned by
    //   the CString bindings above; they are alive for the duration of this call.
    // - flags is passed as c_ulong directly (the kernel accepts 0 as "no flags").
    // - The final data argument is NULL, matching the Go agent which passes "".
    // - mount(2) does not retain any of the pointers after it returns.
    let ret = unsafe {
        libc::mount(
            source.as_ptr(),
            target.as_ptr(),
            fstype.as_ptr(),
            flags as libc::c_ulong,
            std::ptr::null(),
        )
    };
    if ret == 0 {
        Ok(())
    } else {
        Err(io::Error::last_os_error())
    }
}

#[cfg(target_os = "linux")]
fn sethostname_linux(name: &str) -> io::Result<()> {
    // SAFETY:
    // - name.as_ptr() points to the first byte of a live &str; name.len()
    //   bytes are valid. sethostname(2) reads exactly name.len() bytes.
    // - sethostname does not retain the pointer after it returns.
    let ret = unsafe {
        libc::sethostname(name.as_ptr() as *const libc::c_char, name.len())
    };
    if ret == 0 {
        Ok(())
    } else {
        Err(io::Error::last_os_error())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const MOUNTS: &str = "\
proc /proc proc rw,nosuid,nodev,noexec,relatime 0 0
sysfs /sys sysfs rw,nosuid,nodev,noexec,relatime 0 0
devtmpfs /dev devtmpfs rw,nosuid,size=12448k,nr_inodes=31112,mode=755 0 0
/dev/vda / ext4 rw,relatime 0 0
tmpfs /tmp tmpfs rw,relatime 0 0
";

    #[test]
    fn fstype_from_mounts_finds_dev_devtmpfs() {
        assert_eq!(
            fstype_from_mounts(MOUNTS, "/dev"),
            Some("devtmpfs".to_string())
        );
    }

    #[test]
    fn fstype_from_mounts_none_for_unmounted_path() {
        assert_eq!(fstype_from_mounts(MOUNTS, "/run"), None);
    }

    #[test]
    fn fstype_from_mounts_matches_exact_target_only() {
        // "/de" and "/dev/pts" must not match the "/dev" entry.
        assert_eq!(fstype_from_mounts(MOUNTS, "/de"), None);
        assert_eq!(fstype_from_mounts(MOUNTS, "/dev/pts"), None);
    }

    #[test]
    fn fstype_from_mounts_ignores_malformed_lines() {
        assert_eq!(fstype_from_mounts("devtmpfs /dev\n\n", "/dev"), None);
    }

    #[test]
    fn fstype_from_mounts_last_overmount_wins() {
        // /proc/mounts is ordered oldest first; the visible mount at a
        // target is the most recent line.
        let content = "devtmpfs /dev devtmpfs rw 0 0\ntmpfs /dev tmpfs rw 0 0\n";
        assert_eq!(
            fstype_from_mounts(content, "/dev"),
            Some("tmpfs".to_string())
        );
    }

    #[test]
    fn mount_no_op_on_non_linux_or_bad_target() {
        // On macOS this is a compile-out no-op and always returns Ok.
        // On Linux it will fail (EPERM or ENOENT) since tests don't run as root
        // with real mountpoints, which is acceptable: the function's compile path
        // is exercised.
        let _ = mount("proc", "/proc", "proc", 0);
    }

    #[test]
    fn sethostname_no_op_on_non_linux_or_no_perm() {
        // Same rationale as mount: we exercise the function's code path.
        let _ = sethostname("test-sandbox");
    }
}
