package husk

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/cas"
	"mitos.run/mitos/internal/dnsproxy"
	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/guestgrpc"
	"mitos.run/mitos/internal/netconf"
	"mitos.run/mitos/internal/snapcompat"
	"mitos.run/mitos/internal/volume"
	"mitos.run/mitos/internal/workspace"
	internalv1 "mitos.run/mitos/proto/sandbox/controlv1"
)

// entropySize is the number of crypto/rand bytes generated per activation and
// handed to the guest via NotifyForked to reseed the kernel CRNG. It matches
// the fork engine's reseed size (internal/daemon notifyForked uses 32 bytes).
const entropySize = 32

// State is the husk stub lifecycle state.
type State int

const (
	// StateNew is before Prepare: no VMM exists.
	StateNew State = iota
	// StateDormant is after Prepare: the Firecracker process and its API
	// socket are up but no snapshot is loaded and the guest is not running.
	StateDormant
	// StateActive is after a successful Activate: the snapshot is loaded,
	// the VM is resumed, and the guest agent has answered over vsock.
	StateActive
)

func (s State) String() string {
	switch s {
	case StateNew:
		return "new"
	case StateDormant:
		return "dormant"
	case StateActive:
		return "active"
	default:
		return "unknown"
	}
}

// vmm is the subset of *firecracker.Client the stub drives. Keeping it behind an
// interface lets the activate state machine be unit-tested with a fake, with no
// real Firecracker process or KVM.
type vmm interface {
	// LoadSnapshotWithOverrides loads the snapshot mem+vmstate files and (when
	// resume is true) resumes the VM, remapping NICs per overrides. The husk
	// activate path loads with resume=false so it can rebind the rootfs drive
	// (PatchDrive) while the VM is PAUSED, before the guest can write anything,
	// then resumes explicitly via Resume.
	LoadSnapshotWithOverrides(mem, snapshot string, resume bool, overrides []firecracker.NetworkOverride) error
	// VsockHostPath resolves a relative vsock uds_path to its host location.
	VsockHostPath(rel string) string
	// PatchDrive rebinds an existing baked drive (by drive id) to a host backing
	// file via PATCH /drives, on the loaded-but-PAUSED restored VM (before Resume)
	// so the guest never touches the shared template backing. Firecracker's runtime
	// API controller accepts a drive path_on_host PATCH in the Paused state with no
	// root-device restriction (verified against the pinned v1.15 rpc_interface). The
	// husk activate path uses it to point the rootfs drive at this activation's CoW
	// clone, the same rebind the fork engine applies to volume drives.
	PatchDrive(driveID, pathOnHost string) error
	// Resume transitions the loaded VM from Paused to Running (PATCH /vm Resumed).
	// The husk activate path calls it AFTER the rootfs drive rebind so the guest
	// resumes already bound to its own per-activation rootfs clone.
	Resume() error
	// Pause transitions the loaded/running VM to Paused (PATCH /vm Paused). The
	// fork-snapshot op pauses the source VM before CreateSnapshot, which requires
	// a paused VM, then resumes it (unless the fork asked to keep it paused).
	Pause() error
	// CreateSnapshot writes a Full Firecracker snapshot of the PAUSED VM: the
	// guest memory to memPath and the device/vm state to snapshotPath. The
	// fork-snapshot op writes the source VM's snapshot here so child husk pods can
	// restore independent copies of it.
	CreateSnapshot(memPath, snapshotPath string) error
	// Ping reports whether the VMM still answers its API socket. It returns an
	// error once the Firecracker process is gone or defunct, which the husk
	// liveness monitor uses to detect a dead warm slot (issue #527).
	Ping() error
	// Close tears the VMM down.
	Close() error
}

// starter brings up a DORMANT Firecracker VMM (process + API socket, not
// booted) and returns it behind the vmm interface. The production starter wraps
// firecracker.StartVM; tests inject a fake.
type starter func(cfg firecracker.VMConfig) (vmm, error)

// guestReady blocks until the guest agent answers a ping over the vsock UDS at
// vsockPath, or the timeout elapses. The production seam dials the gRPC Control
// service and calls Ping; tests inject a fake. ctx is forwarded so a cancelled
// activate context also cancels the readiness wait.
type guestReady func(ctx context.Context, vsockPath string, timeout time.Duration) error

// notifier runs the post-restore fork-correctness handshake against the guest
// agent at vsockPath: it delivers the fresh generation + entropy via
// NotifyForked (so the guest reseeds its CRNG, steps its clock, and re-addresses
// its NIC) and then delivers the claim-time env/secrets, mirroring the daemon's
// deliverConfig. It FAILS CLOSED: it returns an error when the reseed handshake
// fails or the guest reports it did not reseed, so a VM that still shares its
// siblings' CRNG state is never served. The production seam connects via
// internal/vsock; tests inject a fake. The entropy and secret VALUES are never
// logged by any implementation.
type notifier func(vsockPath string, generation uint64, entropy []byte, req ActivateRequest) error

// dialFunc is the injectable gRPC dial seam used by notifierGRPC and
// guestReadyGRPC. The production seam uses guestgrpc.Dial (vsock); tests inject
// guestgrpc.DialUnix so no real Firecracker process or vsock is needed.
type dialFunc func(vsockPath string) (*guestgrpc.Client, error)

// notifierGRPC runs the post-restore fork-correctness handshake against the
// guest agent's gRPC Control service at vsockPath via the supplied dial function.
// It delivers NotifyForked (generation + entropy + per-fork network + volume
// table) then Configure (env + secrets), in the same order as the daemon's
// deliverConfig. It fails closed: any transport error, or a guest that reports
// ReseededRng=false, returns an error so the stub leaves the VM unserved.
//
// Entropy and secret VALUES are never logged or included in any error text:
// errors carry only the operation name and the underlying transport error.
func notifierGRPC(vsockPath string, generation uint64, entropy []byte, req ActivateRequest, dial dialFunc) error {
	client, err := dial(vsockPath)
	if err != nil {
		return fmt.Errorf("connect guest agent gRPC for fork handshake: %w", err)
	}
	defer client.Close() //nolint:errcheck // best-effort on close

	// Build the network sub-message from the vsock type. Nil network is valid
	// (no per-fork re-addressing needed for this activation).
	var pbNet *internalv1.NotifyForkedNetwork
	if req.Network != nil {
		pbNet = &internalv1.NotifyForkedNetwork{
			GuestIp:    req.Network.GuestIP,
			GatewayIp:  req.Network.GatewayIP,
			PrefixLen:  int32(req.Network.PrefixLen),
			GuestMac:   req.Network.GuestMAC,
			ResolverIp: req.Network.ResolverIP,
		}
	}

	// Build the volume table. Empty slice is valid (no volumes for this fork).
	pbVols := make([]*internalv1.VolumeMountEntry, len(req.Volumes))
	for i, v := range req.Volumes {
		pbVols[i] = &internalv1.VolumeMountEntry{
			Device:    v.Device,
			MountPath: v.MountPath,
			ReadOnly:  v.ReadOnly,
		}
	}

	// NotifyForked: deliver generation, fresh entropy (SENSITIVE: do not log the
	// value), host wall clock, per-fork network, and volume table. The guest
	// reseeds its kernel CRNG, steps CLOCK_REALTIME, re-addresses eth0, and
	// mounts volumes. host_wall_clock_nanos mirrors NotifyForkedWithConfig.
	ctx := context.Background()
	resp, err := client.Control.NotifyForked(ctx, &internalv1.NotifyForkedRequest{
		Generation:         generation,
		HostWallClockNanos: time.Now().UnixNano(),
		Entropy:            entropy,
		Network:            pbNet,
		Volumes:            pbVols,
	})
	if err != nil {
		return fmt.Errorf("notify guest of fork: %w", err)
	}
	// Fail closed: a guest that did not reseed shares CRNG state with its
	// siblings. Do not serve it.
	if resp == nil || !resp.ReseededRng {
		return fmt.Errorf("guest did not reseed its RNG after restore; refusing to serve a fork that shares CRNG state")
	}

	// Configure: deliver claim-time env+secrets exactly as deliverConfig does.
	// Skip when there is nothing to deliver. Secret values are never logged.
	if len(req.Env) == 0 && len(req.Secrets) == 0 {
		return nil
	}
	if _, err := client.Control.Configure(ctx, &internalv1.ConfigureRequest{
		Env:     req.Env,
		Secrets: req.Secrets,
	}); err != nil {
		return fmt.Errorf("configure guest env/secrets: %w", err)
	}
	return nil
}

