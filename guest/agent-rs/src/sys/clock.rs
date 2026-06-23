// Fork-correctness syscall: clock_gettime / clock_settime for CLOCK_REALTIME.
//
// This file is the ONLY place that calls clock_gettime and clock_settime.
// Every unsafe block carries a SAFETY comment.
//
// Mirrors the stepClock logic in guest/agent/notifyforked.go:
// - Read CLOCK_REALTIME via clock_gettime.
// - If |drift| > CLOCK_STEP_THRESHOLD_NS, call clock_settime to step.
// - Returns the applied step in nanoseconds (0 if within tolerance or error).
// - Never panics, never unwraps.
//
// ABI verified on box1:
//   struct timespec { time_t tv_sec; long tv_nsec; }
//   sizeof(timespec)=16, tv_sec at offset 0, tv_nsec at offset 8.
//   CLOCK_REALTIME = 0 (from linux_like/mod.rs in libc crate).

// unsafe_code is permitted in this file via the #[allow(unsafe_code)] on the
// `pub mod sys;` declaration in lib.rs. We do not repeat the allow here
// (clippy flags duplicated attributes).
#![deny(unsafe_op_in_unsafe_fn)]

use std::io;

// CLOCK_STEP_THRESHOLD_NS: drift below this magnitude (in nanoseconds) is
// left untouched to avoid fighting in-guest NTP discipline.
// Mirrors clockStepThresholdNanos = 500ms in notifyforked.go.
pub const CLOCK_STEP_THRESHOLD_NS: i64 = 500_000_000; // 500 ms

/// Read CLOCK_REALTIME and return the value in nanoseconds since the epoch.
///
/// Returns `Err` if the syscall fails. Never panics.
///
/// Available on Linux only; on other platforms this always returns an error.
pub fn clock_now_nanos() -> io::Result<i64> {
    #[cfg(target_os = "linux")]
    {
        clock_now_nanos_linux()
    }

    #[cfg(not(target_os = "linux"))]
    {
        Err(io::Error::new(
            io::ErrorKind::Unsupported,
            "clock_now_nanos is Linux-only",
        ))
    }
}

/// Step CLOCK_REALTIME to `target_nanos` (nanoseconds since the epoch).
///
/// Returns `Err` if clock_settime fails. Never panics.
///
/// Available on Linux only; on other platforms this always returns an error.
pub fn clock_set_realtime(target_nanos: i64) -> io::Result<()> {
    #[cfg(target_os = "linux")]
    {
        clock_set_realtime_linux(target_nanos)
    }

    #[cfg(not(target_os = "linux"))]
    {
        let _ = target_nanos;
        Err(io::Error::new(
            io::ErrorKind::Unsupported,
            "clock_set_realtime is Linux-only",
        ))
    }
}

/// Step CLOCK_REALTIME toward `host_wall_nanos` when drift exceeds the
/// threshold. Returns the signed adjustment applied in nanoseconds
/// (positive = guest was behind; negative = guest was ahead). Returns 0
/// when within the tolerance window, when `host_wall_nanos` is 0, or on
/// any syscall error.
///
/// Mirrors `stepClock` in `guest/agent/notifyforked.go`.
pub fn step_clock(host_wall_nanos: i64) -> i64 {
    if host_wall_nanos == 0 {
        return 0;
    }

    let guest_nanos = match clock_now_nanos() {
        Ok(n) => n,
        Err(e) => {
            eprintln!("sys::clock: clock_gettime: {e}");
            return 0;
        }
    };

    let drift = host_wall_nanos - guest_nanos;
    if drift.abs() <= CLOCK_STEP_THRESHOLD_NS {
        return 0;
    }

    if let Err(e) = clock_set_realtime(host_wall_nanos) {
        eprintln!("sys::clock: clock_settime: {e}");
        return 0;
    }

    drift
}

