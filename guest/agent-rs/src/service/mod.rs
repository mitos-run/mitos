// tonic Sandbox service skeleton: all RPCs return Unimplemented.
//
// Phase 2 tasks replace each stub with a real implementation. The shared state
// held by SandboxService is defined here so Phase 2 tasks can depend on it
// without touching this file's structure.
//
// No unsafe code in this module; tonic-generated code is isolated in lib.rs.

use std::path::PathBuf;
use std::pin::Pin;
use std::sync::Arc;
use tokio::sync::Mutex;
use tonic::{Request, Response, Status};

use crate::env::ConfiguredEnv;
use crate::kernel::KernelManager;
use crate::sandbox_v1;
use sandbox_v1::sandbox_server::Sandbox;

/// Exec and PTY RPC implementation (Task 2.1).
pub mod exec;

/// File RPC implementations (Task 2.2): ReadFile, WriteFile, List, Stat, Mkdir, Remove.
pub mod files;

/// Archive (tar streaming download) and Upload (tar streaming extract) RPC
/// implementations (Task 2.3).
pub mod archive;

/// Watch RPC implementation (Task 2.4): inotify event streaming via the
/// `notify` crate, bridged into a bounded tonic server stream.
pub mod watch;

/// Processes and Signal RPC implementations (Task 2.5): /proc table read
/// and libc::kill delivery.
pub mod processes;

// Type alias used for all server-streaming RPC associated types.
// Pin<Box<dyn Stream<...> + Send + 'static>> satisfies the tonic trait bound
// and lets each Phase 2 task substitute any stream implementation.
type BoxStream<T> =
    Pin<Box<dyn tokio_stream::Stream<Item = Result<T, Status>> + Send + 'static>>;

/// Shared state for the Sandbox gRPC service.
///
/// Held in an `Arc` so tonic clones the service per request while sharing
/// the same underlying state. Each field is wrapped in a lock-free or
/// async-mutex-protected type so concurrent RPC handlers do not contend on
/// a single mutex.
///
/// Phase 2 RPC tasks receive `self: &SandboxService` and access these fields
/// directly.
pub struct SandboxService {
    /// Claim-time env and secrets, populated by the Configure RPC.
    pub env: Arc<ConfiguredEnv>,
    /// The in-guest code-execution kernel, started lazily on first RunCode.
    pub kernel: Arc<Mutex<KernelManager>>,
    /// Workspace root for path allowlist checks in Archive, Upload, and Watch.
    /// Defaults to /workspace in production; tests pass their own temp root.
    pub workspace_root: PathBuf,
}

/// Return an Unimplemented status for any RPC stub. The message names the
/// RPC so the caller can identify which endpoint to target.
fn unimplemented(rpc: &'static str) -> Status {
    Status::unimplemented(format!("{rpc}: not yet implemented in this slice"))
}