// productionNotifier is the production notifier seam: it calls notifierGRPC
// with the real vsock dial function (guestgrpc.Dial, port 53) so the fork
// handshake reaches the guest's gRPC Control service. The legacy JSON protocol
// on AgentPort 52 is no longer used for this path.
//
// Entropy and secret VALUES are never logged or included in any error text.
func productionNotifier(vsockPath string, generation uint64, entropy []byte, req ActivateRequest) error {
	return notifierGRPC(vsockPath, generation, entropy, req, guestgrpc.Dial)
}

// guestReadyGRPC waits for the guest agent's gRPC Control service to answer a
// Ping RPC, retrying at fixed intervals until the timeout elapses or ctx is
// cancelled. It uses the supplied dial function so tests can inject DialUnix.
// The retry semantics mirror the legacy productionGuestReady JSON poll loop.
func guestReadyGRPC(ctx context.Context, vsockPath string, timeout time.Duration, dial dialFunc) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if ctx.Err() != nil {
			return fmt.Errorf("guest agent gRPC not ready: %w", ctx.Err())
		}
		if time.Now().After(deadline) {
			break
		}

		client, err := dial(vsockPath)
		if err != nil {
			lastErr = err
			select {
			case <-ctx.Done():
				return fmt.Errorf("guest agent gRPC not ready: %w", ctx.Err())
			case <-time.After(20 * time.Millisecond):
			}
			continue
		}

		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, pingErr := client.Control.Ping(pingCtx, &internalv1.PingRequest{})
		cancel()
		client.Close() //nolint:errcheck // best-effort; conn closed after check
		if pingErr == nil {
			return nil
		}
		lastErr = pingErr
		select {
		case <-ctx.Done():
			return fmt.Errorf("guest agent gRPC not ready: %w", ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timeout")
	}
	return fmt.Errorf("guest agent gRPC not ready within %s: %w", timeout, lastErr)
}

// reflinker copies a source file to a destination with copy-on-write semantics
// (reflink where the filesystem supports it, full copy otherwise). The husk
// stub clones the template rootfs to a per-activation file through it. The
// production seam is volume.Backend.ReflinkCopy; tests inject a fake. src and
// dst carry no secrets.
type reflinker func(src, dst string) error

// rootfsTemplateWait bounds how long Prepare waits for forkd to finish writing
// the node template rootfs.ext4 before cloning it for this activation. The pool
// builds the template snapshot on the node before creating husk pods, but the
// build is slower with networking enabled (a placeholder tap + NIC boot before
// the snapshot), so a freshly scheduled husk pod can briefly observe the source
// rootfs missing. Waiting (rather than crashing into CrashLoopBackOff) keeps the
// pod recoverable within a claim's readiness window.
const rootfsTemplateWait = 180 * time.Second

// waitForFile polls until path exists, the context is cancelled, or the timeout
// elapses. It is the bounded tolerance for the pool creating a husk pod before
// forkd has finished materializing the template rootfs on the shared node dir.
func waitForFile(ctx context.Context, path string, timeout time.Duration) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s did not appear within %s: %w", path, timeout, ctx.Err())
		case <-ticker.C:
			if _, err := os.Stat(path); err == nil {
				return nil
			}
		}
	}
}

// wsTransporter resolves a workspace.VsockTransport (the bulk tar TarDir/UntarDir
// slice of the guest agent) for the active VM at vsockPath. The dehydrate and
// hydrate workspace ops run the KVM-proven internal/workspace round trip through
// it. The production seam returns a gRPC-backed transport (Archive/Upload on
// AgentGRPCPort 53); tests inject a fake in-memory transport so the ops can be
// exercised with no VM.
type wsTransporter func(vsockPath string) (workspace.VsockTransport, error)

// productionWorkspaceTransport returns a gRPC-backed workspace.VsockTransport
// that uses the guest Sandbox Archive and Upload RPCs on AgentGRPCPort 53 to
// perform bulk tar transfers. This replaces the legacy JSON path on AgentPort 52
// so workspace dehydrate/hydrate works against the gRPC-only Rust agent.
// Workspace content bytes never appear in any log line: only the operation and
// the transport error are reported.
func productionWorkspaceTransport(vsockPath string) (workspace.VsockTransport, error) {
	return &grpcWorkspaceTransport{vsockPath: vsockPath}, nil
}

// productionStarter wraps firecracker.StartVM. *firecracker.Client satisfies
// vmm (it has LoadSnapshotWithOverrides, VsockHostPath, and we adapt Kill to
// Close below).
func productionStarter(cfg firecracker.VMConfig) (vmm, error) {
	client, err := firecracker.StartVM(cfg)
	if err != nil {
		return nil, err
	}
	return &clientVMM{Client: client}, nil
}

// clientVMM adapts *firecracker.Client to the vmm interface. Close maps to Kill
// so the husk teardown reaps the Firecracker process.
type clientVMM struct {
	*firecracker.Client
}

func (c *clientVMM) Close() error {
	return c.Client.Kill()
}

// productionGuestReady waits for the Rust guest agent's gRPC Control service
// to answer a Ping RPC on vsock.AgentGRPCPort (53). The Rust agent is the sole
// guest agent and serves gRPC only (#310). The retry semantics mirror the
// removed JSON poll.
func productionGuestReady(ctx context.Context, vsockPath string, timeout time.Duration) error {
	return guestReadyGRPC(ctx, vsockPath, timeout, guestgrpc.Dial)
}

