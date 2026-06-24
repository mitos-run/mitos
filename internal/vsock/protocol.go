package vsock

// Host-side data types for the gRPC runtime protocol the Rust guest agent
// serves on AgentGRPCPort. The legacy JSON wire protocol and its envelope
// (Request/Response and the per-op JSON structs) were removed with the Go agent
// (#310); the types that remain here are the small value types the host gRPC
// callers in internal/daemon, internal/husk, and internal/guestgrpc still pass
// across the host code, plus the vsock dial constants.

// MaxTarBytes bounds a single archive (tar) payload. The tar of a guest
// directory is buffered whole in memory on both the guest and host (this slice
// does not stream), so the cap keeps the workspace transfer to one bounded
// allocation per side. A directory whose tar would exceed this is refused
// rather than risking an unbounded guest allocation; a streaming (chunked)
// transfer for very large workspaces is a later W4 slice. The guest enforces
// the cap on the tar it produces and on the tar it accepts; the host enforces
// it before sending.
const MaxTarBytes = 64 << 20

// VitalsResponse is the guest telemetry snapshot returned for a Vitals request:
// CPU steal over the guest's sampling window, the guest-visible memory figures
// (and what the host has reclaimed via the balloon), and the in-guest process
// table. The host side labels this with claim/pool/workspace; the guest reports
// raw values only. None of these fields carry secrets: process entries are the
// program name and resource counters, never argv or environment.
type VitalsResponse struct {
	// StealFraction is the fraction of the sampling window the vCPUs spent
	// involuntarily descheduled by the host (from /proc/stat steal), in [0,1].
	StealFraction float64 `json:"steal_fraction"`
	// SampleWindowMs is how long the guest sampled to compute StealFraction.
	SampleWindowMs float64 `json:"sample_window_ms"`
	// MemTotalKB / MemAvailableKB / MemUsedKB are the guest-visible figures from
	// /proc/meminfo. BalloonReclaimedKB is how much the host balloon has taken
	// back (0 when the guest has no balloon device or it was never inflated).
	MemTotalKB         uint64 `json:"mem_total_kb"`
	MemAvailableKB     uint64 `json:"mem_available_kb"`
	MemUsedKB          uint64 `json:"mem_used_kb"`
	BalloonReclaimedKB uint64 `json:"balloon_reclaimed_kb"`
	// Processes is the in-guest process table, one entry per live pid.
	Processes []ProcessEntry `json:"processes,omitempty"`
}

// ProcessEntry is one row of the in-guest process table: the pid, program name,
// single-letter state, accrued CPU jiffies (user + system), and resident set in
// kilobytes. The host carries them verbatim to kubectl mitos ps.
type ProcessEntry struct {
	PID        int    `json:"pid"`
	Comm       string `json:"comm"`
	State      string `json:"state"`
	CPUJiffies uint64 `json:"cpu_jiffies"`
	RSSKB      uint64 `json:"rss_kb"`
}

// NotifyForkedRequest tells the guest a restore just happened so it can repair
// fork-shared state: reseed the kernel CRNG with fresh host entropy, step the
// wall clock back to host time, and signal userspace runtimes to reseed their
// own PRNGs. The host sends fresh values on every fork.
//
// Entropy and HostWallClockNanos are sensitive: Entropy is raw CRNG seed
// material and the clock can leak host timing. Neither value is ever logged by
// host or guest; only counts and applied-step magnitudes are logged.
type NotifyForkedRequest struct {
	Generation         uint64 `json:"generation"`
	HostWallClockNanos int64  `json:"host_wall_clock_nanos"`
	Entropy            []byte `json:"entropy"`
	// Network, when set, carries this fork's per-fork network identity. Every
	// fork restores the SAME snapshot (and thus the same baked guest IP), so
	// the host remaps the NIC to a distinct tap via snapshot/load
	// network_overrides and delivers the fork's distinct guest IP + gateway
	// here; the guest agent reconfigures eth0 (ip addr add, default route) on
	// receipt. Without this step every fork would share one guest IP and the
	// host could not route return traffic per fork. IPs and prefix length are
	// safe to log.
	Network *NotifyForkedNetwork `json:"network,omitempty"`
	// Volumes is the per-fork volume mount table. The host rebinds each baked
	// placeholder drive to this fork's backing (PATCH /drives) BEFORE sending
	// this notification, so the devices are in place; the guest then mounts each
	// entry's Device at MountPath. Empty (the default) means the fork has no
	// volumes and the guest mounts nothing. Device nodes and paths carry no
	// secrets and are safe to log.
	Volumes []VolumeMountEntry `json:"volumes,omitempty"`
}

