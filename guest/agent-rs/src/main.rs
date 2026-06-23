// The binary re-uses the modules defined in lib.rs.
use sandbox_agent::transport;

fn main() {
    let env = std::sync::Arc::new(std::sync::Mutex::new(std::collections::HashMap::new()));

    // AgentPort = 52 (mirrors vsock.AgentPort in internal/vsock/protocol.go).
    transport::serve_vsock(52, env);
}