/// Return an empty server-streaming Unimplemented response.
///
/// Returns `Err(Status)` boxed via a direct `Err(...)` call. The `Status`
/// type is large (>= 176 bytes per clippy); we suppress the lint here because
/// all callers are stub stubs returning immediately and the allocation cost
/// of boxing would add complexity for no runtime benefit at a stub callsite.
#[allow(clippy::result_large_err)]
fn unimplemented_stream<T>(rpc: &'static str) -> Result<Response<BoxStream<T>>, Status>
where
    T: Send + 'static,
{
    Err(unimplemented(rpc))
}

#[tonic::async_trait]
impl Sandbox for SandboxService {
    // --- Streaming type aliases -----------------------------------------------
    // Each Phase 2 task that implements a server-streaming RPC can change the
    // alias to a concrete stream type. For now all aliases are the generic
    // BoxStream.

    type ExecStream = BoxStream<sandbox_v1::ExecResponse>;
    type ExecStreamStream = BoxStream<sandbox_v1::ExecResponse>;
    type ReadFileStream = BoxStream<sandbox_v1::Chunk>;
    type ArchiveStream = BoxStream<sandbox_v1::Chunk>;
    type WatchStream = BoxStream<sandbox_v1::FsEvent>;
    type PortForwardStream = BoxStream<sandbox_v1::Frame>;
    type VitalsStream = BoxStream<sandbox_v1::GuestVitals>;
    type RunCodeStream = BoxStream<sandbox_v1::RunCodeResponse>;
    type RunCodeStreamStream = BoxStream<sandbox_v1::RunCodeResponse>;

    // --- Execution ------------------------------------------------------------

    async fn exec(
        &self,
        request: Request<tonic::Streaming<sandbox_v1::ExecRequest>>,
    ) -> Result<Response<Self::ExecStream>, Status> {
        let env = Arc::clone(&self.env);
        let stream = request.into_inner();

        // Bounded channel: the sender is the exec handler task; the receiver is
        // converted into the response stream the tonic runtime polls. The bound
        // of 32 keeps back-pressure without blocking drain tasks indefinitely.
        let (tx, rx) = tokio::sync::mpsc::channel(32);

        tokio::spawn(async move {
            if let Err(status) = exec::exec_handler(env, stream, tx.clone()).await {
                // Best-effort: send the error as a status; ignore if client is gone.
                let _ = tx.send(Err(status)).await;
            }
        });

        let out_stream = tokio_stream::wrappers::ReceiverStream::new(rx);
        Ok(Response::new(Box::pin(out_stream)))
    }

    async fn exec_stream(
        &self,
        _request: Request<sandbox_v1::ExecStreamRequest>,
    ) -> Result<Response<Self::ExecStreamStream>, Status> {
        unimplemented_stream("ExecStream")
    }

    // --- Filesystem -----------------------------------------------------------

    async fn read_file(
        &self,
        request: Request<sandbox_v1::ReadFileRequest>,
    ) -> Result<Response<Self::ReadFileStream>, Status> {
        files::read_file(request).await
    }

    async fn write_file(
        &self,
        request: Request<tonic::Streaming<sandbox_v1::WriteFileRequest>>,
    ) -> Result<Response<sandbox_v1::WriteFileResult>, Status> {
        files::write_file(request).await
    }

    async fn list(
        &self,
        request: Request<sandbox_v1::ListRequest>,
    ) -> Result<Response<sandbox_v1::ListResponse>, Status> {
        files::list(request).await
    }

    async fn stat(
        &self,
        request: Request<sandbox_v1::StatRequest>,
    ) -> Result<Response<sandbox_v1::FileInfo>, Status> {
        files::stat(request).await
    }

    async fn archive(
        &self,
        request: Request<sandbox_v1::ArchiveRequest>,
    ) -> Result<Response<Self::ArchiveStream>, Status> {
        archive::archive(&self.workspace_root, request).await
    }

    async fn watch(
        &self,
        request: Request<sandbox_v1::WatchRequest>,
    ) -> Result<Response<Self::WatchStream>, Status> {
        watch::watch(&self.workspace_root, request).await
    }

    // --- Processes and network ------------------------------------------------

    async fn processes(
        &self,
        request: Request<sandbox_v1::ProcessesRequest>,
    ) -> Result<Response<sandbox_v1::ProcessList>, Status> {
        processes::processes(request).await
    }

    async fn signal(
        &self,
        request: Request<sandbox_v1::SignalRequest>,
    ) -> Result<Response<sandbox_v1::SignalResponse>, Status> {
        processes::signal(request).await
    }

    async fn port_forward(
        &self,
        _request: Request<tonic::Streaming<sandbox_v1::Frame>>,
    ) -> Result<Response<Self::PortForwardStream>, Status> {
        unimplemented_stream("PortForward")
    }

    // --- Budget-gated self-service --------------------------------------------

    async fn fork(
        &self,
        _request: Request<sandbox_v1::ForkRequest>,
    ) -> Result<Response<sandbox_v1::Operation>, Status> {
        Err(unimplemented("Fork"))
    }

    async fn checkpoint(
        &self,
        _request: Request<sandbox_v1::CheckpointRequest>,
    ) -> Result<Response<sandbox_v1::Revision>, Status> {
        Err(unimplemented("Checkpoint"))
    }

    async fn extend_lifetime(
        &self,
        _request: Request<sandbox_v1::ExtendRequest>,
    ) -> Result<Response<sandbox_v1::Lease>, Status> {
        Err(unimplemented("ExtendLifetime"))
    }

    async fn budget(
        &self,
        _request: Request<sandbox_v1::BudgetRequest>,
    ) -> Result<Response<sandbox_v1::BudgetStatus>, Status> {
        Err(unimplemented("Budget"))
    }

    // --- Telemetry ------------------------------------------------------------

    async fn vitals(
        &self,
        _request: Request<sandbox_v1::VitalsRequest>,
    ) -> Result<Response<Self::VitalsStream>, Status> {
        unimplemented_stream("Vitals")
    }

    // --- Code execution -------------------------------------------------------

    async fn run_code(
        &self,
        _request: Request<tonic::Streaming<sandbox_v1::RunCodeRequest>>,
    ) -> Result<Response<Self::RunCodeStream>, Status> {
        unimplemented_stream("RunCode")
    }

    async fn run_code_stream(
        &self,
        _request: Request<sandbox_v1::RunCodeStreamRequest>,
    ) -> Result<Response<Self::RunCodeStreamStream>, Status> {
        unimplemented_stream("RunCodeStream")
    }

    // --- Filesystem helpers ---------------------------------------------------

    async fn mkdir(
        &self,
        request: Request<sandbox_v1::MkdirRequest>,
    ) -> Result<Response<sandbox_v1::MkdirResponse>, Status> {
        files::mkdir(request).await
    }

    async fn remove(
        &self,
        request: Request<sandbox_v1::RemoveRequest>,
    ) -> Result<Response<sandbox_v1::RemoveResponse>, Status> {
        files::remove(request).await
    }

    async fn upload(
        &self,
        request: Request<tonic::Streaming<sandbox_v1::UploadRequest>>,
    ) -> Result<Response<sandbox_v1::UploadResult>, Status> {
        archive::upload(&self.workspace_root, request).await
    }
}
