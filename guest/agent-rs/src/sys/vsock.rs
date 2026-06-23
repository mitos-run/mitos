// AF_VSOCK listener: wraps the tokio-vsock crate (feature "vsock") behind a
// thin safe API used by the gRPC server task (1.3).
//
// Under the `vsock` feature flag (Linux, production) this calls
// tokio_vsock::VsockListener::bind with VMADDR_CID_ANY so the guest agent
// accepts connections from the host over the Firecracker virtio-vsock device.
//
// The vsock feature is absent on macOS; callers that need a listener in
// non-Linux tests should use the Unix-socket fallback provided by the gRPC
// server module (task 1.3). This module does not contain unsafe code because
// tokio-vsock handles the raw syscalls internally.
//
// No unsafe blocks in this file.

/// The vsock port the guest agent serves the gRPC runtime protocol on.
/// Matches vsock.AgentGRPCPort = 53 in internal/vsock/protocol.go.
pub const AGENT_GRPC_PORT: u32 = 53;

/// The vsock port the legacy JSON-lines protocol listens on.
/// Matches vsock.AgentPort = 52 in internal/vsock/protocol.go.
pub const AGENT_LEGACY_PORT: u32 = 52;

/// Bind an AF_VSOCK listener on `VMADDR_CID_ANY` for `port`.
///
/// Returns the bound `VsockListener` on success.
///
/// Available under the `vsock` Cargo feature only (Linux production build).
/// On non-Linux targets this function does not compile; the caller must use
/// the `cfg(feature = "vsock")` guard.
///
/// The `vsock` crate (underlying tokio-vsock) handles the AF_VSOCK socket
/// creation, bind, and listen syscalls internally. No unsafe code is needed
/// here; safety is delegated to the well-audited tokio-vsock crate.
#[cfg(feature = "vsock")]
pub fn bind_vsock(port: u32) -> std::io::Result<tokio_vsock::VsockListener> {
    use tokio_vsock::{VsockAddr, VsockListener, VMADDR_CID_ANY};
    let addr = VsockAddr::new(VMADDR_CID_ANY, port);
    VsockListener::bind(addr)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn agent_grpc_port_matches_go_constant() {
        // AgentGRPCPort = 53 in internal/vsock/protocol.go.
        assert_eq!(AGENT_GRPC_PORT, 53);
    }

    #[test]
    fn agent_legacy_port_matches_go_constant() {
        // AgentPort = 52 in internal/vsock/protocol.go.
        assert_eq!(AGENT_LEGACY_PORT, 52);
    }

    // bind_vsock is only available under the vsock feature on Linux.
    // We cannot call it in a unit test (it needs a real AF_VSOCK kernel
    // module). The constants and the function signature are verified by
    // compilation; the integration test runs on box1 in CI.
}
