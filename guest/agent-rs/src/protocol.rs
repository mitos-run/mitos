// These types are defined ahead of the handler loop; remove this allow once Task 1.3+ wires them in.
#![allow(dead_code)]

//! Wire-compatible JSON protocol types mirroring internal/vsock/protocol.go.
//!
//! Encoding rules (must match Go's encoding/json behaviour exactly):
//!   - Go []byte fields -> base64 STRING (std alphabet, with padding).
//!   - Go `omitempty`   -> #[serde(skip_serializing_if = "...")] on Option/Vec/map/numeric.
//!   - Go `json:"name"` -> #[serde(rename = "name")].
//!   - Request types: derive Deserialize + #[serde(default)] for permissive parsing.
//!   - Response type: derive Serialize + Default (so ..Default::default() works).

use serde::{Deserialize, Serialize};

// ---------------------------------------------------------------------------
// Hand-written base64 serde module (std alphabet, with '=' padding).
// We do not add the base64 crate; this covers the three []byte fields in scope:
//   ReadFileResponse.content, ExecStreamFrame.data, WriteFileRequest.content.
// ---------------------------------------------------------------------------

/// Decode a standard-alphabet base64 string (with padding). Exposed for tests.
pub fn b64_decode(s: &str) -> Result<Vec<u8>, String> {
    base64_bytes::decode(s)
}

mod base64_bytes {
    use serde::{Deserializer, Serializer};

    const CHARS: &[u8] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";

    pub fn encode(data: &[u8]) -> String {
        let mut out = Vec::with_capacity(data.len().div_ceil(3) * 4);
        let mut chunks = data.chunks_exact(3);
        for chunk in chunks.by_ref() {
            let b0 = chunk[0] as usize;
            let b1 = chunk[1] as usize;
            let b2 = chunk[2] as usize;
            out.push(CHARS[(b0 >> 2) & 0x3f]);
            out.push(CHARS[((b0 << 4) | (b1 >> 4)) & 0x3f]);
            out.push(CHARS[((b1 << 2) | (b2 >> 6)) & 0x3f]);
            out.push(CHARS[b2 & 0x3f]);
        }
        let rem = chunks.remainder();
        match rem.len() {
            1 => {
                let b0 = rem[0] as usize;
                out.push(CHARS[(b0 >> 2) & 0x3f]);
                out.push(CHARS[(b0 << 4) & 0x3f]);
                out.push(b'=');
                out.push(b'=');
            }
            2 => {
                let b0 = rem[0] as usize;
                let b1 = rem[1] as usize;
                out.push(CHARS[(b0 >> 2) & 0x3f]);
                out.push(CHARS[((b0 << 4) | (b1 >> 4)) & 0x3f]);
                out.push(CHARS[(b1 << 2) & 0x3f]);
                out.push(b'=');
            }
            _ => {}
        }
        // SAFETY: all bytes were written from the CHARS table, which is pure ASCII.
        unsafe { String::from_utf8_unchecked(out) }
    }

    fn decode_char(c: u8) -> Option<u8> {
        match c {
            b'A'..=b'Z' => Some(c - b'A'),
            b'a'..=b'z' => Some(c - b'a' + 26),
            b'0'..=b'9' => Some(c - b'0' + 52),
            b'+' => Some(62),
            b'/' => Some(63),
            _ => None,
        }
    }

    pub fn decode(s: &str) -> Result<Vec<u8>, String> {
        let s = s.trim_end_matches('=');
        let mut out = Vec::with_capacity(s.len() * 3 / 4 + 1);
        let bytes = s.as_bytes();
        let mut i = 0;
        while i + 3 < bytes.len() {
            let v0 = decode_char(bytes[i]).ok_or("invalid base64 char")?;
            let v1 = decode_char(bytes[i + 1]).ok_or("invalid base64 char")?;
            let v2 = decode_char(bytes[i + 2]).ok_or("invalid base64 char")?;
            let v3 = decode_char(bytes[i + 3]).ok_or("invalid base64 char")?;
            out.push((v0 << 2) | (v1 >> 4));
            out.push((v1 << 4) | (v2 >> 2));
            out.push((v2 << 6) | v3);
            i += 4;
        }
        let rem = bytes.len() - i;
        if rem == 2 {
            let v0 = decode_char(bytes[i]).ok_or("invalid base64 char")?;
            let v1 = decode_char(bytes[i + 1]).ok_or("invalid base64 char")?;
            out.push((v0 << 2) | (v1 >> 4));
        } else if rem == 3 {
            let v0 = decode_char(bytes[i]).ok_or("invalid base64 char")?;
            let v1 = decode_char(bytes[i + 1]).ok_or("invalid base64 char")?;
            let v2 = decode_char(bytes[i + 2]).ok_or("invalid base64 char")?;
            out.push((v0 << 2) | (v1 >> 4));
            out.push((v1 << 4) | (v2 >> 2));
        }
        Ok(out)
    }

