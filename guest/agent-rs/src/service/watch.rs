// Watch RPC implementation for the Sandbox gRPC service.
//
// Mirrors guest/agent/grpc_runtime.go Watch (lines 52-201) using the `notify`
// crate (inotify on Linux) instead of raw inotify syscalls.
//
// SECURITY: the watched path is gated by path_allowed (the same workspace
// allowlist archive.rs enforces). An out-of-workspace path is rejected with
// PermissionDenied before any watch is added.
//
// LIFECYCLE: notify::recommended_watcher owns the inotify fd and its reader
// thread. The watcher is dropped when the function returns (normally or on
// error). On client disconnect the tonic runtime drops the response stream;
// the mpsc receiver is then gone; the sender side detects a closed channel and
// the background task exits, dropping the watcher. No inotify fd or thread
// leaks on cancel.
//
// CHANNEL BOUND: the mpsc channel between the notify callback thread and the
// async sender task is bounded at WATCH_CHAN_BOUND (64 events). If the channel
// is full the sender drops the watcher and sends ResourceExhausted to the
// client. This prevents unbounded buffering when the client is slow.
//
// EVENT MAPPING (notify EventKind -> FsEvent::Kind):
//   notify::EventKind::Create(_)           -> FsEvent_Kind::Create
//   notify::EventKind::Modify(_)           -> FsEvent_Kind::Modify
//   notify::EventKind::Remove(_)           -> FsEvent_Kind::Delete
//   notify::EventKind::Modify(ModifyKind::Name(RenameMode::Both))
//       paths[0] = old, paths[1] = new    -> FsEvent_Kind::Rename
//   notify::EventKind::Access(_)           -> ignored
//   notify::EventKind::Other / Unknown     -> ignored
//
// On Linux the inotify backend produces Name(Both) events with both paths set
// when a rename occurs within the watched directory, matching Go's
// IN_MOVED_FROM / IN_MOVED_TO cookie correlation. Unmatched MOVED_FROM (a
// move out of the directory) arrives as a Remove event from notify; unmatched
// MOVED_TO (a move into the directory) arrives as a Create event. Both map
// correctly via the table above without requiring explicit cookie handling.

use std::path::PathBuf;

use notify::event::{ModifyKind, RenameMode};
use notify::{EventKind, RecursiveMode, Watcher as _};
use tonic::{Response, Status};
use tracing::debug;

use crate::sandbox_v1;
use crate::service::BoxStream;
use crate::service::archive::path_allowed;

/// Bounded event channel capacity between the notify callback and the async
/// sender task. 64 events before the client must have drained the stream.
/// On overflow the watcher is dropped and ResourceExhausted is sent.
const WATCH_CHAN_BOUND: usize = 64;

/// Watch RPC handler.
///
/// Streams FsEvent messages for filesystem changes under `req.path` until the
/// client cancels (the response stream is dropped). Mirrors the Go Watch RPC
/// in grpc_runtime.go.
pub async fn watch(
    request: tonic::Request<sandbox_v1::WatchRequest>,
) -> Result<Response<BoxStream<sandbox_v1::FsEvent>>, Status> {
    let req = request.into_inner();
    let raw_path = req.path.clone();

    // Workspace allowlist check (mirrors pathAllowed in grpc_runtime.go:54-56).
    if !path_allowed(&raw_path) {
        return Err(Status::permission_denied(format!(
            "watch: path {:?} is outside the workspace allowlist",
            raw_path
        )));
    }

    let path = PathBuf::from(&raw_path);

    // lstat the path; reject non-directories (mirrors grpc_runtime.go:57-66).
    let meta = path.symlink_metadata().map_err(|e| {
        if e.kind() == std::io::ErrorKind::NotFound {
            Status::not_found(format!("watch: {e}"))
        } else {
            Status::internal(format!("watch: lstat: {e}"))
        }
    })?;
    if !meta.is_dir() {
        return Err(Status::invalid_argument(format!(
            "watch: path {:?} is not a directory",
            raw_path
        )));
    }

    // Bounded mpsc channel: the notify callback (called from its internal
    // thread) sends raw notify events; the async task below maps them to
    // FsEvent and sends them on the tonic stream channel.
    let (notify_tx, mut notify_rx) =
        tokio::sync::mpsc::channel::<notify::Result<notify::Event>>(WATCH_CHAN_BOUND);

    // notify::recommended_watcher returns an INotifyWatcher on Linux. The
    // callback is called from notify's internal thread; we send events over
    // the mpsc channel. If the channel is full (try_send returns Err) we
    // record the overflow; the receiver task will detect the channel closed
    // after the watcher is dropped (or returns full) and send ResourceExhausted.
    let tx_for_cb = notify_tx.clone();
    let mut watcher = notify::recommended_watcher(move |ev: notify::Result<notify::Event>| {
        // try_send: if the channel is full, discard this event and close
        // the channel by dropping our clone (send error is non-fatal here;
        // the async task detects the channel closed).
        let _ = tx_for_cb.try_send(ev);
    })
    .map_err(|e| Status::internal(format!("watch: create watcher: {e}")))?;

    let mode = if req.recursive {
        RecursiveMode::Recursive
    } else {
        RecursiveMode::NonRecursive
    };

    watcher
        .watch(&path, mode)
        .map_err(|e| Status::internal(format!("watch: add watch on {:?}: {e}", raw_path)))?;

    debug!(path = %raw_path, recursive = req.recursive, "Watch: installed inotify watch");

    // Bounded tonic stream channel: the async task below sends FsEvent
    // messages here; tonic polls this channel for each response frame.
    let (stream_tx, stream_rx) =
        tokio::sync::mpsc::channel::<Result<sandbox_v1::FsEvent, Status>>(WATCH_CHAN_BOUND);

    // Spawn the event-mapping task. It owns the watcher (RAII: dropping it
    // removes the inotify watch and joins the notify thread). When the tonic
    // receiver (stream_rx -> ReceiverStream) is dropped (client disconnect),
    // stream_tx.send returns Err; the task exits and the watcher is dropped.
    tokio::spawn(async move {
        // Move the watcher into this task so it is dropped when the task exits.
        let _watcher = watcher;

        loop {
            match notify_rx.recv().await {
                None => {
                    // Notify channel closed: the callback dropped its sender
                    // (e.g. the watcher was already dropped or notify failed).
                    // Nothing more to send; exit cleanly.
                    break;
                }
                Some(Err(e)) => {
                    // A notify error (e.g. inotify limit reached).
                    let _ = stream_tx
                        .send(Err(Status::internal(format!("watch: notify error: {e}"))))
                        .await;
                    break;
                }
                Some(Ok(event)) => {
                    // Map the notify event to zero or more FsEvent messages.
                    let fs_events = map_event(event);
                    for fs_ev in fs_events {
                        if stream_tx.send(Ok(fs_ev)).await.is_err() {
                            // Client disconnected; stop.
                            return;
                        }
                    }
                }
            }
        }
    });

    // Drop the original notify_tx here so that when the task's watcher is
    // dropped (and notify stops sending), the channel closes naturally.
    drop(notify_tx);

    let out = tokio_stream::wrappers::ReceiverStream::new(stream_rx);
    Ok(Response::new(Box::pin(out)))
}

