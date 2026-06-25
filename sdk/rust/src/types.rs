//! Value types returned by the sandbox-server REST API.
//!
//! The wire types deserialize the snake_case JSON the Go handlers emit
//! (`cmd/sandbox-server/main.go`); the public types re-expose them with stable
//! field names. Mirrors the Template / ServerSandbox / ExecResult value objects
//! in the Python, TypeScript, and Ruby SDKs.

use serde::Deserialize;

/// A template as reported by the sandbox-server (`POST`/`GET /v1/templates`).
#[derive(Debug, Clone, Deserialize, PartialEq)]
pub struct Template {
    /// The template id.
    pub id: String,
    /// Whether the template is built and ready to fork.
    pub ready: bool,
    /// RFC 3339 creation timestamp as returned by the server, when present.
    #[serde(default)]
    pub created_at: String,
    /// Wall-clock build time in milliseconds, when present.
    #[serde(default)]
    pub creation_time_ms: f64,
}

/// A sandbox summary as reported by `GET /v1/sandboxes`.
#[derive(Debug, Clone, Deserialize, PartialEq)]
pub struct ServerSandbox {
    /// The sandbox id.
    pub id: String,
    /// The template the sandbox was forked from.
    #[serde(default)]
    pub template_id: String,
    /// The sandbox API endpoint.
    #[serde(default)]
    pub endpoint: String,
    /// RFC 3339 creation timestamp, when present.
    #[serde(default)]
    pub created_at: String,
    /// Wall-clock fork time in milliseconds, when present.
    #[serde(default)]
    pub fork_time_ms: f64,
}

/// The result of [`crate::Sandbox::exec`]: `{exit_code, stdout, stderr,
/// exec_time_ms}`. The fields are drained from the Connect
/// `sandbox.v1.Sandbox/ExecStream` response: the stdout and stderr frames plus
/// the terminal exit frame.
#[derive(Debug, Clone, Deserialize, PartialEq)]
pub struct ExecResult {
    /// The command exit code.
    pub exit_code: i32,
    /// Captured standard output.
    #[serde(default)]
    pub stdout: String,
    /// Captured standard error.
    #[serde(default)]
    pub stderr: String,
    /// Wall-clock execution time in milliseconds, when present.
    #[serde(default)]
    pub exec_time_ms: f64,
}

impl ExecResult {
    /// Reports whether the command exited 0.
    pub fn success(&self) -> bool {
        self.exit_code == 0
    }
}

/// The `POST /v1/fork` response wire shape.
#[derive(Debug, Clone, Deserialize)]
pub(crate) struct ForkWire {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub template_id: String,
    #[serde(default)]
    pub endpoint: String,
    #[serde(default)]
    pub fork_time_ms: f64,
}
