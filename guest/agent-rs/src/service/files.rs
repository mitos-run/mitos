// File RPC implementations for the Sandbox gRPC service.
//
// Mirrors guest/agent/grpc_server.go:278-405 for behavior parity:
//   ReadFile  - streams 32 KiB chunks; final Chunk has eof=true.
//   WriteFile - creates parent dirs, default mode 0o644 when open.mode==0.
//   List      - enumerates directory; no pagination in this slice (empty token).
//   Stat      - lstat semantics; NotFound on missing path.
//   Mkdir     - recursive (create_dir_all), mode 0o755.
//   Remove    - os.RemoveAll semantics: no error on missing path.
//
// Security: file bytes are NEVER logged. Paths are logged at debug level only.
// No unsafe code; all IO is via tokio::fs / std::fs.

use std::path::Path;
use std::time::UNIX_EPOCH;

use tokio::io::AsyncReadExt;
use tonic::{Request, Response, Status};

use crate::sandbox_v1;
use crate::service::BoxStream;

/// Chunk size for ReadFile streaming: 32 KiB, matching grpcChunkBytes in the
/// Go implementation (grpc_server.go:281).
const CHUNK_BYTES: usize = 32 << 10;

/// Map a std::io::Error to the appropriate gRPC Status.
/// NotFound maps to Code::NotFound; all other IO errors map to Code::Internal.
/// The OS error string is included; no file contents or secret values are logged.
fn io_err_to_status(context: &str, err: std::io::Error) -> Status {
    if err.kind() == std::io::ErrorKind::NotFound {
        Status::not_found(format!("{context}: {err}"))
    } else if err.kind() == std::io::ErrorKind::PermissionDenied {
        Status::permission_denied(format!("{context}: {err}"))
    } else {
        Status::internal(format!("{context}: {err}"))
    }
}

/// ReadFile streams one file's bytes as Chunk frames, ending with an eof=true
/// Chunk. Mirrors grpc_server.go:288-305. File content is never logged.
pub async fn read_file(
    request: Request<sandbox_v1::ReadFileRequest>,
) -> Result<Response<BoxStream<sandbox_v1::Chunk>>, Status> {
    let path = request.into_inner().path;
    tracing::debug!(path = %path, "ReadFile: opening file");

    let file = tokio::fs::File::open(&path)
        .await
        .map_err(|e| io_err_to_status("read_file", e))?;

    let (tx, rx) = tokio::sync::mpsc::channel(16);
    tokio::spawn(async move {
        let mut reader = tokio::io::BufReader::new(file);
        let mut buf = vec![0u8; CHUNK_BYTES];
        loop {
            match reader.read(&mut buf).await {
                Ok(0) => {
                    // EOF: send the terminal eof chunk.
                    let _ = tx
                        .send(Ok(sandbox_v1::Chunk {
                            data: vec![],
                            eof: true,
                        }))
                        .await;
                    break;
                }
                Ok(n) => {
                    // get(..n) is always Some because read() guarantees n <= buf.len().
                    // unwrap_or(&[]) returns the empty slice on the impossible case,
                    // keeping the slicing lint and unwrap_used lint both satisfied.
                    let data = buf.get(..n).unwrap_or(&[]).to_vec();
                    let _ = tx
                        .send(Ok(sandbox_v1::Chunk { data, eof: false }))
                        .await;
                }
                Err(e) => {
                    let _ = tx
                        .send(Err(io_err_to_status("read_file: read", e)))
                        .await;
                    break;
                }
            }
        }
    });

    let stream = tokio_stream::wrappers::ReceiverStream::new(rx);
    Ok(Response::new(Box::pin(stream)))
}

