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
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"mitos.run/mitos/internal/apierr"
	"mitos.run/mitos/internal/sandboxrpc"
	"mitos.run/mitos/internal/vsock"
	internalv1 "mitos.run/mitos/proto/sandbox/controlv1"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
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
	// firstExecSeen records, per sandbox id, whether the FIRST exec has already
	// been traced with the forkd.first-exec span (issue #164, the trace tail).
	// Guarded by mu. It is the bounded per-sandbox guard so only the first exec
	// after a fork gets the distinct first-exec span name; every later exec is a
	// normal request with no first-exec span. The map is bounded by the live
	// sandbox count: one bool per sandbox, deleted in UnregisterSandbox so it
	// never outlives the sandbox (no leak across sandbox lifetimes). It holds
	// only a boolean keyed by an id, never a command, argv, or any secret.
	firstExecSeen map[string]bool
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
		firstExecSeen:  make(map[string]bool),
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

	api.mu.Lock()
	delete(api.streamPaths, sandboxID)
	delete(api.tokens, sandboxID)
	delete(api.lastActivity, sandboxID)
	delete(api.deadlines, sandboxID)
	delete(api.paused, sandboxID)
	delete(api.vitalsLabels, sandboxID)
	// Drop the first-exec guard so the bounded map does not outlive the sandbox.
	delete(api.firstExecSeen, sandboxID)
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
// combines three distinct auth surfaces on a single mux:
//
//  1. Legacy JSON /v1/* routes: wrapped in requireBearer (body-peeking HTTP
//     middleware that reads the "sandbox" field from the JSON body).
//  2. Connect Sandbox service (issue #24, Task 3.2): mounted on the outer mux
//     WITHOUT the body-peeking wrapper, because Connect auth is handled at the
//     interceptor level via the "Authorization: Bearer <token>" and
//     "X-Sandbox-Id" HTTP headers. BearerInterceptor enforces the same
//     per-sandbox token security as requireBearer.
//  3. PTY WebSocket: similarly outside requireBearer (bodyless GET); auth is
//     handled by ptyAuth (?sandbox= + Authorization: Bearer query/header).
//
// The legacy JSON routes stay active (deprecated-but-working) so existing SDK
// callers are not broken.
func (api *SandboxAPI) Handler() http.Handler {
	jsonMux := http.NewServeMux()
	// Runtime exec/files/run_code/vitals routes are SUPERSEDED by the Connect
	// sandbox.v1.Sandbox protocol (issue #24); they carry a Deprecation note
	// (deprecatedRuntimeNote) so callers learn the JSON runtime shape is the
	// legacy path. They stay active (deprecated-but-working) until every SDK is on
	// Connect (#358), then they are removed.
	jsonMux.HandleFunc("POST /v1/exec", deprecatedRuntimeNote(api.handleExec))
	jsonMux.HandleFunc("POST /v1/exec/stream", deprecatedRuntimeNote(api.handleExecStream))
	jsonMux.HandleFunc("POST /v1/run_code/stream", deprecatedRuntimeNote(api.handleRunCodeStream))
	jsonMux.HandleFunc("POST /v1/files/read", deprecatedRuntimeNote(api.handleReadFile))
	jsonMux.HandleFunc("POST /v1/files/write", deprecatedRuntimeNote(api.handleWriteFile))
	jsonMux.HandleFunc("POST /v1/files/list", deprecatedRuntimeNote(api.handleListDir))
	jsonMux.HandleFunc("POST /v1/files/mkdir", deprecatedRuntimeNote(api.handleMkdir))
	jsonMux.HandleFunc("POST /v1/files/remove", deprecatedRuntimeNote(api.handleRemove))
	jsonMux.HandleFunc("POST /v1/vitals", deprecatedRuntimeNote(api.handleVitals))
	// Lifecycle/management routes have NO Connect runtime successor and are NOT
	// deprecated: they keep working unchanged.
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
	connectPath, connectHandler := sandboxv1connect.NewSandboxHandler(svc, connect.WithInterceptors(authIC))

	// outer combines all three auth surfaces. The order of Handle calls matters:
	// more specific prefixes (Connect, PTY) take precedence over the catch-all "/".
	outer := http.NewServeMux()
	// The legacy PTY WebSocket is superseded by Connect Exec with PtyOptions
	// (issue #24), so it carries the same Deprecation note as the other runtime
	// routes (set on the upgrade handshake response).
	outer.HandleFunc("GET /v1/pty", deprecatedRuntimeNote(api.handlePty))
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

// deprecatedRuntimeNote wraps a legacy JSON /v1 runtime handler so its responses
// carry the RFC 8594 Deprecation header and a Link to the Connect successor (the
// sandbox.v1.Sandbox protocol, issue #24). The runtime exec/files/run_code/pty/
// vitals surface is superseded by the Connect protocol; lifecycle routes
// (set_timeout, pause, resume) have no Connect successor and are NOT wrapped. The
// headers are set BEFORE delegating, so a caller is told of the deprecation
// regardless of the handler's status code (including auth failures and errors).
func deprecatedRuntimeNote(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Deprecation", "true")
		w.Header().Set("Link", `</sandbox.v1.Sandbox>; rel="successor-version"; title="Connect runtime protocol (issue #24)"`)
		next(w, r)
	}
}

