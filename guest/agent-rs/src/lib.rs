// Library target: exposes protocol, handlers, transport, and init so integration
// tests (and future tooling) can call into the agent without the binary wrapper.

pub mod init;
pub mod protocol;
pub mod handlers;
pub mod transport;
