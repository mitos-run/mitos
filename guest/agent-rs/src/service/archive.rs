// Archive and Upload RPC implementations for the Sandbox gRPC service.
//
// Mirrors guest/agent/grpc_server.go:413-469 and guest/agent/tardir.go.
//
// Archive(ArchiveRequest{path, direction}) -> stream Chunk
//   - Rejects UNTAR direction with InvalidArgument.
//   - Rejects paths outside the workspace allowlist with PermissionDenied.
//   - Tars the subtree (regular files and directories only; symlinks skipped).
//   - Streams the tar in CHUNK_BYTES (32 KiB) Chunk frames; sends a final
//     Chunk{eof: true} to signal completion.
//   - Bounds the tar at MAX_TAR_BYTES (64 MiB, matching Go MaxTarBytes); returns
//     ResourceExhausted if the limit is exceeded (checked as a running total during
//     build to cap peak allocation, not only after the buffer is complete).
//
// Upload(stream UploadRequest) -> UploadResult
//   - First message must carry UploadOpen{dest}; rejects if dest is outside the
//     workspace allowlist.
//   - Accumulates chunk bytes up to MAX_TAR_BYTES; returns ResourceExhausted on
//     overflow.
//   - Extracts via a manual entry loop with safe_join as the sole path-traversal
//     barrier (NOT tar::Archive::unpack); rejects absolute paths and "../" escapes
//     before any write; only TypeReg and TypeDir members are materialized.
//   - Returns UploadResult{bytes_written: total_chunk_bytes}.
//
// SECURITY (path traversal):
//   The safe_join function mirrors tardir.go:safeJoin and is the traversal
//   barrier for extraction. It:
//     1. Rejects absolute paths.
//     2. Strips CurDir (".") components.
//     3. Rejects any cleaned path whose first component is ParentDir ("..").
//     4. Applies a final starts_with(dst) check as defense-in-depth.
//   Only TypeReg and TypeDir entry types are materialized; symlinks, devices,
//   fifos, and hard links are rejected with PermissionDenied so a restored
//   symlink cannot re-introduce an escape on a subsequent tar walk.
//
// Blocking tar work:
//   The tar crate performs synchronous (blocking) IO. Both tarDir and untarDir
//   are wrapped in tokio::task::spawn_blocking so the async reactor thread is
//   never stalled by blocking file operations. Results cross the blocking/async
//   boundary via the closure return value.

use std::io::{Read, Write as _};
use std::path::{Component, Path, PathBuf};

use tonic::{Response, Status};
use tracing::debug;

use crate::error::AgentError;
use crate::sandbox_v1;
use crate::service::BoxStream;

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

/// Maximum tar size in bytes: 64 MiB, matching MaxTarBytes in
/// internal/vsock/protocol.go. Exceeding this limit causes ResourceExhausted
/// so a large workspace or a malicious tar cannot exhaust guest memory.
const MAX_TAR_BYTES: usize = 64 << 20;

/// Chunk size for streaming: 32 KiB, matching grpcChunkBytes in grpc_server.go.
const CHUNK_BYTES: usize = 32 << 10;

// ---------------------------------------------------------------------------
// Allowlist gate
// ---------------------------------------------------------------------------

/// Returns true if p is the workspace root or a descendant of it.
/// Mirrors pathAllowed in tardir.go (which uses filepath.Clean before comparing).
///
/// SECURITY: We normalize ".." lexically (without touching the filesystem) before
/// comparing against the workspace root. A naive `Path::new(p).components().collect()`
/// does NOT resolve ".." components, so "/workspace/../etc" would still
/// `starts_with("/workspace")` while the OS resolves it to "/etc". The loop below
/// mirrors filepath.Clean in Go: ParentDir pops the last stack entry (and errors
/// if the stack would go above root), CurDir is skipped, everything else is pushed.
pub(super) fn path_allowed(root: &Path, p: &str) -> bool {
    if p.is_empty() {
        return false;
    }
    let mut normalized: Vec<std::ffi::OsString> = Vec::new();
    for c in Path::new(p).components() {
        match c {
            Component::ParentDir => {
                normalized.pop();
            }
            Component::CurDir => {}
            other => normalized.push(other.as_os_str().to_owned()),
        }
    }
    let clean: PathBuf = normalized.iter().collect();
    clean == root || clean.starts_with(root)
}

