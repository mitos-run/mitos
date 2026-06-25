//! A dependency-free Connect protocol codec over the SDK's existing `ureq`
//! transport.
//!
//! The native direct-mode runtime calls (today: `exec`) speak the Connect
//! `sandbox.v1.Sandbox` service (issue #24/#358) instead of the legacy JSON
//! `/v1/exec` route. Rather than add a generated-stub plus codegen dependency to
//! this thin SDK, this module implements the Connect wire directly over the same
//! blocking `ureq::Agent` `client.rs` already uses for `/v1/*`. The proto-JSON
//! message shapes come straight from `proto/sandbox/v1/sandbox.proto` (camelCase
//! field names, `bytes` fields as base64 strings). It mirrors the Go SDK's
//! `sdk/go/connect.go` and the Python SDK's `sdk/python/mitos/_connect.py`.
//!
//! Only the server-streaming shape is needed here:
//!
//!   - `POST /sandbox.v1.Sandbox/<Method>` with `Content-Type:
//!     application/connect+json`. Each message is an ENVELOPED frame: a 5-byte
//!     prefix (1 flag byte + 4-byte big-endian length) then the JSON payload.
//!     The client sends its single request message as a plain (flag `0x00`)
//!     enveloped frame; the server replies with a stream of frames whose final
//!     frame sets the end-stream flag (`0x02`), carrying trailers and, on
//!     failure, an `{"error":{...}}` object.
//!
//! The direct-mode `exec` only sends the opening message (no live stdin), so the
//! call is half-duplex: the full request body (one enveloped frame) is buffered
//! and sent, then the response frames are read. `exec` output is bounded, so the
//! buffered response body is parsed frame-by-frame after the read completes,
//! reassembling correctly across the byte stream.
//!
//! The bearer token rides on `Authorization` and is never logged; it is redacted
//! from any error cause via the same redactor the rest of the SDK uses.

use std::io::Read;

use crate::error::MitosError;
use crate::types::ExecResult;

/// The Connect service name; the RPC path is `/<service>/<Method>`.
const SERVICE_NAME: &str = "sandbox.v1.Sandbox";
/// The streaming content type. Each message is an enveloped JSON frame.
const STREAM_CONTENT_TYPE: &str = "application/connect+json";
/// The per-sandbox routing header the server keys on (both the tokenless
/// standalone case and the hosted/forkd bearer case).
const SANDBOX_ID_HEADER: &str = "X-Sandbox-Id";

/// The end-stream flag (bit 1) on a Connect enveloped frame. The final server
/// frame sets it; its payload carries trailers and an optional error object.
const FLAG_END_STREAM: u8 = 0b0000_0010;
/// The compressed flag (bit 0). The SDK negotiates identity encoding and never
/// sends or accepts a compressed frame, so this is only used to refuse an
/// unexpected one.
const FLAG_COMPRESSED: u8 = 0b0000_0001;

/// Guards the frame-length prefix so a malformed or hostile length cannot make
/// the SDK allocate unbounded memory. 64 MiB matches the Go SDK's cap.
const MAX_FRAME_BYTES: u32 = 64 << 20;

/// Speaks the Connect `sandbox.v1.Sandbox` protocol over a borrowed
/// `ureq::Agent`. Constructed per call with the server base URL, the per-sandbox
/// id, and the optional bearer token. The token value is never logged.
pub(crate) struct ConnectClient<'a> {
    agent: &'a ureq::Agent,
    base_url: &'a str,
    token: Option<&'a str>,
}

impl<'a> ConnectClient<'a> {
    pub(crate) fn new(agent: &'a ureq::Agent, base_url: &'a str, token: Option<&'a str>) -> Self {
        ConnectClient {
            agent,
            base_url,
            token,
        }
    }

