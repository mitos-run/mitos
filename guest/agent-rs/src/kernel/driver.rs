// KernelManager: owns the single in-guest kernel driver process.
//
// Mirrors guest/agent/kernel.go faithfully:
// - defaultDriverPath = "/opt/mitos/kernel_driver.py"
// - python = "python3" when not overridden
// - Lazy start on first run(); state persists across run() calls.
// - One execution in flight at a time (caller holds the Mutex<KernelManager>).
// - KernelUnavailable emitted as an error frame (exit 127) when the driver is
//   absent, ipykernel is not installed, or the kernel dies unexpectedly.
// - No code text or output bytes are ever logged (only event kinds/counts at
//   debug level).
// - No unsafe code: subprocess via tokio::process.

use std::process::Stdio;
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::process::{Child, ChildStdin, ChildStdout};
// Note: AsyncBufReadExt::read_until is used for bounded line reading.

use crate::sandbox_v1::{run_code_response, RunCodeResponse, RunError, RunResult};
use tokio::sync::mpsc;

/// Default path where guest/rootfs/build.sh installs kernel_driver.py.
const DEFAULT_DRIVER_PATH: &str = "/opt/mitos/kernel_driver.py";

/// Default Python interpreter name.
const DEFAULT_PYTHON: &str = "python3";

/// Fixed execution ID: mirrors Go which always uses "e".
const EXEC_ID: &str = "e";

/// BufReader capacity for the driver stdout (1 MiB, mirrors Go's scanner buffer
/// initial size for large rich-output payloads).
const LINE_BUF_BYTES: usize = 1024 * 1024;

/// Hard cap on a single driver output line. Matches Go's MaxMessageBytes (96 MiB
/// = 96<<20). A driver that emits a line larger than this (e.g. an unbounded
/// base64 image without a newline) is treated as misbehaving: the kernel is
/// marked dead and a graceful KernelOutputTooLarge error frame is sent instead
/// of growing the buffer without limit.
const MAX_LINE_BYTES: usize = 96 * 1024 * 1024;

/// Configuration for KernelManager (mirrors kernelConfig in kernel.go).
/// The zero/default resolves python from PATH and driverPath to the default.
#[derive(Debug, Clone)]
pub struct KernelConfig {
    /// Python interpreter. Empty means "python3".
    pub python: String,
    /// Absolute path to kernel_driver.py. Empty means DEFAULT_DRIVER_PATH.
    pub driver_path: String,
}

impl Default for KernelConfig {
    fn default() -> Self {
        Self {
            python: DEFAULT_PYTHON.to_string(),
            driver_path: DEFAULT_DRIVER_PATH.to_string(),
        }
    }
}

/// One JSON event emitted by kernel_driver.py on stdout.
///
/// Mirrors driverEvent in guest/agent/kernel.go.
#[derive(Debug, serde::Deserialize)]
struct DriverEvent {
    #[allow(dead_code)]
    id: Option<String>,
    kind: String,
    #[serde(default)]
    text: String,
    #[serde(default)]
    data: std::collections::HashMap<String, String>,
    #[serde(default)]
    name: String,
    #[serde(default)]
    value: String,
    #[serde(default)]
    traceback: Vec<String>,
    #[serde(default)]
    status: String,
}

/// JSON request written to driver stdin (mirrors driverRequest in kernel.go).
#[derive(Debug, serde::Serialize)]
struct DriverRequest<'a> {
    id: &'a str,
    code: &'a str,
    #[serde(skip_serializing_if = "is_zero")]
    timeout: i64,
}

fn is_zero(v: &i64) -> bool {
    *v == 0
}

/// State of the live driver process.
struct LiveDriver {
    child: Child,
    stdin: ChildStdin,
    stdout: BufReader<ChildStdout>,
}

