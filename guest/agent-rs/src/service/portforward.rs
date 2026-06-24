// PortForward RPC: bidirectional TCP splice to a guest loopback port.
//
// Security invariant (mirrors tunnel.go): the agent ALWAYS dials 127.0.0.1
// regardless of what the client sends. The client carries only a port number;
// the host is hardcoded. This prevents the tunnel from reaching the guest's
// other network interfaces or the host network.
//
// Protocol (sandbox.proto Frame oneof):
//   - First client frame: open { port }. Port must be in 1..=65535.
//   - Subsequent client frames: data { bytes } or close { true }.
//   - Server frames: data { bytes } for TCP->client relay; close on teardown.
//
// One stream carries exactly one TCP connection (no multiplexing), mirroring Go.

use std::time::Duration;

use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpStream;
use tonic::{Status, Streaming};
use tracing::debug;

use crate::error::AgentError;
use crate::sandbox_v1::{
    frame,
    Frame,
    PortForwardOpen,
};

// Dial timeout mirrors tunnel.go tunnelDialTimeout = 5s.
const DIAL_TIMEOUT: Duration = Duration::from_secs(5);

// Read buffer for the TCP->gRPC relay direction. Chosen to match typical
// page size; large enough to keep throughput reasonable, small enough to
// bound per-connection memory without allocating a static pool.
const TCP_BUF_SIZE: usize = 16 * 1024;

/// Handle the PortForward RPC: receive the open frame, dial 127.0.0.1:<port>,
/// then splice bytes bidirectionally between the gRPC stream and the TCP socket.
///
/// Returns a channel receiver that the caller converts into the server stream.
/// On any error, the returned channel carries the error as the first item and
/// then closes, so the client sees a gRPC error on the stream.
pub async fn port_forward(
    mut inbound: Streaming<Frame>,
) -> Result<tokio::sync::mpsc::Receiver<Result<Frame, Status>>, Status> {
    // Step 1: receive and validate the first frame (must be open).
    let first = inbound
        .message()
        .await
        .map_err(|e| Status::internal(format!("read open frame: {e}")))?
        .ok_or_else(|| {
            Status::invalid_argument(
                "PortForward stream ended before the open frame: the first frame must carry open",
            )
        })?;

    let port = match first.msg {
        Some(frame::Msg::Open(PortForwardOpen { port })) => port,
        Some(frame::Msg::Data(_)) | Some(frame::Msg::Close(_)) | None => {
            return Err(Status::invalid_argument(
                "PortForward first frame must carry open with the target port",
            ));
        }
    };

    // Step 2: validate the port range.
    if port == 0 || port > 65535 {
        return Err(AgentError::InvalidArgument(format!(
            "PortForward port {port} is out of range: must be 1-65535"
        ))
        .into());
    }

    // Step 3: dial loopback ONLY. The host is hardcoded; clients supply only a
    // port. This is the sole network reach guard for the tunnel.
    let addr = format!("127.0.0.1:{port}");
    debug!(port, "PortForward: dialing guest loopback");

    let tcp = tokio::time::timeout(DIAL_TIMEOUT, TcpStream::connect(&addr))
        .await
        .map_err(|_| {
            AgentError::Unavailable(format!(
                "dial 127.0.0.1:{port} in guest: timed out after {}s; \
                 the port may not be listening yet",
                DIAL_TIMEOUT.as_secs(),
            ))
        })?
        .map_err(|e| {
            AgentError::Unavailable(format!(
                "dial 127.0.0.1:{port} in guest: {e}; \
                 ensure the service is listening on that loopback port"
            ))
        })?;

    // Step 4: wire up the bounded reply channel (bound = 32 frames).
    // Back-pressure: the sender tasks block when the channel is full, so a
    // slow gRPC consumer slows TCP reads rather than buffering without bound.
    let (tx, rx) = tokio::sync::mpsc::channel::<Result<Frame, Status>>(32);

    // Split the TCP socket into independent read and write halves so each
    // relay direction owns its half without needing a shared lock.
    let (mut tcp_r, mut tcp_w) = tcp.into_split();

    // Shared cancellation: either relay task signals the other to stop.
    // Using a oneshot pair: the first task to finish sends on `stop_tx`;
    // the other task receives on `stop_rx` and tears down. This mirrors
    // tunnel.go's sync.Once pattern.
    let (stop_tx, stop_rx) = tokio::sync::oneshot::channel::<()>();

    // Direction A: TCP socket -> gRPC stream (server-to-client frames).
    let tx_a = tx.clone();
    tokio::spawn(async move {
        let mut buf = vec![0u8; TCP_BUF_SIZE];
        loop {
            let n = match tcp_r.read(&mut buf).await {
                Ok(0) => break, // TCP FIN: remote closed the connection.
                Ok(n) => n,
                Err(_) => break, // IO error: tear down.
            };
            // NEVER log the tunneled bytes; only the port was logged at open.
            // n is returned by read() which guarantees n <= buf.len(), so
            // .get(..n) is always Some; the else branch is unreachable but
            // avoids the indexing_slicing lint.
            let chunk = match buf.get(..n) {
                Some(s) => s.to_vec(),
                None => break, // unreachable: read() guarantees n <= buf.len()
            };
            let frame = Frame {
                msg: Some(frame::Msg::Data(chunk)),
            };
            if tx_a.send(Ok(frame)).await.is_err() {
                break; // Client stream closed.
            }
        }
        // Signal the other direction to stop.
        let _ = stop_tx.send(());
        // Send a close frame so the client knows the TCP side ended.
        let _ = tx_a
            .send(Ok(Frame {
                msg: Some(frame::Msg::Close(true)),
            }))
            .await;
    });

    // Direction B: gRPC stream -> TCP socket (client-to-server bytes).
    // Also watches for the stop signal from direction A.
    tokio::spawn(async move {
        let mut stop = stop_rx;
        loop {
            // Poll the stop signal and the inbound stream concurrently.
            let next = tokio::select! {
                _ = &mut stop => break,
                msg = inbound.message() => msg,
            };
            let frame = match next {
                Ok(Some(f)) => f,
                Ok(None) | Err(_) => break, // Stream ended or errored.
            };
            match frame.msg {
                Some(frame::Msg::Data(data)) => {
                    if tcp_w.write_all(&data).await.is_err() {
                        break; // TCP write failed; tear down.
                    }
                }
                Some(frame::Msg::Close(_)) | None => break, // Client requested close.
                Some(frame::Msg::Open(_)) => {
                    // Duplicate open after the first is a protocol error; ignore it
                    // rather than hard-failing to match Go's tolerant relay behavior.
                }
            }
        }
        // Shut down the write half so the echo server (or real service) sees EOF.
        let _ = tcp_w.shutdown().await;
    });

    Ok(rx)
}
