// Fork-correctness: SIGUSR2 broadcast to userspace processes after a
// snapshot restore.
//
// After a fork, language runtimes (Go, Python, OpenSSL, etc.) may hold
// PRNG state derived from the snapshot CRNG. SIGUSR2 is the conventional
// in-process reseed signal: runtimes that install a SIGUSR2 handler can
// re-derive their PRNG state on receipt.
//
// This module is a thin, safe wrapper over sys::signal::signal_userspace.
// All syscall logic (getpid, /proc walk, kill) lives in sys/signal.rs.
// Mirrors signalUserspace in guest/agent/notifyforked.go:299-328.

/// Sends SIGUSR2 to all userspace processes except PID 1 and the current
/// process. Returns the count of processes that received the signal.
///
/// Delegates to sys::signal::signal_userspace (production: walks real /proc).
/// Mirrors signalUserspace in notifyforked.go:299-328.
pub fn signal_userspace() -> i32 {
    crate::sys::signal::signal_userspace()
}

// ---------------------------------------------------------------------------
// Tests.
// Note: we do NOT call signal_userspace() (real /proc) in tests because
// the test runner is multi-process and SIGUSR2 would disrupt sibling test
// processes (forked children from other test threads). All SIGUSR2 delivery
// testing is done via the synthetic-proc path in sys::signal::tests.
// ---------------------------------------------------------------------------

#[cfg(test)]
#[allow(
    clippy::expect_used,
    clippy::unwrap_used,
    clippy::panic,
    clippy::indexing_slicing,
)]
mod tests {
    // signal_userspace() delegates entirely to sys::signal; coverage lives
    // in sys::signal::tests::sigusr2_delivered_to_child_via_synthetic_proc.
    // No additional tests needed here that would require calling real /proc.
}