// Options configures a Stub. Zero values select the production seams, so the
// daemon constructs New(cfg, Options{}). Tests inject fakes.
type Options struct {
	// Start brings up the dormant VMM. Nil uses the production starter.
	Start starter
	// Ready waits for the guest agent. Nil uses the production seam.
	Ready guestReady
	// Notify runs the post-restore fork-correctness handshake. Nil uses the
	// production seam (connect the vsock client and NotifyForked + Configure).
	Notify notifier
	// Verify re-verifies the snapshot at activate time BEFORE it is loaded
	// (digest integrity + snapcompat, fail-closed). Nil uses the production
	// verifier built from ManifestPath, Env, and AllowUnverified below. Tests
	// inject a no-op (or a failing) verifier so they need no on-disk manifest.
	Verify snapshotVerifier
	// ManifestPath is the on-disk path of the recorded CAS manifest mounted into
	// the husk pod read-only; the production verifier decodes it, binds it to the
	// request's ExpectedDigest, and re-hashes the loaded files against it. Empty
	// is only valid with AllowUnverified (development).
	ManifestPath string
	// Env is the detected host environment the production verifier checks snapshot
	// compatibility against (Firecracker version, CPU model, kernel, formats).
	Env snapcompat.Environment
	// AllowUnverified is the development escape hatch mirroring forkd's
	// --allow-unverified-snapshots: when true the verifier warns once and proceeds
	// on a missing-digest or failed check. Default false keeps verify enforced.
	AllowUnverified bool
	// ReadyTimeout bounds the guest-readiness wait during Activate. Zero uses
	// DefaultReadyTimeout.
	ReadyTimeout time.Duration
	// OnActivated is invoked exactly once, after a SUCCESSFUL Activate, with the
	// activated guest agent's host vsock UDS path and the per-sandbox bearer
	// token delivered in the ActivateRequest. The husk pod uses it to register
	// the activated VM with a daemon.SandboxAPI and serve the token-gated sandbox
	// HTTP API (exec/files) on the sandbox port, so the endpoint the claim
	// advertises is actually reachable. The token is a SECRET; the hook must
	// never log it. Nil disables the hook (the control-socket CI driver and unit
	// tests that do not need the sandbox API leave it nil).
	OnActivated func(vsockPath, token string) error
	// PrepareSnapshotDir and PrepareExpectedDigest, when both set, move the
	// fail-closed snapshot verification (the ~680 MiB mem+rootfs re-hash) OFF the
	// Activate hot path and INTO Prepare, where it runs during the pre-paid
	// dormant warm period. The snapshot is a read-only, content-addressed,
	// immutable mount, so verifying it once at Prepare is equivalent to verifying
	// at Activate, and Activate then only confirms the request names the same
	// (dir, digest) it already verified before loading. This is what makes the
	// claim->Ready latency the engine cost (~tens of ms) instead of the hash cost
	// (~1.3 s on a slow CPU). Empty (or AllowUnverified) keeps the verify on the
	// Activate path as before. The values are content addresses, not secrets.
	PrepareSnapshotDir    string
	PrepareExpectedDigest string
	// RootfsTemplatePath and RootfsCoWDir, when both set, give this activation its
	// OWN copy-on-write clone of the template rootfs instead of writing the shared
	// template rootfs.ext4 in place. At Prepare the stub reflink-clones
	// RootfsTemplatePath to <RootfsCoWDir>/<vm id>/rootfs.ext4 (pre-paid, dormant),
	// and at Activate it rebinds the snapshot's baked "rootfs" drive to that clone
	// with PatchDrive after the snapshot loads. Both empty keeps the prior behavior
	// (the resumed VM writes the shared template rootfs). The paths are content
	// addresses, not secrets.
	RootfsTemplatePath string
	RootfsCoWDir       string
	// Reflink performs the per-activation rootfs clone. Nil uses the production
	// seam (volume.Backend.ReflinkCopy, which is FICLONE with a full-copy
	// fallback). Tests inject a fake.
	Reflink reflinker
	// ForksDir is the node forks directory mounted into this pod. When set, the
	// fork-snapshot and remove-fork-snapshot control ops confine their writes to
	// within it (fail-closed: a request naming a SnapshotDir outside it is
	// refused), so the control channel can never be steered to write or delete a
	// path outside the mounted forks dir. Empty disables the check (the request's
	// SnapshotDir is used as-is, the prior behavior). A node-local path, not a
	// secret.
	ForksDir string
	// CASDir is the node content-addressed store root mounted into this pod
	// (the same <dataDir>/cas the forkd build path writes). When set, the
	// dehydrate-workspace op captures the guest /workspace into it and returns the
	// manifest digest, and the hydrate-workspace op reads a manifest back from it
	// into the guest. Empty disables the workspace ops (they fail closed): a stub
	// without a node CAS cannot persist or restore a workspace. A node-local path,
	// not a secret; workspace CONTENT is never logged.
	CASDir string
	// WorkspaceTransport resolves the guest-agent bulk-tar transport for the
	// workspace ops. Nil uses the production seam (gRPC Archive/Upload on
	// AgentGRPCPort 53). Tests inject a fake in-memory transport.
	WorkspaceTransport wsTransporter
}

// DefaultReadyTimeout bounds how long Activate waits for the guest agent to
// answer after the snapshot is resumed before failing closed.
const DefaultReadyTimeout = 10 * time.Second

// Stub is a single-VM husk: Prepare brings up a dormant VMM, Activate loads a
// snapshot into it in place, and Serve dispatches one activate request from a
// control socket. It owns exactly one VM for its lifetime.
type Stub struct {
	start        starter
	ready        guestReady
	notify       notifier
	verify       snapshotVerifier
	onActivated  func(vsockPath, token string) error
	cfg          firecracker.VMConfig
	readyTimeout time.Duration

	// prepareSnapshotDir / prepareExpectedDigest are the snapshot the dormant
	// pod verified at Prepare; prepareVerified records that the re-hash passed.
	// Activate skips its own re-hash when the request names this exact snapshot.
	prepareSnapshotDir    string
	prepareExpectedDigest string

	// rootfsTemplatePath / rootfsCoWDir configure the per-activation rootfs CoW;
	// reflink performs the clone; rootfsClonePath records the clone Prepare made so
	// Activate rebinds the drive to it and Close removes it. Empty rootfsClonePath
	// means no per-activation rootfs was prepared (prior behavior).
	rootfsTemplatePath string
	rootfsCoWDir       string
	reflink            reflinker
	rootfsClonePath    string

	// forksDir confines fork-snapshot / remove-fork-snapshot writes to within it
	// when set; empty disables the check.
	forksDir string

	// casStore is the node CAS the workspace dehydrate/hydrate ops persist to and
	// restore from; nil disables those ops (they fail closed). wsTransport resolves
	// the guest-agent bulk-tar transport for them; vsockRelPath is the relative
	// vsock UDS path the active VM's guest agent listens on.
	casStore     *cas.Store
	wsTransport  wsTransporter
	vsockRelPath string

	// netRunner, when non-nil, executes host networking commands in the pod
	// netns so Activate can program the in-pod egress filter. Nil (unit and
	// control-socket paths) skips all network setup. Injected so the filter is
	// testable without root.
	netRunner netfilterRunner
	// nftRunner runs a single nft argv for the DNS proxy pinner. Nil reuses
	// netRunner with empty stdin.
	nftRunner func(argv []string) error
	// dnsUpstream is the real resolver(s) the per-pod DNS proxy forwards allowed
	// queries to: a comma-separated host:port list tried in failover order. Empty
	// disables name-based egress (IP-only mode).
	dnsUpstream string
	// dnsProxy is the running per-pod DNS proxy for the active VM, stopped on
	// Close. Nil when no VM is active or name egress is disabled.
	dnsProxy *dnsproxy.Server
	// enableForwarding turns on IPv4 forwarding in the pod netns before the egress
	// datapath is programmed (the kernel will not route the guest /30 to the pod
	// uplink otherwise). Nil skips it (tests, or a deployment that enables
	// forwarding out of band); cmd/husk-stub wires the real /proc writer.
	enableForwarding func() error
	// activeTap records the active VM's tap so Close can tear the filter down.
	activeTap string

	mu              sync.Mutex
	state           State
	vm              vmm
	generation      uint64
	prepareVerified bool
}