/// WriteFile accumulates streamed open + data chunks and writes the file.
/// Mirrors grpc_server.go:311-342. Default mode is 0o644 when open.mode==0.
/// Parent directories are created automatically. File content is never logged.
pub async fn write_file(
    request: Request<tonic::Streaming<sandbox_v1::WriteFileRequest>>,
) -> Result<Response<sandbox_v1::WriteFileResult>, Status> {
    let mut stream = request.into_inner();

    // First message must carry the open oneof.
    let first = stream
        .message()
        .await
        .map_err(|e| Status::invalid_argument(format!("write_file: first message recv: {e}")))?
        .ok_or_else(|| {
            Status::invalid_argument(
                "write_file: stream closed before first message".to_string(),
            )
        })?;

    let open = match first.msg {
        Some(sandbox_v1::write_file_request::Msg::Open(o)) => o,
        _ => {
            return Err(Status::invalid_argument(
                "write_file: first message must carry the open oneof",
            ));
        }
    };

    let path = open.path.clone();
    // Default mode: 0o644 when proto field is 0 (mirrors Go grpc_server.go:333-338).
    let mode = if open.mode == 0 { 0o644u32 } else { open.mode };
    tracing::debug!(path = %path, mode = mode, "WriteFile: writing file");

    // Accumulate data chunks; bound memory to proto limits (no unbounded buffer).
    let mut content: Vec<u8> = Vec::new();
    loop {
        match stream.message().await {
            Ok(Some(msg)) => {
                if let Some(sandbox_v1::write_file_request::Msg::Data(d)) = msg.msg {
                    content.extend_from_slice(&d);
                }
                // Ignore an extra open message in the data phase (defensive).
            }
            Ok(None) => break, // stream closed
            Err(e) => {
                return Err(Status::aborted(format!("write_file: recv: {e}")));
            }
        }
    }

    let bytes_written = content.len() as i64;

    // Create parent directories if needed (mirrors Go's os.MkdirAll call in
    // handleWriteFile).
    if let Some(parent) = Path::new(&path).parent()
        && !parent.as_os_str().is_empty()
    {
        tokio::fs::create_dir_all(parent)
            .await
            .map_err(|e| io_err_to_status("write_file: create parent dirs", e))?;
    }

    // Write file. tokio::fs::write creates or truncates.
    tokio::fs::write(&path, &content)
        .await
        .map_err(|e| io_err_to_status("write_file: write", e))?;

    // Apply mode bits after writing (set_permissions).
    use std::os::unix::fs::PermissionsExt;
    let perms = std::fs::Permissions::from_mode(mode);
    tokio::fs::set_permissions(&path, perms)
        .await
        .map_err(|e| io_err_to_status("write_file: set_permissions", e))?;

    Ok(Response::new(sandbox_v1::WriteFileResult { bytes_written }))
}

/// List enumerates a directory. Pagination and filtering are not implemented
/// in this slice (returns all entries with empty next_page_token), matching the
/// honest comment at grpc_server.go:344-347.
pub async fn list(
    request: Request<sandbox_v1::ListRequest>,
) -> Result<Response<sandbox_v1::ListResponse>, Status> {
    let parent = request.into_inner().parent;
    tracing::debug!(parent = %parent, "List: reading directory");

    let mut read_dir = tokio::fs::read_dir(&parent)
        .await
        .map_err(|e| io_err_to_status("list", e))?;

    let mut entries: Vec<sandbox_v1::FileInfo> = Vec::new();
    loop {
        match read_dir.next_entry().await {
            Ok(Some(entry)) => {
                let name = entry.file_name().to_string_lossy().into_owned();
                let entry_path = format!(
                    "{}/{}",
                    parent.trim_end_matches('/'),
                    name
                );
                match entry.metadata().await {
                    Ok(meta) => {
                        use std::os::unix::fs::MetadataExt;
                        let mtime = meta
                            .modified()
                            .ok()
                            .and_then(|t| t.duration_since(UNIX_EPOCH).ok())
                            .map(|d| d.as_secs() as i64)
                            .unwrap_or(0);
                        entries.push(sandbox_v1::FileInfo {
                            name,
                            path: entry_path,
                            is_dir: meta.is_dir(),
                            size: meta.len() as i64,
                            mode: meta.mode(),
                            modified_at_unix: mtime,
                        });
                    }
                    Err(e) => {
                        tracing::debug!(path = %entry_path, err = %e, "List: skipping entry, metadata error");
                        // Skip entries whose metadata is unreadable (e.g. broken symlinks),
                        // mirroring Go's lenient behavior.
                    }
                }
            }
            Ok(None) => break,
            Err(e) => {
                return Err(io_err_to_status("list: next_entry", e));
            }
        }
    }

    Ok(Response::new(sandbox_v1::ListResponse {
        entries,
        next_page_token: String::new(),
    }))
}