// ---------------------------------------------------------------------------
// safe_join: path-traversal barrier for tar extraction
// ---------------------------------------------------------------------------

/// Joins a tar member name onto dst and rejects any name that:
///   - is absolute,
///   - after logical resolution of ".." components, escapes dst.
///
/// Mirrors safeJoin in tardir.go:217-234. The final check uses a custom
/// path normalizer that resolves ".." without touching the filesystem, so
/// "subdir/../../escape" is correctly detected as escaping dst even though
/// `Path::starts_with` does not resolve "..".
fn safe_join(dst: &Path, name: &Path) -> Result<PathBuf, AgentError> {
    if name.is_absolute() {
        return Err(AgentError::PathDenied(format!(
            "refusing absolute tar member {:?}",
            name
        )));
    }
    // Build the full logical path by starting from dst's components and appending
    // name's components, resolving ".." along the way (same as filepath.Clean in Go).
    let mut components: Vec<&std::ffi::OsStr> = dst.components()
        .map(|c| c.as_os_str())
        .collect();
    let dst_len = components.len();
    for c in name.components() {
        match c {
            Component::CurDir => {} // skip "."
            Component::ParentDir => {
                // Pop the last component; if we can't pop without going above dst,
                // the entry escapes the target.
                if components.len() <= dst_len {
                    return Err(AgentError::PathDenied(format!(
                        "refusing traversing tar member {:?}",
                        name
                    )));
                }
                components.pop();
            }
            Component::Normal(seg) => components.push(seg),
            Component::RootDir | Component::Prefix(_) => {
                // Absolute component inside a relative path: reject.
                return Err(AgentError::PathDenied(format!(
                    "refusing absolute tar member {:?}",
                    name
                )));
            }
        }
    }
    let joined: PathBuf = components.iter().collect();
    // Defense-in-depth: the joined path must start with dst.
    if !joined.starts_with(dst) {
        return Err(AgentError::PathDenied(format!(
            "tar member {:?} resolves outside target",
            name
        )));
    }
    Ok(joined)
}

// ---------------------------------------------------------------------------
// Archive: tar a directory subtree and stream as Chunks
// ---------------------------------------------------------------------------

