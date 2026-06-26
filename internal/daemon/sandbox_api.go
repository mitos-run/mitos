package daemon

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/grpc"
	"mitos.run/mitos/internal/apierr"
	"mitos.run/mitos/internal/sandboxrpc"
	"mitos.run/mitos/internal/vsock"
	internalv1 "mitos.run/mitos/proto/sandbox/controlv1"
	"mitos.run/mitos/proto/sandbox/v1/sandboxv1connect"
)

// SandboxAPI exposes HTTP endpoints for exec/files on sandboxes managed by this forkd.
// The SDK and sandbox-server talk to this API to interact with running sandboxes.
// All guest communication uses gRPC on vsock.AgentGRPCPort (53); the legacy
// JSON-lines port 52 is no longer opened here.
type SandboxAPI struct {
	mu       sync.RWMutex
	tokens   map[string]string // sandbox ID → bearer token; values never logged
	vsockDir string            // directory containing vsock UDS files
	// streamPaths maps sandbox ID to the vsock UDS path for dialing per-call
	// gRPC connections to the guest agent on AgentGRPCPort (53). Guarded by mu.
	streamPaths map[string]string
	// lastActivity records the time of the most recent exec or file call per
	// sandbox, guarded by mu. Absent until the first touch; used by the GC
	// reconciler via ListSandboxes to drive idle reaping.
	lastActivity map[string]time.Time
	// now is the clock used to stamp lastActivity. Defaults to time.Now;
	// tests override it for determinism.
	now func() time.Time
	// unixFallback allows dialGuestGRPC to fall back
	// to the agent's fixed local unix socket when the vsock UDS path does not
	// exist. Opt-in: see EnableUnixFallback.
	unixFallback bool
	// allowTokenless permits requests for sandboxes that have NO registered
	// token. Opt-in: see AllowTokenless.
	allowTokenless bool
	// singleSandbox, when set, switches the API into single-sandbox mode: it
	// serves exactly ONE sandbox, registered locally under singleSandboxID, and
	// the auth gate (requireBearer, ptyAuth) validates the presented bearer
	// against that one sandbox's token regardless of the request's "sandbox" id,
	// then routes the request to singleSandboxID. This is used ONLY by the
	// husk-stub, whose OnActivated hook registers its single VM under a fixed
	// local id while the SDK addresses the in-pod API with the claim's
	// status.sandboxID (the husk pod name), which never equals that local id.
	// forkd NEVER sets it, so forkd's multi-sandbox per-id token lookup is
	// byte-identical: a token for sandbox A still cannot authorize sandbox B.
	// Opt-in: see SetSingleSandbox.
	singleSandbox bool
	// singleSandboxID is the one local sandbox id served in single-sandbox mode.
	singleSandboxID string
	// auditor records a structured event after each exec/file op. Defaults to
	// NopAuditor (auditing off); set via SetAuditor. It only ever sees safe
	// summaries (command, path, byte count): never file content or secrets.
	auditor Auditor

	// maxStreams is the per-sandbox ceiling on concurrent OPEN streams
	// (production-blocker #2, cap 3). Each streaming exec, run_code, and PTY
	// holds a dedicated vsock connection plus host goroutines for the command
	// lifetime; without a cap a single tenant could open unbounded streams and
	// exhaust host vsock connections and goroutines. acquireStream enforces it at
	// stream OPEN (off the activate path); a NEW stream over the cap is rejected
	// with 429, existing streams are never killed. Zero or negative disables the
	// cap (unbounded, the prior behavior). Set via SetMaxStreamsPerSandbox.
	maxStreams int
	// openStreams counts the currently OPEN streams per sandbox id, guarded by
	// mu. acquireStream increments on open and the returned release decrements on
	// close, deleting the entry at zero so the map does not grow across sandbox
	// lifetimes. A non-zero count is the work-aware idle signal (issue #218): a
	// sandbox with a live streaming exec, run_code, or PTY (a background job) is
	// running ACTUAL work, so the idle reaper must not reap it mid-run.
	openStreams map[string]int
	// deadlines records a live TTL deadline per sandbox id set via set_timeout
	// (issue #218), guarded by mu. It is the running-sandbox TTL the lifetime
	// reaper reads through ListSandboxes; absent until the first set_timeout. The
	// value is the absolute wall-clock instant the sandbox should be reaped.
	deadlines map[string]time.Time
	// paused records sandbox ids whose clock is stopped by a pause (issue #218),
	// guarded by mu. A paused sandbox is held (full state snapshotted on a real
	// engine) and must never be idle-reaped: its idle clock does not run while
	// paused. resume clears the entry.
	paused map[string]bool
	// enginePauser, when set (forkd), drives the real engine's pause/resume
	// (full memory+fs snapshot and restore) from the pause/resume HTTP
	// endpoints. nil on the standalone server and in unit tests, where the
	// endpoints record the held state only. Set via SetEnginePauser.
	enginePauser EnginePauser
	// maxExecTimeout is the ceiling (seconds) on a requested exec or run_code
	// timeout (issue #216). A request over the ceiling is REJECTED with the typed
	// timeout_too_large code, never silently reduced, so a requested deadline is
	// always honored or rejected. Defaults to defaultMaxExecTimeoutSeconds (24h),
	// chosen to clear the exec_background one-day default. <=0 disables the
	// ceiling (any timeout honored). Set via SetMaxExecTimeoutSeconds.
	maxExecTimeout int
	// forwards tracks the live host-side port forwards per sandbox id (issue
	// #228), guarded by mu. Each forward is a host TCP listener bridged over a
	// vsock tunnel to a guest loopback port. UnregisterSandbox closes all of a
	// sandbox's forwards so no host listener or tunnel goroutine outlives the
	// sandbox. Absent until the first ForwardPort.
	forwards map[string][]*portForward
	// maxForwards is the per-sandbox ceiling on concurrent OPEN port forwards
	// (issue #228). Each forward holds a host TCP listener plus a tunnel goroutine
	// per accepted connection; without a cap one sandbox could exhaust host
	// sockets. A NEW forward over the cap is rejected. Zero or negative disables
	// the cap. Set via SetMaxForwardsPerSandbox; defaults to defaultMaxForwards.
	maxForwards int
	// vitalsLabels records the claim/pool/workspace identity per sandbox id,
	// guarded by mu. The forkd Fork path sets it so the /v1/vitals guest
	// telemetry snapshot (Layer 3, issue #164) is labeled with the same identity
	// the OTel spans and metrics carry. Absent until SetVitalsLabels is called;
	// an unlabeled sandbox returns empty label fields, never a fabricated one.
	// The labels are k8s object names, never secrets.
	vitalsLabels map[string]VitalsLabels

	// maxExpose is the per-sandbox ceiling on concurrent OPEN expose tunnels
	// (authenticated guest HTTP proxy, slice-1 follow-up). Each expose request
	// opens its own vsock PortForward tunnel; without a cap a single tenant could
	// open unbounded tunnels and exhaust host vsock connections and goroutines. A
	// NEW tunnel over the cap is rejected with 429; existing tunnels are never
	// killed by the cap. Zero or negative disables the cap (unbounded). Set via
	// SetMaxExposePerSandbox; defaults to defaultMaxExpose.
	maxExpose int
	// openExpose counts the currently OPEN expose tunnels per sandbox id, guarded
	// by mu. acquireExpose increments on open and the returned release decrements
	// on close, deleting the entry at zero so the map does not grow across sandbox
	// lifetimes.
	openExpose map[string]int
	// exposeConns tracks every live expose net.Conn per sandbox id, guarded by mu.
	// In ProxyHTTP's dial closure each created pfConn is registered here so that
	// CloseExpose (called by UnregisterSandbox on terminate) can close every
	// in-flight tunnel and prevent tunnel goroutines from outliving the sandbox.
	// Absent until the first expose dial; cleared by CloseExpose.
	exposeConns map[string]map[net.Conn]struct{}
}