    pub fn serialize<S: Serializer>(data: &[u8], s: S) -> Result<S::Ok, S::Error> {
        s.serialize_str(&encode(data))
    }

    pub fn deserialize<'de, D: Deserializer<'de>>(d: D) -> Result<Vec<u8>, D::Error> {
        let s: &str = serde::de::Deserialize::deserialize(d)?;
        decode(s).map_err(serde::de::Error::custom)
    }
}

// Helper: skip_serializing_if for empty Vec
fn is_empty_vec<T>(v: &[T]) -> bool {
    v.is_empty()
}

// Helper: skip_serializing_if for zero f64. Matches Go encoding/json omitempty,
// which drops both +0.0 and -0.0; comparing the bit pattern avoids clippy::float_cmp.
fn is_zero_f64(v: &f64) -> bool {
    v.to_bits() == 0u64 || v.to_bits() == (-0.0f64).to_bits()
}

// Helper: skip_serializing_if for zero i32/int
fn is_zero_i32(v: &i32) -> bool {
    *v == 0
}

// ---------------------------------------------------------------------------
// Request (host -> guest), deserialize only.
// ---------------------------------------------------------------------------

#[derive(Debug, Default, Deserialize)]
#[serde(default)]
pub struct Request {
    #[serde(rename = "type")]
    pub r#type: String,
    pub exec: Option<ExecRequest>,
    pub read_file: Option<ReadFileRequest>,
    pub write_file: Option<WriteFileRequest>,
    pub list_dir: Option<ListDirRequest>,
    pub mkdir: Option<MkdirRequest>,
    pub remove: Option<RemoveRequest>,
    pub configure: Option<ConfigureRequest>,
    pub notify_forked: Option<NotifyForkedRequest>,
    pub tar_dir: Option<TarDirRequest>,
    pub untar_dir: Option<UntarDirRequest>,
    pub exec_stream: Option<ExecRequest>,
    pub run_code: Option<RunCodeRequest>,
    pub pty: Option<PtyRequest>,
    pub vitals: Option<VitalsRequest>,
    pub tunnel: Option<TunnelRequest>,
}

#[derive(Debug, Default, Deserialize)]
#[serde(default)]
pub struct ExecRequest {
    #[serde(rename = "command")]
    pub command: String,
    #[serde(rename = "working_dir")]
    pub working_dir: String,
    #[serde(rename = "env")]
    pub env: Option<std::collections::HashMap<String, String>>,
    #[serde(rename = "timeout")]
    pub timeout: i32,
}

#[derive(Debug, Default, Deserialize)]
#[serde(default)]
pub struct ReadFileRequest {
    pub path: String,
}

#[derive(Debug, Default, Deserialize)]
#[serde(default)]
pub struct WriteFileRequest {
    pub path: String,
    #[serde(with = "base64_bytes")]
    pub content: Vec<u8>,
    pub mode: u32,
}

#[derive(Debug, Default, Deserialize)]
#[serde(default)]
pub struct ListDirRequest {
    pub path: String,
}

#[derive(Debug, Default, Deserialize)]
#[serde(default)]
pub struct MkdirRequest {
    pub path: String,
}

#[derive(Debug, Default, Deserialize)]
#[serde(default)]
pub struct RemoveRequest {
    pub path: String,
}

#[derive(Debug, Default, Deserialize)]
#[serde(default)]
pub struct ConfigureRequest {
    pub env: Option<std::collections::HashMap<String, String>>,
    pub secrets: Option<std::collections::HashMap<String, String>>,
}

#[derive(Debug, Default, Deserialize)]
#[serde(default)]
pub struct NotifyForkedRequest {
    pub generation: u64,
    pub host_wall_clock_nanos: i64,
    #[serde(with = "base64_bytes")]
    pub entropy: Vec<u8>,
    pub network: Option<NotifyForkedNetwork>,
    pub volumes: Vec<VolumeMountEntry>,
}