/// Archive RPC handler.
///
/// Rejects UNTAR direction with InvalidArgument (mirrors grpc_server.go:414-415).
/// Rejects out-of-allowlist paths with PermissionDenied.
/// Tars the subtree in a spawn_blocking task, then streams the bytes in CHUNK_BYTES
/// frames, ending with Chunk{eof: true}.
pub async fn archive(
    workspace_root: &Path,
    request: tonic::Request<sandbox_v1::ArchiveRequest>,
) -> Result<Response<BoxStream<sandbox_v1::Chunk>>, Status> {
    let req = request.into_inner();

    // Reject UNTAR direction: the symmetric extract is the Upload RPC.
    if req.direction == sandbox_v1::archive_request::Direction::Untar as i32 {
        return Err(Status::invalid_argument(
            "archive: UNTAR direction is served by the Upload RPC; use Upload to extract a tar",
        ));
    }

    let path = req.path.clone();

    // Workspace allowlist check.
    if !path_allowed(workspace_root, &path) {
        return Err(Status::permission_denied(format!(
            "archive: path {:?} is outside the workspace transfer allowlist",
            path
        )));
    }

    debug!(path = %path, "Archive: taring subtree");

    // Build the tar in a blocking thread so the async reactor is not stalled.
    let tar_data = tokio::task::spawn_blocking(move || tar_dir(&path))
        .await
        .map_err(|e| Status::internal(format!("archive: spawn_blocking join: {e}")))?
        .map_err(Status::from)?;

    if tar_data.len() > MAX_TAR_BYTES {
        return Err(Status::resource_exhausted(format!(
            "archive: tar size {} exceeds max {} bytes",
            tar_data.len(),
            MAX_TAR_BYTES
        )));
    }

    // Stream the bytes as Chunk frames.
    let (tx, rx) = tokio::sync::mpsc::channel(16);
    tokio::spawn(async move {
        let mut offset = 0usize;
        let total = tar_data.len();
        while offset < total {
            let end = (offset + CHUNK_BYTES).min(total);
            let data = match tar_data.get(offset..end) {
                Some(slice) => slice.to_vec(),
                // This branch is unreachable: end = (offset + CHUNK_BYTES).min(total)
                // guarantees end <= total = tar_data.len(). If it were ever reached
                // it would mean a logic error, so we surface it as an Internal error
                // rather than silently sending an empty chunk.
                None => {
                    let _ = tx
                        .send(Err(tonic::Status::internal(
                            "archive: slice out of bounds (logic error)",
                        )))
                        .await;
                    return;
                }
            };
            if tx
                .send(Ok(sandbox_v1::Chunk { data, eof: false }))
                .await
                .is_err()
            {
                // Receiver dropped (client disconnected); stop building/sending.
                return;
            }
            offset = end;
        }
        // Final eof sentinel; ignore send error (receiver may have gone).
        let _ = tx
            .send(Ok(sandbox_v1::Chunk {
                data: vec![],
                eof: true,
            }))
            .await;
    });

    let stream = tokio_stream::wrappers::ReceiverStream::new(rx);
    Ok(Response::new(Box::pin(stream)))
}

// ---------------------------------------------------------------------------
// tar_dir: build a tar archive from a directory subtree
// ---------------------------------------------------------------------------

