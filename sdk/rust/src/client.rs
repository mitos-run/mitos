//! The direct-mode client for the standalone and hosted sandbox-server.
//!
//! [`SandboxServer`] is the entry point: it resolves the base URL and the
//! optional bearer token, then exposes `create_template`, `list_templates`,
//! `fork`, and `list_sandboxes`. `fork` returns a [`Sandbox`] bound to the same
//! server so `exec` round-trips through the server URL and `terminate` issues
//! `DELETE /v1/sandboxes/{id}`. Mirrors the Python `SandboxServer`
//! (`sdk/python/mitos/direct.py`), the TypeScript `SandboxServer`
//! (`sdk/typescript/src/server.ts`), and the Ruby `SandboxServer`.

use std::time::Duration;

use serde_json::json;

use crate::connect::ConnectClient;
use crate::error::MitosError;
use crate::types::{ExecResult, ForkWire, ServerSandbox, Template};

/// The hosted production control plane. Used when neither the builder argument
/// nor `MITOS_BASE_URL` is set, so the examples work without a base URL.
/// Self-hosted or local standalone users opt out by setting `MITOS_BASE_URL`
/// (for example `http://localhost:8080`). Mirrors the other SDKs' default.
pub const DEFAULT_BASE_URL: &str = "https://mitos.run";

const ENV_API_KEY: &str = "MITOS_API_KEY";
const ENV_BASE_URL: &str = "MITOS_BASE_URL";
const ENV_CONFIG_DIR: &str = "MITOS_CONFIG_DIR";

/// The sandbox id allowlist: start with an alphanumeric, then up to 63
/// alphanumeric, underscore, or hyphen characters. Mirrors `daemon/validate.go`,
/// the Python SDK, the TypeScript `validSandboxId`, and the Ruby
/// `SANDBOX_ID_RE`. Implemented without the `regex` crate to keep the dependency
/// tree minimal.
pub fn valid_sandbox_id(id: &str) -> bool {
    let bytes = id.as_bytes();
    if bytes.is_empty() || bytes.len() > 64 {
        return false;
    }
    let first = bytes[0];
    if !first.is_ascii_alphanumeric() {
        return false;
    }
    bytes[1..]
        .iter()
        .all(|&b| b.is_ascii_alphanumeric() || b == b'_' || b == b'-')
}

/// Encodes bytes as lowercase hex without pulling in a hex crate.
fn to_hex(bytes: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut out = String::with_capacity(bytes.len() * 2);
    for &b in bytes {
        out.push(HEX[(b >> 4) as usize] as char);
        out.push(HEX[(b & 0x0f) as usize] as char);
    }
    out
}

/// Returns `n` cryptographically strong random bytes. Falls back to a
/// time-derived seed only if the OS RNG is unavailable, which keeps id and key
/// generation infallible at the call site; these values are de-duplication
/// tokens, not secrets.
fn random_bytes(n: usize) -> Vec<u8> {
    let mut buf = vec![0u8; n];
    if getrandom::getrandom(&mut buf).is_err() {
        let nanos = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_nanos())
            .unwrap_or(0);
        let seed = nanos.to_le_bytes();
        for (i, b) in buf.iter_mut().enumerate() {
            *b = seed[i % seed.len()];
        }
    }
    buf
}

/// A fresh client-side idempotency key (16 random bytes as hex), so a retried
/// creating call (template build or fork) is de-duplicated by the server rather
/// than creating a second resource (issue #22). Parity with the other SDKs,
/// which send one on every creating call.
fn new_idempotency_key() -> String {
    to_hex(&random_bytes(16))
}

/// A generated `sandbox-<hex>` id (4 random bytes), matching the
/// `sandbox-<hex>` convention of the other SDKs.
fn random_sandbox_id() -> String {
    format!("sandbox-{}", to_hex(&random_bytes(4)))
}

/// Reads the bearer token from the CLI login credential file, honoring
/// `MITOS_CONFIG_DIR` and otherwise `$HOME/.config/mitos/credentials.json`. A
/// missing, unreadable, or non-JSON file is never an error: it returns `None`.
/// Only the `"token"` field is read. Mirrors `internal/agentcli` credfile.
fn token_from_credfile() -> Option<String> {
    let path = if let Ok(dir) = std::env::var(ENV_CONFIG_DIR) {
        if dir.is_empty() {
            return None;
        }
        std::path::PathBuf::from(dir).join("credentials.json")
    } else {
        let home = std::env::var("HOME").ok().filter(|h| !h.is_empty())?;
        std::path::PathBuf::from(home)
            .join(".config")
            .join("mitos")
            .join("credentials.json")
    };
    let body = std::fs::read_to_string(&path).ok()?;
    let parsed: serde_json::Value = serde_json::from_str(&body).ok()?;
    match parsed.get("token").and_then(|v| v.as_str()) {
        Some(t) if !t.is_empty() => Some(t.to_string()),
        _ => None,
    }
}

