//! sandbox-agent binary entry point.
//!
//! Runs as PID 1 inside the Firecracker microVM. Performs kernel init when run
//! as init, then starts the legacy JSON-lines accept loop on vsock port 52.
//! The gRPC runtime protocol server on vsock port 53 is wired in task 0.2.

// Mirror the crate-wide lint set from lib.rs for the binary compilation unit.
#![deny(unsafe_code)]
#![deny(clippy::unwrap_used)]
#![deny(clippy::expect_used)]
#![deny(clippy::panic)]
#![deny(clippy::indexing_slicing)]
#![warn(missing_docs)]

use sandbox_agent::transport;

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

    eprintln!("sandbox-agent: starting on vsock port 52 (legacy JSON-lines)");

    let env = std::sync::Arc::new(std::sync::Mutex::new(std::collections::HashMap::new()));

    // AgentPort = 52 (mirrors vsock.AgentPort in internal/vsock/protocol.go).
    // The gRPC server on AgentGRPCPort = 53 is wired in task 0.2.
    transport::serve_vsock(52, env);
}