// NetRunner is the exported alias for the host-command runner type so callers
// in other packages (cmd/husk-stub) can construct one. It must run argv in the
// husk pod's network namespace; the production wiring uses an exec-based runner.
type NetRunner = netfilterRunner

// SetNetRunner wires the host-command runner the stub uses to program the
// in-pod egress filter. It must run argv in the pod netns; cmd/husk-stub wires
// an exec-based runner. Nil disables in-pod filtering.
func (s *Stub) SetNetRunner(run NetRunner) { s.netRunner = run }

// SetDNSUpstream sets the real resolver the per-pod DNS proxy forwards
// allowlisted queries to. Empty disables name-based egress.
func (s *Stub) SetDNSUpstream(addr string) { s.dnsUpstream = addr }

// SetForwardingEnabler wires the function the stub calls to enable IPv4
// forwarding in the pod netns before programming the egress datapath. Nil (the
// default) skips it. cmd/husk-stub wires the production /proc writer.
func (s *Stub) SetForwardingEnabler(fn func() error) { s.enableForwarding = fn }

// New builds a Stub for the given VMConfig. By default it uses the production
// starter and guest-readiness seam; opts may inject fakes for tests.
func New(cfg firecracker.VMConfig, opts Options) *Stub {
	s := &Stub{
		start:        opts.Start,
		ready:        opts.Ready,
		notify:       opts.Notify,
		verify:       opts.Verify,
		onActivated:  opts.OnActivated,
		cfg:          cfg,
		readyTimeout: opts.ReadyTimeout,
		state:        StateNew,

		prepareSnapshotDir:    opts.PrepareSnapshotDir,
		prepareExpectedDigest: opts.PrepareExpectedDigest,

		rootfsTemplatePath: opts.RootfsTemplatePath,
		rootfsCoWDir:       opts.RootfsCoWDir,
		reflink:            opts.Reflink,
		forksDir:           opts.ForksDir,
		wsTransport:        opts.WorkspaceTransport,
		vsockRelPath:       firecracker.VsockRelPath,
	}
	if s.start == nil {
		s.start = productionStarter
	}
	if s.ready == nil {
		s.ready = productionGuestReady
	}
	if s.notify == nil {
		s.notify = productionNotifier
	}
	if s.verify == nil {
		s.verify = productionVerifier(verifyConfig{
			manifestPath:    opts.ManifestPath,
			env:             opts.Env,
			allowUnverified: opts.AllowUnverified,
		})
	}
	if s.readyTimeout == 0 {
		s.readyTimeout = DefaultReadyTimeout
	}
	if s.reflink == nil {
		s.reflink = volume.New("").ReflinkCopy
	}
	if s.wsTransport == nil {
		s.wsTransport = productionWorkspaceTransport
	}
	// Open the node CAS when a dir is configured. A failure here is logged (path
	// only, no content) and leaves casStore nil, so the workspace ops fail closed
	// rather than the whole stub failing to start: the fork/activate/warm-pool
	// paths do not need the CAS.
	if opts.CASDir != "" {
		store, err := cas.New(opts.CASDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "husk: open node CAS at %s: %v\n", opts.CASDir, err)
		} else {
			s.casStore = store
		}
	}
	return s
}