    /// Runs `command` in `sandbox_id` over `ExecStream`, draining the stdout,
    /// stderr, and exit frames into an [`ExecResult`]. `timeout_seconds` is sent
    /// only when greater than zero (proto-JSON omits a zero default).
    pub(crate) fn exec_stream(
        &self,
        sandbox_id: &str,
        command: &str,
        timeout_seconds: u32,
    ) -> Result<ExecResult, MitosError> {
        // Build the proto-JSON ExecStreamRequest by hand so a zero timeout is
        // omitted (proto-JSON drops zero-valued scalars), matching the Go and
        // Python clients.
        let request = if timeout_seconds > 0 {
            serde_json::json!({ "command": command, "timeoutSeconds": timeout_seconds })
        } else {
            serde_json::json!({ "command": command })
        };
        let payload = serde_json::to_vec(&request).map_err(|e| {
            MitosError::client(
                "encode_error",
                "failed to encode the ExecStream request",
                e.to_string(),
                "This is an SDK bug; report it with the command that triggered it.",
            )
        })?;
        let body = encode_frame(&payload, false);

        let url = format!("{}/{}/ExecStream", self.base_url, SERVICE_NAME);
        let mut req = self
            .agent
            .post(&url)
            .set("Content-Type", STREAM_CONTENT_TYPE)
            .set("Connect-Protocol-Version", "1")
            .set(SANDBOX_ID_HEADER, sandbox_id);
        if let Some(t) = self.token {
            if !t.is_empty() {
                req = req.set("Authorization", &format!("Bearer {t}"));
            }
        }

        let resp = match req.send_bytes(&body) {
            Ok(resp) => resp,
            Err(ureq::Error::Status(status, resp)) => {
                // A streaming RPC that fails before the first frame returns a
                // normal HTTP error body (the Connect error envelope), not an
                // end-stream frame. Read it and raise the typed error.
                let text = resp.into_string().unwrap_or_default();
                return Err(self.error_from_body(status, text.as_bytes()));
            }
            Err(ureq::Error::Transport(t)) => {
                return Err(MitosError::client(
                    "transport_error",
                    "sandbox runtime request failed to reach the server",
                    self.redact(&t.to_string()),
                    "Check the base URL is reachable and the server is running.",
                ));
            }
        };

        // The response is a 200 stream of enveloped frames. exec output is
        // bounded, so the whole body is read and parsed frame-by-frame. A frame
        // can straddle any read boundary, which is why the body is buffered fully
        // before parsing.
        let mut raw = Vec::new();
        resp.into_reader()
            .take((MAX_FRAME_BYTES as u64) * 2)
            .read_to_end(&mut raw)
            .map_err(|e| {
                MitosError::client(
                    "response_read_error",
                    "failed to read the ExecStream response body",
                    e.to_string(),
                    "Retry the request; if it persists, inspect the sandbox-server logs.",
                )
            })?;

        self.drain(&raw)
    }

    /// Walks the buffered response frames, accumulating stdout/stderr and the
    /// exit frame into an [`ExecResult`]. A compressed frame is refused; an
    /// end-stream frame with an `error` object becomes a typed error; a clean
    /// end-stream (or a clean transport EOF) ends the drain.
    fn drain(&self, raw: &[u8]) -> Result<ExecResult, MitosError> {
        let mut result = ExecResult {
            exit_code: 0,
            stdout: String::new(),
            stderr: String::new(),
            exec_time_ms: 0.0,
        };
        let mut saw_exit = false;
        let mut offset = 0usize;

        while offset < raw.len() {
            let (flag, payload, next) = match parse_frame(raw, offset)? {
                Some(frame) => frame,
                // A trailing partial frame (a clean transport EOF mid-header)
                // ends the stream; the bounded exec output is already drained.
                None => break,
            };
            offset = next;

            if flag & FLAG_COMPRESSED != 0 {
                return Err(MitosError::client(
                    "internal_error",
                    "sandbox runtime returned a compressed frame the SDK did not negotiate",
                    "unexpected compressed Connect frame",
                    "Report this; the SDK negotiates identity encoding.",
                ));
            }

            if flag & FLAG_END_STREAM != 0 {
                if let Some(err) = self.error_from_end_stream(payload) {
                    return Err(err);
                }
                break;
            }

            if payload.is_empty() {
                continue;
            }
            self.apply_response(payload, &mut result, &mut saw_exit)?;
        }

        Ok(result)
    }

