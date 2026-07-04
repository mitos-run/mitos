// PID-1 init: mounts essential filesystems, creates /workspace, sets hostname.
// Mirrors initSystem in guest/agent/main.go:78-112.
//
// All steps are non-fatal: errors are logged with tracing::error and execution
// continues, because a PID-1 init failure must not halt the guest entirely.
// This matches the Go agent's behavior (fmt.Fprintf(os.Stderr, ...) + continue).

use crate::sys::mount::{mount, mounted_fstype, sethostname};
use tracing::error;

/// What to do for a mount-table entry given what is already mounted at its
/// target. The kernel automounts devtmpfs on /dev before init runs
/// (CONFIG_DEVTMPFS_MOUNT), so a blind mount there always returns EBUSY;
/// logging that as an ERROR is boot-log noise that gets real CI failures
/// misattributed to the guest (issue #668).
#[derive(Debug, PartialEq, Eq)]
enum PremountAction {
    /// Nothing mounted at the target: perform the mount.
    Mount,
    /// The target already carries the expected fstype: skip silently
    /// (info-level), the work is done.
    SkipAlreadyMounted,
    /// The target is mounted with a DIFFERENT fstype: skip but log an error,
    /// because the guest image is in a state init does not expect.
    SkipUnexpectedFstype,
}

/// Decide the premount action from the fstype currently mounted at the
/// target (None = not mounted) and the fstype the mount table wants.
fn premount_action(existing: Option<&str>, want_fstype: &str) -> PremountAction {
    match existing {
        None => PremountAction::Mount,
        Some(fs) if fs == want_fstype => PremountAction::SkipAlreadyMounted,
        Some(_) => PremountAction::SkipUnexpectedFstype,
    }
}

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

/// The mounts performed at PID-1 init, in order. The first five mirror
/// initSystem in the retired Go agent (guest/agent/main.go:83-107). devpts is
/// mounted after /dev: without it openpty(3) cannot allocate a PTY (it opens
/// /dev/ptmx and then /dev/pts/N), so every PTY exec failed at open and the
/// client saw the stream close right after spawn (issue #535). It must come
/// after the devtmpfs /dev mount because /dev/pts lives inside that tree.
/// Exported as a const slice so unit tests can inspect the table without
/// invoking any syscalls.
pub const MOUNT_TABLE: &[MountSpec] = &[
    MountSpec { source: "proc",     target: "/proc",     fstype: "proc"     },
    MountSpec { source: "sysfs",    target: "/sys",      fstype: "sysfs"    },
    MountSpec { source: "devtmpfs", target: "/dev",      fstype: "devtmpfs" },
    MountSpec { source: "devpts",   target: "/dev/pts",  fstype: "devpts"   },
    MountSpec { source: "tmpfs",    target: "/tmp",      fstype: "tmpfs"    },
    MountSpec { source: "tmpfs",    target: "/run",      fstype: "tmpfs"    },
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
        let existing = mounted_fstype(m.target);
        match premount_action(existing.as_deref(), m.fstype) {
            PremountAction::SkipAlreadyMounted => {
                tracing::info!("{} already mounted ({}), skipping", m.target, m.fstype);
            }
            PremountAction::SkipUnexpectedFstype => {
                error!(
                    "{}: already mounted as {} (expected {}); leaving it in place",
                    m.target,
                    existing.as_deref().unwrap_or("?"),
                    m.fstype
                );
            }
            PremountAction::Mount => {
                if let Err(e) = mount(m.source, m.target, m.fstype, 0) {
                    error!("mount {}: {}", m.target, e);
                }
            }
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
    fn mount_table_order_is_stable() {
        // The Go-agent order (/proc /sys /dev /tmp /run, guest/agent/main.go
        // lines 83-107) plus /dev/pts, which MUST come after /dev: devpts
        // mounts inside the devtmpfs tree. Without devpts no PTY can be
        // allocated (openpty opens /dev/ptmx then /dev/pts/N), which is the
        // guest-side root cause of the PTY close-after-spawn in issue #535.
        let expected = ["/proc", "/sys", "/dev", "/dev/pts", "/tmp", "/run"];
        assert_eq!(
            MOUNT_TABLE.iter().map(|m| m.target).collect::<Vec<_>>(),
            expected
        );
    }

    #[test]
    fn mount_table_sources_are_stable() {
        let expected = ["proc", "sysfs", "devtmpfs", "devpts", "tmpfs", "tmpfs"];
        assert_eq!(
            MOUNT_TABLE.iter().map(|m| m.source).collect::<Vec<_>>(),
            expected
        );
    }

    #[test]
    fn mount_table_fstypes_are_stable() {
        let expected = ["proc", "sysfs", "devtmpfs", "devpts", "tmpfs", "tmpfs"];
        assert_eq!(
            MOUNT_TABLE.iter().map(|m| m.fstype).collect::<Vec<_>>(),
            expected
        );
    }

    #[test]
    fn mount_table_mounts_devpts_after_dev() {
        // devpts must be mounted after its parent devtmpfs /dev mount so the
        // /dev/pts directory can be created inside the mounted (or kernel
        // automounted, see #668) devtmpfs.
        let dev = MOUNT_TABLE.iter().position(|m| m.target == "/dev");
        let pts = MOUNT_TABLE.iter().position(|m| m.target == "/dev/pts");
        assert!(dev.is_some(), "mount table must mount /dev");
        assert!(pts.is_some(), "mount table must mount /dev/pts (devpts)");
        assert!(pts > dev, "/dev/pts must be mounted after /dev");
    }

    #[test]
    fn mount_table_has_six_entries() {
        assert_eq!(MOUNT_TABLE.len(), 6);
    }

    // Premount decision: the kernel automounts devtmpfs on /dev before init
    // runs (CONFIG_DEVTMPFS_MOUNT), so init must skip an already-mounted
    // target instead of logging a false ERROR on the EBUSY (issue #668).
    use super::{PremountAction, premount_action};

    #[test]
    fn premount_skips_target_already_mounted_with_expected_fstype() {
        assert_eq!(
            premount_action(Some("devtmpfs"), "devtmpfs"),
            PremountAction::SkipAlreadyMounted
        );
    }

    #[test]
    fn premount_flags_target_mounted_with_unexpected_fstype() {
        assert_eq!(
            premount_action(Some("tmpfs"), "devtmpfs"),
            PremountAction::SkipUnexpectedFstype
        );
    }

    #[test]
    fn premount_mounts_when_target_not_mounted() {
        assert_eq!(premount_action(None, "devtmpfs"), PremountAction::Mount);
    }
}