// Prepare brings up a DORMANT Firecracker VMM (process + API socket, not
// booted) and stores it. It is not idempotent across states: calling it once
// the stub is already dormant or active is an error, so a husk never silently
// leaks a second VMM.
func (s *Stub) Prepare(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != StateNew {
		return fmt.Errorf("husk: prepare in state %s: already prepared", s.state)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	vm, err := s.start(s.cfg)
	if err != nil {
		return fmt.Errorf("husk: prepare dormant VMM: %w", err)
	}
	s.vm = vm

	// Verify the snapshot NOW, while dormant, instead of on the Activate hot
	// path. When the controller passes the snapshot dir + expected digest at
	// startup we run the full fail-closed re-hash here, during the warm period a
	// claim has not arrived yet. The snapshot is read-only and content-addressed,
	// so this is the same gate Activate would run, just pre-paid. Prepare fails
	// closed: a tampered or incompatible snapshot keeps the pod out of StateDormant
	// so the pool never offers it for a claim. When the inputs are absent (e.g.
	// AllowUnverified / a pre-digest pool) we skip this and Activate verifies as
	// before.
	if s.prepareSnapshotDir != "" && s.prepareExpectedDigest != "" {
		// Wait (bounded, ctx-aware) for the snapshot to be on disk before verifying
		// it. The pool can schedule this husk pod a moment before forkd finishes
		// writing the template snapshot to the shared node dir; without the wait the
		// verify below fails on the absent mem/vmstate and the pod crashloops, and a
		// pre-digest pool would instead load an absent snapshot at Activate into a
		// VMM that then dies and lingers as a dead warm slot. Mirrors the rootfs
		// wait below (issues #527, #73).
		for _, name := range []string{"mem", "vmstate"} {
			f := filepath.Join(s.prepareSnapshotDir, name)
			if err := waitForFile(ctx, f, rootfsTemplateWait); err != nil {
				_ = s.vm.Close()
				s.vm = nil
				return fmt.Errorf("husk: snapshot file %s not ready: %w", f, err)
			}
		}
		if err := s.verify(ActivateRequest{
			SnapshotDir:    s.prepareSnapshotDir,
			ExpectedDigest: s.prepareExpectedDigest,
		}); err != nil {
			_ = s.vm.Close()
			s.vm = nil
			return fmt.Errorf("husk: prepare-time snapshot verification failed: %w", err)
		}
		s.prepareVerified = true
	}

	// Per-activation rootfs CoW (opt-in): clone the template rootfs to this
	// activation's OWN file NOW, during the dormant pre-paid window, so the
	// Activate hot path is only load + handshake (the clone, especially a
	// full-copy fallback on a non-reflink filesystem, must never land on the hot
	// path). The clone source is read-only and content-addressed, so a clone taken
	// here is byte-identical to one taken at Activate. Fail closed: a clone failure
	// tears the dormant VMM down and keeps the pod out of StateDormant so the pool
	// never offers it.
	if s.rootfsTemplatePath != "" && s.rootfsCoWDir != "" {
		clonePath := filepath.Join(s.rootfsCoWDir, s.cfg.ID, "rootfs.ext4")
		// Create the clone's parent directory before handing the path to the
		// reflinker. The production seam (volume.ReflinkCopy) also MkdirAlls
		// (idempotent), but doing it here keeps the stub the owner of the clone
		// location so any reflinker, including a test fake, writes to a real dir.
		if err := os.MkdirAll(filepath.Dir(clonePath), 0o755); err != nil {
			_ = s.vm.Close()
			s.vm = nil
			return fmt.Errorf("husk: create per-activation rootfs dir: %w", err)
		}
		// Wait (bounded, ctx-aware) for forkd to finish writing the template
		// rootfs on the node before cloning it. The pool can schedule this husk
		// pod a moment before the build's rootfs.ext4 is visible on the shared
		// hostPath dir; crashing here would drop the pod into CrashLoopBackOff and
		// keep it out of the warm pool past a claim's deadline.
		if err := waitForFile(ctx, s.rootfsTemplatePath, rootfsTemplateWait); err != nil {
			_ = s.vm.Close()
			s.vm = nil
			return fmt.Errorf("husk: per-activation rootfs template %s not ready: %w", s.rootfsTemplatePath, err)
		}
		if err := s.reflink(s.rootfsTemplatePath, clonePath); err != nil {
			_ = s.vm.Close()
			s.vm = nil
			return fmt.Errorf("husk: clone per-activation rootfs: %w", err)
		}
		s.rootfsClonePath = clonePath
	}

	s.state = StateDormant
	return nil
}

// Activate loads the snapshot into the dormant VMM in place and waits for the
// guest agent to answer.
//
// It FAILS CLOSED: the stub must be dormant (else error and no result), and any
// snapshot-load or guest-readiness failure returns OK=false plus an error and
// leaves the stub NOT active. A failed Activate never reports a usable VM; the
// caller must treat the husk as unusable.
func (s *Stub) Activate(ctx context.Context, req ActivateRequest) (ActivateResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state != StateDormant {
		// AlreadyActive lets an idempotent caller adopt a VM that a prior Activate
		// already brought up but whose ack/bookkeeping was lost (issue #183),
		// instead of retrying a non-dormant VM forever.
		return ActivateResult{OK: false, AlreadyActive: s.state == StateActive, Error: fmt.Sprintf("activate in state %s: must be dormant", s.state)},
			fmt.Errorf("husk: activate in state %s: must be dormant", s.state)
	}
	if err := ctx.Err(); err != nil {
		return ActivateResult{OK: false, Error: err.Error()}, err
	}
	if req.SnapshotDir == "" {
		return ActivateResult{OK: false, Error: "activate: empty snapshot dir"},
			fmt.Errorf("husk: activate: empty snapshot dir")
	}

	// Same snapshot file layout the fork engine writes: SnapshotDir/mem and
	// SnapshotDir/vmstate.
	memFile := filepath.Join(req.SnapshotDir, "mem")
	vmStateFile := filepath.Join(req.SnapshotDir, "vmstate")

	start := time.Now()

	// Verify-on-activate gate: re-verify the snapshot BEFORE loading it, the same
	// fail-closed integrity + compatibility gate forkd's Fork path applies (digest
	// verify, issue #9, and snapcompat.Check, issue #32). A snapshot tampered on
	// the node disk after forkd's build-time verification, or one incompatible
	// with this node, is refused here and never restored. Runs before any VMM
	// load, so an unverified snapshot never touches the guest.
	//
	// Fast path: if Prepare already verified THIS exact snapshot (same dir + the
	// same content-addressed digest) during the dormant period, the read-only
	// immutable files cannot have changed, so we skip the ~680 MiB re-hash and go
	// straight to load. Any mismatch (a different dir/digest than prepared, or no
	// prepare-time verification) re-verifies here, fail-closed, exactly as before.
	if !(s.prepareVerified && req.SnapshotDir == s.prepareSnapshotDir && req.ExpectedDigest == s.prepareExpectedDigest) {
		if err := s.verify(req); err != nil {
			werr := fmt.Errorf("husk: snapshot verification failed: %w", err)
			return ActivateResult{OK: false, Error: werr.Error()}, werr
		}
	}

	// In-pod egress filter (the load-bearing isolation control). This MUST run
	// BEFORE LoadSnapshotWithOverrides: Firecracker requires the host tap to exist
	// at restore time, and the snapshot's baked NIC is remapped to THIS tap. So we
	// create the tap + install the default-deny egress chain (with the
	// unconditional metadata block) here, then bind the baked NIC (NetIfaceID) to
	// the SAME tap the filter created, so the restored VM comes up on a tap that
	// already exists and is already governed by the egress chain. Deriving the tap
	// from the guest IP keeps the stub's filter and the NIC remap in agreement
	// without a shared allocator (the NIC/tap binding risk: a mismatch here is a
	// VM with a NIC backed by no tap). FAIL CLOSED: a filter error means the VM
	// would have UNFILTERED egress (or a broken NIC), so we never load it. The
	// guest IP and tap carry no secrets.
	overrides := req.NetworkOverrides
	if s.netRunner != nil && req.Network != nil {
		tap := netconf.DeriveTapName(req.Network.GuestIP)
		cfg := NetfilterConfig{
			Tap:          tap,
			GuestIP:      net.ParseIP(req.Network.GuestIP),
			HostIP:       net.ParseIP(req.Network.GatewayIP),
			Egress:       v1.EgressPolicy(req.Egress),
			Allow:        req.Allow,
			BlockNetwork: req.BlockNetwork,
			AllowCIDRs:   req.AllowCIDRs,
			Inbound:      v1.InboundPolicy(req.Inbound),
			InboundCIDRs: req.InboundCIDRs,
			ResolverIP:   net.ParseIP(req.Network.ResolverIP),
		}
		if cfg.Egress == "" {
			cfg.Egress = v1.EgressDeny
		}
		if err := applyEgressFilter(ctx, s.netRunner, s.enableForwarding, cfg); err != nil {
			werr := fmt.Errorf("husk: apply in-pod egress filter: %w", err)
			return ActivateResult{OK: false, Error: werr.Error()}, werr
		}
		// Bind the snapshot's baked NIC to the tap the filter just created, so the
		// restored VM's NIC has a backing tap governed by the egress chain. This
		// override pins HostDevName regardless of what the caller passed, which is
		// what keeps the tap-vs-NIC binding correct.
		overrides = []firecracker.NetworkOverride{{
			IfaceID:     firecracker.NetIfaceID,
			HostDevName: tap,
		}}
		s.activeTap = tap

		// Per-pod DNS proxy for name-based egress: resolve only allowlisted names
		// and pin each resolved IP into this tap's dynamic set. It binds the
		// resolver socket only (independent of VM state), so starting it here is
		// safe. IP-only allowlists (empty name set) still run with an empty
		// registry. FAIL CLOSED on a bad registry: do not load the VM.
		if req.Network.ResolverIP != "" && s.dnsUpstream != "" {
			reg, _, derr := buildEgressDNSRegistry(req.Network.GuestIP, req.Allow)
			if derr != nil {
				werr := fmt.Errorf("husk: build dns registry: %w", derr)
				return ActivateResult{OK: false, Error: werr.Error()}, werr
			}
			nftRun := s.nftRunner
			if nftRun == nil {
				nftRun = func(argv []string) error { return s.netRunner(ctx, argv, "") }
			}
			proxy := newEgressDNSProxy(reg, tap, dnsproxy.ParseUpstreams(s.dnsUpstream), nftRun)
			go func() { _ = proxy.ListenAndServe(net.JoinHostPort(req.Network.ResolverIP, "53")) }()
			s.dnsProxy = proxy
		}
	}

	// Load the snapshot PAUSED (resume=false). The rootfs drive rebind below MUST
	// happen before the guest runs, and PATCH /drives on the ROOT device of an
	// already-RESUMED VM both leaves a write window (any writeback between resume
	// and the rebind hits the SHARED template rootfs) and may be rejected by
	// Firecracker. Loading paused lets us rebind while the guest is frozen, then
	// resume explicitly. nil overrides restores exactly as before.
	if err := s.vm.LoadSnapshotWithOverrides(memFile, vmStateFile, false, overrides); err != nil {
		// Fail closed: the snapshot did not load; the VM is not usable. Leave
		// state dormant so a retry (or teardown) can decide what to do.
		werr := fmt.Errorf("husk: load snapshot from %s: %w", req.SnapshotDir, err)
		return ActivateResult{OK: false, Error: werr.Error()}, werr
	}

	// Rebind the baked "rootfs" drive to THIS activation's CoW clone while the VM
	// is still PAUSED (loaded, not yet resumed), so the guest never writes a single
	// block through the shared template rootfs. This is the husk analog of the fork
	// engine's per-fork volume drive rebind: the snapshot bakes the rootfs block
	// device at path_on_host, and Firecracker's runtime API controller accepts a
	// drive path_on_host PATCH in the Paused state with no root-device restriction.
	// Skipped when no per-activation clone was prepared (the prior shared-rootfs
	// behavior). Fail closed: a rebind failure means the VM is still pointed at the
	// shared template rootfs, which is exactly the corruption hazard this prevents,
	// so do NOT resume or mark active. The drive id and path carry no secrets.
	if s.rootfsClonePath != "" {
		if err := s.vm.PatchDrive("rootfs", s.rootfsClonePath); err != nil {
			werr := fmt.Errorf("husk: rebind rootfs drive to per-activation clone: %w", err)
			return ActivateResult{OK: false, Error: werr.Error()}, werr
		}
	}

	// Resume the VM only AFTER the rootfs drive is rebound, so the guest comes up
	// already bound to its own per-activation rootfs clone, never the shared
	// template. Fail closed: if the resume is rejected the VM never runs, so do NOT
	// mark active.
	if err := s.vm.Resume(); err != nil {
		werr := fmt.Errorf("husk: resume VM after rootfs rebind: %w", err)
		return ActivateResult{OK: false, Error: werr.Error()}, werr
	}

	vsockPath := s.vm.VsockHostPath(firecracker.VsockRelPath)
	if err := s.ready(ctx, vsockPath, s.readyTimeout); err != nil {
		// Fail closed: the snapshot loaded but the guest never answered, so we
		// cannot vouch for the VM. Do NOT mark active or report a usable VM.
		werr := fmt.Errorf("husk: guest not ready after activate: %w", err)
		return ActivateResult{OK: false, Error: werr.Error()}, werr
	}

	// Fork-correctness handshake. The restored guest is a byte-for-byte copy of
	// the snapshot, so it shares the snapshot's CRNG and clock state. Reseed it
	// with fresh entropy and deliver claim-time env/secrets BEFORE marking the
	// VM active. The entropy and secret values are held only in memory here and
	// are NEVER logged.
	entropy := make([]byte, entropySize)
	if _, err := rand.Read(entropy); err != nil {
		// Fail closed: without fresh entropy we cannot reseed, so the VM is not
		// safe to serve. The error mentions no entropy bytes.
		werr := fmt.Errorf("husk: generate fork entropy: %w", err)
		return ActivateResult{OK: false, Error: werr.Error()}, werr
	}
	s.generation++
	if err := s.notify(vsockPath, s.generation, entropy, req); err != nil {
		// Fail closed: the guest did not complete the reseed handshake, so it may
		// still share its siblings' CRNG state. Leave the VM NOT active. The
		// error carries no entropy or secret values.
		werr := fmt.Errorf("husk: fork-correctness handshake failed: %w", err)
		return ActivateResult{OK: false, Error: werr.Error()}, werr
	}

	// Wire the activated VM into the in-pod sandbox HTTP API (exec/files) before
	// reporting success, so the endpoint the claim advertises is reachable the
	// moment the claim goes Ready. The hook registers the sandbox + its bearer
	// token with a daemon.SandboxAPI and serves it on the sandbox port. FAIL
	// CLOSED: if the sandbox API cannot be served, the VM is not actually usable
	// by a tenant, so do NOT mark active or report OK. The token is a secret and
	// is never logged here. The hook is nil for the control-socket CI driver and
	// unit paths that do not serve the sandbox API.
	if s.onActivated != nil {
		if err := s.onActivated(vsockPath, req.Token); err != nil {
			werr := fmt.Errorf("husk: serve sandbox API for activated VM: %w", err)
			return ActivateResult{OK: false, Error: werr.Error()}, werr
		}
	}

	latency := time.Since(start)
	s.state = StateActive
	return ActivateResult{
		OK:        true,
		VsockPath: vsockPath,
		LatencyMs: float64(latency.Microseconds()) / 1000.0,
	}, nil
}

// ForkSnapshot snapshots the CURRENTLY RUNNING VM this stub holds, in place, so
// the controller can restore N independent child husk pods from it. It is the
// husk analog of the fork engine's ForkRunning: the source VM is owned by THIS
// husk pod's stub (not forkd's engine), so the only way to live-fork it is for
// the owning stub to snapshot it.
//
// It pauses the VM (CreateSnapshot requires a paused VM), writes a Full snapshot
// to req.SnapshotDir/{mem,vmstate} (the same layout Activate reads), then resumes
// it UNLESS req.PauseSource is set, in which case it leaves the source paused.
// The stub stays StateActive throughout: it still owns its one VM.
//
// FAIL CLOSED: it requires StateActive (else error, no snapshot); a pause,
// snapshot-create, or resume failure returns OK=false plus an error. On a
// snapshot failure it still attempts to resume the source so a transient
// snapshot error does not leave a live sandbox frozen. The fork id and snapshot
// paths carry no secrets.
func (s *Stub) ForkSnapshot(ctx context.Context, req ForkSnapshotRequest) (ForkSnapshotResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state != StateActive {
		return ForkSnapshotResult{OK: false, Error: fmt.Sprintf("fork-snapshot in state %s: must be active", s.state)},
			fmt.Errorf("husk: fork-snapshot in state %s: must be active", s.state)
	}
	if err := ctx.Err(); err != nil {
		return ForkSnapshotResult{OK: false, Error: err.Error()}, err
	}
	if req.SnapshotDir == "" {
		return ForkSnapshotResult{OK: false, Error: "fork-snapshot: empty snapshot dir"},
			fmt.Errorf("husk: fork-snapshot: empty snapshot dir")
	}
	if err := s.confineToForksDir(req.SnapshotDir); err != nil {
		return ForkSnapshotResult{OK: false, Error: err.Error()}, err
	}

	memFile := filepath.Join(req.SnapshotDir, "mem")
	vmStateFile := filepath.Join(req.SnapshotDir, "vmstate")
	if err := os.MkdirAll(req.SnapshotDir, 0o755); err != nil {
		werr := fmt.Errorf("husk: create fork snapshot dir %s: %w", req.SnapshotDir, err)
		return ForkSnapshotResult{OK: false, Error: werr.Error()}, werr
	}

	start := time.Now()

	if err := s.vm.Pause(); err != nil {
		werr := fmt.Errorf("husk: pause source VM for fork snapshot: %w", err)
		return ForkSnapshotResult{OK: false, Error: werr.Error()}, werr
	}

	if err := s.vm.CreateSnapshot(memFile, vmStateFile); err != nil {
		// Best effort: resume the source so a transient snapshot error does not
		// leave a tenant's live sandbox frozen. The resume error is reported only
		// if the snapshot itself succeeded; here the snapshot already failed.
		_ = s.vm.Resume()
		werr := fmt.Errorf("husk: create fork snapshot in %s: %w", req.SnapshotDir, err)
		return ForkSnapshotResult{OK: false, Error: werr.Error()}, werr
	}

	// Resume the source UNLESS the fork asked to keep it paused. PauseSource
	// trades a brief source interruption for a colder, quiescent snapshot.
	if !req.PauseSource {
		if err := s.vm.Resume(); err != nil {
			werr := fmt.Errorf("husk: resume source VM after fork snapshot: %w", err)
			return ForkSnapshotResult{OK: false, Error: werr.Error()}, werr
		}
	}

	latency := time.Since(start)
	return ForkSnapshotResult{
		OK:          true,
		SnapshotDir: req.SnapshotDir,
		LatencyMs:   float64(latency.Microseconds()) / 1000.0,
	}, nil
}

// RemoveForkSnapshot deletes a fork snapshot dir this stub previously created. It
// is the GC counterpart of ForkSnapshot: the controller calls it when the owning
// SandboxFork is deleted so the node-local snapshot does not outlive its owner.
// It does not touch the VM and is safe in any state. The path carries no secret.
func (s *Stub) RemoveForkSnapshot(req ForkSnapshotRequest) error {
	if req.SnapshotDir == "" {
		return fmt.Errorf("husk: remove fork snapshot: empty snapshot dir")
	}
	if err := s.confineToForksDir(req.SnapshotDir); err != nil {
		return err
	}
	if err := os.RemoveAll(req.SnapshotDir); err != nil {
		return fmt.Errorf("husk: remove fork snapshot %s: %w", req.SnapshotDir, err)
	}
	return nil
}

// DehydrateWorkspace captures the active VM's guest /workspace into the node CAS
// and returns the content manifest digest. It is the node-side delegate of the
// controller's dehydrate-on-terminate: the controller owns the VM's vsock and
// the node CAS through THIS husk pod, not in-process, so it asks the owning stub
// to run the capture. The stub reuses the KVM-proven internal/workspace.Dehydrate
// (vsock TarDir over /workspace, then store into the node CAS); it does NOT
// reimplement tar or CAS.
//
// FAIL CLOSED: it requires StateActive (else error, no capture) and a configured
// node CAS (else error). Secret/credential paths in req.ExcludePaths are stripped
// from the captured tree per the no-secrets-in-revisions policy. The manifest
// digest is a content address, NOT a secret; workspace CONTENT bytes are never
// logged or returned in an error. The stub stays StateActive throughout.
func (s *Stub) DehydrateWorkspace(ctx context.Context, req DehydrateWorkspaceRequest) (DehydrateWorkspaceResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state != StateActive {
		werr := fmt.Errorf("husk: dehydrate-workspace in state %s: must be active", s.state)
		return DehydrateWorkspaceResult{OK: false, Error: werr.Error()}, werr
	}
	if err := ctx.Err(); err != nil {
		return DehydrateWorkspaceResult{OK: false, Error: err.Error()}, err
	}
	if s.casStore == nil {
		werr := fmt.Errorf("husk: dehydrate-workspace: no node CAS configured; set --cas-dir so the stub can persist a workspace revision")
		return DehydrateWorkspaceResult{OK: false, Error: werr.Error()}, werr
	}

	agent, closeAgent, err := s.dialWorkspaceAgent()
	if err != nil {
		return DehydrateWorkspaceResult{OK: false, Error: err.Error()}, err
	}
	defer closeAgent()

	start := time.Now()
	digest, err := workspace.Dehydrate(ctx, agent, s.casStore, req.ExcludePaths, req.CapturePaths)
	if err != nil {
		// The error carries the operation and the transport/store error only; it
		// never carries workspace content bytes.
		werr := fmt.Errorf("husk: dehydrate workspace: %w", err)
		return DehydrateWorkspaceResult{OK: false, Error: werr.Error()}, werr
	}
	if err := digest.Validate(); err != nil {
		werr := fmt.Errorf("husk: dehydrate workspace produced an invalid content digest: %w", err)
		return DehydrateWorkspaceResult{OK: false, Error: werr.Error()}, werr
	}

	// Optional content-hash diff against the parent head. The controller is not on
	// the node and cannot read either manifest, so it asks the stub (which owns the
	// node CAS) to compute the diff here from the two manifests, reusing the same
	// internal/workspace.DiffManifests helper the in-controller path used. An empty
	// ParentManifestDigest skips the diff (a {diff: false} terminate); an empty-but-
	// requested parent (the first revision in a workspace) diffs the child against an
	// empty manifest, so the whole child records as additions. The diff carries
	// content path names only, never chunk bytes; an error names manifests/digests
	// (content addresses), never content.
	var diff *workspace.Diff
	if req.ParentManifestDigest != "" {
		parent := cas.Digest(req.ParentManifestDigest)
		if err := parent.Validate(); err != nil {
			werr := fmt.Errorf("husk: dehydrate workspace: invalid parent manifest digest: %w", err)
			return DehydrateWorkspaceResult{OK: false, Error: werr.Error()}, werr
		}
		d, derr := s.diffManifests(parent, digest)
		if derr != nil {
			werr := fmt.Errorf("husk: dehydrate workspace: compute diff against parent: %w", derr)
			return DehydrateWorkspaceResult{OK: false, Error: werr.Error()}, werr
		}
		diff = &d
	}

	latency := time.Since(start)
	return DehydrateWorkspaceResult{
		OK:             true,
		ManifestDigest: string(digest),
		Diff:           diff,
		LatencyMs:      float64(latency.Microseconds()) / 1000.0,
	}, nil
}

// diffManifests reads the parent and child manifests from the node CAS and
// computes the content-hash diff between them with internal/workspace.DiffManifests
// (the same helper the in-controller diff path used). It works from the manifests
// (path -> chunk-digest lists), never the chunk bytes, so it is cheap and never
// materializes content. The caller holds s.mu and has already validated both
// digests.
func (s *Stub) diffManifests(parent, child cas.Digest) (workspace.Diff, error) {
	parentManifest, err := s.casStore.GetManifest(parent)
	if err != nil {
		return workspace.Diff{}, fmt.Errorf("read parent manifest %s: %w", parent, err)
	}
	childManifest, err := s.casStore.GetManifest(child)
	if err != nil {
		return workspace.Diff{}, fmt.Errorf("read child manifest %s: %w", child, err)
	}
	return workspace.DiffManifests(parentManifest, childManifest), nil
}

// HydrateWorkspace restores a node-CAS manifest into the active VM's guest
// /workspace (the inverse of DehydrateWorkspace), reusing the KVM-proven
// internal/workspace.Hydrate (materialize the manifest from the node CAS, then
// vsock UntarDir into /workspace, which sanitizes every member against
// traversal). It is the node-side delegate of the controller's hydrate-on-activate.
//
// FAIL CLOSED: it requires StateActive, a configured node CAS, and a valid
// content-address manifest digest (else error, no restore). The manifest digest
// is a content address, NOT a secret; workspace CONTENT bytes are never logged.
// The stub stays StateActive throughout.
func (s *Stub) HydrateWorkspace(ctx context.Context, req HydrateWorkspaceRequest) (HydrateWorkspaceResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state != StateActive {
		werr := fmt.Errorf("husk: hydrate-workspace in state %s: must be active", s.state)
		return HydrateWorkspaceResult{OK: false, Error: werr.Error()}, werr
	}
	if err := ctx.Err(); err != nil {
		return HydrateWorkspaceResult{OK: false, Error: err.Error()}, err
	}
	if s.casStore == nil {
		werr := fmt.Errorf("husk: hydrate-workspace: no node CAS configured; set --cas-dir so the stub can restore a workspace revision")
		return HydrateWorkspaceResult{OK: false, Error: werr.Error()}, werr
	}
	digest := cas.Digest(req.ManifestDigest)
	if err := digest.Validate(); err != nil {
		werr := fmt.Errorf("husk: hydrate-workspace: %w", err)
		return HydrateWorkspaceResult{OK: false, Error: werr.Error()}, werr
	}

	agent, closeAgent, err := s.dialWorkspaceAgent()
	if err != nil {
		return HydrateWorkspaceResult{OK: false, Error: err.Error()}, err
	}
	defer closeAgent()

	start := time.Now()
	if err := workspace.Hydrate(ctx, agent, s.casStore, digest); err != nil {
		werr := fmt.Errorf("husk: hydrate workspace: %w", err)
		return HydrateWorkspaceResult{OK: false, Error: werr.Error()}, werr
	}
	latency := time.Since(start)
	return HydrateWorkspaceResult{
		OK:        true,
		LatencyMs: float64(latency.Microseconds()) / 1000.0,
	}, nil
}

// dialWorkspaceAgent resolves the guest-agent bulk-tar transport for the active
// VM and returns it plus a close hook. The production transport is a vsock
// client (closed by the hook); a test transport has no close (the hook is a
// no-op). The caller holds s.mu.
func (s *Stub) dialWorkspaceAgent() (workspace.VsockTransport, func(), error) {
	vsockPath := s.vm.VsockHostPath(s.vsockRelPath)
	agent, err := s.wsTransport(vsockPath)
	if err != nil {
		return nil, func() {}, fmt.Errorf("husk: connect guest agent for workspace transfer: %w", err)
	}
	closeHook := func() {}
	if c, ok := agent.(io.Closer); ok {
		closeHook = func() { _ = c.Close() }
	}
	return agent, closeHook, nil
}

// confineToForksDir refuses a fork-snapshot / remove-fork-snapshot SnapshotDir
// that resolves outside the configured forks dir, so the control channel can
// never be steered to write or delete a path outside the mounted node forks dir.
// It is a fail-closed defense-in-depth gate; when no forks dir is configured
// (the empty default) it permits any dir, preserving the prior behavior. The dir
// carries no secret, so it is safe to name in the error.
func (s *Stub) confineToForksDir(dir string) error {
	if s.forksDir == "" {
		return nil
	}
	base := filepath.Clean(s.forksDir)
	target := filepath.Clean(dir)
	if target != base && !strings.HasPrefix(target, base+string(filepath.Separator)) {
		return fmt.Errorf("husk: fork snapshot dir %q is outside the configured forks dir %q", dir, s.forksDir)
	}
	return nil
}

// Serve accepts control connections on ln and dispatches each to Activate,
// replying with the ActivateResult.
//
// A husk pod is LONG-LIVED: it holds its single active VM until the pod is
// terminated. So a SUCCESSFUL activate does NOT end Serve. After the VM is
// active Serve keeps running, holding the live VM (which now serves the
// sandbox) and rejecting further activate attempts via Activate's state check,
// until ctx is cancelled or the listener closes. Before a successful activate
// it likewise keeps serving so a failed-closed activate can be retried.
//
// Serve never tears the VM down: it returns nil on ctx cancel / listener close
// and leaves the VM running. The caller (cmd/husk-stub) calls Close on real
// shutdown to kill the VM. Per-connection errors are returned to the peer in
// the result and do not stop the server.
func (s *Stub) Serve(ctx context.Context, ln net.Listener) error {
	// Unblock Accept when the context is cancelled.
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("husk: accept control connection: %w", err)
		}
		// The activate result is sent to the peer; whether it succeeded or not,
		// the husk keeps serving and holding its VM until shutdown.
		s.handleConn(ctx, conn)
	}
}