/// Resolves the base URL: explicit argument, then `MITOS_BASE_URL`, then the
/// hosted production endpoint. The trailing slash is trimmed.
fn resolve_base_url(arg: Option<&str>) -> String {
    let chosen = match arg {
        Some(u) if !u.is_empty() => u.to_string(),
        _ => match std::env::var(ENV_BASE_URL) {
            Ok(u) if !u.is_empty() => u,
            _ => DEFAULT_BASE_URL.to_string(),
        },
    };
    chosen.trim_end_matches('/').to_string()
}

/// Resolves the bearer token: explicit argument, then `MITOS_API_KEY`, then the
/// CLI login credential file, then none (tokenless). A missing or unreadable
/// credential file is never an error.
fn resolve_token(arg: Option<&str>) -> Option<String> {
    if let Some(t) = arg {
        if !t.is_empty() {
            return Some(t.to_string());
        }
    }
    if let Ok(t) = std::env::var(ENV_API_KEY) {
        if !t.is_empty() {
            return Some(t);
        }
    }
    token_from_credfile()
}

/// Builder for a [`SandboxServer`]. Set the base URL and API key explicitly, or
/// leave them to the environment / credential-file resolution.
#[derive(Default)]
pub struct SandboxServerBuilder {
    base_url: Option<String>,
    api_key: Option<String>,
    timeout: Option<Duration>,
}

impl SandboxServerBuilder {
    /// Sets the base URL explicitly. Takes precedence over `MITOS_BASE_URL`.
    pub fn base_url(mut self, url: impl Into<String>) -> Self {
        self.base_url = Some(url.into());
        self
    }

    /// Sets the API key (bearer token) explicitly. Takes precedence over
    /// `MITOS_API_KEY` and the credential file.
    pub fn api_key(mut self, key: impl Into<String>) -> Self {
        self.api_key = Some(key.into());
        self
    }

    /// Overrides the per-request HTTP timeout (default 60 seconds).
    pub fn timeout(mut self, timeout: Duration) -> Self {
        self.timeout = Some(timeout);
        self
    }

    /// Resolves the base URL and token per the precedence rules and builds the
    /// client.
    pub fn build(self) -> SandboxServer {
        let url = resolve_base_url(self.base_url.as_deref());
        let token = resolve_token(self.api_key.as_deref());
        let timeout = self.timeout.unwrap_or_else(|| Duration::from_secs(60));
        let agent = ureq::AgentBuilder::new().timeout(timeout).build();
        SandboxServer { url, token, agent }
    }
}

/// Client for the standalone and hosted sandbox-server REST API (direct mode,
/// no Kubernetes). `fork` returns a [`Sandbox`] bound to this server.
#[derive(Clone)]
pub struct SandboxServer {
    url: String,
    token: Option<String>,
    agent: ureq::Agent,
}

impl SandboxServer {
    /// Builds a client with the resolved base URL and token (explicit arg, else
    /// environment, else the credential file). Equivalent to
    /// `SandboxServer::builder().build()`.
    pub fn new() -> Self {
        SandboxServerBuilder::default().build()
    }

    /// Returns a [`SandboxServerBuilder`] to override the base URL, API key, or
    /// timeout.
    pub fn builder() -> SandboxServerBuilder {
        SandboxServerBuilder::default()
    }

    /// The resolved base URL.
    pub fn url(&self) -> &str {
        &self.url
    }

    /// Creates (or builds) the template named `id`. Sends a fresh
    /// `Idempotency-Key` so a retried create returns the same template rather
    /// than a duplicate (issue #22).
    pub fn create_template(&self, id: &str) -> Result<Template, MitosError> {
        self.create_template_opts(id, 5, None)
    }

    /// Like [`SandboxServer::create_template`] with an explicit
    /// `init_wait_seconds` and an optional caller-supplied idempotency key.
    pub fn create_template_opts(
        &self,
        id: &str,
        init_wait_seconds: u32,
        idempotency_key: Option<&str>,
    ) -> Result<Template, MitosError> {
        let body = json!({ "id": id, "init_wait_seconds": init_wait_seconds });
        let key = idempotency_key
            .map(str::to_string)
            .unwrap_or_else(new_idempotency_key);
        let value = self.post("/v1/templates", &body, Some(&key))?;
        serde_json::from_value(value).map_err(decode_err)
    }

    /// Lists the templates known to the server (`GET /v1/templates`).
    pub fn list_templates(&self) -> Result<Vec<Template>, MitosError> {
        let value = self.get("/v1/templates")?;
        if value.is_null() {
            return Ok(Vec::new());
        }
        serde_json::from_value(value).map_err(decode_err)
    }

