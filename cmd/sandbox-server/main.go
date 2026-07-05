package main

import (
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/apierr"
	"mitos.run/mitos/internal/daemon"
	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/fork"
	"mitos.run/mitos/internal/netconf"
	"mitos.run/mitos/internal/preview"
)

// sandbox-server is a standalone REST API. No Kubernetes required.
// For production on k8s, use the controller + forkd instead.
// Both share the same fork engine and guest agent protocol.

type server struct {
	mu sync.RWMutex
	// engine is the real, KVM-backed fork engine used in real mode to build
	// templates, fork sandboxes (restore a snapshot), and reap them. It is the
	// same proven path cmd/mem-smoke and cmd/crash-reap-smoke drive. It is nil
	// in mock mode AND in real-mode unit tests built via newServer (which never
	// touches KVM): every real-mode handler gates engine use on engine != nil so
	// those tests keep using the local-agent fallback path. main() constructs and
	// assigns it in real mode after newServer, because fork.NewEngine calls
	// validateKVM and would fail on a non-KVM host (CI).
	engine *fork.Engine
	// dataDir roots the engine data directory and the per-sandbox vsock UDS paths.
	dataDir    string
	rootfsPath string
	templates  map[string]*templateInfo
	sandboxes  map[string]*sandboxInfo
	mockMode   bool
	// idempotency maps an Idempotency-Key header value to the resource id a prior
	// creating call (template create or fork) returned under that key, with the
	// time it was recorded so an entry expires after idempotencyTTL. A repeat with
	// the same key returns the SAME resource instead of creating a duplicate
	// (issue #22). Agents and scripts retry aggressively and must never double
	// create. Guarded by mu, the same lock the resource maps use.
	idempotency map[string]idempotencyRecord
	sandboxAPI  *daemon.SandboxAPI
	// maxStreamsPerSandbox is the per-sandbox concurrent-stream ceiling applied
	// to sandboxAPI at construction. Retained so the flag plumbing is observable
	// without reaching into the daemon package's unexported state.
	maxStreamsPerSandbox int
	// maxExecTimeoutSecs is the ceiling (seconds) on a requested exec or run_code
	// timeout applied to sandboxAPI at construction (issue #216). Retained so the
	// flag plumbing is observable without reaching into the daemon package.
	maxExecTimeoutSecs int
	// forkGeneration is the monotonically increasing fork generation handed to
	// the guest in each NotifyForked so a guest can tell forks apart. Atomic so
	// concurrent forks get distinct generations.
	forkGeneration atomic.Uint64
	// previewSigner mints signed, expiring preview URLs for get_host(port)
	// (issue #126). nil disables the /v1/preview route (no signing secret
	// configured). previewDomain is the base domain for <id>.<domain>.
	// previewTTL is how long a minted URL stays valid.
	previewSigner *preview.Signer
	previewDomain string
	previewTTL    time.Duration

	// reseedBackoff is the base delay between reseed-handshake retries on a real
	// fork (the delay grows linearly with the attempt). After a snapshot restore
	// the guest agent resets its vsock listener, so the first NotifyForked can see
	// a transient "connection closed"; the fork path retries register+reseed
	// rather than failing the race. Tests set this to a tiny value.
	reseedBackoff time.Duration
}

// newServer builds the standalone server and applies the SandboxAPI policy
// (token mode, unix fallback, and the per-sandbox concurrent-stream cap). It is
// the single construction seam main() and the flag-plumbing test share.
func newServer(dataDir, rootfsPath string, mockMode bool, maxStreamsPerSandbox, maxExecTimeoutSecs int) *server {
	s := &server{
		dataDir:              dataDir,
		rootfsPath:           rootfsPath,
		templates:            make(map[string]*templateInfo),
		sandboxes:            make(map[string]*sandboxInfo),
		idempotency:          make(map[string]idempotencyRecord),
		mockMode:             mockMode,
		sandboxAPI:           daemon.NewSandboxAPI(dataDir),
		maxStreamsPerSandbox: maxStreamsPerSandbox,
		maxExecTimeoutSecs:   maxExecTimeoutSecs,
		reseedBackoff:        250 * time.Millisecond,
	}
	// Standalone local-testing path: if the Firecracker vsock UDS does not
	// exist, fall back to a guest agent running directly on the host
	// (/tmp/sandbox-agent-52.sock). forkd does not opt in to this fallback.
	s.sandboxAPI.EnableUnixFallback()
	// Standalone mode has no token-minting control plane; its sandboxes are
	// tokenless by design. forkd never sets this: there, a sandbox without
	// a registered token fails closed with 401.
	s.sandboxAPI.AllowTokenless()
	// Per-sandbox concurrent-stream ceiling (production-blocker #2): the
	// standalone REST path is otherwise uncapped, so a single sandbox could open
	// unbounded streaming exec/run_code/PTY connections and exhaust host vsock
	// connections and goroutines. Apply the same cap forkd enforces.
	s.sandboxAPI.SetMaxStreamsPerSandbox(maxStreamsPerSandbox)
	// Requested-timeout ceiling (issue #216): a requested exec/run_code timeout
	// over the ceiling is rejected with timeout_too_large, never silently
	// reduced. The same ceiling forkd enforces.
	s.sandboxAPI.SetMaxExecTimeoutSeconds(maxExecTimeoutSecs)
	return s
}

// idempotencyTTL is how long an Idempotency-Key binding is honored. A repeat
// within the window returns the prior resource; after it the key is free to
// bind a fresh resource. It outlives a client's retry budget without pinning
// keys forever in an in-memory map.
const idempotencyTTL = 24 * time.Hour

// idempotencyKind discriminates the two creating endpoints so a key reused
// across different endpoints cannot cross the streams (a template key never
// resolves a fork and vice versa).
type idempotencyKind string

const (
	idempotencyTemplate idempotencyKind = "template"
	idempotencyFork     idempotencyKind = "fork"
)

// idempotencyRecord remembers the resource id a creating call returned under an
// Idempotency-Key, with the recording time so the entry expires after
// idempotencyTTL. It holds an id only, never any secret value.
type idempotencyRecord struct {
	kind       idempotencyKind
	resourceID string
	at         time.Time
}

// idempotencyHeader is the request header a creating call may carry so a retry
// returns the same resource. It mirrors the Stripe / RFC draft convention.
const idempotencyHeader = "Idempotency-Key"

