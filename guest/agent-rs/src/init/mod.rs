// PID-1 init: mounts essential filesystems, creates /workspace, sets hostname.
// Mirrors initSystem in guest/agent/main.go:78-112.
//
// All steps are non-fatal: errors are logged with tracing::error and execution
// continues, because a PID-1 init failure must not halt the guest entirely.
// This matches the Go agent's behavior (fmt.Fprintf(os.Stderr, ...) + continue).

use crate::sys::mount::{mount, sethostname};
use tracing::error;

/// Describes a single filesystem mount.
///
/// The source, target, and fstype fields mirror the corresponding arguments to
/// mount(2) and to syscall.Mount in the Go agent (guest/agent/main.go:83-107).
pub struct MountSpec {
    /// Filesystem source (e.g. "proc", "sysfs", "devtmpfs", "tmpfs").
    pub source: &'static str,
    /// Mount point path (e.g. "/proc", "/sys", "/dev", "/tmp", "/run").
    pub target: &'static str,
    /// Filesystem type (e.g. "proc", "sysfs", "devtmpfs", "tmpfs").
    pub fstype: &'static str,
}

/// The five mounts performed by initSystem in guest/agent/main.go:83-107,
/// in the same order. Exported as a const slice so unit tests can inspect
/// the table without invoking any syscalls.
pub const MOUNT_TABLE: &[MountSpec] = &[
    MountSpec { source: "proc",     target: "/proc", fstype: "proc"     },
    MountSpec { source: "sysfs",    target: "/sys",  fstype: "sysfs"    },
    MountSpec { source: "devtmpfs", target: "/dev",  fstype: "devtmpfs" },
    MountSpec { source: "tmpfs",    target: "/tmp",  fstype: "tmpfs"    },
    MountSpec { source: "tmpfs",    target: "/run",  fstype: "tmpfs"    },
];

/// Perform PID-1 init: mount filesystems, create /workspace, set hostname.
///
/// Each step is non-fatal: errors are logged to the tracing subscriber and
/// execution continues. Mirrors initSystem in guest/agent/main.go:78-112.
///
/// On non-Linux platforms the sys::mount wrappers are no-ops, so this function
/// compiles and runs on macOS for host-side unit tests.
pub fn init_system() {
    for m in MOUNT_TABLE {
        if let Err(e) = std::fs::create_dir_all(m.target) {
            error!("mkdir {}: {}", m.target, e);
        }
        if let Err(e) = mount(m.source, m.target, m.fstype, 0) {
            error!("mount {}: {}", m.target, e);
        }
    }
    if let Err(e) = std::fs::create_dir_all("/workspace") {
        error!("mkdir /workspace: {}", e);
    }
    if let Err(e) = sethostname("sandbox") {
        error!("sethostname: {}", e);
    }
    // Bring up the loopback interface. The kernel assigns 127.0.0.1 to lo but
    // leaves the link DOWN; a serving workload (issue #460) binds 127.0.0.1 and
    // the StartWorkload HTTP ready gate polls 127.0.0.1, so without this the
    // workload is unreachable during the template build and the ready gate times
    // out. eth0 is brought up per-fork by the NotifyForked network path.
    if let Err(e) = crate::sys::netlink::link_up("lo") {
        error!("bring up loopback: {}", e);
    }
    // Seed the guest CRNG so getrandom() does not block. Without this a serving
    // workload (issue #460) that does crypto at startup hangs in
    // wait_for_random_bytes during the build (the guest kernel lacks
    // CONFIG_RANDOM_TRUST_CPU). Best-effort: a workload that does no early crypto
    // is unaffected if seeding fails.
    if !crate::sys::entropy::seed_crng_at_boot() {
        error!("seed CRNG at boot: no hardware entropy source (hwrng/rdrand) available");
    }
    tracing::info!("init complete");
}

#[cfg(test)]
#[allow(clippy::unwrap_used, clippy::expect_used)]
mod tests {
    use super::MOUNT_TABLE;

    #[test]
    fn mount_table_order_matches_go_agent() {
        // Mirrors mount_table_matches_go_agent in the spike's init.rs.
        // initSystem in guest/agent/main.go lines 83-107 mounts in this order.
        let expected = ["/proc", "/sys", "/dev", "/tmp", "/run"];
        assert_eq!(
            MOUNT_TABLE.iter().map(|m| m.target).collect::<Vec<_>>(),
            expected
        );
    }

    #[test]
    fn mount_table_sources_match_go_agent() {
        let expected = ["proc", "sysfs", "devtmpfs", "tmpfs", "tmpfs"];
        assert_eq!(
            MOUNT_TABLE.iter().map(|m| m.source).collect::<Vec<_>>(),
            expected
        );
    }

    #[test]
    fn mount_table_fstypes_match_go_agent() {
        let expected = ["proc", "sysfs", "devtmpfs", "tmpfs", "tmpfs"];
        assert_eq!(
            MOUNT_TABLE.iter().map(|m| m.fstype).collect::<Vec<_>>(),
            expected
        );
    }

    #[test]
    fn mount_table_has_five_entries() {
        assert_eq!(MOUNT_TABLE.len(), 5);
    }
}
