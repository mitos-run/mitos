// Conformance test harness for the Sandbox gRPC service skeleton.
//
// Spins up the tonic SandboxService on a Unix domain socket (so tests run
// on the host without AF_VSOCK), drives it with the tonic-generated client,
// and asserts that the server accepts connections and returns well-formed
// gRPC responses (Unimplemented for all stub RPCs in this slice).
//
// Later Phase 2 tasks extend this harness by adding test cases for each RPC
// as it is implemented.

#![allow(clippy::unwrap_used, clippy::expect_used)]

use std::sync::Arc;
use tokio::sync::Mutex;
use tonic::Code;

use sandbox_agent::env::ConfiguredEnv;
use sandbox_agent::kernel::KernelManager;
use sandbox_agent::sandbox_v1::sandbox_client::SandboxClient;
use sandbox_agent::sandbox_v1::sandbox_server::SandboxServer;
use sandbox_agent::sandbox_v1::StatRequest;
use sandbox_agent::service::SandboxService;

/// Path for the Unix socket used by conformance tests.
/// Each test function must ensure this path is cleaned up before binding.
const TEST_SOCK: &str = "/tmp/agent-conformance-test.sock";

/// Build a SandboxService with empty shared state for use in tests.
fn make_service() -> SandboxService {
    SandboxService {
        env: Arc::new(ConfiguredEnv::new()),
        kernel: Arc::new(Mutex::new(KernelManager::new())),
    }
}

/// Start the Sandbox gRPC service on a Unix domain socket and return a
/// connected client. The server runs in a background tokio task; it is
/// cleaned up when the test process exits.
async fn start_server_and_client(sock_path: &str) -> SandboxClient<tonic::transport::Channel> {
    let _ = std::fs::remove_file(sock_path);
    let uds = tokio::net::UnixListener::bind(sock_path).expect("bind unix socket");
    let incoming = tokio_stream::wrappers::UnixListenerStream::new(uds);

    let service = make_service();
    tokio::spawn(async move {
        tonic::transport::Server::builder()
            .add_service(SandboxServer::new(service))
            .serve_with_incoming(incoming)
            .await
            .ok();
    });

    // Give the server a moment to accept.
    tokio::time::sleep(std::time::Duration::from_millis(50)).await;

    // Connect via the unix socket. The URI is a placeholder; the connector
    // overrides the address.
    // tonic 0.13 uses hyper's IO traits (hyper::rt::{Read, Write}) rather than
    // tokio's AsyncRead/AsyncWrite. TokioIo wraps a tokio IO type to satisfy
    // the hyper bounds that connect_with_connector requires.
    let sock_path = sock_path.to_owned();
    let channel = tonic::transport::Endpoint::from_static("http://[::]:0")
        .connect_with_connector(tower::service_fn(move |_| {
            let path = sock_path.clone();
            async move {
                let stream = tokio::net::UnixStream::connect(path).await?;
                Ok::<_, std::io::Error>(hyper_util::rt::TokioIo::new(stream))
            }
        }))
        .await
        .expect("connect to test server");

    SandboxClient::new(channel)
}

/// The server must accept a connection and return Code::Unimplemented for the
/// Stat RPC (which is a stub in this slice). This validates that the tonic
/// service is correctly wired and the gRPC framing works over a Unix socket.
#[tokio::test]
async fn stat_returns_unimplemented() {
    let mut client = start_server_and_client(TEST_SOCK).await;

    let result = client
        .stat(StatRequest { path: "/".into() })
        .await;

    let status = result.expect_err("stub Stat must return an error");
    assert_eq!(
        status.code(),
        Code::Unimplemented,
        "stub Stat must return Code::Unimplemented, got {:?}",
        status.code()
    );
    assert!(
        status.message().contains("Stat"),
        "error message must name the RPC, got: {}",
        status.message()
    );
}

/// Verify that a Budget RPC also round-trips correctly as Unimplemented.
/// Budget is a unary RPC with no streaming, exercising a different code path
/// than Stat (which is also unary but uses a different message type).
#[tokio::test]
async fn budget_returns_unimplemented() {
    use sandbox_agent::sandbox_v1::BudgetRequest;

    // Use a distinct socket path to avoid racing with stat_returns_unimplemented.
    const BUDGET_SOCK: &str = "/tmp/agent-conformance-budget-test.sock";
    let mut client = start_server_and_client(BUDGET_SOCK).await;

    let result = client.budget(BudgetRequest {}).await;

    let status = result.expect_err("stub Budget must return an error");
    assert_eq!(
        status.code(),
        Code::Unimplemented,
        "stub Budget must return Code::Unimplemented, got {:?}",
        status.code()
    );
}