// handleConn reads one ActivateRequest, runs Activate, and writes the result.
// Connection-level read/write failures are logged to stderr (paths only, no
// secrets) and do not propagate; the server keeps running.
func (s *Stub) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	req, err := ReadRequest(conn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "husk: read activate request: %v\n", err)
		return
	}
	res, _ := s.Activate(ctx, req)
	if werr := WriteResult(conn, res); werr != nil {
		fmt.Fprintf(os.Stderr, "husk: write activate result: %v\n", werr)
		// The result may not have reached the peer, but the VM state is what it
		// is; the husk holds the VM per the result we computed.
	}
}

// State returns the current lifecycle state.
func (s *Stub) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// ErrVMMDead is returned by MonitorVMM when the Firecracker VMM has been
// unresponsive for the configured number of consecutive pings. The husk pod
// treats it as fatal so the kubelet restarts the container.
var ErrVMMDead = errors.New("husk: firecracker VMM is unresponsive")

// pingVMM checks the prepared VMM's Firecracker API socket. It grabs the vmm
// reference under the lock and pings OUTSIDE it so a slow ping never blocks
// Activate/Close.
func (s *Stub) pingVMM() error {
	s.mu.Lock()
	vm := s.vm
	s.mu.Unlock()
	if vm == nil {
		return fmt.Errorf("husk: no VMM prepared")
	}
	return vm.Ping()
}

