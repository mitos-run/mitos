// Vitals streaming RPC implementation (Task 2.7).
//
// Streams GuestVitals samples at the requested cadence:
//   - interval_seconds <= 0: one sample then EOF.
//   - interval_seconds >  0: first sample immediately, then one per interval
//     until the client cancels (stream drop / send error).
//
// Each sample reads:
//   /proc/stat twice, 100 ms apart, to compute CPU steal fraction over the
//     window (mirrors Go agent vitalsSampleWindow = 100 ms).
//   /proc/meminfo for MemTotal and MemAvailable (used = total - available).
//   /proc numeric dirs to count live processes (comm only; NO argv/environ).
//
// Secret hygiene: no argv, no environ, no secret values in errors or logs.
// All /proc reads go via the shared helpers in service::processes (steal
// jiffies) and service::processes::count_processes (process count) so parsing
// logic is not duplicated.

use std::time::{Duration, SystemTime, UNIX_EPOCH};
use tokio::sync::mpsc;
use tonic::{Request, Response, Status};

use crate::error::AgentError;
use crate::sandbox_v1::{GuestVitals, VitalsRequest};
use crate::service::BoxStream;
use crate::service::processes::{count_processes, read_total_and_steal_jiffies};

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

/// Two-snapshot window for steal-fraction computation.
/// Mirrors vitalsSampleWindow = 100 ms in the Go agent (vitals.go).
const STEAL_SAMPLE_WINDOW: Duration = Duration::from_millis(100);

/// Channel capacity for the outbound vitals sample stream.
/// Bounded to avoid unbounded buffering; 16 is well above any burst.
const CHAN_CAP: usize = 16;

// ---------------------------------------------------------------------------
// /proc/meminfo parser
// ---------------------------------------------------------------------------

/// Parsed fields from /proc/meminfo.
struct Meminfo {
    /// MemTotal in bytes.
    total_bytes: i64,
    /// MemAvailable in bytes.
    available_bytes: i64,
}

impl Meminfo {
    /// Bytes currently in use: MemTotal - MemAvailable, floored at 0.
    fn used_bytes(&self) -> i64 {
        self.total_bytes.saturating_sub(self.available_bytes).max(0)
    }
}

/// Parse MemTotal and MemAvailable from /proc/meminfo.
///
/// Lines have the form "MemTotal:   8192000 kB". Values are in kibibytes;
/// we convert to bytes by multiplying by 1024.
///
/// Returns AgentError::Internal if either field is missing or unparseable.
fn read_meminfo(proc: &str) -> Result<Meminfo, AgentError> {
    let path = format!("{proc}/meminfo");
    let data = std::fs::read_to_string(&path)
        .map_err(|e| AgentError::Internal(format!("vitals: read {path}: {e}")))?;

    let mut total_kb: Option<u64> = None;
    let mut available_kb: Option<u64> = None;

    for line in data.lines() {
        if line.starts_with("MemTotal:") {
            total_kb = parse_meminfo_kb(line);
        } else if line.starts_with("MemAvailable:") {
            available_kb = parse_meminfo_kb(line);
        }
        if total_kb.is_some() && available_kb.is_some() {
            break;
        }
    }

    let total_kb = total_kb.ok_or_else(|| {
        AgentError::Internal("vitals: MemTotal not found in /proc/meminfo".into())
    })?;
    let available_kb = available_kb.ok_or_else(|| {
        AgentError::Internal("vitals: MemAvailable not found in /proc/meminfo".into())
    })?;

    Ok(Meminfo {
        total_bytes: (total_kb as i64).saturating_mul(1024),
        available_bytes: (available_kb as i64).saturating_mul(1024),
    })
}

/// Extract the kibibyte value from a /proc/meminfo line.
///
/// Expects the form "MemXxx:   <value> kB". Returns None on any parse error.
fn parse_meminfo_kb(line: &str) -> Option<u64> {
    // Split on ':' to get the value part, then take the first whitespace token.
    let (_, after_colon) = line.split_once(':')?;
    let value_str = after_colon.split_whitespace().next()?;
    value_str.parse().ok()
}

// ---------------------------------------------------------------------------
// Steal fraction sampler
// ---------------------------------------------------------------------------

/// Compute the CPU steal fraction over STEAL_SAMPLE_WINDOW.
///
/// Reads /proc/stat twice, STEAL_SAMPLE_WINDOW apart, and computes:
///   steal_delta / total_delta
///
/// Returns 0.0 on any read error or zero total_delta (conservative).
/// Returns a value in [0.0, 1.0]; clamped to 100.0 percent by the caller.
///
/// Mirrors Go agent guestvitals.StealDelta(t0, t1).StealFraction().
async fn sample_steal_fraction(proc: &str) -> f64 {
    let (total0, steal0) = read_total_and_steal_jiffies(proc);
    tokio::time::sleep(STEAL_SAMPLE_WINDOW).await;
    let (total1, steal1) = read_total_and_steal_jiffies(proc);

    let total_delta = total1.saturating_sub(total0) as f64;
    if total_delta <= 0.0 {
        return 0.0;
    }
    let steal_delta = steal1.saturating_sub(steal0) as f64;
    (steal_delta / total_delta).clamp(0.0, 1.0)
}