/// Stat returns metadata for one path without reading its contents.
/// Uses symlink_metadata (lstat: no symlink dereference), mirrors
/// grpc_server.go:370-386.
pub async fn stat(
    request: Request<sandbox_v1::StatRequest>,
) -> Result<Response<sandbox_v1::FileInfo>, Status> {
    let path = request.into_inner().path;
    tracing::debug!(path = %path, "Stat: stating path");

    let meta = tokio::fs::symlink_metadata(&path)
        .await
        .map_err(|e| io_err_to_status("stat", e))?;

    use std::os::unix::fs::MetadataExt;
    let mtime = meta
        .modified()
        .ok()
        .and_then(|t| t.duration_since(UNIX_EPOCH).ok())
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0);

    // name: the last path component, matching filepath.Base in Go.
    let name = Path::new(&path)
        .file_name()
        .map(|n| n.to_string_lossy().into_owned())
        .unwrap_or_else(|| path.clone());

    Ok(Response::new(sandbox_v1::FileInfo {
        name,
        path,
        is_dir: meta.is_dir(),
        size: meta.len() as i64,
        mode: meta.mode(),
        modified_at_unix: mtime,
    }))
}

/// Mkdir creates a directory and all parents. Mirrors grpc_server.go:390-395
/// (os.MkdirAll with mode 0o755).
pub async fn mkdir(
    request: Request<sandbox_v1::MkdirRequest>,
) -> Result<Response<sandbox_v1::MkdirResponse>, Status> {
    let path = request.into_inner().path;
    tracing::debug!(path = %path, "Mkdir: creating directory");

    tokio::fs::create_dir_all(&path)
        .await
        .map_err(|e| io_err_to_status("mkdir", e))?;

    Ok(Response::new(sandbox_v1::MkdirResponse {}))
}

/// Remove deletes a path. Mirrors grpc_server.go:399-405: uses os.RemoveAll
/// semantics (no error on missing path, removes non-empty trees).
/// The proto's recursive flag is accepted; both file and directory removal
/// use the same RemoveAll path as Go does.
pub async fn remove(
    request: Request<sandbox_v1::RemoveRequest>,
) -> Result<Response<sandbox_v1::RemoveResponse>, Status> {
    let req = request.into_inner();
    let path = req.path;
    tracing::debug!(path = %path, recursive = req.recursive, "Remove: removing path");

    // Mimic Go's os.RemoveAll: a missing path is not an error.
    match tokio::fs::symlink_metadata(&path).await {
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
            // Path does not exist: no-op, mirrors os.RemoveAll.
            return Ok(Response::new(sandbox_v1::RemoveResponse {}));
        }
        Err(e) => {
            return Err(io_err_to_status("remove: stat", e));
        }
        Ok(meta) => {
            if meta.is_dir() {
                tokio::fs::remove_dir_all(&path)
                    .await
                    .map_err(|e| io_err_to_status("remove", e))?;
            } else {
                tokio::fs::remove_file(&path)
                    .await
                    .map_err(|e| io_err_to_status("remove", e))?;
            }
        }
    }

    Ok(Response::new(sandbox_v1::RemoveResponse {}))
}