// defaultMaxExecTimeoutSeconds is the default ceiling on a requested exec or
// run_code timeout: 24 hours. It clears the SDK exec_background one-day default
// so a long-running background command is honored, while still bounding an
// absurd requested deadline. The determinism rule (issue #216) is that a request
// over this ceiling is rejected with timeout_too_large, never silently reduced.
const defaultMaxExecTimeoutSeconds = 86400

func NewSandboxAPI(vsockDir string) *SandboxAPI {
	return &SandboxAPI{
		tokens:         make(map[string]string),
		vsockDir:       vsockDir,
		streamPaths:    make(map[string]string),
		lastActivity:   make(map[string]time.Time),
		now:            time.Now,
		auditor:        NopAuditor{},
		openStreams:    make(map[string]int),
		deadlines:      make(map[string]time.Time),
		paused:         make(map[string]bool),
		maxExecTimeout: defaultMaxExecTimeoutSeconds,
		vitalsLabels:   make(map[string]VitalsLabels),
		forwards:       make(map[string][]*portForward),
		maxForwards:    defaultMaxForwards,
		maxExpose:      defaultMaxExpose,
		openExpose:     make(map[string]int),
		exposeConns:    make(map[string]map[net.Conn]struct{}),
	}
}

// SetMaxExecTimeoutSeconds sets the ceiling on a requested exec or run_code
// timeout (issue #216). A request over the ceiling is rejected with the typed
// timeout_too_large code, never silently reduced. n<=0 disables the ceiling.
// Must be called before the API serves requests; the field is not synchronized.
func (api *SandboxAPI) SetMaxExecTimeoutSeconds(n int) {
	api.maxExecTimeout = n
}

