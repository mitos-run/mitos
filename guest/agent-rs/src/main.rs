//! sandbox-agent binary entry point.
//!
//! Placeholder until the gRPC server is wired in task 1.3.
//! Performs PID-1 init when run as init (pid == 1), then exits.

// Mirror the crate-wide lint set from lib.rs for the binary compilation unit.
#![deny(unsafe_code)]
#![deny(clippy::unwrap_used)]
#![deny(clippy::expect_used)]
#![deny(clippy::panic)]
#![deny(clippy::indexing_slicing)]
#![warn(missing_docs)]

fn main() {
    // If running as PID 1 (inside the Firecracker VM), perform init: mount
    // essential filesystems, create /workspace, and set hostname "sandbox".
    // Mirrors the getpid()==1 guard in guest/agent/main.go.
    //
    // SAFETY: getpid() is always safe. The unsafe block is scoped to this libc
    // call; the init module carries its own #[allow(unsafe_code)].
    #[allow(unsafe_code)]
    if unsafe { libc::getpid() } == 1 {
        sandbox_agent::init::init_system();
    }

    // gRPC server is wired in task 1.3.
}