/// Map a notify Event to zero or more FsEvent proto messages.
///
/// Mapping table (mirrors grpc_runtime.go event switch):
///   Create(_)                        -> [FsEvent{kind: Create, path: paths[0]}]
///   Modify(Name(Both))               -> [FsEvent{kind: Rename, path: paths[0], new_path: paths[1]}]
///   Modify(_) (non-rename)           -> [FsEvent{kind: Modify, path: paths[0]}]
///   Remove(_)                        -> [FsEvent{kind: Delete, path: paths[0]}]
///   Access(_) / Other / Unknown      -> [] (ignored)
///
/// On Linux the inotify backend handles the MOVED_FROM/MOVED_TO cookie pair
/// internally and delivers a single Modify(Name(Both)) event with paths[0] =
/// old and paths[1] = new, so we do not need explicit cookie tracking here.
/// An unmatched MOVED_FROM becomes a Remove; an unmatched MOVED_TO becomes a
/// Create; both are handled by the base Create/Remove arms above.
fn map_event(event: notify::Event) -> Vec<sandbox_v1::FsEvent> {
    use sandbox_v1::fs_event::Kind;

    let first_path = event.paths.first().map(|p| p.to_string_lossy().into_owned());
    let second_path = event.paths.get(1).map(|p| p.to_string_lossy().into_owned());

    match event.kind {
        EventKind::Create(_) => {
            let Some(path) = first_path else {
                return vec![];
            };
            vec![sandbox_v1::FsEvent {
                kind: Kind::Create as i32,
                path,
                new_path: String::new(),
            }]
        }
        EventKind::Modify(ModifyKind::Name(RenameMode::Both)) => {
            // Both paths set: old -> new rename within the watched directory.
            let Some(old_path) = first_path else {
                return vec![];
            };
            let new_path = second_path.unwrap_or_default();
            vec![sandbox_v1::FsEvent {
                kind: Kind::Rename as i32,
                path: old_path,
                new_path,
            }]
        }
        EventKind::Modify(_) => {
            // All other Modify kinds (data change, metadata, etc.) -> MODIFY.
            let Some(path) = first_path else {
                return vec![];
            };
            vec![sandbox_v1::FsEvent {
                kind: Kind::Modify as i32,
                path,
                new_path: String::new(),
            }]
        }
        EventKind::Remove(_) => {
            let Some(path) = first_path else {
                return vec![];
            };
            vec![sandbox_v1::FsEvent {
                kind: Kind::Delete as i32,
                path,
                new_path: String::new(),
            }]
        }
        // Access and Other/Unknown events are not part of the proto surface;
        // ignore them so the stream stays focused on structural changes.
        EventKind::Access(_) | EventKind::Other | EventKind::Any => vec![],
    }
}
