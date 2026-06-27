// Control service: implements sandbox.internal.v1.Control for the Rust guest
// agent. Mirrors the Go controlServer in grpc_server.go:703-802.
//
// Both services (Sandbox and Control) are served on the SAME vsock gRPC port
// (AGENT_GRPC_PORT = 53), matching Go's newGuestGRPCServer (grpc_server.go:43-48).
//
// This service is host-trusted and NEVER exposed on a public surface. It is
// served only over AF_VSOCK inside the microVM, where the vsock channel is
// reachable only by the host (forkd) via Firecracker virtio-vsock.
//
// Security invariants (enforced here):
//   - Entropy bytes are NEVER logged; only the byte count is safe to observe.
//   - host_wall_clock_nanos is NEVER logged as an absolute value; only the
//     applied step magnitude appears in logs (delegated to handle_notify_forked).
//   - env and secrets VALUES are NEVER logged; only key counts are logged.
//   - Ping carries no secrets and is safe to log.

use std::collections::HashSet;
use std::sync::Arc;
use std::time::{Duration, Instant};
use tonic::{Request, Response, Status};

use crate::control_v1::control_server::Control;
use crate::control_v1::{
    ConfigureRequest, ConfigureResponse, NotifyForkedRequest, NotifyForkedResponse, PingRequest,
    PingResponse, StartWorkloadRequest, StartWorkloadResponse,
};
use crate::env::ConfiguredEnv;
use crate::fork::{
    self,
    network::NetworkConfig,
    volumes::VolumeMountEntry,
};
use crate::service::workload::{self, WorkloadRegistry};

/// The tonic Control service for the sandbox.internal.v1.Control gRPC service.
///
/// Holds the start time (for Ping uptime) and the shared ConfiguredEnv (for
/// Configure). NotifyForked delegates to crate::fork::handle_notify_forked_inner
/// via the injectable `signal_fn`, so tests can pass a no-op signal function to
/// avoid broadcasting SIGUSR2 to host processes (box2 safety contract).
pub struct ControlService {
    /// When the agent process started; used to compute uptime in Ping.
    pub start_time: Instant,
    /// Shared env state. Populated by Configure; read by exec handlers.
    pub env: Arc<ConfiguredEnv>,
    /// Injectable signal function for the notify-forked path. In production this
    /// is `fork::signal::signal_userspace`; in tests it is a no-op to avoid
    /// broadcasting SIGUSR2 to host processes during `cargo test` on box2.
    ///
    /// `#[doc(hidden)]`: public for the integration test harness in `tests/`;
    /// not part of the stable API.
    #[doc(hidden)]
    pub signal_fn: fn(&HashSet<i32>) -> i32,
    /// Registered serving-workload sessions (issue #460). StartWorkload records a
    /// started workload's session here; NotifyForked passes the set to signal_fn
    /// so the broadcast excludes the workload and it survives the fork.
    pub workload: Arc<WorkloadRegistry>,
}

impl ControlService {
    /// Create a ControlService wired to the real signal_userspace function.
    /// This is the production constructor. Tests use struct literal syntax to
    /// inject a no-op signal function.
    pub fn new(start_time: Instant, env: Arc<ConfiguredEnv>) -> Self {
        Self {
            start_time,
            env,
            signal_fn: fork::signal::signal_userspace,
            workload: Arc::new(WorkloadRegistry::default()),
        }
    }
}

#[tonic::async_trait]
impl Control for ControlService {
    // Ping: returns agent uptime in fractional seconds. Mirrors the Go
    // controlServer.Ping (grpc_server.go:716-718). Carries no secrets.
    async fn ping(
        &self,
        _request: Request<PingRequest>,
    ) -> Result<Response<PingResponse>, Status> {
        let uptime_seconds = self.start_time.elapsed().as_secs_f64();
        tracing::debug!(uptime_seconds, "control: Ping");
        Ok(Response::new(PingResponse { uptime_seconds }))
    }

