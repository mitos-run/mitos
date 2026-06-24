//! sandbox-agent binary entry point.
//!
//! Runs PID-1 init when invoked as init (pid == 1), then builds shared state
//! and serves the tonic Sandbox gRPC service over an AF_VSOCK listener on
//! AGENT_GRPC_PORT (53). The vsock feature must be enabled for production use;
//! non-vsock builds are compile-only stubs.

// Mirror the crate-wide lint set from lib.rs for the binary compilation unit.
#![deny(unsafe_code)]
#![deny(clippy::unwrap_used)]
#![deny(clippy::expect_used)]
#![deny(clippy::panic)]
#![deny(clippy::indexing_slicing)]
#![warn(missing_docs)]

use std::sync::Arc;
use tokio::sync::Mutex;

use sandbox_agent::env::ConfiguredEnv;
use sandbox_agent::kernel::KernelManager;
use sandbox_agent::sandbox_v1::sandbox_server::SandboxServer;
use sandbox_agent::service::SandboxService;
use sandbox_agent::sys::AGENT_GRPC_PORT;

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    // Initialise the tracing subscriber. RUST_LOG controls the level filter.
    // No secrets are ever logged (the service struct holds values behind
    // Arc<ConfiguredEnv> which is not logged directly).
    tracing_subscriber::fmt()
        .with_env_filter(tracing_subscriber::EnvFilter::from_default_env())
        .init();

    // PID-1 guard: mirrors the getpid()==1 check in guest/agent/main.go:47-49.
    // Runs the filesystem mounts, /workspace mkdir, and sethostname steps.
    //
    // SAFETY: getpid() reads the process ID from the kernel and has no
    // preconditions; calling it is always safe. The unsafe block is the
    // minimum scope required to call a libc function.
    #[allow(unsafe_code)]
    if unsafe { libc::getpid() } == 1 {
        sandbox_agent::init::init_system();
    }

    // Build shared state. Both fields are Arc-wrapped so the service struct
    // can be cloned per tonic request without cloning the underlying state.
    let env = Arc::new(ConfiguredEnv::new());
    let kernel = Arc::new(Mutex::new(KernelManager::new()));

    let service = SandboxService {
        env,
        kernel,
        workspace_root: std::path::PathBuf::from("/workspace"),
    };

    tracing::info!(
        port = AGENT_GRPC_PORT,
        "sandbox-agent: gRPC ready, binding vsock"
    );

    serve(service).await
}

/// Serve the Sandbox gRPC service over AF_VSOCK (vsock feature) or exit
/// immediately (no vsock feature, compile-only stub).
///
/// Under the `vsock` feature: binds `AGENT_GRPC_PORT` on `VMADDR_CID_ANY`
/// and adapts the `VsockListener::incoming()` stream into tonic's
/// `serve_with_incoming`. `VsockStream` is wrapped in `VsockConnected` which
/// implements `tonic::transport::server::Connected` for tonic 0.13, because
/// the `tonic-conn` feature of tokio-vsock 0.6 targets tonic 0.12 and is not
/// compatible. The wrapper is minimal: `AsyncRead + AsyncWrite + Unpin +
/// Connected` only.
#[cfg(feature = "vsock")]
async fn serve(service: SandboxService) -> Result<(), Box<dyn std::error::Error>> {
    use std::pin::Pin;
    use std::task::{Context, Poll};
    use tokio::io::{AsyncRead, AsyncWrite, ReadBuf};
    use tokio_stream::StreamExt as _;
    use tokio_vsock::{VsockAddr, VsockListener, VMADDR_CID_ANY};
    use tonic::transport::server::Connected;

    // Newtype wrapping VsockStream so we can implement tonic::Connected for it.
    // tonic 0.13 requires Connected on the IO type passed to serve_with_incoming.
    struct VsockConnected(tokio_vsock::VsockStream);

    // ConnectInfo is the metadata type required by tonic::Connected.
    // We carry no per-connection metadata at this stage.
    #[derive(Clone)]
    struct VsockConnectInfo;

    impl Connected for VsockConnected {
        type ConnectInfo = VsockConnectInfo;
        fn connect_info(&self) -> VsockConnectInfo {
            VsockConnectInfo
        }
    }

    impl AsyncRead for VsockConnected {
        fn poll_read(
            mut self: Pin<&mut Self>,
            cx: &mut Context<'_>,
            buf: &mut ReadBuf<'_>,
        ) -> Poll<std::io::Result<()>> {
            Pin::new(&mut self.0).poll_read(cx, buf)
        }
    }

    impl AsyncWrite for VsockConnected {
        fn poll_write(
            mut self: Pin<&mut Self>,
            cx: &mut Context<'_>,
            buf: &[u8],
        ) -> Poll<std::io::Result<usize>> {
            Pin::new(&mut self.0).poll_write(cx, buf)
        }
        fn poll_flush(
            mut self: Pin<&mut Self>,
            cx: &mut Context<'_>,
        ) -> Poll<std::io::Result<()>> {
            Pin::new(&mut self.0).poll_flush(cx)
        }
        fn poll_shutdown(
            mut self: Pin<&mut Self>,
            cx: &mut Context<'_>,
        ) -> Poll<std::io::Result<()>> {
            Pin::new(&mut self.0).poll_shutdown(cx)
        }
    }

    // VsockConnected does not contain any non-Unpin fields; the impl is safe.
    impl Unpin for VsockConnected {}

    let addr = VsockAddr::new(VMADDR_CID_ANY, AGENT_GRPC_PORT);
    let listener = VsockListener::bind(addr)?;

    // incoming() is a Stream<Item = io::Result<VsockStream>>; map each item
    // to wrap VsockStream in VsockConnected.
    let incoming = listener.incoming().map(|r| r.map(VsockConnected));

    tonic::transport::Server::builder()
        .add_service(SandboxServer::new(service))
        .serve_with_incoming(incoming)
        .await?;

    Ok(())
}

#[cfg(not(feature = "vsock"))]
async fn serve(_service: SandboxService) -> Result<(), Box<dyn std::error::Error>> {
    tracing::warn!(
        "vsock feature not enabled: sandbox-agent has no listener. \
         Enable the `vsock` Cargo feature for production use."
    );
    Ok(())
}
