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
// PID-1 init module (task 1.3).
// ---------------------------------------------------------------------------

/// PID-1 init: mounts filesystems, creates /workspace, sets hostname.
/// Converted from init.rs to init/ module in task 1.3 to add MOUNT_TABLE const
/// and tracing integration.
#[allow(unsafe_code, missing_docs)]
pub mod init;

// ---------------------------------------------------------------------------
// Kernel manager (task 2.8: RunCode + kernel/ module).
// ---------------------------------------------------------------------------

/// In-guest code-execution kernel (Jupyter-style). Drives the persistent
/// Python kernel driver subprocess via JSON-lines stdin/stdout; serializes
/// executions; maps driverEvent kinds to RunCodeResponse frames.
pub mod kernel;

// ---------------------------------------------------------------------------
// tonic Sandbox service skeleton (task 1.3).
// ---------------------------------------------------------------------------

/// The tonic Sandbox gRPC service implementation.
/// All RPCs return Unimplemented until Phase 2 tasks fill them in.
pub mod service;

// ---------------------------------------------------------------------------
// sys/ module: the ONLY place in the crate with unsafe code (task 1.2).
// Wraps AF_VSOCK, RNDADDENTROPY, and clock_settime behind safe APIs.
// ---------------------------------------------------------------------------

/// Unsafe syscall wrappers: AF_VSOCK listener, RNDADDENTROPY ioctl,
/// clock_gettime / clock_settime. Every unsafe block carries a SAFETY comment.
#[allow(unsafe_code, missing_docs)]
pub mod sys;

// ---------------------------------------------------------------------------
// fork/ module: post-restore state repair (task 3.1+).
// Safe wrappers over sys primitives for the notify-forked path.
// ---------------------------------------------------------------------------

/// Fork-correctness modules: credited CRNG reseed and other post-restore
/// state repair steps invoked by the notify-forked handler (Task 3.5).
pub mod fork;
