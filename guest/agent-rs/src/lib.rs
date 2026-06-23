// Library target: exposes protocol, handlers, and transport so integration
// tests (and future tooling) can call into the agent without the binary wrapper.

pub mod protocol;
pub mod handlers;
pub mod transport;