    /// Forks a sandbox from `template`, generating a `sandbox-<hex>` id. Sends a
    /// fresh `Idempotency-Key`. Returns a [`Sandbox`] bound to this server.
    pub fn fork(&self, template: &str) -> Result<Sandbox, MitosError> {
        self.fork_opts(template, None, None)
    }

    /// Forks a sandbox from `template` with an explicit `id`. The id is
    /// validated against the allowlist; an invalid id yields a typed
    /// `invalid_sandbox_id` error BEFORE any request is sent.
    pub fn fork_as(&self, template: &str, id: &str) -> Result<Sandbox, MitosError> {
        self.fork_opts(template, Some(id), None)
    }

    /// Forks a sandbox from `template`. When `id` is `None` a `sandbox-<hex>` id
    /// is generated. The id is validated against the allowlist; an invalid id
    /// yields a typed error before any request. Sends a fresh `Idempotency-Key`
    /// unless one is supplied.
    pub fn fork_opts(
        &self,
        template: &str,
        id: Option<&str>,
        idempotency_key: Option<&str>,
    ) -> Result<Sandbox, MitosError> {
        let sandbox_id = match id {
            Some(i) => i.to_string(),
            None => random_sandbox_id(),
        };
        if !valid_sandbox_id(&sandbox_id) {
            return Err(invalid_sandbox_id_err(
                &sandbox_id,
                "Pass a sandbox id of alphanumerics, underscore, or hyphen, up to 64 chars.",
            ));
        }
        let body = json!({ "template": template, "id": sandbox_id });
        let key = idempotency_key
            .map(str::to_string)
            .unwrap_or_else(new_idempotency_key);
        let value = self.post("/v1/fork", &body, Some(&key))?;
        let wire: ForkWire = serde_json::from_value(value).map_err(decode_err)?;
        let resolved_id = if wire.id.is_empty() {
            sandbox_id
        } else {
            wire.id
        };
        Ok(Sandbox {
            id: resolved_id,
            template_id: wire.template_id,
            endpoint: wire.endpoint,
            fork_time_ms: wire.fork_time_ms,
            server: self.clone(),
        })
    }

    /// Lists the live sandboxes known to the server (`GET /v1/sandboxes`).
    pub fn list_sandboxes(&self) -> Result<Vec<ServerSandbox>, MitosError> {
        let value = self.get("/v1/sandboxes")?;
        if value.is_null() {
            return Ok(Vec::new());
        }
        serde_json::from_value(value).map_err(decode_err)
    }

    /// Issues `DELETE /v1/sandboxes/{id}`. Called by [`Sandbox::terminate`]. The
    /// id is validated against the allowlist before the request.
    fn terminate(&self, id: &str) -> Result<(), MitosError> {
        if !valid_sandbox_id(id) {
            return Err(invalid_sandbox_id_err(
                id,
                "Terminate only ids that match the sandbox id allowlist.",
            ));
        }
        let url = format!("{}/v1/sandboxes/{}", self.url, id);
        let req = self.auth(self.agent.delete(&url));
        self.send(req, None)?;
        Ok(())
    }

    /// Runs `command` in the sandbox over the Connect `sandbox.v1.Sandbox`
    /// runtime protocol (`POST /sandbox.v1.Sandbox/ExecStream`, issue #358/#24).
    /// Used by [`Sandbox::exec`]. The server-streaming response is drained into
    /// an [`ExecResult`] (stdout, stderr, then the exit frame). The sandbox id
    /// rides the `X-Sandbox-Id` header; the optional bearer token rides
    /// `Authorization` and is never logged.
    fn exec(&self, id: &str, command: &str, timeout: u32) -> Result<ExecResult, MitosError> {
        ConnectClient::new(&self.agent, &self.url, self.token.as_deref())
            .exec_stream(id, command, timeout)
    }

    /// Attaches `Authorization: Bearer <token>` when a token is configured. The
    /// standalone server is tokenless and ignores it; the hosted front door
    /// verifies it. The token VALUE is never logged.
    fn auth(&self, req: ureq::Request) -> ureq::Request {
        match &self.token {
            Some(t) if !t.is_empty() => req.set("Authorization", &format!("Bearer {t}")),
            _ => req,
        }
    }

    fn get(&self, path: &str) -> Result<serde_json::Value, MitosError> {
        let url = format!("{}{}", self.url, path);
        let req = self.auth(self.agent.get(&url));
        self.send(req, None)
    }

    fn post(
        &self,
        path: &str,
        body: &serde_json::Value,
        idempotency_key: Option<&str>,
    ) -> Result<serde_json::Value, MitosError> {
        let url = format!("{}{}", self.url, path);
        let mut req = self
            .auth(self.agent.post(&url))
            .set("Content-Type", "application/json");
        if let Some(key) = idempotency_key {
            req = req.set("Idempotency-Key", key);
        }
        self.send(req, Some(body))
    }

