/// Fork-correctness modules: post-restore state repair after a Firecracker
/// snapshot restore. These modules are invoked by the notify-forked handler
/// (Task 3.5) and mirror the behavior of guest/agent/notifyforked.go.

/// Credited CRNG reseed: injects host-supplied per-fork entropy via the
/// RNDADDENTROPY ioctl. Fail-closed: returns false if the credited inject
/// fails so the host reaps the fork rather than serving it with shared CRNG
/// state.
pub mod reseed;