/// Manager for the in-guest code-execution kernel (Jupyter-style).
///
/// Starts lazily on first run() call; persists for the sandbox lifetime so
/// state (the kernel namespace) survives across run() calls. Callers hold the
/// enclosing `Mutex<KernelManager>` to serialize executions (one at a time).
///
/// Mirrors kernelManager in guest/agent/kernel.go.
#[derive(Debug)]
pub struct KernelManager {
    cfg: KernelConfig,
    // None until first run(); Some after successful start.
    driver: Option<LiveDriver>,
    // True once the driver has died and cannot be restarted.
    dead: bool,
}

// Manual Debug impl for LiveDriver (Child/ChildStdin/ChildStdout are not Debug).
impl std::fmt::Debug for LiveDriver {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("LiveDriver").finish_non_exhaustive()
    }
}

impl KernelManager {
    /// Create a new idle KernelManager with the default config.
    pub fn new() -> Self {
        Self::with_config(KernelConfig::default())
    }

    /// Create a KernelManager with a custom config (used by tests).
    pub fn with_config(cfg: KernelConfig) -> Self {
        Self {
            cfg,
            driver: None,
            dead: false,
        }
    }

    /// Run `code` in the kernel and send RunCodeResponse frames to `tx`.
    ///
    /// Always ends with an exit_code frame (0 for success, 1 for error, 127 for
    /// KernelUnavailable). Never panics; transport errors beyond the channel are
    /// silently dropped (the client disconnected).
    ///
    /// Mirrors run() in guest/agent/kernel.go.
    pub async fn run(
        &mut self,
        code: &str,
        language: &str,
        timeout_seconds: i64,
        tx: &mpsc::Sender<Result<RunCodeResponse, tonic::Status>>,
    ) {
        // Only "python" or empty language accepted.
        if !language.is_empty() && language != "python" {
            let msg = format!(
                "unsupported language {:?}: this base image provides only a python kernel",
                language
            );
            self.send_kernel_unavailable(tx, &msg).await;
            return;
        }

        // Ensure the driver is running.
        if let Err(msg) = self.ensure_started().await {
            self.send_kernel_unavailable(tx, &msg).await;
            return;
        }

        // Write the request to driver stdin.
        let req = DriverRequest {
            id: EXEC_ID,
            code,
            timeout: timeout_seconds,
        };
        let req_line = match serde_json::to_string(&req) {
            Ok(s) => s,
            Err(e) => {
                let msg = format!("encode request: {e}");
                self.dead = true;
                self.send_kernel_unavailable(tx, &msg).await;
                return;
            }
        };

        let driver = match self.driver.as_mut() {
            Some(d) => d,
            None => {
                // Should not happen after ensure_started succeeded.
                self.send_kernel_unavailable(tx, "kernel driver not available").await;
                return;
            }
        };

        // Write request line + newline.
        let write_result = async {
            driver.stdin.write_all(req_line.as_bytes()).await?;
            driver.stdin.write_all(b"\n").await?;
            driver.stdin.flush().await
        }
        .await;

        if let Err(e) = write_result {
            self.dead = true;
            let msg = format!("write to kernel: {e}");
            self.send_kernel_unavailable(tx, &msg).await;
            return;
        }

        // Read events from driver stdout until "done".
        // We use read_until(b'\n', &mut buf) into a Vec<u8> so we can enforce a
        // hard per-line cap of MAX_LINE_BYTES (96 MiB) before allocating more.
        //
        // We use `loop` + `match` rather than `while let` because the loop body
        // needs `self.dead = true` and `self.send_kernel_unavailable(&self, ...)`,
        // both of which require &mut self. A `while let Some(driver) =
        // self.driver.as_mut()` borrow would hold through the body and conflict.
        #[allow(clippy::while_let_loop)]
        let mut event_count: usize = 0;
        loop {
            let driver = match self.driver.as_mut() {
                Some(d) => d,
                None => break,
            };

            // Read one newline-terminated line into a reusable Vec<u8>.
            // read_until appends to the buffer; we start fresh each iteration.
            let mut line_buf: Vec<u8> = Vec::with_capacity(4096);
            let n = match driver.stdout.read_until(b'\n', &mut line_buf).await {
                Err(e) => {
                    self.dead = true;
                    tracing::debug!(error = %e, "kernel stream read error");
                    self.send_kernel_unavailable(tx, "kernel stream read error").await;
                    return;
                }
                Ok(n) => n,
            };

            if n == 0 {
                // Driver closed stdout without a "done": treat as dead.
                self.dead = true;
                self.send_kernel_unavailable(tx, "kernel exited unexpectedly").await;
                return;
            }

            // Enforce the hard line-length cap BEFORE parsing JSON.
            if line_buf.len() > MAX_LINE_BYTES {
                self.dead = true;
                tracing::debug!(
                    bytes = line_buf.len(),
                    cap = MAX_LINE_BYTES,
                    "kernel output line exceeded cap; marking dead"
                );
                let err_frame = crate::sandbox_v1::RunCodeResponse {
                    msg: Some(
                        crate::sandbox_v1::run_code_response::Msg::Error(
                            crate::sandbox_v1::RunError {
                                name: "KernelOutputTooLarge".to_string(),
                                value: format!(
                                    "driver emitted a line larger than the {} MiB cap; \
                                     kernel marked dead",
                                    MAX_LINE_BYTES / (1024 * 1024)
                                ),
                                traceback: vec![],
                            },
                        ),
                    ),
                };
                let _ = tx.send(Ok(err_frame)).await;
                let exit_frame = crate::sandbox_v1::RunCodeResponse {
                    msg: Some(crate::sandbox_v1::run_code_response::Msg::ExitCode(1)),
                };
                let _ = tx.send(Ok(exit_frame)).await;
                return;
            }

            let trimmed = line_buf.trim_ascii();
            if trimmed.is_empty() {
                continue;
            }

            let ev: DriverEvent = match serde_json::from_slice(trimmed) {
                Ok(e) => e,
                Err(e) => {
                    self.dead = true;
                    tracing::debug!(error = %e, "decode kernel event error");
                    // Graceful error frame + exit_code 1 instead of a torn stream,
                    // mirroring Go's KernelStreamError frame path.
                    let err_frame = crate::sandbox_v1::RunCodeResponse {
                        msg: Some(
                            crate::sandbox_v1::run_code_response::Msg::Error(
                                crate::sandbox_v1::RunError {
                                    name: "KernelStreamError".to_string(),
                                    value: format!("decode kernel event: {e}"),
                                    traceback: vec![],
                                },
                            ),
                        ),
                    };
                    let _ = tx.send(Ok(err_frame)).await;
                    let exit_frame = crate::sandbox_v1::RunCodeResponse {
                        msg: Some(crate::sandbox_v1::run_code_response::Msg::ExitCode(1)),
                    };
                    let _ = tx.send(Ok(exit_frame)).await;
                    return;
                }
            };

            event_count += 1;
            tracing::debug!(kind = %ev.kind, event_count, "kernel event received");

            match ev.kind.as_str() {
                "ready" => {
                    // Late ready (should not happen post-start); ignore.
                }
                "stdout" => {
                    let frame = RunCodeResponse {
                        msg: Some(run_code_response::Msg::Stdout(ev.text.into_bytes())),
                    };
                    let _ = tx.send(Ok(frame)).await;
                }
                "stderr" => {
                    let frame = RunCodeResponse {
                        msg: Some(run_code_response::Msg::Stderr(ev.text.into_bytes())),
                    };
                    let _ = tx.send(Ok(frame)).await;
                }
                "result" => {
                    // Map data: HashMap<String, String> -> HashMap<String, Vec<u8>>
                    let data: std::collections::HashMap<String, Vec<u8>> = ev
                        .data
                        .into_iter()
                        .map(|(k, v)| (k, v.into_bytes()))
                        .collect();
                    let frame = RunCodeResponse {
                        msg: Some(run_code_response::Msg::Result(RunResult {
                            text: ev.text,
                            data,
                        })),
                    };
                    let _ = tx.send(Ok(frame)).await;
                }
                "error" => {
                    let frame = RunCodeResponse {
                        msg: Some(run_code_response::Msg::Error(RunError {
                            name: ev.name,
                            value: ev.value,
                            traceback: ev.traceback,
                        })),
                    };
                    let _ = tx.send(Ok(frame)).await;
                }
                "done" => {
                    let exit_code: i32 = if ev.status == "error" { 1 } else { 0 };
                    let frame = RunCodeResponse {
                        msg: Some(run_code_response::Msg::ExitCode(exit_code)),
                    };
                    let _ = tx.send(Ok(frame)).await;
                    return;
                }
                other => {
                    tracing::debug!(kind = %other, "kernel: ignoring unknown event kind");
                }
            }
        }

        // Fell through the loop without "done": kernel is dead.
        self.dead = true;
        self.send_kernel_unavailable(tx, "kernel exited unexpectedly").await;
    }

