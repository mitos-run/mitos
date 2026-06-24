// Fork-correctness: CLOCK_REALTIME step after snapshot restore.
//
// A restored guest's wall clock is frozen at snapshot time. This module
// adjusts it toward the host wall clock delivered in the notify-forked
// request. It is a thin, safe wrapper around sys::clock::step_clock; all
// unsafe syscall code lives in sys::clock.
//
// Mirrors stepClock in guest/agent/notifyforked.go:138-164.
// CLOCK_MONOTONIC is deliberately NOT touched: Linux rejects
// clock_settime(CLOCK_MONOTONIC) with EINVAL. See notifyforked.go:125-135.

/// Drift below this magnitude (in nanoseconds) is left untouched to avoid
/// fighting in-guest NTP discipline. 500ms, mirrors clockStepThresholdNanos
/// in notifyforked.go:23.
pub const CLOCK_STEP_THRESHOLD_NANOS: i64 = crate::sys::clock::CLOCK_STEP_THRESHOLD_NS;

/// Step CLOCK_REALTIME toward `host_wall_clock_nanos` when drift exceeds the
/// 500ms threshold. Returns the signed step applied in nanoseconds (positive
/// means the guest was behind; negative means the guest was ahead). Returns 0
/// when within tolerance, when `host_wall_clock_nanos` is 0, or on any
/// syscall error.
///
/// Mirrors stepClock in notifyforked.go:138-164. The absolute clock value is
/// never logged; only the applied step magnitude may appear in diagnostics.
pub fn step_clock(host_wall_clock_nanos: i64) -> i64 {
    crate::sys::clock::step_clock(host_wall_clock_nanos)
}

#[cfg(test)]
#[allow(clippy::expect_used, clippy::unwrap_used, clippy::panic)]
mod tests {
    #[test]
    fn zero_host_time_returns_zero() {
        assert_eq!(super::step_clock(0), 0);
    }

    #[test]
    fn within_threshold_returns_zero() {
        // A host time very close to now should be within 500ms and return 0.
        let now_nanos = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .expect("system time must be after the Unix epoch")
            .as_nanos() as i64;
        let result = super::step_clock(now_nanos + 100_000_000); // 100ms ahead
        assert_eq!(result, 0);
    }
}