// VolumeMountEntry is one volume the guest agent mounts after a restore. Device
// is the guest block device node (e.g. /dev/vdb) the host assigned by the drive
// attach order (rootfs is /dev/vda, the i-th volume drive is /dev/vd{b+i}).
// MountPath is where the guest mounts it, and ReadOnly attaches it MS_RDONLY so
// a read-only or shared volume cannot be written from the guest. All fields are
// config (no secrets) and safe to log.
type VolumeMountEntry struct {
	Device    string `json:"device"`
	MountPath string `json:"mount_path"`
	ReadOnly  bool   `json:"read_only"`
}

// NotifyForkedNetwork is the per-fork eth0 configuration the guest agent
// applies after a restore: set eth0's hardware address to GuestMAC (when
// present) before the address is assigned, then assign GuestIP/PrefixLen to
// eth0 and install a default route via GatewayIP (the host side of the
// per-sandbox /30). Every fork restores the same snapshot, which bakes one
// shared placeholder MAC, so delivering a distinct GuestMAC per fork is what
// gives each fork its own eth0 MAC. All fields are plain addresses and safe to
// log.
type NotifyForkedNetwork struct {
	GuestIP   string `json:"guest_ip"`
	GatewayIP string `json:"gateway_ip"`
	PrefixLen int    `json:"prefix_len"`
	// GuestMAC, when non-empty, is this fork's distinct eth0 hardware address
	// (e.g. "02:..:.."). The guest agent sets it on eth0 before bringing the
	// link up and assigning the address. Empty leaves the snapshot-baked MAC in
	// place, so existing callers that do not deliver a MAC are unaffected.
	GuestMAC string `json:"guest_mac,omitempty"`
	// ResolverIP, when non-empty, is the node-wide DNS resolver the guest must
	// query for name-based egress. The guest agent writes it as the sole
	// nameserver in /etc/resolv.conf so every name lookup goes through the
	// controlled resolver (which is the only address the egress chain allows on
	// port 53). Empty means name-based egress is disabled and the guest's
	// existing resolv.conf is left untouched. The address is config, not a
	// secret, and is safe to log.
	ResolverIP string `json:"resolver_ip,omitempty"`
}

// NotifyForkedResponse reports what the guest did in response to a fork
// notification, for host-side observability. AppliedClockStepNanos is the
// signed adjustment applied to CLOCK_REALTIME (0 when drift was within
// tolerance), ReseededRNG is true when at least one entropy-injection path
// succeeded, and SignaledProcesses counts userspace processes that received
// the reseed signal.
type NotifyForkedResponse struct {
	AppliedClockStepNanos int64 `json:"applied_clock_step_nanos"`
	ReseededRNG           bool  `json:"reseeded_rng"`
	SignaledProcesses     int   `json:"signaled_processes"`
}

// ExecResponse is the one-shot result of a non-streaming exec.
type ExecResponse struct {
	ExitCode   int     `json:"exit_code"`
	Stdout     string  `json:"stdout"`
	Stderr     string  `json:"stderr"`
	ExecTimeMs float64 `json:"exec_time_ms"`
}

// FrameKind tags an ExecStreamFrame as a data chunk or the terminal exit.
type FrameKind string

const (
	FrameChunk FrameKind = "chunk"
	FrameExit  FrameKind = "exit"
	// FrameResult and FrameError are emitted by the run_code path only. A
	// FrameResult carries one rich display artifact (Result); a FrameError
	// carries a structured exception (ErrorInfo). Plain exec streaming never
	// emits these, so the exec-stream reader rejecting unknown kinds is
	// unaffected.
	FrameResult FrameKind = "result"
	FrameError  FrameKind = "error"
)