// checkTimeout enforces the requested-timeout ceiling for sandboxID and the
// requested timeout (seconds). It returns nil when the timeout is honored (at or
// under the ceiling, or the ceiling is disabled). When the timeout exceeds the
// ceiling it returns the typed timeout_too_large Error (carrying the requested
// value and the ceiling in context) so the caller can reject the request
// deterministically rather than silently clamp it. A nil return is the
// honored-timeout sentinel; the error is built only on the reject path so the
// remediation static check never sees an empty Error literal.
func (api *SandboxAPI) checkTimeout(sandboxID string, timeout int) *apierr.Error {
	if api.maxExecTimeout <= 0 || timeout <= api.maxExecTimeout {
		return nil
	}
	e := apierr.Get(apierr.CodeTimeoutTooLarge).
		WithCause(fmt.Sprintf("requested timeout %ds exceeds the ceiling of %ds", timeout, api.maxExecTimeout)).
		WithContext(map[string]any{
			"sandbox":       sandboxID,
			"requested_s":   timeout,
			"max_timeout_s": api.maxExecTimeout,
		})
	return &e
}

// SetMaxStreamsPerSandbox sets the per-sandbox ceiling on concurrent OPEN
// streams (streaming exec, run_code, PTY). A NEW stream opened while a sandbox
// is already at the cap is rejected with 429; existing streams are never
// killed. n<=0 disables the cap (unbounded). Must be called before the API
// serves requests; the field is not synchronized.
func (api *SandboxAPI) SetMaxStreamsPerSandbox(n int) {
	api.maxStreams = n
}

// acquireStream reserves one concurrent-stream slot for sandboxID, enforcing the
// per-sandbox cap (production-blocker #2, cap 3). It returns a release func and
// true when admitted; the caller MUST call release exactly once (defer) when the
// stream closes. It returns false when the sandbox is already at the cap, in
// which case the caller must reject the NEW stream and never call release. The
// cap is checked here, at stream OPEN, before the dedicated vsock connection is
// dialed; it is a single map lookup under mu and never touches the activate or
// fork hot path. maxStreams<=0 disables the cap.
func (api *SandboxAPI) acquireStream(sandboxID string) (release func(), ok bool) {
	api.mu.Lock()
	// Enforce the cap only when set; the open-stream count is tracked
	// unconditionally because it is also the work-aware idle signal (issue
	// #218): a live stream means a background job is running, so the idle reaper
	// must not reap the sandbox even with the cap disabled.
	if api.maxStreams > 0 && api.openStreams[sandboxID] >= api.maxStreams {
		api.mu.Unlock()
		return nil, false
	}
	api.openStreams[sandboxID]++
	api.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			api.mu.Lock()
			if n := api.openStreams[sandboxID] - 1; n > 0 {
				api.openStreams[sandboxID] = n
			} else {
				delete(api.openStreams, sandboxID)
			}
			api.mu.Unlock()
		})
	}, true
}

// SetAuditor installs the auditor that records a structured event after each
// exec and file operation. Passing nil installs the NopAuditor (auditing off).
// Must be called before the API serves requests; the field is not synchronized.
func (api *SandboxAPI) SetAuditor(a Auditor) {
	if a == nil {
		a = NopAuditor{}
	}
	api.auditor = a
}

// touch stamps the current time as the sandbox's last activity. Called at the
// top of every exec and file handler.
func (api *SandboxAPI) touch(sandboxID string) {
	api.mu.Lock()
	api.lastActivity[sandboxID] = api.now()
	api.mu.Unlock()
}

// RecordActivity stamps t as the sandbox's last-activity time, overriding the
// clock-based touch. It exists so callers (and tests of the GC reconciler) can
// set a known last-activity for a sandbox id; forkd itself relies on the
// implicit touch from exec and file handlers.
func (api *SandboxAPI) RecordActivity(sandboxID string, t time.Time) {
	api.mu.Lock()
	api.lastActivity[sandboxID] = t
	api.mu.Unlock()
}

// LastActivity returns the time of the most recent exec or file call on the
// sandbox. The bool is false when the sandbox has never been accessed.
func (api *SandboxAPI) LastActivity(sandboxID string) (time.Time, bool) {
	api.mu.RLock()
	t, ok := api.lastActivity[sandboxID]
	api.mu.RUnlock()
	return t, ok
}

