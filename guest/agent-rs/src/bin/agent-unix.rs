//! agent-unix: serves the sandbox.v1.Sandbox and sandbox.internal.v1.Control
//! gRPC services over a Unix domain socket instead of AF_VSOCK.
//!
//! Used exclusively by the cross-agent conformance harness
//! (bench/agent-conformance) on box2, where vsock is not available outside a
//! real VM. The socket path is read from AGENT_UNIX_SOCK; the workspace root
//! is read from AGENT_WORKSPACE (default: /workspace).
//!
//! Protocol: on startup, writes "READY\n" to stdout once the tonic server is
//! bound and ready to accept connections. The harness waits for that line
//! before connecting.

#![deny(unsafe_code)]
#![deny(clippy::unwrap_used)]
#![deny(clippy::expect_used)]
#![deny(clippy::panic)]

use std::path::PathBuf;
use std::sync::Arc;
use std::time::Instant;
use tokio::sync::Mutex;

use sandbox_agent::control_v1::control_server::ControlServer;
use sandbox_agent::env::ConfiguredEnv;
use sandbox_agent::kernel::KernelManager;
use sandbox_agent::sandbox_v1::sandbox_server::SandboxServer;
use sandbox_agent::service::control::ControlService;
use sandbox_agent::service::SandboxService;

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    tracing_subscriber::fmt()
        .with_env_filter(tracing_subscriber::EnvFilter::from_default_env())
        .init();

    let sock_path = std::env::var("AGENT_UNIX_SOCK").map_err(|_| {
        "AGENT_UNIX_SOCK environment variable must be set to the unix socket path"
    })?;

    let workspace_root = std::env::var("AGENT_WORKSPACE")
        .unwrap_or_else(|_| "/workspace".to_string());

    // Remove any leftover socket file from a prior run.
    let _ = std::fs::remove_file(&sock_path);

    let uds = tokio::net::UnixListener::bind(&sock_path)?;
    let incoming = tokio_stream::wrappers::UnixListenerStream::new(uds);

    let start_time = Instant::now();
    let env = Arc::new(ConfiguredEnv::new());
    let kernel = Arc::new(Mutex::new(KernelManager::new()));

    let service = SandboxService {
        env: Arc::clone(&env),
        kernel,
        workspace_root: PathBuf::from(workspace_root),
    };

    // No-op signal_fn: the harness does not call NotifyForked with real
    // entropy, and broadcasting SIGUSR2 to host processes on box2 is unsafe.
    // The real signal_userspace is used only in production (main.rs).
    fn noop_signal() -> i32 { 0 }
    let ctrl_service = ControlService {
        start_time,
        env,
        signal_fn: noop_signal,
    };

    // Signal readiness to the harness before starting to accept.
    println!("READY");

    tonic::transport::Server::builder()
        .add_service(SandboxServer::new(service))
        .add_service(ControlServer::new(ctrl_service))
        .serve_with_incoming(incoming)
        .await?;

    Ok(())
}
