package main

import (
	"context"
	"crypto/tls"
	"flag"
	"os"
	"time"

	resourceapi "k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/admission"
	"mitos.run/mitos/internal/controller"
	"mitos.run/mitos/internal/eventfeed"
	"mitos.run/mitos/internal/kms"
	"mitos.run/mitos/internal/observability"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	// v1: the consolidated three-noun API (issue #23, ADR 0007). One served
	// version equals stored version; v1alpha1 and v1alpha2 are removed.
	utilruntime.Must(v1.AddToScheme(scheme))
}

// resolveRunMode picks the single active controller run path from the flags.
// husk pods is the pod-native default; --enable-raw-forkd selects the
// fork-per-claim fallback, and --mock forces it too (the dev mock overlay has no
// KVM, so a husk pod's dormant VMM cannot run). It returns the resolved
// EnableHuskPods (husk on) and a rawForkd marker for logging. Exactly one path
// is active: huskPods == !rawForkd.
func resolveRunMode(enableHuskPods, enableRawForkd, mockMode bool) (huskPods, rawForkd bool) {
	rawForkd = enableRawForkd || mockMode
	huskPods = enableHuskPods && !rawForkd
	return huskPods, rawForkd
}

func main() {
	var metricsAddr string
	var probeAddr string
	var mockMode bool
	var disablePKIBootstrap bool
	var otlpEndpoint string
	var maxPendingDuration time.Duration
	var enableHuskPods bool
	var enableRawForkd bool
	var huskStubImage string
	var huskDNSUpstream string
	var huskControlPort int
	var huskDataDir string
	var huskMemoryHeadroom string
	var huskMemoryHeadroomPercent int
	var eventSinkURL string
	var kekFile string
	var workspaceMemorySnapshots bool
	var enablePrincipalWebhook bool
	var usageCollector bool
	var usageCollectorInterval time.Duration
	var vitalsSampler bool
	var vitalsSamplerInterval time.Duration
	var exposeProxyAdminURL string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&mockMode, "mock", false, "Use mock fork engine (no KVM required, for local dev with kind)")
	flag.BoolVar(&disablePKIBootstrap, "disable-pki-bootstrap", false, "Skip creating the control plane CA and TLS Secrets; forkd dialing is then UNAUTHENTICATED unless the cluster brings its own certs")
	flag.StringVar(&otlpEndpoint, "otlp-endpoint", "", "OTLP gRPC endpoint (host:port) for OpenTelemetry trace export. Empty disables tracing (zero cost). Spans carry ids, counts, and timings only; never secret values")
	flag.BoolVar(&enableHuskPods, "enable-husk-pods", true, "Pod-native default (issue #18): each SandboxPool builds the template snapshot AND maintains a warm pool of pre-scheduled husk pods pinned to the snapshot-holding nodes; claims activate a dormant husk pod in place. This is the default; pass --enable-raw-forkd to fall back to the fork-per-claim path. Ignored when --enable-raw-forkd or --mock is set (both force raw-forkd).")
	flag.BoolVar(&enableRawForkd, "enable-raw-forkd", false, "Fallback run path: build the snapshot and fork per claim on a holder node (no husk pods). Off by default (the husk pod-native path runs). --mock implies this. husk-pods needs real KVM nodes; raw-forkd is the path the mock/dev overlay uses.")
	flag.StringVar(&huskStubImage, "husk-stub-image", "mitos-husk-stub:latest", "Container image that runs the dormant-VMM stub in a husk pod. Only used with --enable-husk-pods.")
	flag.StringVar(&huskDNSUpstream, "husk-dns-upstream", "", "Comma-separated DNS resolver list (host:port) the husk-stub per-pod DNS proxy forwards allowlisted name queries to, tried in failover order (recommended: 1.1.1.1:53,8.8.8.8:53). Empty leaves name-based egress off (IP:port allowlists still enforced). Use a public resolver, not cluster DNS, so untrusted sandboxes cannot resolve internal service names.")
	flag.IntVar(&huskControlPort, "husk-control-port", controller.HuskControlPort, "TCP port the husk stub serves the mTLS network control on; the controller dials podIP:port to activate a dormant husk pod. Only used with --enable-husk-pods.")
	flag.StringVar(&huskDataDir, "husk-data-dir", "/var/lib/mitos", "forkd data directory on the node; the husk pod's read-only snapshot hostPath is rooted here (<dir>/templates/<id>/snapshot). Only used with --enable-husk-pods.")
	flag.StringVar(&huskMemoryHeadroom, "husk-memory-headroom", "256Mi", "Fixed-floor memory headroom added on top of a husk pod's memory request to size its memory LIMIT (production-blocker #2, cap 1). The limit must exceed the request because the cgroup holds MORE than the guest RAM: the Firecracker VMM, the husk-stub, and copy-on-write dirty-page slack. The effective headroom is max(this floor, --husk-memory-headroom-percent% of the request), so a large VM gets proportional slack and a small VM gets at least this floor. A too-tight limit OOM-kills a normal VM and destroys the activate latency; raise this if pods are OOM-killed at their configured RAM. Only used with --enable-husk-pods.")
	flag.IntVar(&huskMemoryHeadroomPercent, "husk-memory-headroom-percent", 25, "Proportional memory headroom (percent of the memory request) for a husk pod's memory LIMIT, considered alongside --husk-memory-headroom; the larger of the two is used. Only used with --enable-husk-pods.")
	flag.DurationVar(&maxPendingDuration, "max-pending-duration", controller.DefaultMaxPendingDuration, "How long a claim may stay Pending for lack of node capacity before it fails with a capacity-exhaustion error. Scale out nodes or raise the overcommit factor to admit more sandboxes.")
	flag.StringVar(&eventSinkURL, "event-sink-url", "", "Optional operator webhook the controller POSTs the workspace revision change feed to as CloudEvents 1.0 (workspace.revision.created, sandbox.phase.changed). Empty disables the webhook (Kubernetes Events are still always recorded). The feed carries names, content digests, lineage, and phases only; never secret values. The URL is operator config, the same trust class as a git rendezvous remote (see docs/threat-model.md).")
	flag.StringVar(&kekFile, "kek-file", "", "Path to the 32-byte AES-256 KEK file (mounted from a Kubernetes Secret) used to WRAP each Encrypted template's per-template DEK (envelope encryption). REQUIRED when any reconciled template sets Encrypted: true; without it EnsureEncKey fails closed. The KEK is a secret value: it is never logged. Cloud KMS providers (AWS/GCP/Vault) are a documented follow-up.")
	flag.BoolVar(&enablePrincipalWebhook, "enable-principal-webhook", false, "Register the validating admission webhook that requires a Sandbox's creator to be authorized to impersonate the ServiceAccount named in spec.serviceAccount (RBAC verb 'impersonate' on 'serviceaccounts'). spec.serviceAccount is the principal a memory-snapshot resume is bound to, so without this gate it is self-asserted. STRONGLY RECOMMENDED whenever --workspace-memory-snapshots is enabled in a multi-tenant cluster. Requires the webhook server certs and a ValidatingWebhookConfiguration (see the Helm chart admissionWebhook values). Off by default so single-tenant and webhook-less deployments are unaffected.")
	flag.BoolVar(&workspaceMemorySnapshots, "workspace-memory-snapshots", false, "Bind the workspace memory-snapshot seams (checkpoint-on-terminate, resume-on-activate, principal-bound existence) to the husk live-VM snapshot path so a checkpointed workspace head becomes RESUMABLE: a later claim with the SAME principal resumes the VM memory image paired with the workspace content. A memory image carries secrets-in-RAM and is bound to the capturing claim's ServiceAccount; it is NEVER served across principals (fail-closed refusal). Off by default: a checkpoint-on-terminate then fails loud rather than producing a falsely-resumable revision. The real bare-metal VM-memory image requires a KVM-capable kubelet and is cluster-gated; see docs/workspaces.md.")
	flag.BoolVar(&usageCollector, "usage-collector", false, "Run the live multi-node metering scraper: on a fixed interval, scrape every forkd node's GET /v1/metering, attribute each sandbox to its org via the trusted mitos.run/org husk-pod label, integrate idempotently into per-(org, sandbox, window) usage records, and publish the per-org mitos_usage_*_total Prometheus series. OFF by default so a self-host deployment that does not want metering is unaffected; turn it on for hosted/multi-tenant. The records land in an in-memory store for now (a durable store is a follow-up); the per-org metric is always published on /metrics. The path carries only ids, byte counts, and seconds, never secret values.")
	flag.DurationVar(&usageCollectorInterval, "usage-collector-interval", 60*time.Second, "Interval between usage metering scrapes when --usage-collector is set. Defaults to the usage window (60s). Only used when --usage-collector is on.")
	flag.BoolVar(&vitalsSampler, "vitals-sampler", false, "Run the guest vitals sampler: on a fixed interval, scrape every forkd node's GET /v1/vitals/node, attribute each sandbox to its org via the trusted mitos.run/org husk-pod label, aggregate per (org, pool), and publish the mitos_guest_cpu_steal_percent, mitos_guest_mem_balloon_bytes, mitos_guest_mem_used_bytes, and mitos_guest_process_count Prometheus gauges. cpu_steal is the MAX across the bucket (the worst-starved sandbox); memory and process_count are SUMs (the fleet footprint). OFF by default so a self-host deployment without guest telemetry is unaffected; turn it on for hosted/multi-tenant. The path carries only ids (for org resolution), pool names, and numeric vitals plus the process-list length, never argv, env, process command lines, pids, or secret values.")
	flag.DurationVar(&vitalsSamplerInterval, "vitals-sampler-interval", 60*time.Second, "Interval between guest vitals samples when --vitals-sampler is set. Defaults to the usage window (60s). Only used when --vitals-sampler is on.")
	flag.StringVar(&exposeProxyAdminURL, "expose-proxy-admin-url", "", "Expose proxy admin endpoint base URL for route-sync (POST /internal/routes); empty disables")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	logger := ctrl.Log.WithName("setup")

	shutdownTracing, err := observability.Setup(context.Background(), "mitos-controller", otlpEndpoint)
	if err != nil {
		logger.Error(err, "tracing setup failed")
		os.Exit(1)
	}
	defer func() { _ = shutdownTracing(context.Background()) }()

	// Resolve the single active run path. husk pods is the pod-native default;
	// --enable-raw-forkd selects the fork-per-claim fallback. --mock forces
	// raw-forkd: the dev mock overlay cannot really run a husk pod's dormant VMM
	// (it has no KVM), so mock implies the raw-forkd path the dev overlay uses.
	// forkd-the-builder runs regardless in both modes (it builds the snapshots).
	enableHuskPods, rawForkd := resolveRunMode(enableHuskPods, enableRawForkd, mockMode)
	if rawForkd {
		logger.Info("run path: raw-forkd (fork per claim); husk pods disabled", "reason-mock", mockMode, "reason-flag", enableRawForkd)
	} else {
		logger.Info("run path: husk pods (pod-native default); the pool builds the snapshot and maintains a warm husk pod pool. husk-pods requires real KVM nodes")
	}

	if mockMode {
		logger.Info("--mock: the controller discovers mock forkd instances via pod discovery and forces the raw-forkd run path (no husk pods, which need real KVM nodes)")
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		// Leader election: the Deployment runs multiple replicas for HA, so EXACTLY
		// ONE may run the reconcilers at a time. Without it every replica reconciles
		// every object, racing on status writes (optimistic-lock "object has been
		// modified") AND each independently selecting + claiming dormant husk pods
		// for the same claim, which under a refilling warm pool becomes a runaway
		// that drains the pool. The lease lives in the controller's own namespace.
		LeaderElection:   true,
		LeaderElectionID: "mitos-controller-leader",
	})
	if err != nil {
		logger.Error(err, "unable to create manager")
		os.Exit(1)
	}

	nodeRegistry := controller.NewNodeRegistry()

	// The revision change feed sink. Empty --event-sink-url builds a NopSink, so
	// only Kubernetes Events are recorded (always-on). A non-empty URL POSTs each
	// feed CloudEvent to the operator webhook, at-least-once with a dedupe id.
	eventSink := eventfeed.NewWebhookSink(eventSinkURL)
	if eventSinkURL == "" {
		logger.Info("revision change feed: webhook disabled (Kubernetes Events only); set --event-sink-url to enable the CloudEvents egress")
	} else {
		logger.Info("revision change feed: posting CloudEvents to the operator webhook", "sink", eventSinkURL)
	}

	// The peer token forkd accepts on its token-gated CAS surface. The controller
	// passes it in every PullTemplate so a deficit node can pull a template from a
	// holder; it must match forkd's --peer-token. Sourced from the environment
	// (not a flag) so it is never exposed in the process argv. Empty disables
	// distribution by pull (every node builds its own snapshot). A credential:
	// never logged.
	peerToken := os.Getenv("FORKD_PEER_TOKEN")

	// The expose proxy admin token used to authenticate route-sync POSTs to
	// --expose-proxy-admin-url. Sourced from the environment (not a flag) so the
	// token is never in the process argv and never logged.
	exposeProxyAdminToken := os.Getenv("EXPOSE_PROXY_ADMIN_TOKEN")

	poolControllerNamespace := os.Getenv("FORKD_NAMESPACE")
	if poolControllerNamespace == "" {
		poolControllerNamespace = "mitos"
	}

	// Build the envelope-encryption KMS from --kek-file. The KEK is loaded by
	// PATH (never a value in argv) and its bytes are never logged; only the
	// non-secret KEK id is. When --kek-file is empty no KMS is wired and
	// EnsureEncKey fails closed for any Encrypted template (a plaintext-only
	// deployment is unaffected). Cloud KMS providers are a documented follow-up.
	var encKMS kms.Wrapper
	if kekFile != "" {
		w, kerr := kms.LoadLocalKEKFromFile(kekFile)
		if kerr != nil {
			logger.Error(kerr, "load KEK file")
			os.Exit(1)
		}
		encKMS = w
		logger.Info("envelope encryption KMS loaded", "kekID", w.KEKID())
	}

	huskHeadroomQty, err := resourceapi.ParseQuantity(huskMemoryHeadroom)
	if err != nil {
		logger.Error(err, "invalid --husk-memory-headroom", "value", huskMemoryHeadroom)
		os.Exit(1)
	}

	// The warm-pool autoscaler's demand signal: a single process-local tracker
	// SHARED between the pool reconciler (reads last claim arrival to gate
	// scale-down) and the claim reconciler (records arrivals). Both reconcilers
	// must get the same pointer.
	poolDemand := controller.NewPoolDemand()

	if err := (&controller.SandboxPoolReconciler{
		Client:                    mgr.GetClient(),
		NodeRegistry:              nodeRegistry,
		PeerToken:                 peerToken,
		EnableHuskPods:            enableHuskPods,
		HuskStubImage:             huskStubImage,
		HuskDNSUpstream:           huskDNSUpstream,
		KVMResourceName:           "mitos.run/kvm",
		DataDir:                   huskDataDir,
		HuskMemoryHeadroom:        huskHeadroomQty,
		HuskMemoryHeadroomPercent: huskMemoryHeadroomPercent,
		HuskTLSSecretName:         controller.HuskTLSSecretName,
		HuskCASecretName:          controller.CASecretName,
		ControllerNamespace:       poolControllerNamespace,
		KMS:                       encKMS,
		Demand:                    poolDemand,
	}).SetupWithManager(mgr); err != nil {
		logger.Error(err, "unable to create controller", "controller", "SandboxPool")
		os.Exit(1)
	}

	// The consolidated v1 Sandbox reconciler OWNS the engine directly (ADR 0007):
	// source.poolRef drives the claim engine, source.fromSandbox the fork engine,
	// source.fromRevision a not-served condition. It holds both the claim and fork
	// husk fields. Its HuskTLS (the controller client mTLS config used to dial a
	// husk stub's network control) is the SAME config EnsurePKI returns for forkd
	// dialing; it is assigned below after bootstrap, exactly like nodeRegistry.TLS.
	sandboxReconciler := &controller.SandboxReconciler{
		Client:             mgr.GetClient(),
		Scheme:             mgr.GetScheme(),
		APIReader:          mgr.GetAPIReader(),
		NodeRegistry:       nodeRegistry,
		MaxPendingDuration: maxPendingDuration,
		EnableHuskPods:     enableHuskPods,
		HuskControlPort:    huskControlPort,
		HuskStubImage:      huskStubImage,
		HuskDNSUpstream:    huskDNSUpstream,
		DataDir:            huskDataDir,
		KVMResourceName:    "mitos.run/kvm",
		HuskTLSSecretName:  controller.HuskTLSSecretName,
		HuskCASecretName:   controller.CASecretName,
		KMS:                encKMS,
		Demand:             poolDemand,
		Feed: controller.NewEmitFeed(
			// record.EventRecorder (the v1 events API) is still supported; the v2
			// events API GetEventRecorder returns a different type with a different
			// signature, so migrating is a separate change. controller-runtime's own
			// tests carry the same nolint on this deprecation.
			mgr.GetEventRecorderFor("mitos-controller"), //nolint:staticcheck // v1 events API supported; v2 migration out of scope
			eventSink,
			nil,
		),
	}
	// Workspace memory snapshots (W4 Phase 2): when --workspace-memory-snapshots
	// is set, bind the sandbox reconciler's checkpoint/resume/exists seams AND the
	// Workspace reconciler's resumable existence check to a single adapter over the
	// husk live-VM snapshot path. The adapter binds each snapshot to the capturing
	// sandbox's principal and refuses any cross-principal resume: a memory image
	// carries secrets-in-RAM and is never served across principals. Off by default,
	// the reconcilers keep their fail-closed defaults, so a checkpoint-on-terminate
	// fails loud instead of producing a revision falsely marked resumable.
	var wsMemAdapter *controller.WorkspaceMemorySnapshotAdapter
	if workspaceMemorySnapshots {
		wsMemAdapter = &controller.WorkspaceMemorySnapshotAdapter{
			// CheckpointLiveVM / RestoreLiveVM / SnapshotPresent are the bare-metal
			// live-VM hooks; left nil here, the adapter fails loud (it does not
			// fabricate a snapshot) until the cluster-gated live-VM path is wired.
		}
		sandboxReconciler.CheckpointMemory = wsMemAdapter.Checkpoint
		sandboxReconciler.ResumeMemory = wsMemAdapter.Resume
		sandboxReconciler.MemorySnapshotExists = wsMemAdapter.Exists
		logger.Info("workspace memory snapshots: ENABLED; checkpoint-on-terminate pairs a principal-bound VM memory image with the revision (resumable head). The bare-metal live-VM image is cluster-gated; without it the adapter fails loud")
	} else {
		logger.Info("workspace memory snapshots: disabled (default); a checkpoint-on-terminate fails loud. Pass --workspace-memory-snapshots to enable resumable heads")
	}

	if enableHuskPods {
		logger.Info("workspace transport: husk delegation ENABLED; a sandbox's /workspace hydrate/dehydrate is delegated to the husk-stub control op (the node owns the VM vsock + node CAS); the controller commits the revision and advances the head")
	}

	if err := sandboxReconciler.SetupWithManager(mgr); err != nil {
		logger.Error(err, "unable to create controller", "controller", "Sandbox")
		os.Exit(1)
	}

	// The Workspace reconciler (W4) is core, not behind the husk flag: it manages
	// the declarative Workspace model (the revision DAG, retention, lineage, and
	// head/revisions/resumable status). Its resumable status verifies a head's
	// paired memory snapshot still exists (principal-bound) through the SAME
	// adapter the claim reconciler resumes through, so a GC'd or cross-principal
	// snapshot flips resumable false and the status never advertises a resume that
	// would be refused. With --workspace-memory-snapshots off, SnapshotExists is
	// left nil (the reconciler's fail-closed default reports absent), so no head is
	// ever marked resumable.
	wsReconciler := &controller.WorkspaceReconciler{
		Client: mgr.GetClient(),
	}
	if wsMemAdapter != nil {
		wsReconciler.SnapshotExists = wsMemAdapter.Exists
	}
	if err := wsReconciler.SetupWithManager(mgr); err != nil {
		logger.Error(err, "unable to create controller", "controller", "Workspace")
		os.Exit(1)
	}

	if exposeProxyAdminURL != "" {
		er := &controller.ExposeRouteReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
			Poster: controller.NewExposePoster(exposeProxyAdminURL, exposeProxyAdminToken),
		}
		if err := er.SetupWithManager(mgr); err != nil {
			logger.Error(err, "unable to create controller", "controller", "ExposeRoute")
			os.Exit(1)
		}
		logger.Info("expose route-sync enabled", "proxy", exposeProxyAdminURL)
	} else {
		logger.Info("expose route-sync disabled (set --expose-proxy-admin-url to enable)")
	}

	discoveryNamespace := os.Getenv("FORKD_NAMESPACE")
	if discoveryNamespace == "" {
		discoveryNamespace = "mitos"
	}
	discovery := &controller.ForkdDiscovery{
		Client:    mgr.GetClient(),
		Registry:  nodeRegistry,
		Namespace: discoveryNamespace,
	}

	if disablePKIBootstrap {
		logger.Info("PKI bootstrap disabled; forkd dialing will be insecure unless the cluster brings its own certs")
		if enableHuskPods {
			// The husk activate channel delivers tenant secrets and refuses to send
			// them over an unauthenticated channel (ActivateHuskPod rejects a nil
			// TLS config). Without PKI there is no controller client cert to present,
			// so husk activation would fail closed; make that explicit at startup.
			logger.Info("PKI bootstrap disabled with --enable-husk-pods: husk activation requires the controller mTLS client cert and will fail closed until certs are provided")
		}
	} else {
		// mgr.GetClient() is cache-backed and the cache only starts with
		// mgr.Start, so bootstrap uses a direct client. Failure is fatal:
		// the control plane must not silently fall back to insecure dials.
		bootstrapClient, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
		if err != nil {
			logger.Error(err, "unable to create PKI bootstrap client")
			os.Exit(1)
		}
		tlsConf, err := controller.EnsurePKI(context.Background(), bootstrapClient, discoveryNamespace)
		if err != nil {
			logger.Error(err, "PKI bootstrap failed; refusing to start with unauthenticated forkd dialing (use --disable-pki-bootstrap to bring your own certs)")
			os.Exit(1)
		}
		nodeRegistry.TLS = tlsConf
		discovery.TLS = tlsConf
		// The husk control channel uses the SAME controller client config as a
		// fallback, but dials each husk pod pinning its PER-NAMESPACE identity
		// (husk.<ns>.mitos) so the shared forkd server key is never needed in a
		// tenant namespace. The controller leaf + CA are read from the controller
		// namespace at dial time (so a cert rotation is picked up).
		sandboxReconciler.HuskTLS = tlsConf
		huskDial := func(dialCtx context.Context, poolNamespace string) (*tls.Config, error) {
			return controller.HuskDialTLSConfig(dialCtx, mgr.GetClient(), discoveryNamespace, poolNamespace)
		}
		sandboxReconciler.HuskTLSFor = huskDial
		logger.Info("PKI bootstrap complete; dialing forkd with mTLS and husk pods with per-namespace identity", "namespace", discoveryNamespace)
	}

	if err := mgr.Add(discovery); err != nil {
		logger.Error(err, "unable to add forkd discovery")
		os.Exit(1)
	}

	if err := mgr.Add(&controller.GarbageCollector{
		Client:   mgr.GetClient(),
		Registry: nodeRegistry,
		// In husk mode node-loss recovery is owned by the husk re-pend path; the
		// GC must not terminally-fail a recoverable husk-backed claim.
		EnableHuskPods: enableHuskPods,
	}); err != nil {
		logger.Error(err, "unable to add garbage collector")
		os.Exit(1)
	}

	// Live usage metering scraper (issue #164), OFF by default. When enabled it
	// scrapes every forkd node's GET /v1/metering, attributes each sandbox to its
	// org via the trusted mitos.run/org husk-pod label, integrates idempotently,
	// and publishes the per-org mitos_usage_*_total Prometheus series from the same
	// records. The forkd operational mux serves /v1/metering over http today (the
	// same access class as /metrics and /healthz), so the scraper uses the http
	// scheme; an https operational mux is a documented follow-up.
	if usageCollector {
		if err := mgr.Add(&controller.UsageCollectorRunnable{
			Registry:   nodeRegistry,
			Client:     mgr.GetClient(),
			Cadence:    usageCollectorInterval,
			HTTPScheme: "http",
		}); err != nil {
			logger.Error(err, "unable to add usage collector")
			os.Exit(1)
		}
		logger.Info("usage collector: enabled", "interval", usageCollectorInterval.String())
	}

	// Guest vitals sampler (issue #164 Phase 1.a), OFF by default. When enabled it
	// scrapes every forkd node's GET /v1/vitals/node, attributes each sandbox to
	// its org via the trusted mitos.run/org husk-pod label, and publishes the
	// mitos_guest_* gauges aggregated per (org, pool). Like /v1/metering the node
	// vitals endpoint is served over http on the forkd operational mux today, so
	// the sampler uses the http scheme; an https mux is a documented follow-up. The
	// path carries no secret values: only ids, pool names, and numeric vitals.
	if vitalsSampler {
		if err := mgr.Add(&controller.VitalsSamplerRunnable{
			Registry:   nodeRegistry,
			Client:     mgr.GetClient(),
			Cadence:    vitalsSamplerInterval,
			HTTPScheme: "http",
		}); err != nil {
			logger.Error(err, "unable to add vitals sampler")
			os.Exit(1)
		}
		logger.Info("vitals sampler: enabled", "interval", vitalsSamplerInterval.String())
	}

	// Validating admission webhook: bind spec.serviceAccount to authorization so
	// the memory-snapshot principal field cannot be self-asserted. Opt-in: it
	// needs the webhook server certs (mgr default cert dir) and a
	// ValidatingWebhookConfiguration. Warn if memory snapshots are on without it,
	// because the principal gate is only an authorization boundary when this runs.
	if enablePrincipalWebhook {
		mgr.GetWebhookServer().Register("/validate-mitos-run-v1-sandbox", &webhook.Admission{
			Handler: &admission.ClaimServiceAccountValidator{
				Client:  mgr.GetClient(),
				Decoder: ctrladmission.NewDecoder(scheme),
			},
		})
		logger.Info("principal admission webhook: enabled (spec.serviceAccount requires impersonate authorization)")
	} else if workspaceMemorySnapshots {
		logger.Info("WARNING: --workspace-memory-snapshots is on but --enable-principal-webhook is off; spec.serviceAccount is self-asserted, so the memory-snapshot principal gate is NOT an authorization boundary in a multi-tenant cluster. Enable the webhook.")
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		logger.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		logger.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	logger.Info("starting controller")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		logger.Error(err, "controller exited with error")
		os.Exit(1)
	}
}
