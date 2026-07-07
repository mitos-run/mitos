// Per-process resource limits for spawned guest processes (issue #786).
//
// The guest agent is PID 1. A bare Linux init inherits a low RLIMIT_NOFILE soft
// limit (commonly 1024), which every exec/run_code child then inherits. Data
// libraries that open many files at import time (pandas, openpyxl, scikit) hit
// EMFILE ("too many open files") under that default, so a plain `import pandas`
// can die inside a sandbox even though the host-side caps (husk pod cgroup,
// per-sandbox stream caps) are nowhere near saturated. Those host caps do NOT
// govern in-guest per-process rlimits; only setrlimit inside the guest does.
//
// This module raises the soft RLIMIT_NOFILE of every process the agent spawns to
// a sane documented default, clamped to the inherited hard limit and never
// lowered below what the process already has. An operator override is read from
// the agent's environment (MITOS_RLIMIT_NOFILE); see apply_nofile_limit.
//
// The raise is installed via a `pre_exec` hook, which runs in the forked child
// after `fork(2)` and before `execve(2)`. That context permits only
// async-signal-safe work: the closure performs a getrlimit syscall, integer
// arithmetic (resolve_nofile_soft, a pure function), and a setrlimit syscall. It
// allocates nothing and touches no locks, mirroring the safety contract of the
// setsid/TIOCSCTTY hook in sys/pty.rs.

#![deny(unsafe_op_in_unsafe_fn)]

/// Documented default soft limit for RLIMIT_NOFILE inside the guest.
///
/// 65536 is high enough for the data-science import path that motivated this
/// change and well under the kernel's default fs.nr_open ceiling (1048576), so
/// setrlimit never fails when the value is clamped to the inherited hard limit.
pub const DEFAULT_NOFILE_SOFT: u64 = 65536;

/// Environment variable an operator (or the future template knob) sets to
/// override the default soft RLIMIT_NOFILE. The value is a decimal file count.
/// It is read from the agent's own environment, so it applies uniformly to
/// every spawned process (exec, PTY, run_code kernel, serving workload).
pub const RLIMIT_NOFILE_ENV: &str = "MITOS_RLIMIT_NOFILE";

/// Parse the operator override value for the soft NOFILE limit.
///
/// Accepts a decimal file count. Returns `None` for empty or unparseable input
/// so the caller falls back to `DEFAULT_NOFILE_SOFT`. Only finite decimal values
/// are accepted deliberately: an "unlimited" soft NOFILE is rejected by the
/// kernel above fs.nr_open and would turn a benign misconfiguration into a spawn
/// failure, which violates the boring-failure rule.
pub fn parse_nofile_override(raw: &str) -> Option<u64> {
    let trimmed = raw.trim();
    if trimmed.is_empty() {
        return None;
    }
    trimmed.parse::<u64>().ok()
}

/// Compute the soft RLIMIT_NOFILE to install given the process's current
/// `(soft, hard)` pair and an optional operator override.
///
/// Pure and side-effect free so it is both unit-testable and safe to call from
/// the async-signal-safe pre_exec closure (integer arithmetic only).
///
/// Rules:
///   - With an explicit override, honor it exactly, clamped to the hard limit
///     (raising the hard limit needs CAP_SYS_RESOURCE and is out of scope).
///     An operator may deliberately choose a lower value than the current soft.
///   - Without an override, raise toward `DEFAULT_NOFILE_SOFT` but never above
///     the hard limit and never below the current soft limit. Clamping to the
///     hard limit guarantees the subsequent setrlimit (soft <= hard) succeeds.
pub fn resolve_nofile_soft(current_soft: u64, hard: u64, override_target: Option<u64>) -> u64 {
    match override_target {
        Some(target) => target.min(hard),
        None => DEFAULT_NOFILE_SOFT.min(hard).max(current_soft),
    }
}

/// Read the operator override for the soft NOFILE limit from the agent's
/// environment. Logs a warning (never the raw secret-free value silently) when
/// the variable is set but cannot be parsed, then returns `None` so the default
/// applies. The value is a plain file count, never a secret.
fn configured_nofile_override() -> Option<u64> {
    match std::env::var(RLIMIT_NOFILE_ENV) {
        Ok(raw) => {
            let parsed = parse_nofile_override(&raw);
            if parsed.is_none() {
                tracing::warn!(
                    var = RLIMIT_NOFILE_ENV,
                    value = %raw,
                    "ignoring unparseable RLIMIT_NOFILE override; using default {}",
                    DEFAULT_NOFILE_SOFT
                );
            }
            parsed
        }
        Err(_) => None,
    }
}

/// Register a `pre_exec` hook on `cmd` that raises the child's soft
/// RLIMIT_NOFILE to the resolved default (or operator override) before exec.
///
/// Linux only: on other platforms this is a no-op (the guest runs Linux; the
/// hook is compiled out on the darwin developer build, matching sys/pty.rs).
///
/// The override is read once here, in the parent, so the closure captures a
/// plain `Option<u64>` and performs no allocation or environment lookup in the
/// post-fork child.
pub fn apply_nofile_limit(cmd: &mut std::process::Command) {
    let override_target = configured_nofile_override();
    apply_nofile_limit_inner(cmd, override_target);
}