/// Walk dir and write a tar of its regular files and directories (relative to
/// dir) into an in-memory buffer. Symlinks, devices, sockets, and fifos are
/// skipped so extraction can never be tricked via a restored symlink.
///
/// Mirrors tarDir in tardir.go:64-135. Uses walkdir rather than filepath.Walk
/// for a Rust-idiomatic directory walk with explicit symlink-following control.
fn tar_dir(dir: &str) -> Result<Vec<u8>, AgentError> {
    let dir_path = Path::new(dir);

    // A missing directory yields an empty tar (mirrors the Go side).
    if !dir_path.exists() {
        // An empty tar still needs the end-of-archive two 512-byte zero blocks.
        let mut buf: Vec<u8> = Vec::new();
        {
            let mut builder = tar::Builder::new(&mut buf);
            builder.finish()?;
        }
        return Ok(buf);
    }

    let meta = std::fs::metadata(dir_path)?;
    if !meta.is_dir() {
        return Err(AgentError::InvalidArgument(format!(
            "{dir} is not a directory"
        )));
    }

    // CountingWriter wraps a Vec<u8> and tracks the total bytes written so we
    // can check the running total while builder holds the mutable borrow on buf.
    struct CountingWriter {
        inner: Vec<u8>,
        written: usize,
    }
    impl std::io::Write for CountingWriter {
        fn write(&mut self, data: &[u8]) -> std::io::Result<usize> {
            self.inner.write(data).inspect(|&n| { self.written += n; })
        }
        fn flush(&mut self) -> std::io::Result<()> {
            self.inner.flush()
        }
    }

    let mut cw = CountingWriter { inner: Vec::new(), written: 0 };
    {
        let mut builder = tar::Builder::new(&mut cw);

        // walkdir visits the root first; follow_links(false) keeps symlinks as
        // symlink entries (we skip them below) rather than resolving them.
        for entry in walkdir::WalkDir::new(dir_path).follow_links(false).sort_by_file_name() {
            let entry = entry.map_err(|e| AgentError::Internal(format!("walk: {e}")))?;

            // Skip the root itself; members are relative to dir.
            if entry.path() == dir_path {
                continue;
            }

            let rel = entry
                .path()
                .strip_prefix(dir_path)
                .map_err(|e| AgentError::Internal(format!("strip_prefix: {e}")))?;

            let rel_str = rel
                .to_str()
                .ok_or_else(|| AgentError::Internal(format!("non-UTF-8 path: {:?}", rel)))?;

            let file_type = entry.file_type();

            if file_type.is_dir() {
                let mut header = tar::Header::new_gnu();
                let mode = entry
                    .metadata()
                    .map(|m| {
                        use std::os::unix::fs::MetadataExt as _;
                        m.mode() & 0o777
                    })
                    .unwrap_or(0o755);
                header.set_mode(mode);
                header.set_size(0);
                header.set_entry_type(tar::EntryType::Directory);
                header.set_cksum();
                // Directories in tar have a trailing slash.
                let dir_name = format!("{}/", rel_str.replace('\\', "/"));
                builder
                    .append_data(&mut header, &dir_name, std::io::empty())
                    .map_err(AgentError::Io)?;
            } else if file_type.is_file() {
                let path = entry.path();
                let meta = std::fs::metadata(path)?;
                let size = meta.len();

                let mode = {
                    use std::os::unix::fs::MetadataExt as _;
                    meta.mode() & 0o777
                };

                let mut header = tar::Header::new_gnu();
                header.set_size(size);
                header.set_mode(mode);
                header.set_entry_type(tar::EntryType::Regular);
                header.set_cksum();

                let f = std::fs::File::open(path)?;
                // Bound the read at MAX_TAR_BYTES to prevent unbounded memory use
                // when an individual file grows between stat and open.
                let bounded = f.take(MAX_TAR_BYTES as u64);
                builder
                    .append_data(&mut header, rel_str.replace('\\', "/"), bounded)
                    .map_err(AgentError::Io)?;
            }
            // Symlinks and other entry types are skipped (no else branch).

            // Running-total check via the CountingWriter: abort early so we never
            // accumulate a multi-GiB buffer before the post-build size check in
            // the Archive handler. builder.get_ref() gives &CountingWriter.
            if builder.get_ref().written > MAX_TAR_BYTES {
                return Err(AgentError::ResourceExhausted(format!(
                    "tar size exceeded max {} bytes while building archive",
                    MAX_TAR_BYTES
                )));
            }
        }

        builder.finish()?;
    } // builder dropped here, releasing the borrow on cw
    Ok(cw.inner)
}

// ---------------------------------------------------------------------------
// Upload: extract a streamed tar at a destination directory
// ---------------------------------------------------------------------------

/// Upload RPC handler.
///
/// First message must carry UploadOpen{dest}; subsequent messages carry raw tar
/// bytes as chunk. After EOF the accumulated bytes are extracted in a
/// spawn_blocking task using safe_join to reject path traversal. Returns
/// UploadResult{bytes_written} on success.
pub async fn upload(
    workspace_root: &Path,
    request: tonic::Request<tonic::Streaming<sandbox_v1::UploadRequest>>,
) -> Result<Response<sandbox_v1::UploadResult>, Status> {
    let mut stream = request.into_inner();

    // First message: must be UploadOpen.
    let first = stream
        .message()
        .await
        .map_err(|e| Status::invalid_argument(format!("upload: first message recv: {e}")))?
        .ok_or_else(|| Status::invalid_argument("upload: stream ended before open message"))?;

    let dest = match first.msg {
        Some(sandbox_v1::upload_request::Msg::Open(o)) => o.dest,
        _ => {
            return Err(Status::invalid_argument(
                "upload: first message must carry the open oneof",
            ))
        }
    };

    // Workspace allowlist check.
    if !path_allowed(workspace_root, &dest) {
        return Err(Status::permission_denied(format!(
            "upload: dest {:?} is outside the workspace transfer allowlist",
            dest
        )));
    }

    debug!(dest = %dest, "Upload: receiving tar chunks");

    // Accumulate chunk bytes up to MAX_TAR_BYTES.
    let mut tar_bytes: Vec<u8> = Vec::new();
    loop {
        match stream.message().await {
            Ok(Some(msg)) => {
                if let Some(sandbox_v1::upload_request::Msg::Chunk(data)) = msg.msg {
                    let new_len = tar_bytes.len().saturating_add(data.len());
                    if new_len > MAX_TAR_BYTES {
                        return Err(Status::resource_exhausted(format!(
                            "upload: tar size would exceed max {} bytes",
                            MAX_TAR_BYTES
                        )));
                    }
                    tar_bytes.extend_from_slice(&data);
                }
                // Ignore messages with no Chunk (e.g. a second open).
            }
            Ok(None) => break, // stream EOF
            Err(e) => return Err(Status::aborted(format!("upload: recv: {e}"))),
        }
    }

    let bytes_written = tar_bytes.len();

    // Extract in a blocking thread.
    tokio::task::spawn_blocking(move || untar_dir(&dest, tar_bytes))
        .await
        .map_err(|e| Status::internal(format!("upload: spawn_blocking join: {e}")))?
        .map_err(Status::from)?;

    Ok(Response::new(sandbox_v1::UploadResult {
        bytes_written: bytes_written as i64,
    }))
}

