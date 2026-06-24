//! Fork-correctness modules: post-restore state repair after a Firecracker
//! snapshot restore. These modules are invoked by the notify-forked handler
//! and mirror the behavior of guest/agent/notifyforked.go.

/// Clock step: adjusts CLOCK_REALTIME toward the host wall clock after a
/// snapshot restore. Returns the signed step applied in nanoseconds (0 when
/// within the 500ms tolerance window, 0 on any error, 0 when host time is 0).
pub mod clock;

/// Credited CRNG reseed: injects host-supplied per-fork entropy via the
/// RNDADDENTROPY ioctl. Fail-closed: returns false if the credited inject
/// fails so the host reaps the fork rather than serving it with shared CRNG
/// state.
pub mod reseed;

/// Network reconfiguration: reconfigures eth0 with the per-fork address,
/// default route, and optionally the MAC address and resolver, after a
/// snapshot restore. Mirrors configureNetwork + writeResolvConf in
/// notifyforked.go:193-232.
pub mod network;

/// SIGUSR2 broadcast: sends SIGUSR2 to all userspace processes except PID 1
/// and self, so language runtimes can reseed their PRNGs after a fork.
/// Mirrors signalUserspace in notifyforked.go:299-328.
pub mod signal;

/// Volume mounts: mounts per-fork volume entries delivered in the notify-forked
/// request. Idempotent on re-fork (skips already-mounted paths). Mirrors
/// mountVolumes in notifyforked.go:247-276.
pub mod volumes;

pub use volumes::{VolumeMountEntry, mount_volumes};

// ---------------------------------------------------------------------------
// handle_notify_forked orchestrator (Task 3.5).
// Mirrors handleNotifyForked in guest/agent/notifyforked.go:33-57 EXACTLY,
// including step order and which result goes into which response field.
// ---------------------------------------------------------------------------

/// Inputs for the notify-forked orchestrator. Mirrors vsock.NotifyForkedRequest
/// in the Go guest agent. Proto-to-typed conversion happens at the gRPC layer
/// (Task 4.1); this struct is the clean seam between the proto world and the
/// fork-correctness actions.
pub struct NotifyForkedRequest {
    /// Monotonically increasing fork counter (echoed in logs; never secret).
    pub generation: u64,
    /// Host wall-clock time at fork, in nanoseconds since the Unix epoch.
    /// Used to step CLOCK_REALTIME if drift exceeds 500ms. Never logged as
    /// an absolute value; only the applied step magnitude may appear in logs.
    pub host_wall_clock_nanos: i64,
    /// Per-fork entropy from the host. Injected via RNDADDENTROPY so each
    /// fork diverges from its siblings. NEVER logged; only the byte count
    /// appears in log output.
    pub entropy: Vec<u8>,
    /// Per-fork network identity. None when the host did not deliver a
    /// network config (feature off or not applicable).
    pub network: Option<network::NetworkConfig>,
    /// Per-fork volume mount table. Empty when no volumes are attached.
    pub volumes: Vec<volumes::VolumeMountEntry>,
}

/// Outputs of the notify-forked orchestrator. Mirrors vsock.NotifyForkedResponse
/// in the Go guest agent (notifyforked.go:50-56).
pub struct NotifyForkedResponse {
    /// Signed clock adjustment applied, in nanoseconds. 0 when within the
    /// 500ms tolerance window, on error, or when host_wall_clock_nanos was 0.
    /// Mirrors AppliedClockStepNanos.
    pub applied_clock_step_nanos: i64,
    /// True when the kernel CRNG was credibly reseeded via RNDADDENTROPY.
    /// False on empty entropy or ioctl failure (fail-closed). Mirrors
    /// ReseededRNG.
    pub reseeded_rng: bool,
    /// Count of userspace processes that received SIGUSR2. Mirrors
    /// SignaledProcesses.
    pub signaled_processes: i32,
}

/// Writes the fork generation number as a decimal string to
/// `/run/sandbox/fork-generation`. Best effort: failures are printed to stderr
/// and do not interrupt the orchestrator.
///
/// Mirrors writeForkGeneration in notifyforked.go:168-177.
fn write_fork_generation(generation: u64) {
    if let Err(err) = std::fs::create_dir_all("/run/sandbox") {
        eprintln!("sandbox-agent: mkdir /run/sandbox: {err}");
        return;
    }
    let data = generation.to_string();
    if let Err(err) = std::fs::write("/run/sandbox/fork-generation", data) {
        eprintln!("sandbox-agent: write fork-generation: {err}");
    }
}

/// Orchestrates all fork-correctness actions after a Firecracker snapshot
/// restore.
///
/// Order mirrors handleNotifyForked in guest/agent/notifyforked.go:33-57
/// EXACTLY:
///   1. Reseed kernel CRNG with per-fork entropy.
///   2. Step CLOCK_REALTIME toward host wall clock.
///   3. Write fork generation to /run/sandbox/fork-generation.
///   4. Configure per-fork network identity.
///   5. Mount per-fork volumes.
///   6. Send SIGUSR2 to userspace processes.
///   7. Log summary (counts/magnitudes only; entropy bytes and absolute clock
///      value are never logged).
///   8. Return NotifyForkedResponse.
///
/// The response field mapping mirrors Go (notifyforked.go:50-56):
///   applied_clock_step_nanos <- step (from step 2)
///   reseeded_rng             <- reseeded (from step 1)
///   signaled_processes       <- signaled (from step 6)
pub fn handle_notify_forked(req: &NotifyForkedRequest) -> NotifyForkedResponse {
    handle_notify_forked_inner(req, signal::signal_userspace)
}

