//! sandbox-agent library: gRPC runtime protocol for the Firecracker guest agent.
//!
//! Task 0.1: establishes the production gRPC toolchain and crate lint set.
//! Later tasks will fill out the gRPC service implementations.

// ---------------------------------------------------------------------------
// Crate-wide lint set (required by task-0.1-brief).
// ---------------------------------------------------------------------------

// No unsafe code is permitted anywhere in the crate root or any module outside
// sys/. The unsafe keyword is permitted inside libc call sites in the legacy
// protocol and init modules during the migration window; those will be audited
// and moved to sys/ in a follow-up task.
#![deny(unsafe_code)]

// Panic-equivalent lints: deny on production paths, warn on tests.
#![deny(clippy::unwrap_used)]
#![deny(clippy::expect_used)]
#![deny(clippy::panic)]
#![deny(clippy::indexing_slicing)]

// Documentation lint: warn, not deny, to avoid blocking compilation on
// in-progress stubs.
#![warn(missing_docs)]

// ---------------------------------------------------------------------------
// Proto-generated types (sandbox.v1).
// ---------------------------------------------------------------------------

/// Generated types for the sandbox.v1 gRPC service.
///
/// The source of truth is proto/sandbox/v1/sandbox.proto (vendored under
/// guest/agent-rs/proto/ for a self-contained build). tonic-build generates
/// this module at build time via build.rs.
pub mod sandbox_v1 {
    // The generated code is third-party output; suppress missing_docs and
    // allow unsafe_code for the pattern-matched codegen patterns tonic emits.
    #![allow(missing_docs, unsafe_code)]
    tonic::include_proto!("sandbox.v1");
}

// ---------------------------------------------------------------------------
// Legacy JSON-lines modules (spike scaffolding, kept during migration).
//
// These modules retain the existing logic that later tasks will migrate to
// the gRPC service implementation. They are NOT deleted in task 0.1 because:
//   - init: PID-1 init logic (fork-correctness; reused in task 0.x).
//   - protocol: JSON types used by the existing transport/handler test suite.
//   - handlers: exec, file, notify_forked implementations (reused in task 1.x).
//   - transport: JSON-lines vsock accept loop (legacy path, stays in force
//     during wire migration per CLAUDE.md and grpc_server.go comments).
//
// Deferred removal: once the gRPC handlers are complete and the JSON-lines
// path is retired (issue #24 wire migration complete), these modules will be
// deleted. That is out of scope for task 0.1.
// ---------------------------------------------------------------------------

/// PID-1 init: mounts filesystems, creates /workspace, sets hostname.
#[allow(unsafe_code, missing_docs)]
pub mod init;

/// Legacy JSON-lines protocol types (spike wire format, kept during migration).
#[allow(unsafe_code, clippy::unwrap_used, clippy::expect_used, missing_docs)]
pub mod protocol;

/// Legacy JSON-lines request handlers (exec, file IO, notify_forked).
#[allow(unsafe_code, clippy::unwrap_used, clippy::expect_used, clippy::panic, clippy::indexing_slicing, missing_docs)]
pub mod handlers;

/// Legacy JSON-lines transport (vsock accept loop, kept during wire migration).
#[allow(unsafe_code, clippy::unwrap_used, clippy::expect_used, clippy::panic, clippy::indexing_slicing, missing_docs)]
pub mod transport;
