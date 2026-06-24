//! Fork-correctness modules: post-restore state repair after a Firecracker
//! snapshot restore. These modules are invoked by the notify-forked handler
//! (Task 3.5) and mirror the behavior of guest/agent/notifyforked.go.

/// Clock step: adjusts CLOCK_REALTIME toward the host wall clock after a
/// snapshot restore. Returns the signed step applied in nanoseconds (0 when
/// within the 500ms tolerance window, 0 on any error, 0 when host time is 0).
pub mod clock;

/// Credited CRNG reseed: injects host-supplied per-fork entropy via the
/// RNDADDENTROPY ioctl. Fail-closed: returns false if the credited inject
/// fails so the host reaps the fork rather than serving it with shared CRNG
/// state.
pub mod reseed;