/// Inner orchestrator with an injectable signal function. Keeps tests
/// parallel-safe: tests pass a no-op so they never send SIGUSR2 to sibling
/// test-runner processes (which are forked children visible in /proc).
fn handle_notify_forked_inner(
    req: &NotifyForkedRequest,
    do_signal: fn() -> i32,
) -> NotifyForkedResponse {
    // Step 1: reseed kernel CRNG.
    let reseeded = reseed::reseed(&req.entropy);

    // Step 2: step CLOCK_REALTIME toward host wall clock.
    let step = clock::step_clock(req.host_wall_clock_nanos);

    // Step 3: record fork generation.
    write_fork_generation(req.generation);

    // Step 4: configure per-fork network.
    network::configure_network(req.network.as_ref());

    // Step 5: mount per-fork volumes.
    let mounted = volumes::mount_volumes(&req.volumes);

    // Step 6: signal userspace processes.
    let signaled = do_signal();

    // Step 7: log summary. Entropy bytes and absolute clock value are NEVER
    // logged; only counts and the applied step magnitude appear.
    tracing::info!(
        generation = req.generation,
        entropy_bytes = req.entropy.len(),
        reseeded,
        clock_step_ns = step,
        volumes_mounted = mounted,
        signaled,
        "notify_forked"
    );

    // Step 8: return response.
    NotifyForkedResponse {
        applied_clock_step_nanos: step,
        reseeded_rng: reseeded,
        signaled_processes: signaled,
    }
}

// ---------------------------------------------------------------------------
// Tests.
// ---------------------------------------------------------------------------

#[cfg(test)]
#[allow(
    clippy::expect_used,
    clippy::unwrap_used,
    clippy::panic,
    clippy::indexing_slicing
)]
mod tests {
    use super::*;

    // No-op signal function for tests: does NOT walk /proc or send SIGUSR2.
    // Using this in all handle_notify_forked tests keeps the suite
    // parallel-safe: sibling test processes (forked children from other test
    // threads) are never signaled.
    fn noop_signal() -> i32 { 0 }

    // --- Brief's mandated tests (Task 3.5, Step 1) ---

    #[test]
    fn notify_forked_with_empty_entropy_returns_false_reseed() {
        let req = NotifyForkedRequest {
            generation: 1,
            host_wall_clock_nanos: 0,
            entropy: vec![],
            network: None,
            volumes: vec![],
        };
        let resp = handle_notify_forked_inner(&req, noop_signal);
        assert!(!resp.reseeded_rng);
        assert_eq!(resp.applied_clock_step_nanos, 0);
    }

    #[test]
    fn notify_forked_logs_count_not_entropy_bytes() {
        // This test is enforced more rigorously by the secret_log_audit test
        // in Task 3.6. Here we just confirm the function does not panic with
        // real entropy bytes.
        let req = NotifyForkedRequest {
            generation: 2,
            host_wall_clock_nanos: 0,
            entropy: vec![0xde, 0xad, 0xbe, 0xef],
            network: None,
            volumes: vec![],
        };
        let _ = handle_notify_forked_inner(&req, noop_signal);
    }

    // --- Additional orchestrator contract tests ---

    #[test]
    fn notify_forked_response_fields_match_go_mapping() {
        // Verify the three response fields are populated from the correct
        // sub-functions (using empty/zero inputs for determinism).
        let req = NotifyForkedRequest {
            generation: 42,
            host_wall_clock_nanos: 0,
            entropy: vec![],
            network: None,
            volumes: vec![],
        };
        let resp = handle_notify_forked_inner(&req, noop_signal);
        // Empty entropy -> reseeded_rng false (fail-closed).
        assert!(!resp.reseeded_rng, "empty entropy must yield reseeded_rng=false");
        // host_wall_clock_nanos=0 -> applied_clock_step_nanos=0.
        assert_eq!(resp.applied_clock_step_nanos, 0, "zero host time must yield step=0");
        // noop_signal returns 0.
        assert_eq!(
            resp.signaled_processes, 0,
            "noop signal must yield signaled_processes=0"
        );
    }

    // Verify that non-empty entropy does NOT panic and returns a valid bool.
    // Return value depends on whether RNDADDENTROPY succeeds (root vs non-root
    // in CI); both true and false are acceptable.
    #[test]
    fn notify_forked_with_real_entropy_does_not_panic() {
        let req = NotifyForkedRequest {
            generation: 3,
            host_wall_clock_nanos: 0,
            entropy: (0u8..=31u8).collect(),
            network: None,
            volumes: vec![],
        };
        let resp = handle_notify_forked_inner(&req, noop_signal);
        // Just assert the response is a valid bool (no panic).
        let _ = resp.reseeded_rng;
    }
}