    // Configure: merges claim-time env and secrets into the shared ConfiguredEnv.
    // THIS RPC CARRIES SECRET VALUES. Values are NEVER logged; only the key
    // count after merge is safe to observe. Mirrors Go's controlServer.Configure
    // (grpc_server.go:726-735): additive merge via ConfiguredEnv::apply.
    async fn configure(
        &self,
        request: Request<ConfigureRequest>,
    ) -> Result<Response<ConfigureResponse>, Status> {
        let req = request.into_inner();
        let env_count = req.env.len();
        let secrets_count = req.secrets.len();
        // Values are moved into apply and never appear in this scope again.
        self.env.apply(req.env, req.secrets).await;
        // Log only counts; never log keys or values of secrets.
        tracing::info!(
            env_keys = env_count,
            secret_keys = secrets_count,
            "control: Configure applied"
        );
        Ok(Response::new(ConfigureResponse {}))
    }

    // NotifyForked: applies post-restore fork-correctness repairs. Mirrors Go's
    // controlServer.NotifyForked (grpc_server.go:744-765):
    //   proto NotifyForkedRequest -> typed fork::NotifyForkedRequest
    //   -> handle_notify_forked_inner(req, signal_fn) -> typed fork::NotifyForkedResponse
    //   -> proto NotifyForkedResponse
    //
    // Entropy and absolute host_wall_clock_nanos are NEVER logged here or in
    // handle_notify_forked_inner (only counts and applied step magnitude are logged).
    // The signal field (signaled_processes) comes from self.signal_fn; in
    // production this is the real /proc walk + SIGUSR2 broadcast.
    async fn notify_forked(
        &self,
        request: Request<NotifyForkedRequest>,
    ) -> Result<Response<NotifyForkedResponse>, Status> {
        let req = request.into_inner();

        // Map proto network message to the typed NetworkConfig. Returns None
        // when network is absent (proto default), preserving the nil-means-no-op
        // behavior from Go (grpc_server.go:771-782).
        let network = req.network.map(|n| NetworkConfig {
            guest_ip: n.guest_ip,
            gateway_ip: n.gateway_ip,
            prefix_len: n.prefix_len as u32,
            guest_mac: n.guest_mac,
            resolver_ip: n.resolver_ip,
        });

        // Map repeated VolumeMountEntry proto messages to typed entries.
        // Empty input maps to an empty Vec, preserving the no-op behavior.
        let volumes: Vec<VolumeMountEntry> = req
            .volumes
            .into_iter()
            .map(|v| VolumeMountEntry {
                device: v.device,
                mount_path: v.mount_path,
                read_only: v.read_only,
            })
            .collect();

        let typed_req = fork::NotifyForkedRequest {
            generation: req.generation,
            host_wall_clock_nanos: req.host_wall_clock_nanos,
            entropy: req.entropy,
            network,
            volumes,
        };

        // Delegate to the inner orchestrator with the injectable signal function.
        // In production signal_fn = signal_userspace; in tests it is a no-op. The
        // registered workload sessions are excluded so a captured-running serving
        // workload survives the fork (#460).
        let exclude = self.workload.excluded_sids();
        let signal_fn = self.signal_fn;
        let resp = fork::handle_notify_forked_inner(&typed_req, move || signal_fn(&exclude));

        Ok(Response::new(NotifyForkedResponse {
            applied_clock_step_nanos: resp.applied_clock_step_nanos,
            reseeded_rng: resp.reseeded_rng,
            signaled_processes: resp.signaled_processes,
        }))
    }