// AllowTokenless permits requests targeting sandboxes that have no
// registered bearer token. Used ONLY by the standalone sandbox-server
// (which has no token-minting control plane) and by unit tests of other
// layers. forkd never sets it: a forkd sandbox without a token fails
// closed with 401. Sandboxes WITH a registered token are always enforced,
// even under AllowTokenless.
//
// Must be called before the API serves requests; the flag is not synchronized.
func (api *SandboxAPI) AllowTokenless() {
	api.allowTokenless = true
}

// SetSingleSandbox switches the API into single-sandbox mode for the husk-stub,
// which serves exactly ONE VM per pod. In this mode the auth gate (requireBearer
// and ptyAuth) validates the presented bearer against the single sandbox's
// registered token regardless of the request's "sandbox" id, then routes the
// request to id. This is required because the husk-stub registers its one VM
// under a fixed local id while the SDK addresses the in-pod API with the claim's
// status.sandboxID (the husk pod name), which never equals that local id; a
// strict per-id lookup would 401 every SDK request.
//
// The token gate is NOT weakened: a wrong or absent bearer is still rejected
// (401), the comparison stays constant-time, and a sandbox with no registered
// token still fails closed (unless AllowTokenless). forkd never calls this, so
// its multi-sandbox per-id token lookup is unchanged: a token for sandbox A
// cannot authorize sandbox B.
//
// Must be called before the API serves requests; the fields are not synchronized.
func (api *SandboxAPI) SetSingleSandbox(id string) {
	api.singleSandbox = true
	api.singleSandboxID = id
}

// resolveSandboxID maps the request's sandbox id to the id the API operates on.
// In single-sandbox mode every request resolves to the one served sandbox id,
// so an SDK that sends the husk pod name still reaches the single VM. In the
// default (forkd) multi-sandbox mode it returns the request id unchanged, so the
// per-id token lookup and agent routing are exactly as before.
func (api *SandboxAPI) resolveSandboxID(requested string) string {
	if api.singleSandbox {
		return api.singleSandboxID
	}
	return requested
}

// RegisterToken registers the bearer token required on every HTTP request
// targeting sandboxID. An empty token is a no-op: the sandbox stays
// tokenless and fails closed (unless AllowTokenless). Token values are
// never logged.
func (api *SandboxAPI) RegisterToken(sandboxID, token string) {
	if token == "" {
		return
	}
	api.mu.Lock()
	api.tokens[sandboxID] = token
	api.mu.Unlock()
}

// EnableUnixFallback lets dialGuestGRPC fall back to the guest agent's fixed
// local unix socket (/tmp/sandbox-agent-<port>.sock) when the vsock UDS path
// does not exist. This supports the standalone sandbox-server's local-testing
// workflow (agent running on the host, no Firecracker).
//
// forkd deliberately does NOT enable this: its vsock paths come from the
// fork engine, and a fallback to a global socket could deliver claim-time
// secrets to an unrelated local process.
//
// Must be called before the API serves requests; the flag is not synchronized.
func (api *SandboxAPI) EnableUnixFallback() {
	api.unixFallback = true
}

// RegisterSandbox records the vsock UDS path for sandboxID and registers the
// sandbox as active. All subsequent guest communication uses gRPC on
// vsock.AgentGRPCPort (53) dialed on demand; there is no persistent shared
// connection. callers should call RegisterStreamPath separately only when the
// path differs from vsockPath; RegisterSandbox already records vsockPath as the
// stream path. For forkd both calls use the same path, so calling
// RegisterSandbox is sufficient.
func (api *SandboxAPI) RegisterSandbox(sandboxID, vsockPath string) error {
	api.mu.Lock()
	api.streamPaths[sandboxID] = vsockPath
	api.mu.Unlock()
	return nil
}

// UnregisterSandbox clears the sandbox's path and bearer token.
func (api *SandboxAPI) UnregisterSandbox(sandboxID string) {
	// Close any live host-side port forwards first (issue #228): each holds a
	// host TCP listener and tunnel goroutines that must not outlive the sandbox.
	// CloseForwards takes mu itself, so call it before the lock below.
	api.CloseForwards(sandboxID)
	// Close any in-flight expose tunnels: each pfConn holds a vsock PortForward
	// stream that must not outlive the sandbox. CloseExpose takes mu itself, so
	// call it before the lock below.
	api.CloseExpose(sandboxID)

	api.mu.Lock()
	delete(api.streamPaths, sandboxID)
	delete(api.tokens, sandboxID)
	delete(api.lastActivity, sandboxID)
	delete(api.deadlines, sandboxID)
	delete(api.paused, sandboxID)
	delete(api.vitalsLabels, sandboxID)
	api.mu.Unlock()
}

