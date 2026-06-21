package main

import (
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"mitos.run/mitos/api/v1alpha1"
	"mitos.run/mitos/internal/apierr"
	"mitos.run/mitos/internal/daemon"
	"mitos.run/mitos/internal/firecracker"
	"mitos.run/mitos/internal/netconf"
	"mitos.run/mitos/internal/preview"
)

// sandbox-server is a standalone REST API. No Kubernetes required.
// For production on k8s, use the controller + forkd instead.
// Both share the same fork engine and guest agent protocol.

type server struct {
	mu          sync.RWMutex
	templateMgr *firecracker.TemplateManager
	rootfsPath  string
	templates   map[string]*templateInfo
	sandboxes   map[string]*sandboxInfo
	mockMode    bool
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
	// configured). previewDomain is the base domain for <id>.preview.<domain>.
	// previewTTL is how long a minted URL stays valid.
	previewSigner *preview.Signer
	previewDomain string
	previewTTL    time.Duration
}

// newServer builds the standalone server and applies the SandboxAPI policy
// (token mode, unix fallback, and the per-sandbox concurrent-stream cap). It is
// the single construction seam main() and the flag-plumbing test share.
func newServer(dataDir, rootfsPath string, mockMode bool, maxStreamsPerSandbox, maxExecTimeoutSecs int) *server {
	s := &server{
		rootfsPath:           rootfsPath,
		templates:            make(map[string]*templateInfo),
		sandboxes:            make(map[string]*sandboxInfo),
		mockMode:             mockMode,
		sandboxAPI:           daemon.NewSandboxAPI(dataDir),
		maxStreamsPerSandbox: maxStreamsPerSandbox,
		maxExecTimeoutSecs:   maxExecTimeoutSecs,
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
	Network *v1alpha1.NetworkPolicy `json:"network,omitempty"`
}

func main() {
	var (
		addr                 string
		dataDir              string
		firecrackerBin       string
		kernelPath           string
		rootfsPath           string
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
	flag.StringVar(&rootfsPath, "rootfs", "", "Guest rootfs path (required unless --mock)")
	flag.BoolVar(&mockMode, "mock", false, "Mock mode (no KVM, simulated responses)")
	flag.StringVar(&auditLog, "audit-log", "", "Structured audit log of exec and file operations. A file path, or '-'/'stderr' for stderr. Empty disables auditing. Records command strings, paths, and byte counts only; never file content or secret values")
	flag.IntVar(&maxStreamsPerSandbox, "max-streams-per-sandbox", 16, "Per-sandbox ceiling on concurrent OPEN streams (production-blocker #2): streaming exec, run_code, and PTY each hold a dedicated vsock connection plus host goroutines for the command lifetime, so an unbounded number would exhaust host vsock connections and goroutines. A NEW stream opened over this cap is rejected with 429 (the too_many_streams error); existing streams are never killed. The cap is checked at stream OPEN, off the fork path. 0 disables the cap (unbounded, the prior behavior). Matches the forkd default of 16.")
	flag.IntVar(&maxExecTimeoutSecs, "max-exec-timeout-seconds", 86400, "Ceiling (seconds) on a requested exec or run_code timeout (issue #216). A request over the ceiling is REJECTED with the typed timeout_too_large error, never silently reduced. Default 86400 (24h). 0 disables the ceiling. Matches the forkd default.")
	flag.StringVar(&previewDomain, "preview-domain", "", "Base domain for per-sandbox preview URLs (issue #126): get_host(port) mints https://<id>.preview.<domain>/?token=... . Requires MITOS_PREVIEW_SECRET (>=16 bytes). Empty disables the /v1/preview route. The preview reverse proxy that serves these URLs is a separate component (cmd/preview-proxy).")
	flag.IntVar(&previewTTLSecs, "preview-ttl-seconds", 3600, "How long a minted preview URL stays valid (issue #126). Default 3600 (1h).")
	flag.Parse()

	if !mockMode && (kernelPath == "" || rootfsPath == "") {
		fmt.Fprintln(os.Stderr, "error: --kernel and --rootfs are required (or use --mock)")
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
		// Standalone sandbox-server keeps the direct-exec dev path; the
		// jailer launch path is wired through forkd.
		s.templateMgr = firecracker.NewTemplateManager(firecrackerBin, kernelPath, dataDir, firecracker.JailerConfig{})
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("POST /v1/templates", s.handleCreateTemplate)
	mux.HandleFunc("GET /v1/templates", s.handleListTemplates)
	mux.HandleFunc("POST /v1/fork", s.handleFork)
	mux.HandleFunc("GET /v1/sandboxes", s.handleListSandboxes)
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", s.handleTerminate)
	// Preview URLs (issue #126): mint a signed, expiring URL for get_host(port).
	mux.HandleFunc("POST /v1/preview", s.handlePreview)

	// Exec and files go through SandboxAPI → vsock → guest agent
	apiHandler := s.sandboxAPI.Handler()
	mux.Handle("POST /v1/exec", apiHandler)
	mux.Handle("POST /v1/exec/stream", apiHandler)
	mux.Handle("POST /v1/run_code/stream", apiHandler)
	mux.Handle("POST /v1/files/", apiHandler)
	// Live lifecycle controls (issue #218): adjust a running sandbox's TTL, and
	// pause/resume. These go through the same SandboxAPI handler forkd serves.
	mux.Handle("POST /v1/set_timeout", apiHandler)
	mux.Handle("POST /v1/pause", apiHandler)
	mux.Handle("POST /v1/resume", apiHandler)
	// The PTY route lives on the SandboxAPI's own outer mux (registered there
	// outside requireBearer); delegate the WebSocket upgrade GET to it.
	mux.Handle("GET /v1/pty", apiHandler)

	log.Printf("sandbox-server listening on %s (mock=%v)", addr, mockMode)
	if err := http.ListenAndServe(addr, mux); err != nil {
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

type createTemplateReq struct {
	ID           string         `json:"id"`
	InitWaitSecs int            `json:"init_wait_seconds"`
	Network      *networkConfig `json:"network,omitempty"`
}

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
	if req.InitWaitSecs == 0 {
		req.InitWaitSecs = 5
	}

	// Resolve and validate the network posture (issue #219). A nil network means
	// the secure default: deny-by-default egress and inbound. A malformed CIDR is
	// rejected here (fail-closed) so a sandbox never comes up with a partially
	// parsed allowlist.
	netCfg, nerr := resolveNetworkConfig(req.Network)
	if nerr != nil {
		errResp(w, fmt.Sprintf("invalid network config: %v", nerr), 400)
		return
	}

	start := time.Now()
	if s.mockMode {
		time.Sleep(100 * time.Millisecond)
	} else {
		cfg := firecracker.DefaultVMConfig()
		cfg.RootfsPath = s.rootfsPath
		if _, err := s.templateMgr.CreateTemplate(req.ID, cfg, nil); err != nil {
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
		return &networkConfig{Egress: string(v1alpha1.EgressDeny), Inbound: string(v1alpha1.InboundDeny)}, nil
	}
	out := *in
	if out.Egress == "" {
		out.Egress = string(v1alpha1.EgressDeny)
	}
	if out.Egress != string(v1alpha1.EgressDeny) && out.Egress != string(v1alpha1.EgressAllow) {
		return nil, fmt.Errorf("egress must be %q or %q, got %q", v1alpha1.EgressDeny, v1alpha1.EgressAllow, out.Egress)
	}
	if out.Inbound == "" {
		out.Inbound = string(v1alpha1.InboundDeny)
	}
	if out.Inbound != string(v1alpha1.InboundDeny) && out.Inbound != string(v1alpha1.InboundAllow) {
		return nil, fmt.Errorf("inbound must be %q or %q, got %q", v1alpha1.InboundDeny, v1alpha1.InboundAllow, out.Inbound)
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
func toNetworkPolicy(in *networkConfig) *v1alpha1.NetworkPolicy {
	if in == nil {
		return &v1alpha1.NetworkPolicy{Egress: v1alpha1.EgressDeny}
	}
	egress := v1alpha1.EgressPolicy(in.Egress)
	if egress == "" {
		egress = v1alpha1.EgressDeny
	}
	return &v1alpha1.NetworkPolicy{
		Egress:       egress,
		Allow:        in.AllowDomains,
		BlockNetwork: in.Block,
		AllowCIDRs:   in.AllowCIDRs,
		Inbound:      v1alpha1.InboundPolicy(in.Inbound),
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

	s.mu.RLock()
	tmpl, ok := s.templates[req.Template]
	s.mu.RUnlock()
	if !ok {
		errResp(w, fmt.Sprintf("template %q not found", req.Template), 404)
		return
	}

	start := time.Now()
	if s.mockMode {
		time.Sleep(800 * time.Microsecond)
	}

	// Inherit the template's network posture (issue #219). The same CRD-shaped
	// NetworkPolicy drives the host nftables datapath on a real forkd; here it is
	// recorded on the sandbox so the policy that governs its traffic is visible.
	info := &sandboxInfo{
		ID: req.ID, TemplateID: req.Template,
		Endpoint: "http://localhost:8080", CreatedAt: time.Now(),
		ForkTimeMs: float64(time.Since(start).Microseconds()) / 1000.0,
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
		vsockPath := fmt.Sprintf("/tmp/sandbox-server/sandboxes/%s/vsock.sock", req.ID)
		if err := s.sandboxAPI.RegisterSandbox(req.ID, vsockPath); err != nil {
			// Fail closed: a fork whose guest agent is unreachable cannot reseed.
			errResp(w, fmt.Sprintf("fork %q: guest agent not connected: %v", req.ID, err), 500)
			return
		}
		s.sandboxAPI.RegisterStreamPath(req.ID, vsockPath)
		if err := s.reseedFork(req.ID); err != nil {
			// Fail closed: drop the half-wired sandbox so an un-reseeded VM that
			// shares CRNG state with its siblings is never served. The error
			// carries no entropy or secret values.
			s.sandboxAPI.UnregisterSandbox(req.ID)
			errResp(w, fmt.Sprintf("fork %q: %v", req.ID, err), 500)
			return
		}
	}

	s.mu.Lock()
	s.sandboxes[req.ID] = info
	s.mu.Unlock()

	log.Printf("fork %q from %q in %.2fms", req.ID, req.Template, info.ForkTimeMs)
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
	log.Printf("terminated sandbox %q", id)
	resp(w, map[string]string{"status": "terminated", "id": id})
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