#[derive(Debug, Default, Deserialize)]
#[serde(default)]
pub struct NotifyForkedNetwork {
    pub guest_ip: String,
    pub gateway_ip: String,
    pub prefix_len: i32,
    pub guest_mac: String,
    pub resolver_ip: String,
}

#[derive(Debug, Default, Deserialize)]
#[serde(default)]
pub struct VolumeMountEntry {
    pub device: String,
    pub mount_path: String,
    pub read_only: bool,
}

#[derive(Debug, Default, Deserialize)]
#[serde(default)]
pub struct TarDirRequest {
    pub path: String,
}

#[derive(Debug, Default, Deserialize)]
#[serde(default)]
pub struct UntarDirRequest {
    pub path: String,
    #[serde(with = "base64_bytes")]
    pub tar: Vec<u8>,
}

#[derive(Debug, Default, Deserialize)]
#[serde(default)]
pub struct RunCodeRequest {
    pub code: String,
    pub language: String,
    pub timeout: i32,
}

#[derive(Debug, Default, Deserialize)]
#[serde(default)]
pub struct PtyRequest {
    pub command: String,
    pub working_dir: String,
    pub env: Option<std::collections::HashMap<String, String>>,
    pub cols: i32,
    pub rows: i32,
}

#[derive(Debug, Default, Deserialize)]
#[serde(default)]
pub struct VitalsRequest {}

#[derive(Debug, Default, Deserialize)]
#[serde(default)]
pub struct TunnelRequest {
    pub port: i32,
}

// ---------------------------------------------------------------------------
// Response (guest -> host), serialize only.
// ---------------------------------------------------------------------------

#[derive(Debug, Default, Serialize)]
pub struct Response {
    #[serde(rename = "ok")]
    pub ok: bool,
    #[serde(rename = "error", skip_serializing_if = "String::is_empty")]
    pub error: String,
    #[serde(rename = "exec", skip_serializing_if = "Option::is_none")]
    pub exec: Option<ExecResponse>,
    #[serde(rename = "read_file", skip_serializing_if = "Option::is_none")]
    pub read_file: Option<ReadFileResponse>,
    #[serde(rename = "list_dir", skip_serializing_if = "Option::is_none")]
    pub list_dir: Option<ListDirResponse>,
    #[serde(rename = "ping", skip_serializing_if = "Option::is_none")]
    pub ping: Option<PingResponse>,
    #[serde(rename = "notify_forked", skip_serializing_if = "Option::is_none")]
    pub notify_forked: Option<NotifyForkedResponse>,
    #[serde(rename = "tar_dir", skip_serializing_if = "Option::is_none")]
    pub tar_dir: Option<TarDirResponse>,
    #[serde(rename = "vitals", skip_serializing_if = "Option::is_none")]
    pub vitals: Option<VitalsResponse>,
}

#[derive(Debug, Default, Serialize)]
pub struct ExecResponse {
    #[serde(rename = "exit_code")]
    pub exit_code: i32,
    #[serde(rename = "stdout")]
    pub stdout: String,
    #[serde(rename = "stderr")]
    pub stderr: String,
    #[serde(rename = "exec_time_ms")]
    pub exec_time_ms: f64,
}

#[derive(Debug, Default, Serialize)]
pub struct ReadFileResponse {
    #[serde(rename = "content", with = "base64_bytes")]
    pub content: Vec<u8>,
    #[serde(rename = "size")]
    pub size: i64,
}

#[derive(Debug, Default, Serialize)]
pub struct ListDirResponse {
    #[serde(rename = "entries")]
    pub entries: Vec<FileEntry>,
}

#[derive(Debug, Default, Serialize)]
pub struct FileEntry {
    #[serde(rename = "name")]
    pub name: String,
    #[serde(rename = "is_dir")]
    pub is_dir: bool,
    #[serde(rename = "size")]
    pub size: i64,
    #[serde(rename = "mode")]
    pub mode: u32,
    #[serde(rename = "modified_at")]
    pub modified_at: i64,
}

#[derive(Debug, Default, Serialize)]
pub struct PingResponse {
    #[serde(rename = "uptime_seconds")]
    pub uptime_seconds: f64,
}

#[derive(Debug, Default, Serialize)]
pub struct NotifyForkedResponse {
    // Go's NotifyForkedResponse has no omitempty on any field, so all three always serialize.
    #[serde(rename = "applied_clock_step_nanos")]
    pub applied_clock_step_nanos: i64,
    #[serde(rename = "reseeded_rng")]
    pub reseeded_rng: bool,
    #[serde(rename = "signaled_processes")]
    pub signaled_processes: i32,
}