// ---------------------------------------------------------------------------
// untar_dir: extract a tar archive into a destination directory
// ---------------------------------------------------------------------------

/// Extract data (a tar archive) into dst using safe_join to block path
/// traversal. Only TypeReg and TypeDir entries are materialized; any other type
/// (symlink, device, fifo, hard link) is rejected with PermissionDenied to
/// mirror tardir.go:untarDir behavior.
///
/// SECURITY:
///   - safe_join rejects absolute member paths and "../" escapes before any fs
///     operation.
///   - Non-file, non-directory entry types are rejected so a symlink member
///     cannot point outside dst on subsequent operations.
///   - Each file write is bounded by MAX_TAR_BYTES via io::copy with a
///     LimitReader so a header that lies about size cannot cause unbounded IO.
fn untar_dir(dst: &str, data: Vec<u8>) -> Result<(), AgentError> {
    let dst_path = Path::new(dst);
    std::fs::create_dir_all(dst_path)?;

    let cursor = std::io::Cursor::new(data);
    let mut archive = tar::Archive::new(cursor);

    for entry_result in archive.entries()? {
        let mut entry = entry_result?;
        let entry_path = entry.path()?.into_owned();
        let target = safe_join(dst_path, &entry_path)?;

        match entry.header().entry_type() {
            tar::EntryType::Directory => {
                let mode = entry.header().mode().unwrap_or(0o755) & 0o777;
                std::fs::create_dir_all(&target)?;
                // Apply the mode from the tar header.
                #[cfg(unix)]
                {
                    use std::os::unix::fs::PermissionsExt as _;
                    std::fs::set_permissions(&target, std::fs::Permissions::from_mode(mode))?;
                }
                #[cfg(not(unix))]
                let _ = mode;
            }
            tar::EntryType::Regular => {
                // Create parent directories if the tar lacks an explicit dir entry.
                if let Some(parent) = target.parent() {
                    std::fs::create_dir_all(parent)?;
                }
                let mode = entry.header().mode().unwrap_or(0o644) & 0o777;
                let actual_mode = if mode == 0 { 0o644 } else { mode };
                write_regular_entry(&target, &mut entry, actual_mode)?;
            }
            other => {
                return Err(AgentError::PermissionDenied(format!(
                    "refusing tar member {:?} with unsupported type {:?}",
                    entry_path, other
                )));
            }
        }
    }
    Ok(())
}

/// Write a single tar entry's contents to target. The read is bounded by
/// MAX_TAR_BYTES so a header that lies about size cannot drive an unbounded
/// write (mirrors writeRegular in tardir.go).
fn write_regular_entry(
    target: &Path,
    reader: &mut impl Read,
    mode: u32,
) -> Result<(), AgentError> {
    let mut f = std::fs::OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .open(target)?;

    // Bound the read at MAX_TAR_BYTES.
    let mut bounded = reader.take(MAX_TAR_BYTES as u64);
    std::io::copy(&mut bounded, &mut f)?;
    f.flush()?;

    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt as _;
        f.set_permissions(std::fs::Permissions::from_mode(mode))?;
    }
    #[cfg(not(unix))]
    let _ = mode;

    Ok(())
}