// StreamName identifies which standard stream a chunk came from.
type StreamName string

const (
	StreamStdout StreamName = "stdout"
	StreamStderr StreamName = "stderr"
)

// ExecStreamFrame is one frame in a streaming exec reply. The guest emits zero
// or more FrameChunk frames (each carrying a slice of one stream's bytes)
// followed by exactly one FrameExit frame.
type ExecStreamFrame struct {
	Kind       FrameKind  `json:"kind"`
	Stream     StreamName `json:"stream,omitempty"`
	Data       []byte     `json:"data,omitempty"`
	ExitCode   int        `json:"exit_code,omitempty"`
	Error      string     `json:"error,omitempty"`
	ExecTimeMs float64    `json:"exec_time_ms,omitempty"`
	// Result is set only on a FrameResult frame (run_code path): one rich
	// display artifact. ErrorInfo is set only on a FrameError frame: a
	// structured guest-code exception or a KernelUnavailable signal. Both are
	// nil on chunk/exit frames. They are distinct from the Error string above,
	// which carries a transport-level spawn failure on the terminal frame.
	Result    *ResultFrame `json:"result,omitempty"`
	ErrorInfo *ErrorFrame  `json:"error_info,omitempty"`
}

// ResultFrame is the payload of an ExecStreamFrame with Kind FrameResult. It is
// a single rich display artifact emitted by the kernel: Data maps a MIME type
// to its payload (base64 for binary types like image/png; raw UTF-8 text for
// text/html, image/svg+xml, text/markdown, text/latex, application/json,
// text/plain). Text is the REPL last-expression value (the text/plain rendering
// of an execute_result); it is empty for a display_data result that is not the
// cell's return value. None of these fields carry secrets.
type ResultFrame struct {
	Text string            `json:"text,omitempty"`
	Data map[string]string `json:"data,omitempty"`
}

// ErrorFrame is the payload of an ExecStreamFrame with Kind FrameError. It
// mirrors a Jupyter IOPub error message: Name is the exception class (ename),
// Value its string form (evalue), and Traceback the formatted lines. Tracebacks
// may contain ANSI color codes from the kernel; the host passes them through
// verbatim. Used both for guest code exceptions and for KernelUnavailable.
type ErrorFrame struct {
	Name      string   `json:"name"`
	Value     string   `json:"value"`
	Traceback []string `json:"traceback,omitempty"`
}

// FileEntry is one entry of a directory listing.
type FileEntry struct {
	Name       string `json:"name"`
	IsDir      bool   `json:"is_dir"`
	Size       int64  `json:"size"`
	Mode       uint32 `json:"mode"`
	ModifiedAt int64  `json:"modified_at"`
}

// PtyFrameKind tags a PtyFrame. input/resize flow client->guest; output/exit
// flow guest->client.
type PtyFrameKind string

const (
	PtyInput  PtyFrameKind = "input"
	PtyResize PtyFrameKind = "resize"
	PtyOutput PtyFrameKind = "output"
	PtyExit   PtyFrameKind = "exit"
)

// PtyFrame is one frame on the bidirectional PTY stream (and one WebSocket text
// frame at the forkd edge). Data is raw terminal bytes (control sequences).
// Cols/Rows are set only on a resize frame; ExitCode and Error only on the
// terminal exit frame.
type PtyFrame struct {
	Kind     PtyFrameKind `json:"kind"`
	Data     []byte       `json:"data,omitempty"`
	Cols     int          `json:"cols,omitempty"`
	Rows     int          `json:"rows,omitempty"`
	ExitCode int          `json:"exit_code,omitempty"`
	Error    string       `json:"error,omitempty"`
}

const (
	// GuestCID is the vsock CID assigned to the guest by Firecracker.
	GuestCID = 3
	// AgentGRPCPort is the vsock port the Rust guest agent serves the gRPC
	// runtime protocol on: the public sandbox.v1.Sandbox service and the
	// host-trusted sandbox.internal.v1.Control service. The transport is
	// insecure; the microVM boundary is the isolation layer.
	AgentGRPCPort = 53
)