    // StartWorkload: starts a declared serving workload in its own session so it
    // outlives the build's exec and the fork SIGUSR2 reset, records its session
    // for the fork exclusion, and (when Ready is set) waits until it is listening
    // so the snapshot captures a serving app (issue #460). Host-trusted, build
    // time only. Never logs command argv or env values.
    async fn start_workload(
        &self,
        request: Request<StartWorkloadRequest>,
    ) -> Result<Response<StartWorkloadResponse>, Status> {
        let req = request.into_inner();
        if req.command.is_empty() {
            return Err(Status::invalid_argument("start_workload: empty command"));
        }
        // Build a single shell command line from argv (the supervisor runs it via
        // `sh -lc`, matching how the build's exec path runs commands).
        let command = req.command.join(" ");
        let env: Vec<(String, String)> = req.env.into_iter().collect();
        let cwd = if req.cwd.is_empty() { "/" } else { &req.cwd };

        let sid = workload::spawn_detached(&command, &env, cwd)
            .map_err(|e| Status::internal(format!("start_workload: spawn: {e}")))?;
        self.workload.register(sid);
        tracing::info!(sid, env_keys = env.len(), "control: StartWorkload started");

        let mut ready = true;
        if let Some(h) = req.ready {
            let timeout = if h.timeout_seconds == 0 {
                Duration::from_secs(120)
            } else {
                Duration::from_secs(u64::from(h.timeout_seconds))
            };
            let port = u16::try_from(h.port)
                .map_err(|_| Status::invalid_argument("start_workload: ready.port out of range"))?;
            let expect = u16::try_from(h.expect).unwrap_or(0);
            // Belt-and-suspenders: ensure the loopback link is up before gating on
            // a 127.0.0.1 workload. init brings lo up at boot; re-assert here so a
            // boot-order or timing issue cannot leave the gate polling a down lo.
            if let Err(e) = crate::sys::netlink::link_up("lo") {
                tracing::warn!("start_workload: bring up lo: {e}");
            }
            workload::await_http_ready(port, &h.path, expect, timeout)
                .await
                .map_err(|e| {
                    // lo link flags (IFF_UP is bit 0) and whether anything is
                    // LISTENing on the port, to tell a down loopback apart from a
                    // workload that never bound. /proc/net/tcp lists the local port
                    // in uppercase hex; 0A is the LISTEN state.
                    let lo_flags = std::fs::read_to_string("/sys/class/net/lo/flags")
                        .map(|s| s.trim().to_string())
                        .unwrap_or_else(|_| "?".to_string());
                    let hexport = format!(":{port:04X}");
                    let listening = std::fs::read_to_string("/proc/net/tcp")
                        .map(|t| t.lines().any(|l| l.contains(&hexport)))
                        .unwrap_or(false);
                    // Surface why the workload never became ready: whether its
                    // process is still alive and the tail of its own stdout/stderr
                    // (the build VM is ephemeral, so this is the only window into a
                    // workload that failed to start or listen). The build env is
                    // non-secret pool config; per-fork secrets are injected later.
                    let alive = std::path::Path::new(&format!("/proc/{sid}")).exists();
                    let tail = std::fs::read_to_string("/tmp/mitos-workload.log")
                        .map(|s| {
                            s.lines()
                                .rev()
                                .take(12)
                                .collect::<Vec<_>>()
                                .into_iter()
                                .rev()
                                .collect::<Vec<_>>()
                                .join(" | ")
                        })
                        .unwrap_or_else(|_| "<no workload log>".to_string());
                    // wchan names the kernel function the process is sleeping in,
                    // so a workload that is alive but never listens (blocked on a
                    // syscall: getrandom, futex, a file read) is distinguishable
                    // from one still doing CPU-bound startup.
                    let wchan = std::fs::read_to_string(format!("/proc/{sid}/wchan"))
                        .map(|s| s.trim().to_string())
                        .unwrap_or_default();
                    Status::deadline_exceeded(format!(
                        "start_workload: {e}; workload pid {sid} alive={alive} listening={listening} lo_flags={lo_flags} wchan={wchan}; log tail: {tail}"
                    ))
                })?;
            ready = true;
        }
        Ok(Response::new(StartWorkloadResponse { sid, ready }))
    }
}
