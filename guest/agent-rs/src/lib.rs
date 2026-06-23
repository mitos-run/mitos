//! sandbox-agent library: gRPC runtime protocol for the Firecracker guest agent.
//!
//! Task 0.1: establishes the production gRPC toolchain and crate lint set.
//! Later tasks will fill out the gRPC service implementations.

// ---------------------------------------------------------------------------
// Crate-wide lint set (required by task-0.1-brief).
// ---------------------------------------------------------------------------

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
// Shared primitives: error types and environment merge.
// ---------------------------------------------------------------------------

/// Typed agent errors with tonic Status mapping (task 1.1).
pub mod error;

/// Guest environment merge: base < configured < request precedence (task 1.1).
pub mod env;

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
// PID-1 init module (task 1.3 builds the init/ module on this).
// ---------------------------------------------------------------------------

/// PID-1 init: mounts filesystems, creates /workspace, sets hostname.
#[allow(unsafe_code, missing_docs)]
pub mod init;