    /// Applies one non-terminal ExecResponse frame. Exactly one of `stdout`,
    /// `stderr`, or `exit` is set per the contract; an unrecognized message is
    /// tolerated (forward compatibility), an undecodable base64 is an error.
    fn apply_response(
        &self,
        payload: &[u8],
        result: &mut ExecResult,
        saw_exit: &mut bool,
    ) -> Result<(), MitosError> {
        let msg: serde_json::Value = serde_json::from_slice(payload).map_err(|e| {
            MitosError::client(
                "decode_error",
                "failed to decode an ExecStream response frame",
                e.to_string(),
                "The server returned a frame that does not match the ExecResponse schema; check server and SDK versions.",
            )
        })?;

        if let Some(b64) = msg.get("stdout").and_then(|v| v.as_str()) {
            result.stdout.push_str(&decode_b64_utf8(b64, "stdout")?);
        } else if let Some(b64) = msg.get("stderr").and_then(|v| v.as_str()) {
            result.stderr.push_str(&decode_b64_utf8(b64, "stderr")?);
        } else if let Some(exit) = msg.get("exit") {
            *saw_exit = true;
            if let Some(code) = exit.get("exitCode").and_then(|v| v.as_i64()) {
                result.exit_code = code as i32;
            }
            if let Some(ms) = exit.get("execTimeMs").and_then(|v| v.as_f64()) {
                result.exec_time_ms = ms;
            }
            // The exit frame may carry a non-fatal "error" string (for example a
            // command that could not be spawned). It is surfaced through stderr
            // so the caller still gets the exit code; the typed-error path is
            // reserved for the Connect end-stream error object.
            if let Some(err) = exit.get("error").and_then(|v| v.as_str()) {
                if !err.is_empty() {
                    if !result.stderr.is_empty() && !result.stderr.ends_with('\n') {
                        result.stderr.push('\n');
                    }
                    result.stderr.push_str(err);
                }
            }
        }
        Ok(())
    }

    /// Inspects the terminal end-stream frame. A payload carrying an
    /// `{"error":{...}}` object yields a typed error; a clean payload (trailers
    /// only, empty, or non-JSON) yields `None`.
    fn error_from_end_stream(&self, payload: &[u8]) -> Option<MitosError> {
        if payload.is_empty() {
            return None;
        }
        let end: serde_json::Value = serde_json::from_slice(payload).ok()?;
        let err = end.get("error")?;
        let code = err.get("code").and_then(|v| v.as_str()).unwrap_or("");
        let message = err.get("message").and_then(|v| v.as_str()).unwrap_or("");
        Some(self.connect_error(code, message, connect_code_status(code)))
    }

    /// Turns a non-2xx Connect response into a typed error. Prefers the Connect
    /// error envelope `{"code","message"}`; falls back to the raw redacted body
    /// and the HTTP status when the body is not the envelope (a proxy 502, a
    /// transport error).
    fn error_from_body(&self, status: u16, body: &[u8]) -> MitosError {
        if let Ok(env) = serde_json::from_slice::<serde_json::Value>(body) {
            let code = env.get("code").and_then(|v| v.as_str()).unwrap_or("");
            if !code.is_empty() {
                let message = env.get("message").and_then(|v| v.as_str()).unwrap_or("");
                let mut e = self.connect_error(code, message, status);
                e.status = status;
                return e;
            }
        }
        let text = String::from_utf8_lossy(body);
        MitosError::client_with_status(
            status_code(status),
            format!("sandbox runtime request failed: HTTP {status}"),
            self.redact(text.trim()),
            status_remediation(status),
            status,
        )
    }

    /// Builds a typed error from a Connect error code and message. The Connect
    /// textual code is the stable `code`; `status` is mapped so the typed layer
    /// picks the right remediation, and the message is redacted of any token.
    fn connect_error(&self, code: &str, message: &str, status: u16) -> MitosError {
        let stable = if code.is_empty() { "internal" } else { code };
        let cause = {
            let redacted = self.redact(message);
            if redacted.is_empty() {
                format!("connect error {stable}")
            } else {
                redacted
            }
        };
        MitosError::client_with_status(
            stable,
            format!("sandbox RPC failed: {stable}"),
            cause,
            "Inspect the request against the sandbox.v1.Sandbox contract.",
            status,
        )
    }