// RegisterStreamPath records the vsock UDS path for opening per-call gRPC
// connections to a sandbox's guest agent. RegisterSandbox already records this
// path; call RegisterStreamPath only when the path must be updated after
// initial registration.
func (api *SandboxAPI) RegisterStreamPath(sandboxID, vsockPath string) {
	api.mu.Lock()
	api.streamPaths[sandboxID] = vsockPath
	api.mu.Unlock()
}

// dialGuestGRPC opens a RAW net.Conn to the sandbox's guest gRPC server on
// vsock.AgentGRPCPort, performing the Firecracker UDS CONNECT preamble. It uses
// the per-sandbox vsock UDS path stored in streamPaths (registered by
// RegisterStreamPath or RegisterSandbox). The returned conn is handed to
// vsock.DialGRPCOverConn to build the gRPC client.
//
// When the standalone unix fallback is enabled and the Firecracker UDS is
// absent, it dials the guest's plain gRPC unix socket (no preamble).
func (api *SandboxAPI) dialGuestGRPC(sandboxID string) (net.Conn, error) {
	api.mu.RLock()
	path, ok := api.streamPaths[sandboxID]
	api.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("sandbox %s has no stream path", sandboxID)
	}
	conn, err := vsock.DialGRPCConn(path, vsock.AgentGRPCPort)
	if err == nil {
		return conn, nil
	}
	if api.unixFallback && errors.Is(err, fs.ErrNotExist) {
		return vsock.DialGRPCConnUnix(fmt.Sprintf("/tmp/sandbox-agent-%d.sock", vsock.AgentGRPCPort))
	}
	return nil, err
}

// Configure delivers claim-time env and secrets to a sandbox's guest agent
// over gRPC. Values are never logged.
func (api *SandboxAPI) Configure(sandboxID string, env, secrets map[string]string) error {
	cc, ctrl, err := api.dialControl(sandboxID)
	if err != nil {
		return fmt.Errorf("configure sandbox %s: %w", sandboxID, err)
	}
	defer cc.Close() //nolint:errcheck // best-effort cleanup
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, cerr := ctrl.Configure(ctx, &internalv1.ConfigureRequest{
		Env:     env,
		Secrets: secrets,
	})
	if cerr != nil {
		return fmt.Errorf("configure sandbox %s: %w", sandboxID, cerr)
	}
	return nil
}