// ---------------------------------------------------------------------------
// Sample assembly
// ---------------------------------------------------------------------------

/// Assemble one GuestVitals sample.
///
/// Reads /proc/stat (twice, 100 ms apart), /proc/meminfo, and the process
/// count. No argv or environ is ever read.
async fn sample_vitals(proc: &str) -> Result<GuestVitals, AgentError> {
    let steal_fraction = sample_steal_fraction(proc).await;
    let mem = read_meminfo(proc)?;
    let process_count = count_processes(proc);

    let sampled_at_unix = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or(Duration::ZERO)
        .as_secs() as i64;

    Ok(GuestVitals {
        sampled_at_unix,
        cpu_percent: 0.0,
        cpu_steal_percent: steal_fraction * 100.0,
        mem_used_bytes: mem.used_bytes(),
        mem_total_bytes: mem.total_bytes,
        mem_balloon_bytes: 0,
        process_count,
    })
}

// ---------------------------------------------------------------------------
// Vitals RPC handler
// ---------------------------------------------------------------------------

/// Implement the Vitals server-streaming RPC.
///
/// Spawns a tokio task that emits GuestVitals samples at the requested cadence:
///   interval_seconds <= 0: one sample then closes (one-shot mode).
///   interval_seconds > 0:  first sample immediately, then one per interval
///     until the send fails (client disconnected).
///
/// The streaming task is self-terminating: when the mpsc receiver is dropped
/// (client gone, stream closed), the next send returns Err and the task exits.
/// No manual cancellation token or Arc<AtomicBool> is needed; the bounded
/// channel back-pressure and send-error detection are sufficient.
pub async fn vitals(
    request: Request<VitalsRequest>,
) -> Result<Response<BoxStream<GuestVitals>>, Status> {
    let req = request.into_inner();
    let interval_secs = req.interval_seconds;
    let proc = "/proc";

    let (tx, rx) = mpsc::channel::<Result<GuestVitals, Status>>(CHAN_CAP);

    tokio::spawn(async move {
        if interval_secs <= 0 {
            // One-shot: send a single sample and let the task exit (channel closes).
            let sample = sample_vitals(proc).await.map_err(Status::from);
            let _ = tx.send(sample).await;
            return;
        }

        // Streaming mode: first sample immediately.
        let sample = sample_vitals(proc).await.map_err(Status::from);
        if tx.send(sample).await.is_err() {
            return;
        }

        // Then one per interval until client disconnects.
        let interval = Duration::from_secs(interval_secs as u64);
        let mut ticker = tokio::time::interval(interval);
        // Consume the first tick that fires immediately (interval starts now).
        ticker.tick().await;

        loop {
            ticker.tick().await;
            let sample = sample_vitals(proc).await.map_err(Status::from);
            if tx.send(sample).await.is_err() {
                return;
            }
        }
    });

    let out = tokio_stream::wrappers::ReceiverStream::new(rx);
    Ok(Response::new(Box::pin(out)))
}

// ---------------------------------------------------------------------------
// Unit tests
// ---------------------------------------------------------------------------

#[cfg(test)]
#[allow(clippy::unwrap_used, clippy::expect_used)]
mod tests {
    use super::*;

    // --- parse_meminfo_kb ---

    #[test]
    fn parse_meminfo_kb_standard_line() {
        assert_eq!(parse_meminfo_kb("MemTotal:       8192000 kB"), Some(8_192_000));
    }

    #[test]
    fn parse_meminfo_kb_no_leading_space() {
        assert_eq!(parse_meminfo_kb("MemAvailable:1024 kB"), Some(1024));
    }

    #[test]
    fn parse_meminfo_kb_missing_value_returns_none() {
        assert_eq!(parse_meminfo_kb("MemTotal:"), None);
    }

    // --- Meminfo.used_bytes ---

    #[test]
    fn meminfo_used_bytes_is_total_minus_available() {
        let m = Meminfo {
            total_bytes: 8_192_000 * 1024,
            available_bytes: 4_096_000 * 1024,
        };
        assert_eq!(m.used_bytes(), 4_096_000 * 1024);
    }

    #[test]
    fn meminfo_used_bytes_saturates_at_zero() {
        let m = Meminfo {
            total_bytes: 100,
            available_bytes: 200,
        };
        assert_eq!(m.used_bytes(), 0);
    }

    // --- GuestVitals field hygiene: no argv or environ ---

    /// Verify that GuestVitals carries no cmdline or environ field.
    /// Compile-time guard: if such a field were added, the literal below would
    /// not compile (missing field error).
    #[test]
    fn guest_vitals_has_no_argv_or_environ_field() {
        let v = GuestVitals {
            sampled_at_unix: 0,
            cpu_percent: 0.0,
            cpu_steal_percent: 0.0,
            mem_used_bytes: 0,
            mem_total_bytes: 0,
            mem_balloon_bytes: 0,
            process_count: 0,
        };
        let _ = v;
    }
}