#[derive(Debug, Default, Serialize)]
pub struct TarDirResponse {
    #[serde(rename = "tar", with = "base64_bytes")]
    pub tar: Vec<u8>,
}

#[derive(Debug, Default, Serialize)]
pub struct VitalsResponse {
    #[serde(rename = "steal_fraction")]
    pub steal_fraction: f64,
    #[serde(rename = "sample_window_ms")]
    pub sample_window_ms: f64,
    #[serde(rename = "mem_total_kb")]
    pub mem_total_kb: u64,
    #[serde(rename = "mem_available_kb")]
    pub mem_available_kb: u64,
    #[serde(rename = "mem_used_kb")]
    pub mem_used_kb: u64,
    #[serde(rename = "balloon_reclaimed_kb")]
    pub balloon_reclaimed_kb: u64,
    #[serde(rename = "processes", skip_serializing_if = "is_empty_vec")]
    pub processes: Vec<ProcessEntry>,
}

#[derive(Debug, Default, Serialize)]
pub struct ProcessEntry {
    #[serde(rename = "pid")]
    pub pid: i32,
    #[serde(rename = "comm")]
    pub comm: String,
    #[serde(rename = "state")]
    pub state: String,
    #[serde(rename = "cpu_jiffies")]
    pub cpu_jiffies: u64,
    #[serde(rename = "rss_kb")]
    pub rss_kb: u64,
}

// ---------------------------------------------------------------------------
// ExecStreamFrame: serialized as newline-delimited JSON on streaming connections.
// ---------------------------------------------------------------------------

#[derive(Debug, Default, Serialize)]
pub struct ExecStreamFrame {
    #[serde(rename = "kind")]
    pub kind: String,
    #[serde(rename = "stream", skip_serializing_if = "String::is_empty")]
    pub stream: String,
    #[serde(rename = "data", with = "base64_bytes", skip_serializing_if = "is_empty_vec")]
    pub data: Vec<u8>,
    #[serde(rename = "exit_code", skip_serializing_if = "is_zero_i32")]
    pub exit_code: i32,
    #[serde(rename = "error", skip_serializing_if = "String::is_empty")]
    pub error: String,
    #[serde(rename = "exec_time_ms", skip_serializing_if = "is_zero_f64")]
    pub exec_time_ms: f64,
    #[serde(rename = "result", skip_serializing_if = "Option::is_none")]
    pub result: Option<ResultFrame>,
    #[serde(rename = "error_info", skip_serializing_if = "Option::is_none")]
    pub error_info: Option<ErrorFrame>,
}

#[derive(Debug, Default, Serialize)]
pub struct ResultFrame {
    #[serde(rename = "text", skip_serializing_if = "String::is_empty")]
    pub text: String,
    #[serde(rename = "data", skip_serializing_if = "Option::is_none")]
    pub data: Option<std::collections::HashMap<String, String>>,
}

#[derive(Debug, Default, Serialize)]
pub struct ErrorFrame {
    #[serde(rename = "name")]
    pub name: String,
    #[serde(rename = "value")]
    pub value: String,
    #[serde(rename = "traceback", skip_serializing_if = "is_empty_vec")]
    pub traceback: Vec<String>,
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ping_response_matches_go_json() {
        let r = Response { ok: true, ping: Some(PingResponse { uptime_seconds: 1.5 }), ..Default::default() };
        let s = serde_json::to_string(&r).unwrap();
        // Go omitempty drops zero/None fields; ok and ping must be present.
        assert!(s.contains("\"ok\":true"));
        assert!(s.contains("\"uptime_seconds\":1.5"));
        assert!(!s.contains("\"error\""));
    }

    #[test]
    fn read_file_content_is_base64() {
        let r = Response { ok: true, read_file: Some(ReadFileResponse { content: b"hi".to_vec(), size: 2 }), ..Default::default() };
        let s = serde_json::to_string(&r).unwrap();
        assert!(s.contains("\"content\":\"aGk=\"")); // base64("hi")
        assert!(s.contains("\"size\":2"));
    }

    #[test]
    fn exec_request_parses_from_go_json() {
        let line = r#"{"type":"exec","exec":{"command":"echo hi","working_dir":"/workspace","timeout":30}}"#;
        let req: Request = serde_json::from_str(line).unwrap();
        assert_eq!(req.r#type, "exec");
        assert_eq!(req.exec.unwrap().command, "echo hi");
    }
}