#[cfg(target_os = "linux")]
fn clock_now_nanos_linux() -> io::Result<i64> {
    // SAFETY:
    // - libc::timespec is initialized to zero before being passed as a mutable
    //   pointer to clock_gettime. The kernel writes into the struct only if
    //   the call succeeds (return value 0).
    // - CLOCK_REALTIME (0) is a valid clock ID on all Linux kernels this
    //   agent runs on.
    // - The returned ts is read only after the success check; no use-before-init.
    let mut ts = libc::timespec {
        tv_sec: 0,
        tv_nsec: 0,
    };
    let ret = unsafe {
        // SAFETY: &mut ts is a valid, aligned pointer to a zeroed libc::timespec.
        // clock_gettime fills it on success and does not retain the pointer.
        libc::clock_gettime(libc::CLOCK_REALTIME, &mut ts)
    };
    if ret != 0 {
        return Err(io::Error::last_os_error());
    }
    // Convert tv_sec and tv_nsec to total nanoseconds.
    // On Linux x86_64/aarch64 both fields are i64 (time_t = i64, c_long = i64);
    // the casts would be no-ops and clippy flags them. We use the fields directly.
    // Saturating arithmetic: if the system clock is so far in the future that
    // this overflows, the drift will be large and a step will be attempted,
    // which is the correct conservative behavior.
    let nanos = ts.tv_sec
        .saturating_mul(1_000_000_000)
        .saturating_add(ts.tv_nsec);
    Ok(nanos)
}

#[cfg(target_os = "linux")]
fn clock_set_realtime_linux(target_nanos: i64) -> io::Result<()> {
    let tv_sec = target_nanos / 1_000_000_000;
    let tv_nsec = target_nanos % 1_000_000_000;
    let ts = libc::timespec {
        tv_sec: tv_sec as libc::time_t,
        tv_nsec: tv_nsec as libc::c_long,
    };
    // SAFETY:
    // - &ts is a valid, aligned pointer to a fully initialized libc::timespec.
    //   tv_sec and tv_nsec are computed from target_nanos; tv_nsec is in
    //   [0, 999_999_999] (a valid nanosecond count) because we used %, and
    //   target_nanos is a caller-supplied value (not a secret).
    // - CLOCK_REALTIME (0) is a valid clock ID.
    // - clock_settime does not retain the pointer after the call returns.
    let ret = unsafe {
        // SAFETY: ts is a fully initialized libc::timespec (see above).
        libc::clock_settime(libc::CLOCK_REALTIME, &ts)
    };
    if ret != 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(())
}

#[cfg(test)]
#[allow(clippy::expect_used, clippy::unwrap_used)]
mod tests {
    use super::*;

    #[test]
    fn step_clock_zero_input_returns_zero() {
        // host_wall_nanos == 0 must be a no-op and return 0.
        assert_eq!(step_clock(0), 0);
    }

    #[test]
    fn clock_step_threshold_is_500ms() {
        assert_eq!(CLOCK_STEP_THRESHOLD_NS, 500_000_000);
    }

    // On Linux we can test clock_now_nanos returns a plausible value.
    #[cfg(target_os = "linux")]
    mod linux {
        use super::*;

        #[test]
        fn clock_now_nanos_returns_plausible_value() {
            let nanos = clock_now_nanos().expect("clock_now_nanos must succeed on Linux");
            // Must be after 2020-01-01 (1577836800 seconds = 1577836800_000_000_000 ns)
            // and before 2100-01-01 (4102444800_000_000_000 ns). These bounds are
            // loose enough that a running CI system will always satisfy them.
            let min_nanos: i64 = 1_577_836_800_000_000_000;
            let max_nanos: i64 = 4_102_444_800_000_000_000;
            assert!(
                nanos > min_nanos && nanos < max_nanos,
                "clock_now_nanos returned {nanos} which is outside the plausible range"
            );
        }

        #[test]
        fn step_clock_within_threshold_returns_zero() {
            // If the host time is very close to now (within threshold), step_clock
            // must return 0 (no adjustment needed).
            let now = clock_now_nanos().expect("clock_now_nanos must succeed");
            // Add 100ms (well within 500ms threshold).
            let close = now + 100_000_000;
            let result = step_clock(close);
            assert_eq!(
                result, 0,
                "drift within threshold must produce step of 0, got {result}"
            );
        }
    }
}