// NotifyForked tells a sandbox's guest agent a restore just happened so it can
// reseed the kernel CRNG, step the wall clock, and signal userspace, over gRPC.
// When guestNet is non-nil it also carries this fork's distinct eth0 address +
// gateway so the guest re-addresses its NIC. When volumes is non-empty it
// carries the per-fork volume mount table the guest mounts after the host
// rebound the drives. Entropy is sensitive seed material and is never logged;
// the network addresses, device nodes, and paths are safe to log.
//
// It RETURNS the guest's NotifyForkedResponse so the caller can enforce the
// fork-correctness gate (ReseededRNG): a transport success alone does not mean
// the guest reseeded its CRNG. The response carries booleans and counts only,
// never entropy bytes.
func (api *SandboxAPI) NotifyForked(sandboxID string, generation uint64, entropy []byte, guestNet *vsock.NotifyForkedNetwork, volumes []vsock.VolumeMountEntry) (*vsock.NotifyForkedResponse, error) {
	cc, ctrl, err := api.dialControl(sandboxID)
	if err != nil {
		return nil, fmt.Errorf("notify-forked sandbox %s: %w", sandboxID, err)
	}
	defer cc.Close() //nolint:errcheck // best-effort cleanup
	req := &internalv1.NotifyForkedRequest{
		Generation:         generation,
		HostWallClockNanos: time.Now().UnixNano(),
		Entropy:            entropy,
	}
	if guestNet != nil {
		req.Network = &internalv1.NotifyForkedNetwork{
			GuestIp:    guestNet.GuestIP,
			GatewayIp:  guestNet.GatewayIP,
			PrefixLen:  int32(guestNet.PrefixLen),
			GuestMac:   guestNet.GuestMAC,
			ResolverIp: guestNet.ResolverIP,
		}
	}
	for _, v := range volumes {
		req.Volumes = append(req.Volumes, &internalv1.VolumeMountEntry{
			Device:    v.Device,
			MountPath: v.MountPath,
			ReadOnly:  v.ReadOnly,
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, nerr := ctrl.NotifyForked(ctx, req)
	if nerr != nil {
		return nil, fmt.Errorf("notify-forked sandbox %s: %w", sandboxID, nerr)
	}
	return &vsock.NotifyForkedResponse{
		AppliedClockStepNanos: resp.GetAppliedClockStepNanos(),
		ReseededRNG:           resp.GetReseededRng(),
		SignaledProcesses:     int(resp.GetSignaledProcesses()),
	}, nil
}

// dialControl dials a fresh gRPC connection to the sandbox's guest Control
// service on vsock.AgentGRPCPort (53) and returns the connection and a typed
// ControlClient. The caller owns the connection and must Close it.
func (api *SandboxAPI) dialControl(sandboxID string) (*grpc.ClientConn, internalv1.ControlClient, error) {
	conn, err := api.dialGuestGRPC(sandboxID)
	if err != nil {
		return nil, nil, fmt.Errorf("dial control for sandbox %s: %w", sandboxID, err)
	}
	cc, werr := vsock.DialGRPCOverConn(func() (net.Conn, error) {
		c := conn
		conn = nil
		if c == nil {
			return nil, fmt.Errorf("control dialer: reconnect not supported")
		}
		return c, nil
	})
	if werr != nil {
		conn.Close() //nolint:errcheck // error already returned
		return nil, nil, fmt.Errorf("wrap grpc conn for sandbox %s: %w", sandboxID, werr)
	}
	return cc, internalv1.NewControlClient(cc), nil
}

// checkSandboxRegistered returns nil when sandboxID has a registered vsock path,
// or an error describing the missing registration. It replaces the old getAgent
// liveness check: all actual guest RPCs now dial gRPC on demand, so no
// persistent connection is held.
func (api *SandboxAPI) checkSandboxRegistered(sandboxID string) error {
	api.mu.RLock()
	_, ok := api.streamPaths[sandboxID]
	api.mu.RUnlock()
	if !ok {
		return fmt.Errorf("sandbox %s not found or not registered", sandboxID)
	}
	return nil
}

// Handler returns an http.Handler for the sandbox exec/files API. The handler
// combines distinct auth surfaces on a single mux:
//
//  1. Lifecycle JSON /v1/* routes (set_timeout, pause, resume): wrapped in
//     requireBearer (body-peeking HTTP middleware that reads the "sandbox" field
//     from the JSON body).
//  2. Connect Sandbox service (issue #24, Task 3.2): mounted on the outer mux
//     WITHOUT the body-peeking wrapper, because Connect auth is handled at the
//     interceptor level via the "Authorization: Bearer <token>" and
//     "X-Sandbox-Id" HTTP headers. BearerInterceptor enforces the same
//     per-sandbox token security as requireBearer. The full runtime surface
//     (exec, files, run_code, vitals, interactive PTY) is served here; the legacy
//     JSON /v1 runtime routes were removed once every SDK and kubectl-mitos moved
//     to Connect (#358).
//  3. Connect-over-WebSocket Exec: outside requireBearer (bodyless GET); auth is
//     handled by ptyAuth (?sandbox= + Authorization: Bearer query/header).
func (api *SandboxAPI) Handler() http.Handler {
	jsonMux := http.NewServeMux()
	// Lifecycle/management routes have NO Connect runtime successor: they keep
	// working unchanged. The runtime exec/files/run_code/vitals/pty routes are
	// served by the Connect sandbox.v1.Sandbox protocol below (#358).
	jsonMux.HandleFunc("POST /v1/set_timeout", api.handleSetTimeout)
	jsonMux.HandleFunc("POST /v1/pause", api.handlePause)
	jsonMux.HandleFunc("POST /v1/resume", api.handleResume)

	// Connect Sandbox service (issue #24, Task 3.2). The BearerInterceptor
	// enforces the same per-sandbox bearer-token security as requireBearer, but
	// operates on HTTP headers (not the JSON body), so the Connect handler is
	// mounted on the outer mux OUTSIDE the body-peeking requireBearer wrapper.
	// When allowTokenless is true (standalone sandbox-server only),
	// AllowTokenlessInterceptor is used. The interceptor injects the authenticated
	// sandbox id into the request context; the resolver reads it back.
	var authIC connect.Interceptor
	if api.allowTokenless {
		authIC = sandboxrpc.AllowTokenlessInterceptor()
	} else {
		authIC = sandboxrpc.BearerInterceptor(api.connectLookupToken)
	}
	// ExecBackend is never called when Guest is set; pass nil. The Guest factory
	// below is always set, so the ExecBackend code path in sandboxrpc.Service.Exec
	// is unreachable.
	svc := sandboxrpc.NewService(
		nil,
		nil,
		sandboxrpc.WithSandboxResolver(func(ctx context.Context) (string, error) {
			id, ok := sandboxrpc.SandboxIDFromContext(ctx)
			if !ok {
				// allowTokenless path or AllowTokenlessInterceptor: the interceptor
				// did not inject an id. Fall back to empty (single-sandbox mode).
				return "", nil
			}
			return api.resolveSandboxID(id), nil
		}),
	)
	// Wire the GuestConn factory: returns a vsockGuestConn for the given sandbox
	// id so EVERY Connect RPC delegates through the guest agent's gRPC server
	// over vsock (vsock.AgentGRPCPort). This is the sole guest transport; the
	// legacy JSON port 52 is no longer used anywhere in this package.
	svc.Guest = func(sandboxID string) (sandboxrpc.GuestConn, error) {
		return newVsockGuestConn(api, sandboxID), nil
	}
	// Plumb the exec-timeout ceiling onto the Connect runtime path (issue #216)
	// so an over-ceiling Exec/RunCode is rejected with CodeInvalidArgument before
	// the guest stream is opened, never silently reduced; <=0 disables it. This
	// mirrors the legacy /v1 handlers' checkTimeout gate.
	svc.MaxExecTimeoutSeconds = api.maxExecTimeout

	// auditIC records one AuditEvent per sandbox.v1.Sandbox runtime RPC AFTER it
	// completes, restoring the per-op audit the legacy /v1 handlers performed.
	// It is constructed with the SAME auditor SetAuditor controls (NopAuditor =
	// off), and it carries ONLY the op string, the authenticated sandbox id, and
	// OK: never a command, argv, env, path, or content.
	auditIC := newConnectAuditInterceptor(func() Auditor { return api.auditor })
	// Interceptor order: AUTH is OUTERMOST so it populates the authenticated
	// sandbox id in ctx (sandboxrpc.SandboxIDFromContext) BEFORE audit runs and
	// reads it. connect.WithInterceptors applies them outer-to-inner in the order
	// given, so authIC must precede auditIC.
	connectPath, connectHandler := sandboxv1connect.NewSandboxHandler(svc, connect.WithInterceptors(authIC, auditIC))

	// outer combines all auth surfaces. The order of Handle calls matters: more
	// specific prefixes (Connect, exec-over-ws) take precedence over the catch-all
	// "/".
	outer := http.NewServeMux()
	// Connect-over-WebSocket bidi Exec: the same sandbox.v1.Sandbox.Exec schema
	// the Connect HTTP handler serves over HTTP/2, but on a GET WebSocket upgrade
	// so the thin half-duplex-over-HTTP/1.1 SDK clients can reach the full-duplex
	// interactive (PTY) case. The Connect HTTP handler keeps POST on this path;
	// this more-specific GET pattern takes the upgrade (issue #358).
	outer.HandleFunc("GET "+execWSPath, api.handleExecWS)
	outer.Handle(connectPath, connectHandler)
	// Authenticated guest HTTP proxy (Mitos Expose slice 1). Mounted OUTSIDE
	// requireBearer because it proxies arbitrary and streaming HTTP and cannot be
	// body-peeked; checkBearer enforces the same per-sandbox token gate. The
	// trailing-slash pattern captures every sub-path under the port.
	outer.HandleFunc("/v1/sandboxes/{id}/expose/{port}/", api.handleExpose)
	// Also match the bare (no trailing slash) form so a request to the app root
	// /v1/sandboxes/{id}/expose/{port} reaches handleExpose rather than falling
	// through to the JSON catch-all (which would answer a confusing 400 invalid
	// json). handleExpose strips the same prefix for both: the bare form strips to
	// "" and normalizes to "/", the slash form strips to "/sub/path".
	outer.HandleFunc("/v1/sandboxes/{id}/expose/{port}", api.handleExpose)
	outer.Handle("/", api.requireBearer(jsonMux))
	return outer
}

// connectLookupToken is the token lookup function passed to BearerInterceptor
// for the Connect Sandbox handler. It reads the registered token for sandboxID
// from the same token map the legacy JSON gate used. Token values are never
// logged or placed in error messages.
//
// It resolves single-sandbox (husk-stub) mode first, exactly as the JSON gate
// (requireBearer, ptyAuth via resolveSandboxID) does: in a husk pod the cluster
// SDK addresses the in-pod API with the claim's sandbox id (the husk pod name),
// which never equals the stub's fixed local id, so a STRICT per-id lookup would
// 401 every cluster SDK request (the cluster-e2e bug). resolveSandboxID maps the
// requested id to the one registered id in single-sandbox mode, and is the
// identity in forkd's default multi-sandbox mode, so a token for sandbox A still
// cannot authorize sandbox B there.
func (api *SandboxAPI) connectLookupToken(sandboxID string) (string, bool) {
	id := api.resolveSandboxID(sandboxID)
	api.mu.RLock()
	token, ok := api.tokens[id]
	api.mu.RUnlock()
	return token, ok
}

// maxAuthBodyBytes bounds how much request body the auth middleware buffers.
// File writes are hex-encoded JSON, so this is the effective request cap.
const maxAuthBodyBytes = 32 << 20 // 32 MiB

// requireBearer enforces per-sandbox bearer tokens. The body is read and
// buffered ONCE: the middleware peeks the JSON "sandbox" field, checks
// Authorization: Bearer against the registered token in constant time, and
// hands the buffered body to the real handler. Failure modes:
//   - no token registered for the sandbox: 401 (fail closed), unless
//     AllowTokenless was set (standalone sandbox-server only)
//   - missing or malformed Authorization header: 401
//   - token mismatch: 401
//
// Token values never appear in responses or logs.
func (api *SandboxAPI) requireBearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxAuthBodyBytes+1))
		if err != nil {
			writeErr(w, "read request body", 400)
			return
		}
		if len(body) > maxAuthBodyBytes {
			writeErr(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))

		var peek struct {
			Sandbox string `json:"sandbox"`
		}
		if err := json.Unmarshal(body, &peek); err != nil {
			writeErr(w, "invalid json", 400)
			return
		}

		// In single-sandbox mode (husk-stub) the request id is whatever the SDK
		// sent (the husk pod name); resolve it to the one served sandbox id so
		// the token lookup hits the single registered token. In forkd's default
		// multi-sandbox mode this is the request id unchanged, so the per-id gate
		// is byte-identical.
		sandboxID := api.resolveSandboxID(peek.Sandbox)

		api.mu.RLock()
		token, hasToken := api.tokens[sandboxID]
		api.mu.RUnlock()

		if !hasToken {
			if api.allowTokenless {
				next.ServeHTTP(w, r)
				return
			}
			writeErr(w, "unauthorized: no token registered for sandbox", 401)
			return
		}

		auth := r.Header.Get("Authorization")
		presented, ok := strings.CutPrefix(auth, "Bearer ")
		if !ok {
			writeErr(w, "unauthorized: bearer token required", 401)
			return
		}
		if subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
			writeErr(w, "unauthorized: invalid token", 401)
			return
		}

		// Single-sandbox mode: the downstream handlers route the agent and stream
		// by the body's "sandbox" field, but the SDK sent the pod name, which is
		// not the local id the VM is registered under. Rewrite the body so the
		// handlers reach the single VM. This rewrite ONLY happens in single-
		// sandbox mode; in forkd's multi-sandbox mode the body is untouched.
		if api.singleSandbox && peek.Sandbox != sandboxID {
			if rewritten, err := rewriteSandboxField(body, sandboxID); err == nil {
				body = rewritten
				r.Body = io.NopCloser(bytes.NewReader(body))
				r.ContentLength = int64(len(body))
			} else {
				writeErr(w, "invalid json", 400)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// rewriteSandboxField returns body with its top-level "sandbox" field set to
// id, preserving every other field. Used only in single-sandbox mode to route
// the SDK's request (which carries the husk pod name) to the one local sandbox
// id the VM is registered under. The body was already buffered and size-bounded
// by requireBearer before this is called.
func rewriteSandboxField(body []byte, id string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("rewrite sandbox field: %w", err)
	}
	idJSON, err := json.Marshal(id)
	if err != nil {
		return nil, fmt.Errorf("rewrite sandbox field: %w", err)
	}
	if m == nil {
		m = make(map[string]json.RawMessage, 1)
	}
	m["sandbox"] = idJSON
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("rewrite sandbox field: %w", err)
	}
	return out, nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// writeAPIErr writes an LLM-legible error envelope. Prefer this over writeErr at
// new call sites: it carries a stable code and an actionable remediation.
func writeAPIErr(w http.ResponseWriter, e apierr.Error) {
	apierr.Encode(w, e)
}

// writeErr is the legacy shim. It maps a status to the closest catalogue entry
// and uses msg as the cause, so every existing call site now emits the
// {error:{code,message,cause,remediation}} envelope without a per-site rewrite.
// The cause is built from sandbox ids, paths, and operation names only and never
// carries a secret value.
func writeErr(w http.ResponseWriter, msg string, code int) {
	writeAPIErr(w, codeForStatus(code).WithCause(msg))
}

// codeForStatus picks the catalogue entry for an HTTP status used by the legacy
// writeErr call sites.
func codeForStatus(status int) apierr.Error {
	switch status {
	case http.StatusBadRequest:
		return apierr.Get(apierr.CodeInvalidJSON)
	case http.StatusRequestEntityTooLarge:
		return apierr.Get(apierr.CodeBodyTooLarge)
	case http.StatusUnauthorized:
		return apierr.Get(apierr.CodeUnauthorized)
	case http.StatusNotFound:
		return apierr.Get(apierr.CodeNotFound)
	default:
		return apierr.Get(apierr.CodeInternal)
	}
}