    /// Redacts the bearer token from a string in the unlikely event a body or
    /// transport message carries it.
    fn redact(&self, text: &str) -> String {
        match self.token {
            Some(t) if !t.is_empty() => text.replace(t, "[REDACTED]"),
            _ => text.to_string(),
        }
    }
}

/// Wraps one message payload in the Connect 5-byte envelope prefix (1 flag byte
/// + 4-byte big-endian length + payload).
fn encode_frame(payload: &[u8], end_stream: bool) -> Vec<u8> {
    let flag = if end_stream { FLAG_END_STREAM } else { 0 };
    let len = payload.len() as u32;
    let mut out = Vec::with_capacity(5 + payload.len());
    out.push(flag);
    out.extend_from_slice(&len.to_be_bytes());
    out.extend_from_slice(payload);
    out
}

/// One parsed enveloped frame: its flag byte, its payload slice, and the offset
/// just past it in the buffer.
type ParsedFrame<'a> = (u8, &'a [u8], usize);

/// Parses one enveloped frame starting at `offset` in `buf`. Returns
/// `Some((flag, payload, next_offset))` for a complete frame, or `None` when the
/// remaining bytes are a partial frame (a clean truncation at the end of the
/// buffered body). Refuses a compressed flag-length only at the caller; here it
/// guards the declared length against the size cap and against a short payload.
fn parse_frame(buf: &[u8], offset: usize) -> Result<Option<ParsedFrame<'_>>, MitosError> {
    // Need at least the 5-byte header.
    if offset + 5 > buf.len() {
        return Ok(None);
    }
    let flag = buf[offset];
    let len = u32::from_be_bytes([
        buf[offset + 1],
        buf[offset + 2],
        buf[offset + 3],
        buf[offset + 4],
    ]);
    if len > MAX_FRAME_BYTES {
        return Err(MitosError::client(
            "internal_error",
            "sandbox runtime returned an oversized response frame",
            format!("connect response frame too large ({len} bytes)"),
            "Report this; the server should not emit frames over the SDK cap.",
        ));
    }
    let start = offset + 5;
    let end = start + len as usize;
    if end > buf.len() {
        // A truncated payload: treat as a clean end of the buffered stream.
        return Ok(None);
    }
    Ok(Some((flag, &buf[start..end], end)))
}

/// Decodes standard base64 (RFC 4648, with optional `=` padding) into a UTF-8
/// String. exec stdout/stderr are text; invalid base64 or invalid UTF-8 is a
/// typed decode error rather than a panic. Implemented inline to avoid adding a
/// base64 crate.
fn decode_b64_utf8(input: &str, field: &str) -> Result<String, MitosError> {
    let bytes = decode_b64(input).ok_or_else(|| {
        MitosError::client(
            "decode_error",
            format!("failed to base64-decode the ExecStream {field}"),
            "the server returned a non-base64 value for a bytes field",
            "Check the server and SDK versions; the runtime contract sends base64 bytes.",
        )
    })?;
    String::from_utf8(bytes).map_err(|e| {
        MitosError::client(
            "decode_error",
            format!("the ExecStream {field} was not valid UTF-8"),
            e.to_string(),
            "The captured output was not UTF-8; this SDK surfaces exec output as text.",
        )
    })
}