// MonitorVMM watches the dormant/active Firecracker VMM and returns ErrVMMDead
// after failures consecutive ping failures spaced interval apart. It returns nil
// when ctx is cancelled (a normal shutdown is not a death).
//
// The husk pod runs this after Prepare. A husk-stub pod that started before its
// snapshot existed, or whose Firecracker died for any other reason, leaves a
// defunct VMM while husk-stub (PID 1) keeps the TCP control listener open, so the
// pod stays 1/1 Ready and the pool counts it a warm slot; every claim that lands
// on it then fails connection-refused to the dead socket. By exiting on a dead
// VMM the pod goes NotReady and the kubelet restarts it (RestartPolicy Always),
// which re-runs Prepare and, once the snapshot is present, serves a healthy slot;
// the pod self-heals instead of advertising a dead slot (issue #527).
//
// The consecutive-failure threshold tolerates a transient blip, for example a
// slow API call while Activate drives the same socket, without flapping the pod.
func (s *Stub) MonitorVMM(ctx context.Context, interval time.Duration, failures int) error {
	if failures < 1 {
		failures = 1
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	consecutive := 0
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := s.pingVMM(); err != nil {
				consecutive++
				if consecutive >= failures {
					return fmt.Errorf("%w after %d consecutive failed pings: %v", ErrVMMDead, consecutive, err)
				}
			} else {
				consecutive = 0
			}
		}
	}
}

