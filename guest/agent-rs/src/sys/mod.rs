// sys/ module: the ONLY place in the crate with `unsafe` code.
//
// This module wraps three Linux-specific unsafe surfaces behind safe,
// typed APIs:
//
// 1. entropy: RNDADDENTROPY ioctl for fork-correctness CRNG reseed.
// 2. clock: clock_gettime / clock_settime for CLOCK_REALTIME step.
// 3. vsock: AF_VSOCK listener via tokio-vsock (vsock feature).
//
// Lint configuration (required by the task-1.2 safety bar):
//   - unsafe_code is allowed for this module by the `#[allow(unsafe_code)]`
//     attribute on the `pub mod sys;` declaration in lib.rs. We do NOT repeat
//     `#![allow(unsafe_code)]` here because clippy flags it as a duplicated
//     attribute.
//   - #![deny(unsafe_op_in_unsafe_fn)]: every unsafe operation must sit inside
//     an explicit `unsafe {}` block even inside unsafe fns, making the unsafe
//     surface granular and auditable.

#![deny(unsafe_op_in_unsafe_fn)]

pub mod clock;
pub mod entropy;
pub mod kill;
pub mod netlink;
pub mod pty;
pub mod mount;
pub mod rlimit;
pub mod signal;
pub mod vsock;

// Re-export the most-used surface so callers write sys::reseed_crng etc.
pub use clock::{clock_now_nanos, clock_set_realtime, step_clock, CLOCK_STEP_THRESHOLD_NS};
pub use entropy::{reseed_crng, reseed_crng_at};
pub use kill::kill;
pub use mount::{is_mounted, mount, sethostname, MS_PRIVATE, MS_RDONLY, MS_REC};
pub use vsock::{AGENT_GRPC_PORT, AGENT_LEGACY_PORT};
#[cfg(feature = "vsock")]
pub use vsock::bind_vsock;