/// Decodes standard base64 (alphabet `A-Za-z0-9+/`, `=` padding). Returns `None`
/// on any invalid character or malformed length. Whitespace is ignored so a
/// pretty-printed value still decodes.
fn decode_b64(input: &str) -> Option<Vec<u8>> {
    fn val(c: u8) -> Option<u8> {
        match c {
            b'A'..=b'Z' => Some(c - b'A'),
            b'a'..=b'z' => Some(c - b'a' + 26),
            b'0'..=b'9' => Some(c - b'0' + 52),
            b'+' => Some(62),
            b'/' => Some(63),
            _ => None,
        }
    }

    // Collect the significant (non-whitespace, non-padding) sextets.
    let mut sextets: Vec<u8> = Vec::with_capacity(input.len());
    let mut padding = 0usize;
    let mut seen_padding = false;
    for &c in input.as_bytes() {
        match c {
            b'\r' | b'\n' | b' ' | b'\t' => continue,
            b'=' => {
                seen_padding = true;
                padding += 1;
                if padding > 2 {
                    return None;
                }
            }
            _ => {
                // A data character after padding is malformed.
                if seen_padding {
                    return None;
                }
                sextets.push(val(c)?);
            }
        }
    }

    // 4 sextets -> 3 bytes. The remainder (after removing padding) must be 0, 2,
    // or 3 sextets; a remainder of 1 is impossible in valid base64.
    let mut out = Vec::with_capacity(sextets.len() / 4 * 3 + 2);
    let mut chunks = sextets.chunks_exact(4);
    for chunk in &mut chunks {
        let n = (u32::from(chunk[0]) << 18)
            | (u32::from(chunk[1]) << 12)
            | (u32::from(chunk[2]) << 6)
            | u32::from(chunk[3]);
        out.push((n >> 16) as u8);
        out.push((n >> 8) as u8);
        out.push(n as u8);
    }
    let rem = chunks.remainder();
    match rem.len() {
        0 => {}
        2 => {
            let n = (u32::from(rem[0]) << 18) | (u32::from(rem[1]) << 12);
            out.push((n >> 16) as u8);
        }
        3 => {
            let n =
                (u32::from(rem[0]) << 18) | (u32::from(rem[1]) << 12) | (u32::from(rem[2]) << 6);
            out.push((n >> 16) as u8);
            out.push((n >> 8) as u8);
        }
        _ => return None,
    }
    Some(out)
}

/// Maps a Connect textual error code to the HTTP-ish status the typed-error
/// layer keys remediation on. An unmapped code falls back to 500. Mirrors the Go
/// and Python maps.
fn connect_code_status(code: &str) -> u16 {
    match code {
        "canceled" => 499,
        "unknown" => 500,
        "invalid_argument" => 400,
        "deadline_exceeded" => 504,
        "not_found" => 404,
        "already_exists" => 409,
        "permission_denied" => 403,
        "resource_exhausted" => 429,
        "failed_precondition" => 400,
        "aborted" => 409,
        "out_of_range" => 400,
        "unimplemented" => 501,
        "internal" => 500,
        "unavailable" => 503,
        "data_loss" => 500,
        "unauthenticated" => 401,
        _ => 500,
    }
}

/// The default machine code for an HTTP status when a streaming open fails with
/// a body that is not the Connect error envelope. Mirrors `error.rs`.
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

/// The default remediation hint for an HTTP status. Mirrors `error.rs`.
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn base64_round_trip_known_vectors() {
        assert_eq!(decode_b64("").unwrap(), b"");
        assert_eq!(decode_b64("aGVsbG8=").unwrap(), b"hello");
        assert_eq!(decode_b64("aGVsbG8gd29ybGQ=").unwrap(), b"hello world");
        assert_eq!(decode_b64("Zm9v").unwrap(), b"foo");
        assert_eq!(decode_b64("Zm8=").unwrap(), b"fo");
        assert_eq!(decode_b64("Zg==").unwrap(), b"f");
        // Whitespace is ignored.
        assert_eq!(decode_b64("aGVs\nbG8=").unwrap(), b"hello");
    }

    #[test]
    fn base64_rejects_invalid() {
        assert!(decode_b64("a").is_none()); // length-1 remainder is impossible
        assert!(decode_b64("****").is_none());
        assert!(decode_b64("ab=c").is_none()); // data after padding
    }

    #[test]
    fn frame_round_trips() {
        let payload = br#"{"command":"echo hi"}"#;
        let framed = encode_frame(payload, false);
        let (flag, body, next) = parse_frame(&framed, 0).unwrap().unwrap();
        assert_eq!(flag, 0);
        assert_eq!(body, payload);
        assert_eq!(next, framed.len());
    }

    #[test]
    fn frame_partial_returns_none() {
        let framed = encode_frame(b"abc", false);
        // Truncate mid-payload.
        assert!(parse_frame(&framed[..6], 0).unwrap().is_none());
        // Truncate mid-header.
        assert!(parse_frame(&framed[..3], 0).unwrap().is_none());
    }
}