// Close tears down the VMM if one was prepared. It is safe to call in any state.
func (s *Stub) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Best effort: remove this activation's rootfs CoW clone so it does not
	// outlive the pod. A reflink clone shares extents with the template until
	// written, so removing it frees only the activation's own divergent blocks.
	// Path only is logged on failure; the clone carries no secrets. Done before
	// the vm == nil early return so a clone is reaped even when no VMM is held.
	if s.rootfsClonePath != "" {
		if rmErr := os.Remove(s.rootfsClonePath); rmErr != nil && !os.IsNotExist(rmErr) {
			fmt.Fprintf(os.Stderr, "husk: remove per-activation rootfs clone %s: %v\n", s.rootfsClonePath, rmErr)
		}
		s.rootfsClonePath = ""
	}

	// Stop the per-pod DNS proxy and tear down the in-pod egress filter (tap +
	// per-tap nft state) for the VM this stub held. Best effort: a teardown error
	// must not block VMM close. Done before the vm == nil early return so a
	// filter applied during a failed activate is still reaped.
	if s.dnsProxy != nil {
		_ = s.dnsProxy.Shutdown(context.Background())
		s.dnsProxy = nil
	}
	if s.netRunner != nil && s.activeTap != "" {
		_ = teardownEgressFilter(context.Background(), s.netRunner, s.activeTap)
		s.activeTap = ""
	}

	if s.vm == nil {
		return nil
	}
	err := s.vm.Close()
	s.vm = nil
	s.state = StateNew
	return err
}