// idempotencyStatus is the outcome of reserving an Idempotency-Key for a create.
type idempotencyStatus int

const (
	// idemProceed: the caller holds the reservation and must perform the create,
	// then record it on success or release it on failure.
	idemProceed idempotencyStatus = iota
	// idemReplay: a prior create under this key already completed; return its
	// resource (the returned id) instead of creating again.
	idemReplay
	// idemInFlight: a concurrent create under this key is reserved but not yet
	// complete; the caller must NOT start a second create.
	idemInFlight
)

// beginIdempotent atomically resolves an Idempotency-Key before a create. The
// lookup and the in-flight reservation happen under one exclusive lock, so two
// concurrent calls with the same key cannot both observe "absent" and both
// proceed to create (the TOCTOU the earlier RLock-then-release-then-create
// allowed). An empty key (no header) always proceeds without reserving. On
// idemProceed the caller MUST later call recordIdempotent (success, under s.mu)
// or releaseIdempotent (failure) for the same key.
func (s *server) beginIdempotent(key string, kind idempotencyKind) (string, idempotencyStatus) {
	if key == "" {
		return "", idemProceed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.idempotency[key]
	if ok && rec.kind == kind && time.Since(rec.at) <= idempotencyTTL {
		if rec.resourceID != "" {
			return rec.resourceID, idemReplay
		}
		return "", idemInFlight
	}
	// Absent, expired, or a different kind: reserve this key in-flight (an empty
	// resourceID) so a concurrent same-key create observes idemInFlight.
	s.idempotency[key] = idempotencyRecord{kind: kind, at: time.Now()}
	return "", idemProceed
}

// releaseIdempotent drops a still-in-flight reservation so a later retry can
// proceed; a failed create does not consume the key. It never removes a
// completed binding (non-empty resourceID). An empty key is a no-op.
func (s *server) releaseIdempotent(key string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.idempotency[key]; ok && rec.resourceID == "" {
		delete(s.idempotency, key)
	}
}

// recordIdempotent binds key to resourceID for kind so a later repeat returns
// the same resource. An empty key is a no-op (the call was not idempotency
// scoped). Must be called with s.mu held.
func (s *server) recordIdempotent(key string, kind idempotencyKind, resourceID string) {
	if key == "" {
		return
	}
	s.idempotency[key] = idempotencyRecord{kind: kind, resourceID: resourceID, at: time.Now()}
}

type templateInfo struct {
	ID        string    `json:"id"`
	Ready     bool      `json:"ready"`
	CreatedAt time.Time `json:"created_at"`
	TimeMs    float64   `json:"creation_time_ms"`
	// Network is the per-sandbox network posture attached at create time (issue
	// #219); nil means the secure default (deny egress, deny-by-default inbound).
	// It is echoed back so a caller can confirm the policy the server recorded.
	Network *networkConfig `json:"network,omitempty"`
}

type sandboxInfo struct {
	ID         string    `json:"id"`
	TemplateID string    `json:"template_id"`
	Endpoint   string    `json:"endpoint"`
	CreatedAt  time.Time `json:"created_at"`
	ForkTimeMs float64   `json:"fork_time_ms"`
	// Network is the resolved per-sandbox network posture inherited from the
	// template (issue #219), echoed so a caller can confirm what governs the
	// sandbox's traffic. On a real forkd this same policy drives the host
	// nftables datapath; the standalone mock path records it for visibility.
	Network *v1.NetworkPolicy `json:"network,omitempty"`
}

func main() {
	var (
		addr                 string
		dataDir              string
		firecrackerBin       string
		kernelPath           string
		rootfsPath           string
		agentBin             string
		mockMode             bool
		auditLog             string
		maxStreamsPerSandbox int
		maxExecTimeoutSecs   int
		previewDomain        string
		previewTTLSecs       int
	)

	flag.StringVar(&addr, "addr", ":8080", "Listen address")
	flag.StringVar(&dataDir, "data-dir", "/tmp/sandbox-server", "Data directory")
	flag.StringVar(&firecrackerBin, "firecracker", "/usr/local/bin/firecracker", "Firecracker binary path")
	flag.StringVar(&kernelPath, "kernel", "", "Guest kernel path (required unless --mock)")
	flag.StringVar(&rootfsPath, "rootfs", "", "Guest rootfs path the engine builds templates from (required unless --mock). The guest agent is supplied separately via --agent-bin and injected as /init by the engine; pass a plain rootfs here.")
	flag.StringVar(&agentBin, "agent-bin", "", "Path to the guest agent binary the engine injects as /init when it builds a template rootfs (required unless --mock). Mirrors mem-smoke's --agent-bin.")
	flag.BoolVar(&mockMode, "mock", false, "Mock mode (no KVM, simulated responses)")
	flag.StringVar(&auditLog, "audit-log", "", "Structured audit log of exec and file operations. A file path, or '-'/'stderr' for stderr. Empty disables auditing. Records command strings, paths, and byte counts only; never file content or secret values")
	flag.IntVar(&maxStreamsPerSandbox, "max-streams-per-sandbox", 16, "Per-sandbox ceiling on concurrent OPEN streams (production-blocker #2): streaming exec, run_code, and PTY each hold a dedicated vsock connection plus host goroutines for the command lifetime, so an unbounded number would exhaust host vsock connections and goroutines. A NEW stream opened over this cap is rejected with 429 (the too_many_streams error); existing streams are never killed. The cap is checked at stream OPEN, off the fork path. 0 disables the cap (unbounded, the prior behavior). Matches the forkd default of 16.")
	flag.IntVar(&maxExecTimeoutSecs, "max-exec-timeout-seconds", 86400, "Ceiling (seconds) on a requested exec or run_code timeout (issue #216). A request over the ceiling is REJECTED with the typed timeout_too_large error, never silently reduced. Default 86400 (24h). 0 disables the ceiling. Matches the forkd default.")
	flag.StringVar(&previewDomain, "preview-domain", "", "Base domain for per-sandbox preview URLs (issue #126): get_host(port) mints https://<id>.<domain>/?token=... . Requires MITOS_PREVIEW_SECRET (>=16 bytes). Empty disables the /v1/preview route. The preview reverse proxy that serves these URLs is a separate component (cmd/preview-proxy).")
	flag.IntVar(&previewTTLSecs, "preview-ttl-seconds", 3600, "How long a minted preview URL stays valid (issue #126). Default 3600 (1h).")
	flag.Parse()

	if !mockMode && (kernelPath == "" || rootfsPath == "" || agentBin == "") {
		fmt.Fprintln(os.Stderr, "error: --kernel, --rootfs and --agent-bin are required (or use --mock)")
		os.Exit(1)
	}

	if err := os.MkdirAll(dataDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "error: create data dir %s: %v\n", dataDir, err)
		os.Exit(1)
	}

	s := newServer(dataDir, rootfsPath, mockMode, maxStreamsPerSandbox, maxExecTimeoutSecs)

	// Preview URLs (issue #126): enabled when a domain is set and the signing
	// secret is present. The secret VALUE is never logged; only its presence is
	// asserted. Disabled by default so the standalone server stays minimal.
	if previewDomain != "" {
		secret := os.Getenv("MITOS_PREVIEW_SECRET")
		if secret == "" {
			fmt.Fprintln(os.Stderr, "error: --preview-domain set but MITOS_PREVIEW_SECRET is empty (need >=16 bytes)")
			os.Exit(1)
		}
		signer, err := preview.NewSigner([]byte(secret))
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: preview signer: %v\n", err)
			os.Exit(1)
		}
		s.previewSigner = signer
		s.previewDomain = previewDomain
		s.previewTTL = time.Duration(previewTTLSecs) * time.Second
	}

	auditor, auditCloser, err := daemon.AuditorFromFlag(auditLog)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if auditCloser != nil {
		defer auditCloser.Close()
	}
	s.sandboxAPI.SetAuditor(auditor)

	if !mockMode {
		// Real mode forks real VMs through the proven KVM-backed fork engine, the
		// same path cmd/mem-smoke and cmd/crash-reap-smoke drive: CreateTemplate
		// builds a snapshot template (injecting --agent-bin as /init and appending
		// init=/init to the boot args), Fork restores it into a live microVM and
		// returns the real per-fork vsock UDS, and Terminate reaps it. The engine
		// is constructed HERE (not in newServer) because fork.NewEngine calls
		// validateKVM and would fail on a non-KVM host; the real-mode unit tests
		// build the server via newServer and never touch KVM.
		engine, err := fork.NewEngine(dataDir, firecrackerBin, kernelPath, firecracker.JailerConfig{}, fork.EngineOpts{
			AllowUnverified: true,
			AgentBinPath:    agentBin,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: fork engine: %v\n", err)
			os.Exit(1)
		}
		s.engine = engine
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("POST /v1/templates", s.handleCreateTemplate)
	mux.HandleFunc("GET /v1/templates", s.handleListTemplates)
	mux.HandleFunc("POST /v1/fork", s.handleFork)
	// Live-state fork (issue #596): fork an ALREADY-RUNNING sandbox, carrying its
	// live memory AND its on-disk filesystem to the child, instead of re-forking
	// the cold template. The source is the {id} path component; the child boots
	// from the source's paused checkpoint through engine.ForkRunning. This is the
	// standalone-server peer of the cluster FromSandbox path the cluster SDK
	// already drives.
	mux.HandleFunc("POST /v1/sandboxes/{id}/fork", s.handleForkRunning)
	mux.HandleFunc("GET /v1/sandboxes", s.handleListSandboxes)
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", s.handleTerminate)
	// Standalone guest-port forward (issue #228): open a host TCP listener that
	// bridges to a guest loopback port over a vsock tunnel, returning the host
	// address the caller dials. Real mode only; the Kubernetes Service/Ingress
	// routing and the CRD port-declaration fields are tracked follow-ups.
	mux.HandleFunc("POST /v1/sandboxes/{id}/forward", s.handleForward)
	// Authenticated guest HTTP proxy (Mitos Expose slice 1): reverse-proxy to a
	// guest port over the vsock tunnel, streaming responses (SSE-safe). On the
	// standalone server this inherits the loopback tokenless trust model.
	mux.HandleFunc("/v1/sandboxes/{id}/expose/{port}/", s.sandboxAPI.HandleExpose)
	mux.HandleFunc("/v1/sandboxes/{id}/expose/{port}", s.sandboxAPI.HandleExpose)
	// Preview URLs (issue #126): mint a signed, expiring URL for get_host(port).
	mux.HandleFunc("POST /v1/preview", s.handlePreview)

	// Runtime exec, files, and run_code go through the Connect sandbox.v1.Sandbox
	// protocol mounted below; the legacy JSON /v1 runtime and /v1/pty routes were
	// removed once every SDK and kubectl-mitos moved to Connect (#358).
	apiHandler := s.sandboxAPI.Handler()
	// Live lifecycle controls (issue #218): adjust a running sandbox's TTL, and
	// pause/resume. These go through the same SandboxAPI handler forkd serves and
	// have no Connect runtime successor.
	mux.Handle("POST /v1/set_timeout", apiHandler)
	mux.Handle("POST /v1/pause", apiHandler)
	mux.Handle("POST /v1/resume", apiHandler)

	// Connect runtime protocol (issue #24): route the Sandbox service RPC path
	// (/sandbox.v1.Sandbox/...) to the SandboxAPI.Handler() handler, which serves
	// the FULL Guest-wired Connect service (every RPC delegates through the guest
	// agent's gRPC server over vsock via vsockGuestConn), tokenless on the
	// standalone server (AllowTokenlessInterceptor). The path does not overlap the
	// /v1/* REST routes above. The handler is reached over Connect HTTP, including
	// the gRPC and gRPC-Web protocols.
	mux.Handle("/sandbox.v1.Sandbox/", apiHandler)

	// Unencrypted HTTP/2 lets Connect bidi streams (Exec) work without TLS, while
	// plain HTTP/1.1 requests to the /v1/* REST routes pass through unchanged: the
	// server negotiates HTTP/1.1 or h2c per connection on the same mux, so the
	// REST surface is unaffected.
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{Addr: addr, Handler: mux, Protocols: &protocols}
	log.Printf("sandbox-server listening on %s (mock=%v)", addr, mockMode)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	resp(w, map[string]any{
		"status": "ok", "mock": s.mockMode,
		"templates": len(s.templates), "sandboxes": len(s.sandboxes),
	})
}

// networkConfig is the standalone REST representation of the per-sandbox network
// posture (issue #219), mirroring the CRD NetworkPolicy and Modal's knobs. It is
// attached to a template at create time and applies to every sandbox forked from
// it. The secure default for an untrusted sandbox is deny-by-default in both
// directions: when no network config is supplied, egress is "deny" (no allows)
// and inbound is deny-by-default. All fields are config, never secrets.
type networkConfig struct {
	// Block drops ALL egress (Modal block_network=True), overriding Egress and
	// the allowlists below.
	Block bool `json:"block,omitempty"`
	// Egress is the default verdict for traffic matching no allow rule: "deny"
	// (the secure default) or "allow". Empty defaults to deny.
	Egress string `json:"egress,omitempty"`
	// AllowDomains is the DNS-name egress allowlist (host:port; enforced via the
	// controlled resolver). Modal outbound_domain_allowlist.
	AllowDomains []string `json:"allow_domains,omitempty"`
	// AllowCIDRs is the egress CIDR allowlist (Modal outbound_cidr_allowlist).
	AllowCIDRs []string `json:"allow_cidrs,omitempty"`
	// Inbound governs unsolicited inbound to the guest: "deny" (the secure
	// default) or "allow". Empty defaults to deny-by-default.
	Inbound string `json:"inbound,omitempty"`
	// InboundCIDRs narrows an inbound "allow" to source CIDRs (Modal
	// inbound_cidr_allowlist).
	InboundCIDRs []string `json:"inbound_cidrs,omitempty"`
}

type workloadReadyReq struct {
	Port           uint32 `json:"port"`
	Path           string `json:"path,omitempty"`
	Expect         uint32 `json:"expect,omitempty"`
	TimeoutSeconds uint32 `json:"timeout_seconds,omitempty"`
}

type workloadReq struct {
	Command []string          `json:"command,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Ready   *workloadReadyReq `json:"ready,omitempty"`
}

type resourcesReq struct {
	VcpuCount  int32 `json:"vcpu_count,omitempty"`
	MemSizeMib int64 `json:"mem_size_mib,omitempty"`
}

type createTemplateReq struct {
	ID           string         `json:"id"`
	InitWaitSecs int            `json:"init_wait_seconds"`
	Network      *networkConfig `json:"network,omitempty"`
	// Workload starts a long-running process during template build so the
	// snapshot captures it already serving and a fork wakes warm (issue #460).
	// The engine already supports this; the cluster path wires it via forkd.
	Workload *workloadReq `json:"workload,omitempty"`
	// Resources sizes the build VM. Omitted leaves the engine default (512 MiB,
	// 1 vCPU); a Chromium template needs more.
	Resources *resourcesReq `json:"resources,omitempty"`
}

// workloadFromReq maps the JSON workload to the engine's firecracker.WorkloadSpec.
// nil maps to nil so the engine keeps its no-workload default.
func workloadFromReq(w *workloadReq) *firecracker.WorkloadSpec {
	if w == nil || len(w.Command) == 0 {
		return nil
	}
	spec := &firecracker.WorkloadSpec{Command: w.Command, Env: w.Env}
	if w.Ready != nil {
		spec.Ready = &firecracker.WorkloadHTTPReady{
			Port:           w.Ready.Port,
			Path:           w.Ready.Path,
			Expect:         w.Ready.Expect,
			TimeoutSeconds: w.Ready.TimeoutSeconds,
		}
	}
	return spec
}

// vmResFromReq maps the JSON resources to the engine's firecracker.VMResources.
// nil maps to nil so the engine keeps its default sizing.
func vmResFromReq(r *resourcesReq) *firecracker.VMResources {
	if r == nil || (r.VcpuCount == 0 && r.MemSizeMib == 0) {
		return nil
	}
	return &firecracker.VMResources{VcpuCount: r.VcpuCount, MemSizeMib: r.MemSizeMib}
}

// idPattern constrains every caller-supplied id (template, sandbox) that the
// engine later embeds in host filesystem paths (templates/<id>/...,
// sandboxes/<id>/...). No dots and no separators, so a validated id can never
// introduce a `..` segment or an extra path element. This is the REST-boundary
// half of the C1 traversal defense, mirroring the forkd gRPC validateSandboxID
// guard; the engine re-validates as defense in depth.
var idPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// safeIDComponent reports whether id is a safe single host-path segment.
func safeIDComponent(id string) bool { return idPattern.MatchString(id) }

func (s *server) handleCreateTemplate(w http.ResponseWriter, r *http.Request) {
	var req createTemplateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errResp(w, "invalid json", 400)
		return
	}
	if req.ID == "" {
		errResp(w, "id is required", 400)
		return
	}
	if !safeIDComponent(req.ID) {
		errResp(w, "invalid id: must be 1-64 characters of [a-zA-Z0-9_-], starting with a letter or digit (no dots, no slashes)", 400)
		return
	}
	if req.InitWaitSecs == 0 {
		req.InitWaitSecs = 5
	}

	// Idempotency (issue #22): a repeat with the same Idempotency-Key returns the
	// template the first call created, never a duplicate. Resolve the binding
	// before doing any creating work; a client retrying after a timeout must not
	// drive a second create.
	idemKey := r.Header.Get(idempotencyHeader)
	existingID, idemSt := s.beginIdempotent(idemKey, idempotencyTemplate)
	if idemSt == idemInFlight {
		errResp(w, "a create with this Idempotency-Key is already in progress; retry shortly", 409)
		return
	}
	if idemSt == idemReplay {
		s.mu.RLock()
		existing := s.templates[existingID]
		s.mu.RUnlock()
		if existing != nil {
			resp(w, existing)
			return
		}
		// The recorded template was since deleted; fall through and recreate under
		// the same key (recordIdempotent below rebinds it).
	}

	// Resolve and validate the network posture (issue #219). A nil network means
	// the secure default: deny-by-default egress and inbound. A malformed CIDR is
	// rejected here (fail-closed) so a sandbox never comes up with a partially
	// parsed allowlist.
	netCfg, nerr := resolveNetworkConfig(req.Network)
	if nerr != nil {
		s.releaseIdempotent(idemKey)
		errResp(w, fmt.Sprintf("invalid network config: %v", nerr), 400)
		return
	}

	start := time.Now()
	if s.mockMode {
		time.Sleep(100 * time.Millisecond)
	} else if s.engine != nil {
		// Real mode: build the snapshot template through the fork engine from the
		// base rootfs. The engine injects the guest agent (--agent-bin) as /init
		// and appends init=/init to the boot args, which the prior
		// TemplateManager-direct path did not, so an agent-at-/init rootfs no
		// longer panics with "no working init found".
		// forceRebuild is always false here: the standalone sandbox-server REST API
		// does not yet expose a force-rebuild knob (issue #584 wires that through
		// the k8s controller/forkd gRPC path only for now), so it always takes the
		// reuse-or-rebuild gate's default reuse-if-healthy behavior.
		if err := s.engine.CreateTemplate(req.ID, s.rootfsPath, nil, nil, workloadFromReq(req.Workload), vmResFromReq(req.Resources), false, false); err != nil {
			s.releaseIdempotent(idemKey)
			errResp(w, fmt.Sprintf("create template: %v", err), 500)
			return
		}
	}

	info := &templateInfo{
		ID: req.ID, Ready: true, CreatedAt: time.Now(),
		TimeMs:  float64(time.Since(start).Milliseconds()),
		Network: netCfg,
	}

	s.mu.Lock()
	s.templates[req.ID] = info
	s.recordIdempotent(idemKey, idempotencyTemplate, req.ID)
	s.mu.Unlock()

	log.Printf("template %q created in %.0fms", req.ID, info.TimeMs)
	resp(w, info)
}

// resolveNetworkConfig applies the secure default and validates a template's
// network posture (issue #219). The secure default for an untrusted sandbox is
// deny-by-default in BOTH directions: when no config is supplied, egress is
// "deny" with no allows and inbound is deny-by-default. Egress and Inbound
// strings are normalized to "deny"/"allow" (empty becomes "deny"), and the CIDR
// allowlists are validated with the same parser the datapath uses so a malformed
// CIDR is rejected fail-closed rather than silently dropped. The returned config
// is what the server records and echoes back.
func resolveNetworkConfig(in *networkConfig) (*networkConfig, error) {
	if in == nil {
		// Secure default: deny egress, deny-by-default inbound.
		return &networkConfig{Egress: string(v1.EgressDeny), Inbound: string(v1.InboundDeny)}, nil
	}
	out := *in
	if out.Egress == "" {
		out.Egress = string(v1.EgressDeny)
	}
	if out.Egress != string(v1.EgressDeny) && out.Egress != string(v1.EgressAllow) {
		return nil, fmt.Errorf("egress must be %q or %q, got %q", v1.EgressDeny, v1.EgressAllow, out.Egress)
	}
	if out.Inbound == "" {
		out.Inbound = string(v1.InboundDeny)
	}
	if out.Inbound != string(v1.InboundDeny) && out.Inbound != string(v1.InboundAllow) {
		return nil, fmt.Errorf("inbound must be %q or %q, got %q", v1.InboundDeny, v1.InboundAllow, out.Inbound)
	}
	if _, _, err := netconf.ParseCIDRList(out.AllowCIDRs); err != nil {
		return nil, fmt.Errorf("allow_cidrs: %w", err)
	}
	if _, _, err := netconf.ParseCIDRList(out.InboundCIDRs); err != nil {
		return nil, fmt.Errorf("inbound_cidrs: %w", err)
	}
	return &out, nil
}

// toNetworkPolicy maps the standalone REST networkConfig onto the CRD-shaped
// NetworkPolicy the fork engine consumes, so the standalone path and the k8s
// path drive the SAME datapath from the SAME policy model. The domain and CIDR
// allowlists both flow through (domains via the controlled resolver, CIDRs via
// the static chain rules). A nil input yields the fail-closed default policy.
func toNetworkPolicy(in *networkConfig) *v1.NetworkPolicy {
	if in == nil {
		return &v1.NetworkPolicy{Egress: v1.EgressDeny}
	}
	egress := v1.EgressPolicy(in.Egress)
	if egress == "" {
		egress = v1.EgressDeny
	}
	return &v1.NetworkPolicy{
		Egress:       egress,
		Allow:        in.AllowDomains,
		BlockNetwork: in.Block,
		AllowCIDRs:   in.AllowCIDRs,
		Inbound:      v1.InboundPolicy(in.Inbound),
		InboundCIDRs: in.InboundCIDRs,
	}
}

func (s *server) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	templates := make([]*templateInfo, 0, len(s.templates))
	for _, t := range s.templates {
		templates = append(templates, t)
	}
	resp(w, templates)
}

type forkReq struct {
	Template string `json:"template"`
	ID       string `json:"id"`
}

func (s *server) handleFork(w http.ResponseWriter, r *http.Request) {
	var req forkReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errResp(w, "invalid json", 400)
		return
	}
	// Path-traversal guard: both the new sandbox id and the source template id
	// become host path components in the engine. Reject unsafe ids at the
	// boundary with a 400 (CodeQL go/path-injection).
	if !safeIDComponent(req.ID) {
		errResp(w, "invalid id: must be 1-64 characters of [a-zA-Z0-9_-], starting with a letter or digit (no dots, no slashes)", 400)
		return
	}
	if !safeIDComponent(req.Template) {
		errResp(w, "invalid template: must be 1-64 characters of [a-zA-Z0-9_-], starting with a letter or digit (no dots, no slashes)", 400)
		return
	}

	// Idempotency (issue #22): a repeat fork with the same Idempotency-Key
	// returns the sandbox the first call forked, never a duplicate. Resolved
	// before any forking work so a retry never drives a second restore.
	idemKey := r.Header.Get(idempotencyHeader)
	existingID, idemSt := s.beginIdempotent(idemKey, idempotencyFork)
	if idemSt == idemInFlight {
		errResp(w, "a fork with this Idempotency-Key is already in progress; retry shortly", 409)
		return
	}
	if idemSt == idemReplay {
		s.mu.RLock()
		existing := s.sandboxes[existingID]
		s.mu.RUnlock()
		if existing != nil {
			resp(w, existing)
			return
		}
		// The recorded sandbox was since terminated; fall through and re-fork under
		// the same key (recordIdempotent below rebinds it).
	}
	s.mu.RLock()
	tmpl, ok := s.templates[req.Template]
	s.mu.RUnlock()
	if !ok {
		s.releaseIdempotent(idemKey)
		errResp(w, fmt.Sprintf("template %q not found", req.Template), 404)
		return
	}

	start := time.Now()
	if s.mockMode {
		time.Sleep(800 * time.Microsecond)
	}

	// vsockPath is the per-fork guest agent UDS exec/files dial through. In real
	// mode with a live engine it is the REAL path the restore returned; otherwise
	// (real-mode unit tests with no engine) it is derived from the data dir, which
	// does not exist on disk so the standalone unix fallback routes the dial to the
	// local agent. forkTimeMs is the measured restore time when the engine forks.
	vsockPath := ""
	forkTimeMs := float64(time.Since(start).Microseconds()) / 1000.0
	if !s.mockMode {
		if s.engine != nil {
			// Real fork: restore the snapshot into a live microVM through the proven
			// engine path. On failure release the idempotency reservation and reap any
			// partial VM so a failed fork never leaks a Firecracker process.
			res, err := s.engine.Fork(req.Template, req.ID, fork.ForkOpts{})
			if err != nil {
				s.releaseIdempotent(idemKey)
				_ = s.engine.Terminate(req.ID)
				errResp(w, fmt.Sprintf("fork %q: %v", req.ID, err), 500)
				return
			}
			vsockPath = res.VsockPath
			forkTimeMs = res.ForkTimeMs
		} else {
			// No engine (real-mode unit tests): derive the vsock path from the data
			// dir. It will not exist, so RegisterSandbox's unix fallback applies.
			vsockPath = filepath.Join(s.dataDir, "sandboxes", req.ID, "vsock.sock")
		}
	}

	// Inherit the template's network posture (issue #219). The same CRD-shaped
	// NetworkPolicy drives the host nftables datapath on a real forkd; here it is
	// recorded on the sandbox so the policy that governs its traffic is visible.
	info := &sandboxInfo{
		ID: req.ID, TemplateID: req.Template,
		Endpoint: "http://localhost:8080", CreatedAt: time.Now(),
		ForkTimeMs: forkTimeMs,
		Network:    toNetworkPolicy(tmpl.Network),
	}

	// In real mode, register the vsock connection for exec/files and run the
	// fork-correctness reseed handshake. A real-mode fork restores a snapshot,
	// so the guest shares the snapshot's CRNG state with every sibling fork;
	// without a reseed two forks emit duplicate TLS keys / tokens / nonces. We
	// FAIL CLOSED, the same policy as forkd and the husk path: if the agent is
	// unreachable, the notify fails, or the guest reports it did not reseed, the
	// fork is rejected and never registered. The mock mode has no guest, so it
	// skips the handshake.
	if !s.mockMode {
		if err := s.registerAndReseed(req.ID, vsockPath); err != nil {
			// Fail closed: a fork that never confirmed a reseed (even after bounded
			// retries) shares CRNG state with its siblings and is never served. Drop
			// any half-wired registration and reap the restored VM so it does not
			// leak. The error carries no entropy or secret values.
			s.sandboxAPI.UnregisterSandbox(req.ID)
			s.releaseIdempotent(idemKey)
			if s.engine != nil {
				_ = s.engine.Terminate(req.ID)
			}
			errResp(w, fmt.Sprintf("fork %q: %v", req.ID, err), 500)
			return
		}
	}

	s.mu.Lock()
	s.sandboxes[req.ID] = info
	s.recordIdempotent(idemKey, idempotencyFork, req.ID)
	s.mu.Unlock()

	log.Printf("fork %q from %q in %.2fms", req.ID, req.Template, info.ForkTimeMs)
	resp(w, info)
}

// liveForkReq is the body for POST /v1/sandboxes/{id}/fork: the child id and
// whether to pause the source across the checkpoint. Template is accepted and
// echoed for the hosted-gateway compatibility path (the gateway maps this route
// to sandbox.create, whose control-plane handler reads the template as the pool)
// but the STANDALONE live fork ignores it: the source is the {id} path
// component, and the child descends from that running sandbox, not a template.
type liveForkReq struct {
	ID          string `json:"id"`
	PauseSource bool   `json:"pause_source"`
	Template    string `json:"template"`
}

// handleForkRunning forks an ALREADY-RUNNING sandbox (issue #596). Unlike
// handleFork, which restores a cold template, this checkpoints the running
// source (memory + on-disk filesystem, captured while the source is paused so
// the two are consistent) and boots the child from that checkpoint through
// engine.ForkRunning. It reuses handleFork's guards: safe-id path-traversal
// checks on BOTH the source and child ids, the Idempotency-Key contract, and the
// FAIL-CLOSED reseed handshake (a child that never confirmed a fresh CRNG seed
// is never served). It returns the same sandboxInfo shape handleFork returns, so
// the SDK's post-fork reconnect logic is unchanged. The standalone server keeps
// the loopback tokenless trust model of the other /v1 routes.
func (s *server) handleForkRunning(w http.ResponseWriter, r *http.Request) {
	sourceID := r.PathValue("id")
	var req liveForkReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errResp(w, "invalid json", 400)
		return
	}
	// Path-traversal guard: BOTH the source (path component) and the new child id
	// (path component) become host path components in the engine. Reject unsafe
	// ids at the boundary with a 400 (CodeQL go/path-injection).
	if !safeIDComponent(sourceID) {
		errResp(w, "invalid source id: must be 1-64 characters of [a-zA-Z0-9_-], starting with a letter or digit (no dots, no slashes)", 400)
		return
	}
	if !safeIDComponent(req.ID) {
		errResp(w, "invalid id: must be 1-64 characters of [a-zA-Z0-9_-], starting with a letter or digit (no dots, no slashes)", 400)
		return
	}

	// Idempotency (issue #22): a repeat live fork with the same Idempotency-Key
	// returns the child the first call forked, never a duplicate. Resolved before
	// any checkpoint work so a retry never drives a second restore.
	idemKey := r.Header.Get(idempotencyHeader)
	existingID, idemSt := s.beginIdempotent(idemKey, idempotencyFork)
	if idemSt == idemInFlight {
		errResp(w, "a fork with this Idempotency-Key is already in progress; retry shortly", 409)
		return
	}
	if idemSt == idemReplay {
		s.mu.RLock()
		existing := s.sandboxes[existingID]
		s.mu.RUnlock()
		if existing != nil {
			resp(w, existing)
			return
		}
		// The recorded child was since terminated; fall through and re-fork under
		// the same key (recordIdempotent below rebinds it).
	}

	// The source must be an ALREADY-RUNNING sandbox: a live fork descends from a
	// running VM, not a template. A missing source is a 404, never a template
	// lookup.
	s.mu.RLock()
	source, ok := s.sandboxes[sourceID]
	s.mu.RUnlock()
	if !ok {
		s.releaseIdempotent(idemKey)
		errResp(w, fmt.Sprintf("sandbox %q not found", sourceID), 404)
		return
	}

	start := time.Now()
	if s.mockMode {
		time.Sleep(800 * time.Microsecond)
	}

	// vsockPath is the child's per-fork guest agent UDS. In real mode with a live
	// engine it is the REAL path engine.ForkRunning returned; otherwise (real-mode
	// unit tests with no engine) it is derived from the data dir, which does not
	// exist on disk so the standalone unix fallback routes the dial to the local
	// agent. forkTimeMs is the measured checkpoint+restore time when the engine
	// forks.
	vsockPath := ""
	forkTimeMs := float64(time.Since(start).Microseconds()) / 1000.0
	if !s.mockMode {
		if s.engine != nil {
			// Real live fork: checkpoint the running source and restore the child
			// through the proven engine path. On failure release the idempotency
			// reservation and reap any partial VM so a failed fork never leaks a
			// Firecracker process.
			res, err := s.engine.ForkRunning(sourceID, req.ID, req.PauseSource)
			if err != nil {
				s.releaseIdempotent(idemKey)
				_ = s.engine.Terminate(req.ID)
				errResp(w, fmt.Sprintf("live fork %q from %q: %v", req.ID, sourceID, err), 500)
				return
			}
			vsockPath = res.VsockPath
			forkTimeMs = res.ForkTimeMs
		} else {
			// No engine (real-mode unit tests): derive the vsock path from the data
			// dir. It will not exist, so RegisterSandbox's unix fallback applies.
			vsockPath = filepath.Join(s.dataDir, "sandboxes", req.ID, "vsock.sock")
		}
	}

	// The child inherits the SOURCE's template id and network posture: it descends
	// from the running source, so it lives under the same template and is governed
	// by the same policy.
	info := &sandboxInfo{
		ID: req.ID, TemplateID: source.TemplateID,
		Endpoint: "http://localhost:8080", CreatedAt: time.Now(),
		ForkTimeMs: forkTimeMs,
		Network:    source.Network,
	}

	// FAIL CLOSED reseed handshake, identical to the cold fork path: a live fork
	// restores a checkpoint, so the child shares the source's CRNG state until it
	// reseeds. If the agent is unreachable, the notify fails, or the guest reports
	// it did not reseed, the fork is rejected and never registered. Mock mode has
	// no guest, so it skips the handshake.
	if !s.mockMode {
		if err := s.registerAndReseed(req.ID, vsockPath); err != nil {
			s.sandboxAPI.UnregisterSandbox(req.ID)
			s.releaseIdempotent(idemKey)
			if s.engine != nil {
				_ = s.engine.Terminate(req.ID)
			}
			errResp(w, fmt.Sprintf("live fork %q from %q: %v", req.ID, sourceID, err), 500)
			return
		}
	}

	s.mu.Lock()
	s.sandboxes[req.ID] = info
	s.recordIdempotent(idemKey, idempotencyFork, req.ID)
	s.mu.Unlock()

	log.Printf("live fork %q from running %q in %.2fms", req.ID, sourceID, info.ForkTimeMs)
	resp(w, info)
}

// reseedFork runs the post-restore fork-correctness handshake against a real
// fork's guest agent: it generates 32 bytes of fresh crypto/rand entropy and a
// distinct generation, sends them via NotifyForked so the guest reseeds its
// kernel CRNG and steps its wall clock, and FAILS CLOSED on the reseed result.
// A nil response or ReseededRNG:false means the guest still shares its siblings'
// CRNG state, which is incorrect (not merely degraded), so it returns an error
// and the caller refuses to serve the fork. This mirrors the daemon's
// notifyForked and the husk productionNotifier. Entropy bytes are never logged;
// only the boolean reseed result is inspected.
// registerAndReseed connects to the fork's guest agent and runs the reseed
// handshake, retrying a bounded number of times on a transient failure. After a
// snapshot restore the guest agent resets its vsock listener, so the host's
// first connection can be stale and the first NotifyForked sees "connection
// closed"; that is a readiness race, not a reseed refusal. Each attempt gets a
// FRESH connection (RegisterSandbox), so a retry recovers once the restored
// agent is listening again. It FAILS CLOSED: if no attempt confirms a reseed
// (ReseededRNG:true), the last error is returned and the caller never serves the
// fork. The backoff grows linearly with the attempt (s.reseedBackoff base).
func (s *server) registerAndReseed(sandboxID, vsockPath string) error {
	const attempts = 6
	var last error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := s.sandboxAPI.RegisterSandbox(sandboxID, vsockPath); err != nil {
			last = fmt.Errorf("guest agent not connected: %w", err)
		} else {
			s.sandboxAPI.RegisterStreamPath(sandboxID, vsockPath)
			if err := s.reseedFork(sandboxID); err != nil {
				// Drop the stale registration before the next attempt redials.
				s.sandboxAPI.UnregisterSandbox(sandboxID)
				last = err
			} else {
				return nil
			}
		}
		if attempt < attempts {
			time.Sleep(time.Duration(attempt) * s.reseedBackoff)
		}
	}
	return last
}

func (s *server) reseedFork(sandboxID string) error {
	entropy := make([]byte, 32)
	if _, err := rand.Read(entropy); err != nil {
		return fmt.Errorf("generate fork entropy: %w", err)
	}
	gen := s.forkGeneration.Add(1)
	resp, err := s.sandboxAPI.NotifyForked(sandboxID, gen, entropy, nil, nil)
	if err != nil {
		return fmt.Errorf("notify guest of fork: %w", err)
	}
	if resp == nil || !resp.ReseededRNG {
		return fmt.Errorf("guest did not reseed its RNG after restore; refusing to serve a fork that shares CRNG state")
	}
	return nil
}

func (s *server) handleListSandboxes(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sandboxes := make([]*sandboxInfo, 0, len(s.sandboxes))
	for _, sb := range s.sandboxes {
		sandboxes = append(sandboxes, sb)
	}
	resp(w, sandboxes)
}

func (s *server) handleTerminate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	_, ok := s.sandboxes[id]
	if ok {
		delete(s.sandboxes, id)
	}
	s.mu.Unlock()

	if !ok {
		errResp(w, fmt.Sprintf("sandbox %q not found", id), 404)
		return
	}

	s.sandboxAPI.UnregisterSandbox(id)
	// Real mode: reap the live microVM through the engine (kills the Firecracker
	// process, removes its working dir, drops the journal record), the same path
	// crash-reap-smoke asserts. A terminate error is logged but does not fail the
	// response: the sandbox is already unregistered and gone from our maps, and
	// the engine's startup reconcile reaps any residue on a later restart.
	if s.engine != nil {
		if err := s.engine.Terminate(id); err != nil {
			log.Printf("terminate sandbox %q: engine reap: %v", id, err)
		}
	}
	log.Printf("terminated sandbox %q", id)
	resp(w, map[string]string{"status": "terminated", "id": id})
}

type forwardReq struct {
	GuestPort int `json:"guest_port"`
}

// handleForward opens a host-side TCP forward to a guest loopback port (issue
// #228, standalone slice): it asks the SandboxAPI to open a host TCP listener on
// loopback bridged over a vsock tunnel to the guest's 127.0.0.1:guest_port, and
// returns the host address (host:port) the caller dials. The host listener
// inherits the standalone server's tokenless trust model and binds to loopback
// only, so it is reachable only from the host running the server. The forward is
// torn down when the sandbox is terminated (UnregisterSandbox closes it).
//
// It is REAL MODE only: mock mode has no guest agent to tunnel to, so it returns
// a clean 501 unsupported rather than opening a dead listener. The Kubernetes
// Service/Ingress routing and the CRD port-declaration fields are explicit
// follow-ups of #228 and are not built here.
func (s *server) handleForward(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.mockMode {
		// Discriminable by HTTP status (501): port forwarding bridges a real guest
		// TCP socket, which mock mode does not have.
		e := apierr.Get(apierr.CodeInternal).WithCause("port forwarding is not supported in mock mode: it bridges a real guest TCP socket over vsock; run sandbox-server in real mode (a KVM-backed engine) to forward a guest port")
		e.Status = http.StatusNotImplemented
		apierr.Encode(w, e)
		return
	}

	var req forwardReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errResp(w, "invalid json", http.StatusBadRequest)
		return
	}
	if req.GuestPort < 1 || req.GuestPort > 65535 {
		errResp(w, fmt.Sprintf("guest_port %d out of range 1-65535", req.GuestPort), http.StatusBadRequest)
		return
	}

	s.mu.RLock()
	_, known := s.sandboxes[id]
	s.mu.RUnlock()
	if !known {
		errResp(w, fmt.Sprintf("sandbox %q not found", id), http.StatusNotFound)
		return
	}

	hostAddr, err := s.sandboxAPI.ForwardPort(id, req.GuestPort)
	if err != nil {
		// A forward for a sandbox with no connected agent surfaces here; report it
		// as a 404 (the sandbox is not reachable) with an actionable cause. The
		// cause names ids and ports only, never a secret value.
		errResp(w, fmt.Sprintf("open forward for sandbox %q to guest port %d: %v", id, req.GuestPort, err), http.StatusNotFound)
		return
	}

	log.Printf("opened forward for sandbox %q: host %s -> guest 127.0.0.1:%d", id, hostAddr, req.GuestPort)
	resp(w, map[string]any{"host": hostAddr, "guest_port": req.GuestPort})
}

type previewReq struct {
	Sandbox string `json:"sandbox"`
	Port    int    `json:"port"`
}

// handlePreview mints a signed, expiring preview URL for a sandbox port (issue
// #126, get_host(port)). The signing secret lives here, never in the SDK; the
// minted URL VALUE is a bearer credential and is never logged. The reverse proxy
// that serves the URL is cmd/preview-proxy.
func (s *server) handlePreview(w http.ResponseWriter, r *http.Request) {
	if s.previewSigner == nil {
		// Discriminable by HTTP status (501): preview is a server-side feature a
		// caller cannot turn on. Reuse the internal catalogue entry's envelope
		// shape but send 501 so the SDK can tell "not enabled" from a 500.
		e := apierr.Get(apierr.CodeInternal).WithCause("preview URLs are not enabled on this server: start sandbox-server with --preview-domain and MITOS_PREVIEW_SECRET, or run the preview proxy (cmd/preview-proxy)")
		e.Status = http.StatusNotImplemented
		apierr.Encode(w, e)
		return
	}
	var req previewReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errResp(w, "invalid json", http.StatusBadRequest)
		return
	}
	if req.Sandbox == "" {
		errResp(w, "sandbox is required", http.StatusBadRequest)
		return
	}
	if req.Port < 1 || req.Port > 65535 {
		errResp(w, fmt.Sprintf("port %d out of range 1-65535", req.Port), http.StatusBadRequest)
		return
	}
	s.mu.RLock()
	_, known := s.sandboxes[req.Sandbox]
	s.mu.RUnlock()
	if !known {
		errResp(w, fmt.Sprintf("sandbox %q not found", req.Sandbox), http.StatusNotFound)
		return
	}
	expiry := time.Now().Add(s.previewTTL)
	url, err := preview.MintURL(s.previewSigner, s.previewDomain, req.Sandbox, req.Port, expiry)
	if err != nil {
		errResp(w, "could not mint preview URL", http.StatusInternalServerError)
		return
	}
	resp(w, map[string]any{"url": url, "expires_unix": expiry.Unix()})
}

func resp(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func errResp(w http.ResponseWriter, msg string, code int) {
	apierr.Encode(w, codeForStatus(code).WithCause(msg))
}

// codeForStatus picks the catalogue entry for the HTTP statuses the standalone
// sandbox-server emits. It mirrors the daemon shim so both encoders share the
// same envelope shape.
func codeForStatus(status int) apierr.Error {
	switch status {
	case http.StatusBadRequest:
		return apierr.Get(apierr.CodeInvalidJSON)
	case http.StatusNotFound:
		return apierr.Get(apierr.CodeNotFound)
	default:
		return apierr.Get(apierr.CodeInternal)
	}
}