#[cfg(target_os = "linux")]
fn apply_nofile_limit_inner(cmd: &mut std::process::Command, override_target: Option<u64>) {
    use std::os::unix::process::CommandExt;

    // SAFETY: this closure runs in the child after fork(2) and before execve(2),
    // where only async-signal-safe work is permitted. It performs exactly:
    //   1. getrlimit(RLIMIT_NOFILE): reads the inherited (soft, hard) pair. A
    //      raw syscall, async-signal-safe.
    //   2. resolve_nofile_soft(...): pure integer arithmetic, no allocation.
    //   3. setrlimit(RLIMIT_NOFILE): installs the new soft limit (<= hard, so it
    //      cannot fail on the value). A raw syscall, async-signal-safe.
    // No heap allocation, no locks, no Rust runtime calls. io::Error::last_os_error
    // reads errno only (same pattern as sys/pty.rs). rlim_t is u64 on Linux.
    unsafe {
        cmd.pre_exec(move || {
            let mut lim = libc::rlimit {
                rlim_cur: 0,
                rlim_max: 0,
            };
            if libc::getrlimit(libc::RLIMIT_NOFILE, &mut lim) != 0 {
                return Err(std::io::Error::last_os_error());
            }
            let new_soft =
                resolve_nofile_soft(lim.rlim_cur as u64, lim.rlim_max as u64, override_target);
            lim.rlim_cur = new_soft as libc::rlim_t;
            if libc::setrlimit(libc::RLIMIT_NOFILE, &lim) != 0 {
                return Err(std::io::Error::last_os_error());
            }
            Ok(())
        });
    }
}

#[cfg(not(target_os = "linux"))]
fn apply_nofile_limit_inner(cmd: &mut std::process::Command, override_target: Option<u64>) {
    let _ = (cmd, override_target);
}

#[cfg(test)]
#[allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]
mod tests {
    use super::*;

    #[test]
    fn parse_accepts_plain_decimal() {
        assert_eq!(parse_nofile_override("65536"), Some(65536));
        assert_eq!(parse_nofile_override("  4096  "), Some(4096));
        assert_eq!(parse_nofile_override("1"), Some(1));
    }

    #[test]
    fn parse_rejects_empty_and_garbage() {
        assert_eq!(parse_nofile_override(""), None);
        assert_eq!(parse_nofile_override("   "), None);
        assert_eq!(parse_nofile_override("lots"), None);
        assert_eq!(parse_nofile_override("-1"), None);
        assert_eq!(parse_nofile_override("6.5"), None);
        assert_eq!(parse_nofile_override("unlimited"), None);
    }

    #[test]
    fn default_raises_soft_toward_documented_value() {
        // A bare init: soft 1024, generous hard. We raise to the default.
        assert_eq!(
            resolve_nofile_soft(1024, 1_048_576, None),
            DEFAULT_NOFILE_SOFT
        );
    }

    #[test]
    fn default_never_exceeds_hard() {
        // Hard below the default: clamp to hard so setrlimit cannot fail.
        assert_eq!(resolve_nofile_soft(1024, 4096, None), 4096);
    }

    #[test]
    fn default_never_lowers_an_already_high_soft() {
        // A process that already has a soft above the default keeps it.
        assert_eq!(resolve_nofile_soft(100_000, 1_048_576, None), 100_000);
    }

    #[test]
    fn override_is_honored_and_clamped_to_hard() {
        // Operator raises higher than the default.
        assert_eq!(resolve_nofile_soft(1024, 1_048_576, Some(200_000)), 200_000);
        // Operator override above hard is clamped down to hard.
        assert_eq!(resolve_nofile_soft(1024, 8192, Some(200_000)), 8192);
    }

    #[test]
    fn override_may_lower_deliberately() {
        // An operator that explicitly asks for a lower value gets it.
        assert_eq!(resolve_nofile_soft(4096, 1_048_576, Some(512)), 512);
    }

    // The real proof (a spawned child actually sees the raised limit) needs a
    // running Linux process; this asserts the pre_exec hook installs and the
    // child reports the raised soft limit. It is Linux-gated (matches the guest
    // target) and so is skipped on the darwin developer build, exactly like the
    // spawn tests in service/workload.rs.
    #[cfg(target_os = "linux")]
    #[test]
    fn spawned_child_sees_raised_soft_nofile() {
        use std::process::Command;
        // Lower this process's own soft NOFILE so the raise is observable even
        // when the test host already starts high.
        let mut cur = libc::rlimit {
            rlim_cur: 0,
            rlim_max: 0,
        };
        // SAFETY: getrlimit reads into a local; setrlimit lowers our own soft.
        unsafe {
            assert_eq!(libc::getrlimit(libc::RLIMIT_NOFILE, &mut cur), 0);
        }
        let hard = cur.rlim_max as u64;
        // Only meaningful when the hard limit is above 1024.
        if hard <= 1024 {
            return;
        }
        let lowered = libc::rlimit {
            rlim_cur: 1024,
            rlim_max: cur.rlim_max,
        };
        // SAFETY: lowering our own soft limit; harmless for the test process.
        unsafe {
            assert_eq!(libc::setrlimit(libc::RLIMIT_NOFILE, &lowered), 0);
        }

        let mut cmd = Command::new("/bin/sh");
        cmd.arg("-c").arg("ulimit -Sn");
        apply_nofile_limit(&mut cmd);
        let out = cmd.output().expect("spawn ulimit");
        let reported = String::from_utf8_lossy(&out.stdout);
        let soft: u64 = reported.trim().parse().expect("numeric ulimit -Sn");
        let expected = resolve_nofile_soft(1024, hard, None);
        assert_eq!(
            soft, expected,
            "child soft NOFILE should be raised to the resolved default"
        );

        // Restore our own soft limit so later tests are unaffected.
        // SAFETY: restoring the original limit read above.
        unsafe {
            let _ = libc::setrlimit(libc::RLIMIT_NOFILE, &cur);
        }
    }
}
