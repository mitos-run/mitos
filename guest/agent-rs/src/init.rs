// PID-1 init: mounts essential filesystems, creates /workspace, sets hostname.
// Mirrors initSystem in guest/agent/main.go. All steps are non-fatal on error:
// log to stderr and continue, because a failure in PID 1 init must not halt
// the guest entirely.

/// Describes a single filesystem mount. The fstype and source fields mirror the
/// corresponding arguments to mount(2) and to syscall.Mount in the Go agent.
pub struct MountSpec {
    pub source: &'static str,
    pub target: &'static str,
    pub fstype: &'static str,
}

/// Returns the five mounts performed by initSystem in guest/agent/main.go,
/// in the same order. This function is cross-platform so the unit test runs on
/// macOS; the actual syscalls are gated with #[cfg(target_os = "linux")].
pub fn mount_table() -> Vec<MountSpec> {
    vec![
        MountSpec { source: "proc",     target: "/proc", fstype: "proc"     },
        MountSpec { source: "sysfs",    target: "/sys",  fstype: "sysfs"    },
        MountSpec { source: "devtmpfs", target: "/dev",  fstype: "devtmpfs" },
        MountSpec { source: "tmpfs",    target: "/tmp",  fstype: "tmpfs"    },
        MountSpec { source: "tmpfs",    target: "/run",  fstype: "tmpfs"    },
    ]
}

/// Perform PID-1 init: mount filesystems, create /workspace, set hostname.
/// Each step is non-fatal: errors are logged to stderr and execution continues.
/// Mirrors initSystem in guest/agent/main.go.
///
/// On non-Linux platforms the syscall-dependent steps are compiled out so the
/// crate builds on macOS for host development. mount_table() itself is always
/// available.
pub fn init_system() {
    #[cfg(target_os = "linux")]
    for m in mount_table() {
        // mkdir -p the target before mounting, same as os.MkdirAll in Go.
        if let Err(e) = std::fs::create_dir_all(m.target) {
            eprintln!("mkdir {}: {}", m.target, e);
        }

        // Perform the mount. Flags = 0 matches the Go agent (no MS_* flags).
        let source = std::ffi::CString::new(m.source).unwrap();
        let target = std::ffi::CString::new(m.target).unwrap();
        let fstype = std::ffi::CString::new(m.fstype).unwrap();
        let data = std::ffi::CString::new("").unwrap();
        let ret = unsafe {
            libc::mount(
                source.as_ptr(),
                target.as_ptr(),
                fstype.as_ptr(),
                0,
                data.as_ptr() as *const libc::c_void,
            )
        };
        if ret != 0 {
            let err = std::io::Error::last_os_error();
            eprintln!("mount {}: {}", m.target, err);
        }
    }

    // mkdir -p /workspace (non-fatal on failure, mirroring Go).
    #[cfg(target_os = "linux")]
    {
        if let Err(e) = std::fs::create_dir_all("/workspace") {
            eprintln!("mkdir /workspace: {}", e);
        }
    }

    // sethostname "sandbox" (non-fatal on failure, mirroring Go).
    #[cfg(target_os = "linux")]
    {
        let name = b"sandbox";
        let ret = unsafe { libc::sethostname(name.as_ptr() as *const libc::c_char, name.len()) };
        if ret != 0 {
            let err = std::io::Error::last_os_error();
            eprintln!("sethostname: {}", err);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::mount_table;
    #[test]
    fn mount_table_matches_go_agent() {
        let t = mount_table();
        let targets: Vec<&str> = t.iter().map(|m| m.target).collect();
        assert_eq!(targets, vec!["/proc", "/sys", "/dev", "/tmp", "/run"]);
    }
}