// ---------------------------------------------------------------------------
// Unit tests for path_allowed and safe_join
// ---------------------------------------------------------------------------

#[cfg(test)]
#[allow(clippy::unwrap_used, clippy::expect_used)]
mod tests {
    use super::*;

    #[test]
    fn path_allowed_workspace_root_exact() {
        assert!(path_allowed(Path::new("/workspace"), "/workspace"));
    }

    #[test]
    fn path_allowed_workspace_subpath() {
        assert!(path_allowed(Path::new("/workspace"), "/workspace/project/main.py"));
    }

    #[test]
    fn path_allowed_rejects_etc() {
        assert!(!path_allowed(Path::new("/workspace"), "/etc"));
    }

    #[test]
    fn path_allowed_rejects_workspace_prefix_but_different_dir() {
        // /workspaceExtra must not pass just because it starts with /workspace.
        assert!(!path_allowed(Path::new("/workspace"), "/workspaceExtra"));
    }

    #[test]
    fn path_allowed_rejects_empty() {
        assert!(!path_allowed(Path::new("/workspace"), ""));
    }

    // C1: path traversal via ".." must be rejected by path_allowed.
    // The naive components().collect() does NOT resolve "..", so
    // "/workspace/../etc" would have passed starts_with("/workspace") while the
    // OS resolves the path to "/etc". The fixed implementation normalizes ".."
    // lexically (like filepath.Clean) before the workspace comparison.
    #[test]
    fn path_allowed_rejects_dotdot_traversal() {
        assert!(!path_allowed(Path::new("/workspace"), "/workspace/../etc"));
    }

    #[test]
    fn path_allowed_rejects_dotdot_absolute() {
        assert!(!path_allowed(Path::new("/workspace"), "/etc"));
    }

    #[test]
    fn path_allowed_workspace_root_exact_after_normalization() {
        // "/workspace/." normalizes to "/workspace" and must be allowed.
        assert!(path_allowed(Path::new("/workspace"), "/workspace/."));
    }

    #[test]
    fn path_allowed_workspace_subpath_after_normalization() {
        // "/workspace/a/../b" normalizes to "/workspace/b" and must be allowed.
        assert!(path_allowed(Path::new("/workspace"), "/workspace/a/../b"));
    }

    #[test]
    fn safe_join_normal_path() {
        let dst = Path::new("/tmp/dest");
        let result = safe_join(dst, Path::new("subdir/file.txt")).unwrap();
        assert_eq!(result, PathBuf::from("/tmp/dest/subdir/file.txt"));
    }

    #[test]
    fn safe_join_rejects_absolute() {
        let dst = Path::new("/tmp/dest");
        let err = safe_join(dst, Path::new("/etc/passwd")).unwrap_err();
        assert!(matches!(err, AgentError::PathDenied(_)));
    }

    #[test]
    fn safe_join_rejects_dotdot_escape() {
        let dst = Path::new("/tmp/dest");
        let err = safe_join(dst, Path::new("../escape")).unwrap_err();
        assert!(matches!(err, AgentError::PathDenied(_)));
    }

    #[test]
    fn safe_join_rejects_deep_dotdot_escape() {
        let dst = Path::new("/tmp/dest");
        let err = safe_join(dst, Path::new("subdir/../../escape")).unwrap_err();
        // After cleaning, subdir/../../escape becomes ../escape which starts with "..".
        assert!(matches!(err, AgentError::PathDenied(_)));
    }

    #[test]
    fn safe_join_strips_cur_dir() {
        let dst = Path::new("/tmp/dest");
        let result = safe_join(dst, Path::new("./file.txt")).unwrap();
        assert_eq!(result, PathBuf::from("/tmp/dest/file.txt"));
    }
}