    /// Send a KernelUnavailable error frame followed by exit_code 127.
    /// Mirrors errorFrames() in guest/agent/kernel.go.
    async fn send_kernel_unavailable(
        &self,
        tx: &mpsc::Sender<Result<RunCodeResponse, tonic::Status>>,
        msg: &str,
    ) {
        let err_frame = RunCodeResponse {
            msg: Some(run_code_response::Msg::Error(RunError {
                name: "KernelUnavailable".to_string(),
                value: msg.to_string(),
                traceback: vec![],
            })),
        };
        let _ = tx.send(Ok(err_frame)).await;
        let exit_frame = RunCodeResponse {
            msg: Some(run_code_response::Msg::ExitCode(127)),
        };
        let _ = tx.send(Ok(exit_frame)).await;
    }

    /// Lazily start the driver process. Mirrors ensureStarted() in kernel.go.
    ///
    /// Returns Ok(()) if already started or successfully started now.
    /// Returns Err(String) with an actionable message if the driver cannot start.
    async fn ensure_started(&mut self) -> Result<(), String> {
        if self.dead {
            return Err("kernel previously died".to_string());
        }
        if self.driver.is_some() {
            return Ok(());
        }

        let driver_path = &self.cfg.driver_path;
        if !std::path::Path::new(driver_path).exists() {
            return Err(format!(
                "kernel unavailable: driver {driver_path} not found; rebuild the base image \
                 with FULL_ROOTFS=1 so ipykernel and {driver_path} are installed"
            ));
        }

        let mut cmd = tokio::process::Command::new(&self.cfg.python);
        cmd.arg(driver_path);
        cmd.stdin(Stdio::piped());
        cmd.stdout(Stdio::piped());
        // Route driver stderr to this process's stderr (ipykernel debug noise).
        cmd.stderr(Stdio::inherit());
        // Kill the driver when its Child handle is dropped (agent shutdown).
        cmd.kill_on_drop(true);

        // Set working directory to /workspace when it exists (mirrors Go).
        if std::path::Path::new("/workspace").is_dir() {
            cmd.current_dir("/workspace");
        }

        let mut child = cmd.spawn().map_err(|e| {
            format!(
                "kernel unavailable: start kernel driver: {e}; rebuild the base image \
                 with FULL_ROOTFS=1 so ipykernel and {driver_path} are installed"
            )
        })?;

        let stdin = child.stdin.take().ok_or_else(|| "kernel stdin pipe missing".to_string())?;
        let stdout_raw = child
            .stdout
            .take()
            .ok_or_else(|| "kernel stdout pipe missing".to_string())?;
        let mut stdout = BufReader::with_capacity(LINE_BUF_BYTES, stdout_raw);

        // Wait for the "ready" line before returning.
        // Use read_until so the ready-line read is also subject to the hard cap
        // (a misbehaving driver emitting a huge first line must not OOM us).
        let mut ready_buf: Vec<u8> = Vec::with_capacity(4096);
        let ready_n = stdout.read_until(b'\n', &mut ready_buf).await.map_err(|e| {
            format!("kernel driver produced no ready line: {e}")
        })?;

        if ready_n == 0 || ready_buf.is_empty() {
            let _ = child.kill().await;
            return Err("kernel driver produced no ready line".to_string());
        }

        if ready_buf.len() > MAX_LINE_BYTES {
            let _ = child.kill().await;
            return Err("kernel driver ready line exceeded size cap".to_string());
        }

        // Parse and validate the ready event.
        let trimmed = ready_buf.trim_ascii();
        let ev: DriverEvent = serde_json::from_slice(trimmed).map_err(|_| {
            "kernel driver did not signal ready".to_string()
        })?;
        if ev.kind != "ready" {
            let _ = child.kill().await;
            return Err("kernel driver did not signal ready".to_string());
        }

        tracing::debug!(python = %self.cfg.python, driver = %driver_path, "kernel driver started");

        self.driver = Some(LiveDriver {
            child,
            stdin,
            stdout,
        });

        Ok(())
    }