// connectLookupToken is the token lookup function passed to BearerInterceptor
// for the Connect Sandbox handler. It reads the registered token for sandboxID
// from the same token map as the JSON /v1/* routes. Token values are never
// logged or placed in error messages.
func (api *SandboxAPI) connectLookupToken(sandboxID string) (string, bool) {
	api.mu.RLock()
	token, ok := api.tokens[sandboxID]
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

type execRequest struct {
	Sandbox    string            `json:"sandbox"`
	Command    string            `json:"command"`
	WorkingDir string            `json:"working_dir,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Timeout    int               `json:"timeout,omitempty"`
}

func (api *SandboxAPI) handleExec(w http.ResponseWriter, r *http.Request) {
	var req execRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "invalid json", 400)
		return
	}
	// Determinism (issue #216): reject an over-ceiling timeout BEFORE any work,
	// with the typed timeout_too_large code, rather than silently reducing it.
	if e := api.checkTimeout(req.Sandbox, req.Timeout); e != nil {
		writeAPIErr(w, *e)
		return
	}
	api.touch(req.Sandbox)

	if err := api.checkSandboxRegistered(req.Sandbox); err != nil {
		writeErr(w, err.Error(), 404)
		return
	}

	var out, errb strings.Builder
	exit, err := api.runExecStream(traceContextFromRequest(r), req, func(stream vsock.StreamName, data []byte) error {
		if stream == vsock.StreamStdout {
			out.Write(data)
		} else {
			errb.Write(data)
		}
		return nil
	})
	if err != nil {
		writeAPIErr(w, apierr.Get(apierr.CodeExecFailed).WithCause(fmt.Sprintf("exec failed: %v", err)).
			WithContext(map[string]any{"sandbox": req.Sandbox}))
		return
	}

	// Execution-deadline discrimination (issue #216): the guest kills a command
	// that ran past its deadline and reports the conventional 124 exit code. On
	// the blocking /v1/exec path we surface that as the typed exec_timeout
	// envelope (504), so a caller can branch on the deadline without comparing
	// exit codes. The streaming path keeps 124 in the terminal frame (the status
	// header is already 200), and the SDK maps that frame to the same typed
	// error.
	if exit.ExitCode == execTimeoutExitCode {
		writeAPIErr(w, apierr.Get(apierr.CodeExecTimeout).
			WithCause(fmt.Sprintf("command exceeded its %ds execution deadline and was terminated", execTimeoutSeconds(req.Timeout))).
			WithContext(map[string]any{"sandbox": req.Sandbox, "timeout_s": execTimeoutSeconds(req.Timeout)}))
		return
	}

	result := &vsock.ExecResponse{
		ExitCode:   exit.ExitCode,
		Stdout:     out.String(),
		Stderr:     errb.String(),
		ExecTimeMs: exit.ExecTimeMs,
	}

	// The command is safe to record (commands are not secret values); it is
	// truncated to a bound. The exit code rides in Detail. OK reports that the
	// call was served, not the exit code.
	api.auditor.Record(AuditEvent{
		SandboxID: req.Sandbox,
		Op:        "exec",
		Detail:    fmt.Sprintf("exit=%d cmd=%s", result.ExitCode, truncateCommand(req.Command)),
		OK:        true,
	})

	writeJSON(w, result)
}

// runExecStream opens a dedicated stream connection and drives one exec,
// invoking onChunk per chunk and returning the terminal frame. It falls back to
// the shared connection's aggregated Exec when no stream path is registered so
// callers still work on hosts that have not wired streaming.
// execTimeoutExitCode is the conventional exit code the guest agent reports for
// a command killed because it ran past its execution deadline (matching the
// shell `timeout` utility). It is the signal handleExec maps to the typed
// exec_timeout envelope.
const execTimeoutExitCode = 124

// defaultExecTimeoutSeconds is the per-endpoint exec default applied when the
// request omits a timeout. Kept here so the execution-deadline error reports the
// deadline that was actually in force.
const defaultExecTimeoutSeconds = 30

// execTimeoutSeconds resolves the deadline that was in force for a request: the
// requested value, or the default when the request omitted one.
func execTimeoutSeconds(requested int) int {
	if requested == 0 {
		return defaultExecTimeoutSeconds
	}
	return requested
}

// traceContextFromRequest extracts a W3C trace context (traceparent header)
// from the incoming exec request into the request context, so the first-exec
// span CONTINUES the sandbox's trace when the SDK or controller propagated one
// (the same W3C context that crosses controller -> forkd on the fork RPC, and
// the trace id the controller stamps on the mitos.run/trace-id annotation). When
// no trace context is present the request context is returned unchanged and the
// first-exec span starts a new root, per the task. It reads only headers; no
// body, command, or secret is touched.
func traceContextFromRequest(r *http.Request) context.Context {
	return otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
}

// startFirstExecSpan starts the forkd.first-exec span for the FIRST exec served
// for a sandbox after its fork (issue #164, the trace tail), and returns the
// span-carrying context and the span. For every later exec it is a no-op: it
// returns the input ctx and a nil span, so subsequent execs are NOT marked
// first. The per-sandbox guard (firstExecSeen) is bounded by the live sandbox
// count and cleaned up in UnregisterSandbox, so it never leaks.
//
// The span continues the sandbox's trace when the exec request carried a W3C
// trace context (extracted into ctx by the handler); otherwise tracer.Start
// begins a new root span for the exec. The attributes are ONLY the sandbox id
// and a first=true marker: never the command, argv, env, cwd, or any output.
// When tracing is off the package tracer is the no-op tracer, so this costs
// nothing and the returned span is a no-op (its End is a no-op too).
func (api *SandboxAPI) startFirstExecSpan(ctx context.Context, sandboxID string) (context.Context, trace.Span) {
	api.mu.Lock()
	first := !api.firstExecSeen[sandboxID]
	if first {
		api.firstExecSeen[sandboxID] = true
	}
	api.mu.Unlock()
	if !first {
		return ctx, nil
	}
	ctx, span := tracer.Start(ctx, "forkd.first-exec", trace.WithAttributes(
		attribute.String("sandbox.id", sandboxID),
		attribute.Bool("first", true),
	))
	return ctx, span
}

// runExecStream drives one exec via gRPC, invoking onChunk per output chunk
// and returning the terminal frame. It opens a fresh gRPC connection per call.
func (api *SandboxAPI) runExecStream(ctx context.Context, req execRequest, onChunk vsock.ChunkFunc) (*vsock.ExecStreamFrame, error) {
	// First exec after a fork gets the forkd.first-exec span (the trace tail);
	// later execs are normal. The span carries only the sandbox id and a first
	// marker, never the command or env.
	ctx, firstSpan := api.startFirstExecSpan(ctx, req.Sandbox)
	if firstSpan != nil {
		defer firstSpan.End()
	}
	timeout := req.Timeout
	if timeout == 0 {
		timeout = defaultExecTimeoutSeconds
	}
	g := newVsockGuestConn(api, req.Sandbox).(*vsockGuestConn)
	open := &sandboxv1.ExecOpen{
		Command:        req.Command,
		Cwd:            req.WorkingDir,
		TimeoutSeconds: int32(timeout),
	}
	for k, v := range req.Env {
		open.Env = append(open.Env, &sandboxv1.EnvVar{Key: k, Value: v})
	}
	stream, err := g.Exec(ctx, open)
	if err != nil {
		if firstSpan != nil {
			firstSpan.RecordError(err)
		}
		return nil, fmt.Errorf("exec guest gRPC: %w", err)
	}
	defer stream.Close()
	var exitCode int
	var execTimeMs float64
	for {
		frame, ferr := stream.Recv()
		if ferr == io.EOF {
			break
		}
		if ferr != nil {
			if firstSpan != nil {
				firstSpan.RecordError(ferr)
			}
			return nil, ferr
		}
		if frame.Done {
			exitCode = int(frame.ExitCode)
			execTimeMs = frame.ExecTimeMs
			break
		}
		if len(frame.Stdout) > 0 {
			if cerr := onChunk(vsock.StreamStdout, frame.Stdout); cerr != nil {
				return nil, cerr
			}
		}
		if len(frame.Stderr) > 0 {
			if cerr := onChunk(vsock.StreamStderr, frame.Stderr); cerr != nil {
				return nil, cerr
			}
		}
	}
	return &vsock.ExecStreamFrame{Kind: vsock.FrameExit, ExitCode: exitCode, ExecTimeMs: execTimeMs}, nil
}

func (api *SandboxAPI) handleExecStream(w http.ResponseWriter, r *http.Request) {
	var req execRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "invalid json", 400)
		return
	}
	// Reject an over-ceiling timeout here, BEFORE the 200 stream header is
	// written, so it surfaces as a clean enveloped 400, not a terminal frame.
	if e := api.checkTimeout(req.Sandbox, req.Timeout); e != nil {
		writeAPIErr(w, *e)
		return
	}
	api.touch(req.Sandbox)

	if err := api.checkSandboxRegistered(req.Sandbox); err != nil {
		writeErr(w, err.Error(), 404)
		return
	}

	// Open the exec stream BEFORE writing the 200 header so a cap rejection
	// (vsockGuestConn.Exec is the single point of slot acquisition) surfaces as
	// a clean 429 envelope rather than a terminal frame inside a 200 body.
	timeout := req.Timeout
	if timeout == 0 {
		timeout = defaultExecTimeoutSeconds
	}
	g := newVsockGuestConn(api, req.Sandbox).(*vsockGuestConn)
	open := &sandboxv1.ExecOpen{
		Command:        req.Command,
		Cwd:            req.WorkingDir,
		TimeoutSeconds: int32(timeout),
	}
	for k, v := range req.Env {
		open.Env = append(open.Env, &sandboxv1.EnvVar{Key: k, Value: v})
	}
	// First exec after a fork gets the forkd.first-exec span (the trace tail),
	// continuing the request's W3C trace context when present. The span carries
	// only the sandbox id and a first marker, never the command or env.
	execCtx, firstSpan := api.startFirstExecSpan(traceContextFromRequest(r), req.Sandbox)
	if firstSpan != nil {
		defer firstSpan.End()
	}
	stream, err := g.Exec(execCtx, open)
	if err != nil {
		if firstSpan != nil {
			firstSpan.RecordError(err)
		}
		// vsockGuestConn.Exec returns a recognisable "concurrent exec-stream limit"
		// message when the per-sandbox slot cap is full; map that to the typed 429
		// so callers can branch. Any other open failure (dial, vsock) maps to 503.
		if strings.Contains(err.Error(), "concurrent exec-stream limit") {
			writeAPIErr(w, apierr.Get(apierr.CodeTooManyStreams).WithCause(fmt.Sprintf("sandbox %s is at its concurrent-stream limit", req.Sandbox)).
				WithContext(map[string]any{"sandbox": req.Sandbox}))
		} else {
			writeAPIErr(w, apierr.Get(apierr.CodeExecFailed).WithCause(fmt.Sprintf("exec failed: %v", err)).
				WithContext(map[string]any{"sandbox": req.Sandbox}))
		}
		return
	}
	defer stream.Close()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	enc := json.NewEncoder(w)

	writeLine := func(v any) error {
		if err := enc.Encode(v); err != nil {
			return err
		}
		return rc.Flush()
	}

	var exitCode int
	var execTimeMs float64
	var streamErr error
	for {
		frame, ferr := stream.Recv()
		if ferr == io.EOF {
			break
		}
		if ferr != nil {
			streamErr = ferr
			break
		}
		if frame.Done {
			exitCode = int(frame.ExitCode)
			execTimeMs = frame.ExecTimeMs
			break
		}
		if len(frame.Stdout) > 0 {
			if werr := writeLine(map[string]any{"stream": string(vsock.StreamStdout), "data": frame.Stdout}); werr != nil {
				streamErr = werr
				break
			}
		}
		if len(frame.Stderr) > 0 {
			if werr := writeLine(map[string]any{"stream": string(vsock.StreamStderr), "data": frame.Stderr}); werr != nil {
				streamErr = werr
				break
			}
		}
	}
	if streamErr != nil {
		// The stream has already started; emit a terminal error frame rather
		// than an HTTP status (status was sent 200 with the first byte). The
		// message carries actionable text and never echoes secrets.
		_ = writeLine(map[string]any{"exit_code": 1, "error": fmt.Sprintf("exec stream failed: %v", streamErr)})
		return
	}
	_ = writeLine(map[string]any{"exit_code": exitCode, "exec_time_ms": execTimeMs})

	api.auditor.Record(AuditEvent{
		SandboxID: req.Sandbox,
		Op:        "exec_stream",
		Detail:    fmt.Sprintf("exit=%d cmd=%s", exitCode, truncateCommand(req.Command)),
		OK:        true,
	})
}

type runCodeRequest struct {
	Sandbox  string `json:"sandbox"`
	Code     string `json:"code"`
	Language string `json:"language,omitempty"`
	Timeout  int    `json:"timeout,omitempty"`
}

// runRunCodeStream drives one run_code against the guest kernel via gRPC,
// invoking onFrame per RunCodeFrame. It opens a fresh gRPC connection per call.
func (api *SandboxAPI) runRunCodeStream(ctx context.Context, req runCodeRequest, onFrame func(vsock.ExecStreamFrame)) error {
	timeout := req.Timeout
	if timeout == 0 {
		timeout = 60
	}
	g := newVsockGuestConn(api, req.Sandbox).(*vsockGuestConn)
	stream, err := g.RunCode(ctx, &sandboxv1.RunCodeOpen{
		Code:           req.Code,
		Language:       req.Language,
		TimeoutSeconds: int64(timeout),
	})
	if err != nil {
		return fmt.Errorf("run_code guest gRPC: %w", err)
	}
	defer stream.Close()
	for {
		frame, ferr := stream.Recv()
		if ferr == io.EOF {
			break
		}
		if ferr != nil {
			return ferr
		}
		switch frame.Kind {
		case sandboxrpc.RunCodeFrameStdout:
			onFrame(vsock.ExecStreamFrame{Kind: vsock.FrameChunk, Stream: vsock.StreamStdout, Data: frame.Stdout})
		case sandboxrpc.RunCodeFrameStderr:
			onFrame(vsock.ExecStreamFrame{Kind: vsock.FrameChunk, Stream: vsock.StreamStderr, Data: frame.Stderr})
		case sandboxrpc.RunCodeFrameResult:
			rf := &vsock.ResultFrame{}
			if frame.Result != nil {
				rf.Text = frame.Result.Text
				if len(frame.Result.Data) > 0 {
					rf.Data = make(map[string]string, len(frame.Result.Data))
					for mime, payload := range frame.Result.Data {
						// []byte encodes as base64 in JSON, matching vsock.ResultFrame.Data.
						rf.Data[mime] = string(payload)
					}
				}
			}
			onFrame(vsock.ExecStreamFrame{Kind: vsock.FrameResult, Result: rf})
		case sandboxrpc.RunCodeFrameError:
			ef := &vsock.ErrorFrame{}
			if frame.Error != nil {
				ef.Name = frame.Error.Name
				ef.Value = frame.Error.Value
				ef.Traceback = frame.Error.Traceback
			}
			onFrame(vsock.ExecStreamFrame{Kind: vsock.FrameError, ErrorInfo: ef})
		case sandboxrpc.RunCodeFrameExit:
			onFrame(vsock.ExecStreamFrame{Kind: vsock.FrameExit, ExitCode: int(frame.ExitCode)})
		}
	}
	return nil
}

// handleRunCodeStream streams a run_code execution back as chunked NDJSON. Each
// guest ExecStreamFrame is re-encoded with an explicit "kind" field so the SDKs
// can distinguish stdout/stderr/result/error/exit frames; this is a distinct
// wire shape from /v1/exec/stream (which uses keyless chunk/exit maps) because
// run_code carries rich result and structured-error payloads exec does not.
// Result/error payloads are tenant code output and are never logged.
func (api *SandboxAPI) handleRunCodeStream(w http.ResponseWriter, r *http.Request) {
	var req runCodeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "invalid json", 400)
		return
	}
	// Reject an over-ceiling timeout before any work, BEFORE the 200 stream
	// header is written, so it surfaces as a clean enveloped 400 (issue #216).
	if e := api.checkTimeout(req.Sandbox, req.Timeout); e != nil {
		writeAPIErr(w, *e)
		return
	}
	api.touch(req.Sandbox)

	if err := api.checkSandboxRegistered(req.Sandbox); err != nil {
		writeErr(w, err.Error(), 404)
		return
	}

	// Per-sandbox concurrent-stream cap (production-blocker #2, cap 3): a run_code
	// stream holds a gRPC connection for the command lifetime, so it counts against
	// the same per-sandbox ceiling. Reject a NEW one over the cap before writing
	// the 200 header; existing streams are never touched.
	release, ok := api.acquireStream(req.Sandbox)
	if !ok {
		writeAPIErr(w, apierr.Get(apierr.CodeTooManyStreams).WithCause(fmt.Sprintf("sandbox %s is at its concurrent-stream limit", req.Sandbox)).
			WithContext(map[string]any{"sandbox": req.Sandbox}))
		return
	}
	defer release()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	enc := json.NewEncoder(w)

	writeLine := func(v any) {
		if err := enc.Encode(v); err != nil {
			return
		}
		_ = rc.Flush()
	}

	var lastExit int
	err := api.runRunCodeStream(r.Context(), req, func(fr vsock.ExecStreamFrame) {
		switch fr.Kind {
		case vsock.FrameChunk:
			if fr.Stream == vsock.StreamStderr {
				writeLine(map[string]any{"kind": "stderr", "stderr": fr.Data})
			} else {
				writeLine(map[string]any{"kind": "stdout", "stdout": fr.Data})
			}
		case vsock.FrameResult:
			writeLine(map[string]any{"kind": "result", "result": fr.Result})
		case vsock.FrameError:
			writeLine(map[string]any{"kind": "error", "error": fr.ErrorInfo})
		case vsock.FrameExit:
			lastExit = fr.ExitCode
			writeLine(map[string]any{"kind": "exit", "exit_code": fr.ExitCode})
		}
	})
	if err != nil {
		// The stream has already started (200 sent); surface the failure as a
		// final error frame rather than an HTTP status. The message carries
		// actionable text and never echoes secrets.
		writeLine(map[string]any{"kind": "error", "error": map[string]any{"name": "KernelStreamError", "value": fmt.Sprintf("run_code stream failed: %v", err)}})
		writeLine(map[string]any{"kind": "exit", "exit_code": 1})
		lastExit = 1
	}

	// The code is safe to record (not a secret value), truncated to a bound.
	api.auditor.Record(AuditEvent{
		SandboxID: req.Sandbox,
		Op:        "run_code",
		Detail:    fmt.Sprintf("exit=%d code=%s", lastExit, truncateCommand(req.Code)),
		OK:        true,
	})
}

type filePathRequest struct {
	Sandbox string `json:"sandbox"`
	Path    string `json:"path"`
}

type fileWriteRequest struct {
	Sandbox string `json:"sandbox"`
	Path    string `json:"path"`
	Content string `json:"content"`
	Mode    uint32 `json:"mode,omitempty"`
}

func (api *SandboxAPI) handleReadFile(w http.ResponseWriter, r *http.Request) {
	var req filePathRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "invalid json", 400)
		return
	}
	api.touch(req.Sandbox)

	if err := api.checkSandboxRegistered(req.Sandbox); err != nil {
		writeErr(w, err.Error(), 404)
		return
	}

	g := newVsockGuestConn(api, req.Sandbox)
	chunks, err := g.ReadFile(r.Context(), req.Path, 0, 0)
	if err != nil {
		writeAPIErr(w, apierr.Get(apierr.CodeFileFailed).WithCause(err.Error()).
			WithContext(map[string]any{"sandbox": req.Sandbox, "path": req.Path}))
		return
	}
	var size int
	var buf []byte
	for _, c := range chunks {
		buf = append(buf, c...)
		size += len(c)
	}

	// Record the path and byte count only; the content is never audited.
	api.auditor.Record(AuditEvent{
		SandboxID: req.Sandbox,
		Op:        "read",
		Detail:    req.Path,
		Bytes:     size,
		OK:        true,
	})

	writeJSON(w, map[string]any{"content": string(buf), "size": size})
}

func (api *SandboxAPI) handleWriteFile(w http.ResponseWriter, r *http.Request) {
	var req fileWriteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "invalid json", 400)
		return
	}
	api.touch(req.Sandbox)

	if err := api.checkSandboxRegistered(req.Sandbox); err != nil {
		writeErr(w, err.Error(), 404)
		return
	}

	mode := req.Mode
	if mode == 0 {
		mode = 0o644
	}

	g := newVsockGuestConn(api, req.Sandbox)
	data := []byte(req.Content)
	if _, err := g.WriteFile(r.Context(), req.Path, mode, [][]byte{data}); err != nil {
		writeAPIErr(w, apierr.Get(apierr.CodeFileFailed).WithCause(err.Error()).
			WithContext(map[string]any{"sandbox": req.Sandbox, "path": req.Path}))
		return
	}

	// Record the path and byte count only; the content is never audited.
	api.auditor.Record(AuditEvent{
		SandboxID: req.Sandbox,
		Op:        "write",
		Detail:    req.Path,
		Bytes:     len(req.Content),
		OK:        true,
	})

	writeJSON(w, map[string]string{"status": "ok"})
}

func (api *SandboxAPI) handleListDir(w http.ResponseWriter, r *http.Request) {
	var req filePathRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "invalid json", 400)
		return
	}
	api.touch(req.Sandbox)

	if err := api.checkSandboxRegistered(req.Sandbox); err != nil {
		writeErr(w, err.Error(), 404)
		return
	}

	g := newVsockGuestConn(api, req.Sandbox)
	result, err := g.List(r.Context(), req.Path, 0, "", "")
	if err != nil {
		writeAPIErr(w, apierr.Get(apierr.CodeFileFailed).WithCause(err.Error()).
			WithContext(map[string]any{"sandbox": req.Sandbox, "path": req.Path}))
		return
	}

	api.auditor.Record(AuditEvent{
		SandboxID: req.Sandbox,
		Op:        "list",
		Detail:    req.Path,
		OK:        true,
	})

	// Map gRPC FileInfo entries to the legacy vsock.FileEntry shape so existing
	// SDK callers see the same JSON structure, including mode and modified_at
	// which the gRPC FileInfo carries and the legacy JSON entry always exposed.
	type legacyEntry struct {
		Name       string `json:"name"`
		IsDir      bool   `json:"is_dir"`
		Size       int64  `json:"size"`
		Mode       uint32 `json:"mode"`
		ModifiedAt int64  `json:"modified_at"`
	}
	entries := make([]legacyEntry, 0, len(result.Entries))
	for _, e := range result.Entries {
		entries = append(entries, legacyEntry{
			Name:       e.Name,
			IsDir:      e.IsDir,
			Size:       e.Size,
			Mode:       e.Mode,
			ModifiedAt: e.ModifiedAtUnix,
		})
	}
	writeJSON(w, map[string]any{"entries": entries})
}

func (api *SandboxAPI) handleMkdir(w http.ResponseWriter, r *http.Request) {
	var req filePathRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "invalid json", 400)
		return
	}
	api.touch(req.Sandbox)

	if err := api.checkSandboxRegistered(req.Sandbox); err != nil {
		writeErr(w, err.Error(), 404)
		return
	}

	g := newVsockGuestConn(api, req.Sandbox)
	if err := g.Mkdir(r.Context(), req.Path, true); err != nil {
		writeAPIErr(w, apierr.Get(apierr.CodeFileFailed).WithCause(err.Error()).
			WithContext(map[string]any{"sandbox": req.Sandbox, "path": req.Path}))
		return
	}

	api.auditor.Record(AuditEvent{
		SandboxID: req.Sandbox,
		Op:        "mkdir",
		Detail:    req.Path,
		OK:        true,
	})

	writeJSON(w, map[string]string{"status": "ok"})
}

func (api *SandboxAPI) handleRemove(w http.ResponseWriter, r *http.Request) {
	var req filePathRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "invalid json", 400)
		return
	}
	api.touch(req.Sandbox)

	if err := api.checkSandboxRegistered(req.Sandbox); err != nil {
		writeErr(w, err.Error(), 404)
		return
	}

	g := newVsockGuestConn(api, req.Sandbox)
	if err := g.Remove(r.Context(), req.Path, true); err != nil {
		writeAPIErr(w, apierr.Get(apierr.CodeFileFailed).WithCause(err.Error()).
			WithContext(map[string]any{"sandbox": req.Sandbox, "path": req.Path}))
		return
	}

	api.auditor.Record(AuditEvent{
		SandboxID: req.Sandbox,
		Op:        "remove",
		Detail:    req.Path,
		OK:        true,
	})

	writeJSON(w, map[string]string{"status": "ok"})
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
