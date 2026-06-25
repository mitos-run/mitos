//! Native Rust client for Mitos: snapshot-fork sandboxes for AI agents.
//!
//! The crate covers both modes, at parity with the Python and TypeScript SDKs:
//!
//! - **Direct mode** ([`SandboxServer`]): the standalone `cmd/sandbox-server`
//!   and the hosted control plane at `https://mitos.run`. Create a template,
//!   fork a sandbox, run `exec`, and terminate. Mirrors the Python
//!   `SandboxServer` (`sdk/python/mitos/direct.py`), the TypeScript
//!   `SandboxServer`, and the Ruby SDK.
//! - **Cluster mode** ([`AgentRun`]): the Kubernetes operator path, driving the
//!   declarative CRDs (`SandboxPool`, `Sandbox`, `Workspace`) in the
//!   `mitos.run/v1` API group. Mirrors the Python `AgentRun`
//!   (`sdk/python/mitos/client.py`).
//!
//! Direct mode keeps a tiny dependency tree (`ureq`, `serde`, `getrandom`).
//! Cluster mode reuses the same `ureq` transport and the rustls it re-exports,
//! trusting the cluster CA; it adds only a small YAML parser for kubeconfig and
//! a PEM reader for the CA and client certificates.
//!
//! # Auth and base URL resolution (direct mode)
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
//!
//! # Cluster mode example
//!
//! ```no_run
//! use mitos::AgentRun;
//!
//! # fn main() -> Result<(), mitos::MitosError> {
//! // From a kubeconfig (None resolves $KUBECONFIG, then $HOME/.kube/config).
//! let client = AgentRun::from_kubeconfig("default", None)?;
//!
//! // One-liner: get-or-create the mitos-default-python pool, then a Sandbox.
//! let sandbox = client.sandbox("python")?;
//! println!("{}", sandbox.name);
//! # Ok(())
//! # }
//! ```

#![forbid(unsafe_code)]
#![warn(missing_docs)]

mod client;
mod cluster;
mod connect;
mod error;
mod k8s;
mod types;

pub use client::{
    valid_sandbox_id, Sandbox, SandboxServer, SandboxServerBuilder, DEFAULT_BASE_URL,
};
pub use cluster::{
    default_pool_name, AgentRun, ClusterSandbox, CreateSandbox, PoolStatus, SandboxPhase,
    Workspace, API_GROUP, API_VERSION,
};
pub use error::MitosError;
pub use types::{ExecResult, ServerSandbox, Template};