    /// Shut down the driver process. Safe to call when never started.
    /// Mirrors shutdown() in guest/agent/kernel.go.
    pub async fn shutdown(&mut self) {
        if let Some(mut d) = self.driver.take() {
            let _ = d.child.kill().await;
        }
        self.dead = true;
    }
}

impl Default for KernelManager {
    fn default() -> Self {
        Self::new()
    }
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
#[allow(
    clippy::unwrap_used,
    clippy::expect_used,
    clippy::panic,
    clippy::indexing_slicing
)]
mod tests {
    use super::*;
    use tokio::sync::mpsc;

    /// Helper: collect all RunCodeResponse frames from a channel into a Vec.
    async fn collect(
        mut rx: mpsc::Receiver<Result<RunCodeResponse, tonic::Status>>,
    ) -> Vec<RunCodeResponse> {
        let mut out = vec![];
        while let Some(item) = rx.recv().await {
            out.push(item.expect("unexpected Err frame"));
        }
        out
    }

    /// Return true when the frame is a KernelUnavailable error frame.
    fn is_kernel_unavailable(frame: &RunCodeResponse) -> bool {
        matches!(
            &frame.msg,
            Some(run_code_response::Msg::Error(e)) if e.name == "KernelUnavailable"
        )
    }

    /// Return true when the frame is an exit_code frame.
    fn exit_code(frame: &RunCodeResponse) -> Option<i32> {
        match &frame.msg {
            Some(run_code_response::Msg::ExitCode(c)) => Some(*c),
            _ => None,
        }
    }

