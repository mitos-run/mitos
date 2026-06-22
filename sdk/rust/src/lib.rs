//! Thin, direct-mode Rust client for the mitos standalone and hosted
//! sandbox-server REST API.
//!
//! This crate mirrors the direct-mode surface of the Python SDK
//! (`sdk/python/mitos/direct.py`), the TypeScript SDK
//! (`sdk/typescript/src/server.ts`), and the Ruby SDK
//! (`sdk/ruby/lib/mitos/sandbox_server.rb`): create a template, fork a sandbox,
//! run `exec`, and terminate. It covers DIRECT mode only (the standalone
//! `cmd/sandbox-server` and the hosted control plane at `https://mitos.run`);
//! the Kubernetes / cluster mode is served by the Python and TypeScript SDKs.
//!
//! # Auth and base URL resolution
//!
//! The base URL is the explicit builder argument, else `MITOS_BASE_URL`, else
//! the hosted endpoint `https://mitos.run`. The bearer token is the explicit
//! argument, else `MITOS_API_KEY`, else the CLI login credential file
//! (`~/.config/mitos/credentials.json`, honoring `MITOS_CONFIG_DIR`, the
//! `"token"` field), else none (tokenless). A missing or unreadable credential
//! file is never an error. The token is sent as `Authorization: Bearer <key>`;
//! the standalone server is tokenless and ignores it, while the hosted front
//! door verifies it. The token VALUE is never logged and is redacted from any
//! error.
//!
//! # Example
//!
//! ```no_run
//! use mitos::SandboxServer;
//!
//! # fn main() -> Result<(), mitos::MitosError> {
//! // Base URL + API key resolved from the environment (explicit args override).
//! let server = SandboxServer::new();
//! server.create_template("python")?;            // build (or get) the template
//! let sandbox = server.fork("python")?;         // fork a fresh sandbox
//!
//! let result = sandbox.exec("echo hello")?;
//! assert_eq!(result.exit_code, 0);
//!
//! sandbox.terminate()?;
//! # Ok(())
//! # }
//! ```

#![forbid(unsafe_code)]
#![warn(missing_docs)]

mod client;
mod error;
mod types;

pub use client::{
    valid_sandbox_id, Sandbox, SandboxServer, SandboxServerBuilder, DEFAULT_BASE_URL,
};
pub use error::MitosError;
pub use types::{ExecResult, ServerSandbox, Template};
