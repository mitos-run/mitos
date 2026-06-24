// RunCode and RunCodeStream RPC implementations.
//
// Mirrors handleRunCodeStream() in guest/agent/kernel.go:
// - First message must carry RunCodeOpen (open oneof).
// - Empty or "python" language accepted; anything else -> KernelUnavailable.
// - One kernel per sandbox; runs are serialized by the Mutex<KernelManager>.
// - Always ends with an exit_code frame (0, 1, or 127 for KernelUnavailable).
// - No code text or output bytes are ever logged.

use std::sync::Arc;
use tokio::sync::mpsc;
use tokio::sync::Mutex;
use tonic::{Request, Response, Status};

use crate::kernel::KernelManager;
use crate::sandbox_v1;
use sandbox_v1::run_code_request::Msg as ReqMsg;

use super::BoxStream;

/// Handle the bidi RunCode stream.
///
/// Reads the first RunCodeRequest from `stream`; it must carry `open`.
/// Locks the kernel mutex, runs the code, and streams RunCodeResponse frames
/// (stdout, stderr, result, error, exit_code) to `tx`. The stream ends with
/// exactly one exit_code frame. RunCodeStream (HTTP/1.1 server-streaming) uses
/// the same kernel path via `run_code_stream_handler`.
pub async fn run_code_handler(
    kernel: Arc<Mutex<KernelManager>>,
    mut stream: tonic::Streaming<sandbox_v1::RunCodeRequest>,
    tx: mpsc::Sender<Result<sandbox_v1::RunCodeResponse, Status>>,
) -> Result<(), Status> {
    // The first message must carry `open`.
    let first = stream
        .message()
        .await
        .map_err(|e| Status::internal(format!("receive RunCode open: {e}")))?
        .ok_or_else(|| Status::invalid_argument("RunCode: stream closed before open message"))?;

    let open = match first.msg {
        Some(ReqMsg::Open(o)) => o,
        _ => {
            return Err(Status::invalid_argument(
                "RunCode: first message must carry open",
            ));
        }
    };

    // Lock the kernel mutex to serialize executions (one at a time).
    let mut km = kernel.lock().await;
    km.run(&open.code, &open.language, open.timeout_seconds, &tx)
        .await;

    Ok(())
}

/// Handle the RunCodeStream server-streaming RPC (HTTP/1.1 counterpart to
/// the bidi RunCode). Accepts a RunCodeStreamRequest (no stdin) and runs the
/// code through the same KernelManager path.
pub async fn run_code_stream_handler(
    kernel: Arc<Mutex<KernelManager>>,
    request: Request<sandbox_v1::RunCodeStreamRequest>,
) -> Result<Response<BoxStream<sandbox_v1::RunCodeResponse>>, Status> {
    let req = request.into_inner();

    let (tx, rx) = mpsc::channel(32);

    tokio::spawn(async move {
        let mut km = kernel.lock().await;
        km.run(&req.code, &req.language, req.timeout_seconds, &tx)
            .await;
    });

    let out_stream = tokio_stream::wrappers::ReceiverStream::new(rx);
    Ok(Response::new(Box::pin(out_stream)))
}
