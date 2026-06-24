//! Typed agent errors with tonic Status mapping.
//!
//! Every variant carries an LLM-legible message (issue #28). Secret values are
//! never included in error messages; only keys, counts, and non-secret context
//! are permitted.

use thiserror::Error;
use tonic::{Code, Status};

/// Canonical error type for the Rust guest agent.
///
/// Each variant maps to a specific gRPC status code so callers can
/// programmatically distinguish error categories. The Display message is the
/// LLM-legible remediation string required by issue #28; it must never contain
/// secret values.
#[derive(Debug, Error)]
pub enum AgentError {
    /// The request referred to a path outside the workspace allowlist
    /// (only /workspace and below are accessible).
    #[error("permission denied: {0}")]
    PermissionDenied(String),

    /// The requested resource (file, process, directory) was not found.
    #[error("not found: {0}")]
    NotFound(String),

    /// The request was malformed or carried an invalid argument.
    #[error("invalid argument: {0}")]
    InvalidArgument(String),

    /// The requested operation is not yet implemented in this slice.
    #[error("unimplemented: {0}")]
    Unimplemented(String),

    /// An internal agent error (IO failure, syscall error, etc.).
    /// The message carries the non-secret OS reason only.
    #[error("internal: {0}")]
    Internal(String),

    /// An IO error from std::io. Converted automatically via the From impl,
    /// allowing handlers to use `?` on std::io results without wrapping manually.
    /// The OS error message is included; no secret values are present in IO errors.
    #[error("io: {0}")]
    Io(#[from] std::io::Error),

    /// The downstream service or transport was unavailable.
    #[error("unavailable: {0}")]
    Unavailable(String),

    /// A tar member path was rejected by the traversal guard: absolute path or
    /// "../" escape detected. Maps to PermissionDenied so the caller can
    /// distinguish this from a workspace-allowlist rejection.
    #[error("path denied: {0}")]
    PathDenied(String),

    /// A resource limit was exceeded (e.g. MAX_TAR_BYTES for archive/upload).
    /// Maps to gRPC ResourceExhausted so callers can distinguish this from
    /// argument or internal errors.
    #[error("resource exhausted: {0}")]
    ResourceExhausted(String),
}

impl From<AgentError> for Status {
    fn from(e: AgentError) -> Self {
        match e {
            AgentError::PermissionDenied(msg) => Status::new(Code::PermissionDenied, msg),
            AgentError::NotFound(msg) => Status::new(Code::NotFound, msg),
            AgentError::InvalidArgument(msg) => Status::new(Code::InvalidArgument, msg),
            AgentError::Unimplemented(msg) => Status::new(Code::Unimplemented, msg),
            AgentError::Internal(msg) => Status::new(Code::Internal, msg),
            AgentError::Io(err) => Status::new(Code::Internal, err.to_string()),
            AgentError::Unavailable(msg) => Status::new(Code::Unavailable, msg),
            AgentError::PathDenied(msg) => Status::new(Code::PermissionDenied, msg),
            AgentError::ResourceExhausted(msg) => Status::new(Code::ResourceExhausted, msg),
        }
    }
}

#[cfg(test)]
#[allow(clippy::unwrap_used, clippy::expect_used)]
mod tests {
    use super::*;
    use tonic::Code;

    #[test]
    fn permission_denied_maps_to_grpc_permission_denied() {
        let e = AgentError::PermissionDenied("path outside workspace".to_string());
        let s: Status = e.into();
        assert_eq!(s.code(), Code::PermissionDenied);
        assert!(s.message().contains("path outside workspace"));
    }

    #[test]
    fn not_found_maps_to_grpc_not_found() {
        let e = AgentError::NotFound("file does not exist".to_string());
        let s: Status = e.into();
        assert_eq!(s.code(), Code::NotFound);
        assert!(s.message().contains("file does not exist"));
    }

    #[test]
    fn invalid_argument_maps_to_grpc_invalid_argument() {
        let e = AgentError::InvalidArgument("first message must carry open".to_string());
        let s: Status = e.into();
        assert_eq!(s.code(), Code::InvalidArgument);
        assert!(s.message().contains("first message must carry open"));
    }

    #[test]
    fn unimplemented_maps_to_grpc_unimplemented() {
        let e = AgentError::Unimplemented("argv exec not implemented in this slice".to_string());
        let s: Status = e.into();
        assert_eq!(s.code(), Code::Unimplemented);
        assert!(s.message().contains("not implemented"));
    }

    #[test]
    fn internal_maps_to_grpc_internal() {
        let e = AgentError::Internal("inotify init failed: EMFILE".to_string());
        let s: Status = e.into();
        assert_eq!(s.code(), Code::Internal);
        assert!(s.message().contains("inotify init failed"));
    }

    #[test]
    fn unavailable_maps_to_grpc_unavailable() {
        let e = AgentError::Unavailable("stream send failed: broken pipe".to_string());
        let s: Status = e.into();
        assert_eq!(s.code(), Code::Unavailable);
        assert!(s.message().contains("broken pipe"));
    }

    #[test]
    fn io_error_maps_to_grpc_internal() {
        let io_err = std::io::Error::new(std::io::ErrorKind::BrokenPipe, "broken pipe");
        let e: AgentError = io_err.into();
        let s: Status = e.into();
        assert_eq!(s.code(), Code::Internal);
        assert!(s.message().contains("broken pipe"));
    }

    #[test]
    fn io_error_question_mark_conversion() {
        // Confirm that std::io::Error converts to AgentError via From,
        // enabling ? in functions returning Result<_, AgentError>.
        fn returns_agent_error() -> Result<(), AgentError> {
            let io_err = std::io::Error::new(std::io::ErrorKind::NotFound, "no such file");
            Err(io_err)?;
            Ok(())
        }
        let result = returns_agent_error();
        assert!(result.is_err());
        let s: Status = result.unwrap_err().into();
        assert_eq!(s.code(), Code::Internal);
    }

    #[test]
    fn display_does_not_include_secret_values() {
        // Verify that the Display representation never leaks secret values.
        // Constructors take the message string, so callers are responsible for
        // not passing secrets; this test documents the contract by asserting on
        // a message that contains only non-secret context.
        let e = AgentError::PermissionDenied("path /etc/passwd is outside /workspace".to_string());
        let display = format!("{e}");
        // The display must carry the variant prefix and the non-secret context.
        assert!(display.starts_with("permission denied:"));
        assert!(display.contains("/workspace"));
    }
}
