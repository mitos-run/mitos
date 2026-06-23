// The binary re-uses the modules defined in lib.rs.
use sandbox_agent::transport;

fn main() {
    // If running as PID 1 (inside the Firecracker VM), perform init: mount
    // essential filesystems, create /workspace, and set hostname "sandbox".
    // Mirrors the getpid()==1 guard in guest/agent/main.go.
    if unsafe { libc::getpid() } == 1 {
        sandbox_agent::init::init_system();
    }

    eprintln!("sandbox-agent: starting on vsock port 52");

    let env = std::sync::Arc::new(std::sync::Mutex::new(std::collections::HashMap::new()));

    // AgentPort = 52 (mirrors vsock.AgentPort in internal/vsock/protocol.go).
    transport::serve_vsock(52, env);
}
