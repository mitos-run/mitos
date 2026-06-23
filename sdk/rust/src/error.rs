//! The structured error type for the Mitos SDK.
//!
//! Every non-2xx response from the sandbox-server is turned into a
//! [`MitosError`] that carries the server envelope
//! `{error:{code, message, cause, remediation}}`. Callers branch on
//! [`MitosError::code`], never on the human message text. The bearer token is
//! never placed in any error field: any echo of it in a response body is
//! redacted before it becomes the `cause`. Mirrors the Python `AgentRunError`,
//! the TypeScript `AgentRunError`, and the Ruby `MitosError`.

use std::fmt;

/// A structured, LLM-legible error from the mitos sandbox API.
///
/// `code` is the stable, machine-branchable identifier (for example
/// `not_found`, `invalid_sandbox_id`, `unauthorized`). `status` is the HTTP
/// status when the error came from a response (0 for a client-side error such
/// as an invalid id or a transport failure). `cause` and `remediation` give an
/// actionable, human- and model-readable explanation.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct MitosError {
    /// The stable, machine-branchable error code.
    pub code: String,
    /// A short human-readable summary.
    pub message: String,
    /// The underlying cause, with the bearer token redacted.
    pub cause: String,
    /// An actionable remediation hint.
    pub remediation: String,
    /// The HTTP status, or 0 for a client-side error.
    pub status: u16,
}

impl MitosError {
    /// Builds a client-side error (no HTTP round trip happened), for example an
    /// invalid sandbox id or a transport failure. `status` is 0.
    pub(crate) fn client(
        code: &str,
        message: impl Into<String>,
        cause: impl Into<String>,
        remediation: impl Into<String>,
    ) -> Self {
        MitosError {
            code: code.to_string(),
            message: message.into(),
            cause: cause.into(),
            remediation: remediation.into(),
            status: 0,
        }
    }

    /// Builds a [`MitosError`] from a non-2xx response body. Prefers the
    /// structured server envelope `{error:{code, message, cause, remediation}}`
    /// and falls back to status-derived defaults for an older or non-Mitos
    /// server. Any occurrence of `token` in the body is redacted before it
    /// becomes the `cause`, so the bearer value never reaches an error field.
    pub(crate) fn from_response(status: u16, raw_body: &str, token: Option<&str>) -> Self {
        let body = redact(raw_body, token);

        let mut code = status_code(status).to_string();
        let mut message = format!("sandbox API request failed: HTTP {status} ({code})");
        let mut cause = {
            let trimmed = body.trim();
            if trimmed.is_empty() {
                format!("HTTP {status}")
            } else {
                trimmed.to_string()
            }
        };
        let mut remediation = status_remediation(status).to_string();

        if let Ok(parsed) = serde_json::from_str::<serde_json::Value>(&body) {
            if let Some(err) = parsed.get("error") {
                if let Some(obj) = err.as_object() {
                    // New structured envelope.
                    if let Some(v) = nonempty(obj.get("code")) {
                        code = v;
                    }
                    if let Some(v) = nonempty(obj.get("message")) {
                        message = v;
                    }
                    if let Some(v) = nonempty(obj.get("cause")) {
                        cause = redact(&v, token);
                    }
                    if let Some(v) = nonempty(obj.get("remediation")) {
                        remediation = v;
                    }
                } else if let Some(s) = err.as_str() {
                    // Legacy bare {"error": "msg"} shape.
                    if !s.is_empty() {
                        cause = redact(s, token);
                    }
                }
            }
        }

        MitosError {
            code,
            message,
            cause,
            remediation,
            status,
        }
    }
}

impl fmt::Display for MitosError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "{} (code={}, status={}): {} [remediation: {}]",
            self.message, self.code, self.status, self.cause, self.remediation
        )
    }
}

impl std::error::Error for MitosError {}

/// Returns the JSON string value if it is present and non-empty.
fn nonempty(value: Option<&serde_json::Value>) -> Option<String> {
    match value.and_then(|v| v.as_str()) {
        Some(s) if !s.is_empty() => Some(s.to_string()),
        _ => None,
    }
}

/// Replaces every occurrence of a non-empty token with `[REDACTED]`. Mirrors
/// the redaction in the other SDKs so the bearer value never reaches a log or
/// an error message.
fn redact(text: &str, token: Option<&str>) -> String {
    match token {
        Some(t) if !t.is_empty() => text.replace(t, "[REDACTED]"),
        _ => text.to_string(),
    }
}

/// The default machine code for an HTTP status when the body is not the
/// structured server envelope (an older server, a proxy 502, a transport layer).
fn status_code(status: u16) -> &'static str {
    match status {
        400 => "bad_request",
        401 => "unauthorized",
        403 => "forbidden",
        404 => "not_found",
        409 => "conflict",
        413 => "request_too_large",
        429 => "rate_limited",
        500 => "internal_error",
        503 => "unavailable",
        s if s >= 500 => "server_error",
        _ => "request_failed",
    }
}

/// The default remediation hint for an HTTP status when the body carries none.
fn status_remediation(status: u16) -> &'static str {
    match status {
        401 | 403 => "Check the API key is set and authorizes this request.",
        404 => "Confirm the sandbox id exists and is Ready before calling.",
        413 => "Reduce the request payload size.",
        429 => "Back off and retry the request after a short delay.",
        s if s >= 500 => "Retry the request; if it persists, inspect the sandbox-server logs.",
        _ => "Inspect the request fields against the sandbox API contract.",
    }
}