    // -------------------------------------------------------------------------
    // (a) print(2+2) -> stdout "4\n" + exit_code 0
    // -------------------------------------------------------------------------
    #[tokio::test]
    async fn runcode_print_stdout_and_clean_exit() {
        let (tx, rx) = mpsc::channel(64);
        let mut km = KernelManager::with_config(KernelConfig::default());
        km.run("print(2+2)", "", 10, &tx).await;
        drop(tx);
        let frames = collect(rx).await;

        // Find a stdout frame containing "4".
        let has_stdout = frames.iter().any(|f| {
            matches!(&f.msg, Some(run_code_response::Msg::Stdout(b)) if b.windows(1).any(|w| w == b"4"))
        });
        assert!(has_stdout, "expected stdout frame with '4', got: {frames:?}");

        // Last frame must be exit_code 0.
        let last = frames.last().expect("no frames");
        assert_eq!(exit_code(last), Some(0), "expected clean exit (0), got: {last:?}");
    }

    // -------------------------------------------------------------------------
    // (b) NameError -> RunError frame with name "NameError"
    // -------------------------------------------------------------------------
    #[tokio::test]
    async fn runcode_nameerror_produces_run_error_frame() {
        let (tx, rx) = mpsc::channel(64);
        let mut km = KernelManager::with_config(KernelConfig::default());
        km.run("undefined_variable_xyz", "", 10, &tx).await;
        drop(tx);
        let frames = collect(rx).await;

        let has_name_error = frames.iter().any(|f| {
            matches!(
                &f.msg,
                Some(run_code_response::Msg::Error(e)) if e.name == "NameError"
            )
        });
        assert!(has_name_error, "expected NameError frame, got: {frames:?}");

        // Last frame must be exit_code 1 (error status from done event).
        let last = frames.last().expect("no frames");
        assert_eq!(exit_code(last), Some(1), "expected exit_code 1 after error");
    }