    /// Sends the request, returning the parsed JSON body on a 2xx (or
    /// `Value::Null` on an empty body) and a [`MitosError`] otherwise. A non-2xx
    /// status is parsed from the server envelope with the token redacted; a
    /// transport failure becomes a client-side `transport_error`.
    fn send(
        &self,
        req: ureq::Request,
        body: Option<&serde_json::Value>,
    ) -> Result<serde_json::Value, MitosError> {
        let result = match body {
            Some(b) => req.send_json(b.clone()),
            None => req.call(),
        };
        match result {
            Ok(resp) => parse_body(resp),
            Err(ureq::Error::Status(status, resp)) => {
                let text = resp.into_string().unwrap_or_default();
                Err(MitosError::from_response(
                    status,
                    &text,
                    self.token.as_deref(),
                ))
            }
            Err(ureq::Error::Transport(t)) => Err(MitosError::client(
                "transport_error",
                "sandbox API request failed to reach the server",
                redact_transport(&t.to_string(), self.token.as_deref()),
                "Check the base URL is reachable and the server is running.",
            )),
        }
    }
}

impl Default for SandboxServer {
    fn default() -> Self {
        Self::new()
    }
}

/// Reads a 2xx response body into JSON, returning `Value::Null` for an empty
/// body (a bare 200/204 such as DELETE).
fn parse_body(resp: ureq::Response) -> Result<serde_json::Value, MitosError> {
    let text = resp.into_string().map_err(|e| {
        MitosError::client(
            "response_read_error",
            "failed to read the sandbox API response body",
            e.to_string(),
            "Retry the request; if it persists, inspect the sandbox-server logs.",
        )
    })?;
    if text.trim().is_empty() {
        return Ok(serde_json::Value::Null);
    }
    serde_json::from_str(&text).map_err(decode_err)
}

/// Maps a JSON decode failure to a typed client-side error.
fn decode_err(e: serde_json::Error) -> MitosError {
    MitosError::client(
        "decode_error",
        "failed to decode the sandbox API response",
        e.to_string(),
        "The server returned a body that does not match the expected schema; check server and SDK versions.",
    )
}

/// Builds the typed `invalid_sandbox_id` error, never echoing a token.
fn invalid_sandbox_id_err(id: &str, remediation: &str) -> MitosError {
    MitosError::client(
        "invalid_sandbox_id",
        format!("invalid sandbox id: {id:?}"),
        "id must match ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$",
        remediation,
    )
}

/// Redacts the token from a transport error string in the unlikely event the
/// URL or message carries it.
fn redact_transport(text: &str, token: Option<&str>) -> String {
    match token {
        Some(t) if !t.is_empty() => text.replace(t, "[REDACTED]"),
        _ => text.to_string(),
    }
}

/// A running sandbox handle returned by [`SandboxServer::fork`]. `exec`
/// round-trips through the server URL over the Connect runtime protocol
/// (`POST /sandbox.v1.Sandbox/ExecStream`) and `terminate` issues
/// `DELETE /v1/sandboxes/{id}`. Holds the [`SandboxServer`] it was forked from
/// so requests carry the same base URL and optional bearer header.
pub struct Sandbox {
    /// The sandbox id.
    pub id: String,
    /// The template the sandbox was forked from.
    pub template_id: String,
    /// The sandbox API endpoint reported by the server.
    pub endpoint: String,
    /// Wall-clock fork time in milliseconds.
    pub fork_time_ms: f64,
    server: SandboxServer,
}

impl Sandbox {
    /// Runs `command` in the sandbox with the default 30 second timeout.
    /// Requires a Ready sandbox: the server routes exec through the guest agent
    /// over vsock, so a sandbox that is not yet up returns a typed error.
    pub fn exec(&self, command: &str) -> Result<ExecResult, MitosError> {
        self.exec_with_timeout(command, 30)
    }

    /// Runs `command` with an explicit timeout (seconds).
    pub fn exec_with_timeout(&self, command: &str, timeout: u32) -> Result<ExecResult, MitosError> {
        self.server.exec(&self.id, command, timeout)
    }

    /// Terminates the sandbox via `DELETE /v1/sandboxes/{id}`.
    pub fn terminate(&self) -> Result<(), MitosError> {
        self.server.terminate(&self.id)
    }
}

impl std::fmt::Debug for Sandbox {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Sandbox")
            .field("id", &self.id)
            .field("template_id", &self.template_id)
            .field("endpoint", &self.endpoint)
            .field("fork_time_ms", &self.fork_time_ms)
            .finish()
    }
}