    // -------------------------------------------------------------------------
    // (c) STATE PERSISTENCE: x=41 in call 1, print(x+1) in call 2 -> "42"
    // -------------------------------------------------------------------------
    #[tokio::test]
    async fn runcode_state_persists_across_calls() {
        // Same KernelManager: state must persist.
        let mut km = KernelManager::with_config(KernelConfig::default());

        // Call 1: define x = 41.
        let (tx1, rx1) = mpsc::channel(64);
        km.run("x = 41", "", 10, &tx1).await;
        drop(tx1);
        let frames1 = collect(rx1).await;
        let last1 = frames1.last().expect("no frames from call 1");
        assert_eq!(exit_code(last1), Some(0), "call 1 must exit cleanly");

        // Call 2: use x.
        let (tx2, rx2) = mpsc::channel(64);
        km.run("print(x + 1)", "", 10, &tx2).await;
        drop(tx2);
        let frames2 = collect(rx2).await;

        let has_42 = frames2.iter().any(|f| {
            matches!(
                &f.msg,
                Some(run_code_response::Msg::Stdout(b)) if String::from_utf8_lossy(b).contains("42")
            )
        });
        assert!(has_42, "expected '42' in stdout frames of call 2, got: {frames2:?}");
    }

    // -------------------------------------------------------------------------
    // (d) Unsupported language -> KernelUnavailable error frame + exit_code 127
    // -------------------------------------------------------------------------
    #[tokio::test]
    async fn runcode_unsupported_language_returns_error_frame() {
        let (tx, rx) = mpsc::channel(64);
        let mut km = KernelManager::with_config(KernelConfig::default());
        km.run("print('hello')", "ruby", 10, &tx).await;
        drop(tx);
        let frames = collect(rx).await;

        assert!(
            frames.len() >= 2,
            "expected at least 2 frames (error + exit_code), got: {frames:?}"
        );
        assert!(
            is_kernel_unavailable(&frames[0]),
            "first frame must be KernelUnavailable, got: {:?}",
            frames[0]
        );
        assert_eq!(
            exit_code(frames.last().expect("no frames")),
            Some(127),
            "expected exit_code 127 for unsupported language"
        );
        // Verify the error value mentions the unsupported language.
        if let Some(run_code_response::Msg::Error(e)) = &frames[0].msg {
            assert!(
                e.value.contains("ruby"),
                "error value should mention the unsupported language, got: {}",
                e.value
            );
        }
    }

    // -------------------------------------------------------------------------
    // KernelUnavailable when driver path is missing (no ipykernel / wrong path)
    // -------------------------------------------------------------------------
    #[tokio::test]
    async fn runcode_kernel_unavailable_when_driver_missing() {
        let cfg = KernelConfig {
            python: "python3".to_string(),
            driver_path: "/nonexistent/kernel_driver_xyz.py".to_string(),
        };
        let (tx, rx) = mpsc::channel(64);
        let mut km = KernelManager::with_config(cfg);
        km.run("print(1)", "", 10, &tx).await;
        drop(tx);
        let frames = collect(rx).await;

        assert!(
            frames.len() >= 2,
            "expected KernelUnavailable + exit_code, got: {frames:?}"
        );
        assert!(
            is_kernel_unavailable(&frames[0]),
            "first frame must be KernelUnavailable, got: {:?}",
            frames[0]
        );
        assert_eq!(
            exit_code(frames.last().expect("no frames")),
            Some(127),
            "expected exit_code 127 for missing driver"
        );
    }
}
