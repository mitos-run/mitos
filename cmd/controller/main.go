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
	"mitos.run/mitos/internal/saas/pgstore"
	"mitos.run/mitos/internal/usage"
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
	var multiVMFork bool
	var liveCowFork bool
	var huskConnReuse bool
	var liveCowChildImport bool
	var prewarmChild bool
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
	var usageAPIAddr string
	var usageDatabaseDSN string
	var vitalsSampler bool
	var vitalsSamplerInterval time.Duration
	var exposeProxyAdminURL string
	var enableOrgTenancy bool
	var orgDefaultMaxSandboxes int
	var orgDefaultCPU string
	var orgDefaultMemory string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&mockMode, "mock", false, "Use mock fork engine (no KVM required, for local dev with kind)")
	flag.BoolVar(&disablePKIBootstrap, "disable-pki-bootstrap", false, "Skip creating the control plane CA and TLS Secrets; forkd dialing is then UNAUTHENTICATED unless the cluster brings its own certs")
	flag.StringVar(&otlpEndpoint, "otlp-endpoint", "", "OTLP gRPC endpoint (host:port) for OpenTelemetry trace export. Empty disables tracing (zero cost). Spans carry ids, counts, and timings only; never secret values")
	flag.BoolVar(&enableHuskPods, "enable-husk-pods", true, "Pod-native default (issue #18): each SandboxPool builds the template snapshot AND maintains a warm pool of pre-scheduled husk pods pinned to the snapshot-holding nodes; claims activate a dormant husk pod in place. This is the default; pass --enable-raw-forkd to fall back to the fork-per-claim path. Ignored when --enable-raw-forkd or --mock is set (both force raw-forkd).")
	flag.BoolVar(&enableRawForkd, "enable-raw-forkd", false, "Fallback run path: build the snapshot and fork per claim on a holder node (no husk pods). Off by default (the husk pod-native path runs). --mock implies this. husk-pods needs real KVM nodes; raw-forkd is the path the mock/dev overlay uses.")
	flag.BoolVar(&multiVMFork, "multi-vm-fork", false, "EXPERIMENTAL, DEFAULT OFF: route a hosted fork child into an ADDITIONAL VM spawned INSIDE the source husk pod (the spawn-vm control op) instead of a brand-new child pod, when the source pod is multi-VM capable (its stub runs --multi-vm). OFF is byte-for-byte the current new-pod-per-fork path, so nothing changes unless you opt in AND the source pod is multi-VM capable; a non-capable source silently falls back to a new pod. This wires the routing + status (child status.pod = source pod, status.vmId = the spawned VM); the per-pod and node memory accounting that pends or spills a fork when a pod is full is a follow-up, so co-location is capped conservatively and the remainder spills to new pods. Only used with --enable-husk-pods.")
	flag.BoolVar(&liveCowFork, "live-cow-fork", false, "EXPERIMENTAL, DEFAULT OFF: start warm husk pods with --live-cow-fork so a CO-LOCATED fork child shares the PARENT's resident guest memory (patched Firecracker memfd + userfaultfd write-protect) instead of restoring from the disk fork snapshot (milestone m4b), driving the hosted fork toward sub-100ms. SEPARATE from --multi-vm-fork so it can be deployed off and canaried independently. OFF is byte-for-byte the current disk co-location. The co-located child still falls back to the disk restore where the child-side memfd import is not yet complete, so turning it on never breaks a fork; it is a no-op off Linux (userfaultfd write-protect is Linux-only). Only meaningful with --enable-husk-pods.")
	flag.BoolVar(&liveCowChildImport, "live-cow-child-import", false, "EXPERIMENTAL, DEFAULT OFF: with --live-cow-fork on, opt warm husk pods onto the VMSTATE-ONLY fork capture (skip the ~364ms disk mem write) so a co-located child boots its guest RAM from the source shared memfd instead of the disk fork snapshot. REQUIRES a child-side-import patched Firecracker; off keeps the armed source writing the disk mem so every child restores from disk and no fork hangs. Only meaningful with --live-cow-fork and --enable-husk-pods.")
	flag.BoolVar(&prewarmChild, "prewarm-child", false, "EXPERIMENTAL, DEFAULT OFF: keep one dormant generic co-located child Firecracker pre-prepared per multi-vm husk pod so a fork adopts the ready child (fc_boot=0, prepare off the hot path) instead of booting one at fork time. Requires --multi-vm-fork and --enable-husk-pods; a fresh slot re-warms async, a miss falls back to on-demand prepare byte-for-byte.")
	flag.BoolVar(&huskConnReuse, "husk-conn-reuse", false, "EXPERIMENTAL, DEFAULT OFF: reuse ONE authenticated mTLS husk control connection per husk pod across control-plane RPCs (activate, fork-snapshot, spawn-vm, remove-fork-snapshot) instead of opening a fresh TCP+TLS handshake per RPC. A co-located fork does fork-snapshot then spawn-vm to the SAME source pod, so reuse saves the second full handshake and cuts the per-RPC connection-setup overhead toward zero, driving the hosted fork toward sub-100ms. OFF is byte-for-byte the current one-shot dial-per-RPC path. The husk server always supports both (a one-shot client that closes after one request and a reused connection that sends several), so this flag only changes the CONTROLLER side and can be canaried + rolled back independently. mTLS identity is verified on every dial and one request is in flight per connection, so a reused connection is neither less authenticated nor frame-interleaved (see docs/threat-model.md). Only meaningful with --enable-husk-pods.")
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
	flag.StringVar(&usageAPIAddr, "usage-api-address", "", "Serve the INTERNAL usage API (GET /internal/usage) on this address (for example :8092) so the hosted console can read the SAME per-org usage the collector recorded, without a shared database. Bearer-gated by the MITOS_USAGE_API_TOKEN environment variable (empty token fails closed: every request is refused). Empty address disables the listener. Only meaningful with --usage-collector on. The endpoint is machine-to-machine: the org is taken from the X-Mitos-Org header the console sets after it verified the session, and the store still scopes every read to that org. The path carries only ids, byte counts, and seconds, never secret values.")
	flag.StringVar(&usageDatabaseDSN, "usage-database-dsn", "", "Postgres DSN for DURABLE per-org usage records (issue #211): when set, the collector upserts billable usage into Postgres so metered consumption survives a controller restart, instead of the in-memory store that is lost on restart. Falls back to the "+pgstore.EnvDSN+" env var. Empty means the in-memory usage store (DEV ONLY; usage is lost on restart). Only used with --usage-collector on. The value is a secret and is never logged.")
	flag.BoolVar(&vitalsSampler, "vitals-sampler", false, "Run the guest vitals sampler: on a fixed interval, scrape every forkd node's GET /v1/vitals/node, attribute each sandbox to its org via the trusted mitos.run/org husk-pod label, aggregate per (org, pool), and publish the mitos_guest_cpu_steal_percent, mitos_guest_mem_balloon_bytes, mitos_guest_mem_used_bytes, and mitos_guest_process_count Prometheus gauges. cpu_steal is the MAX across the bucket (the worst-starved sandbox); memory and process_count are SUMs (the fleet footprint). OFF by default so a self-host deployment without guest telemetry is unaffected; turn it on for hosted/multi-tenant. The path carries only ids (for org resolution), pool names, and numeric vitals plus the process-list length, never argv, env, process command lines, pids, or secret values.")
	flag.DurationVar(&vitalsSamplerInterval, "vitals-sampler-interval", 60*time.Second, "Interval between guest vitals samples when --vitals-sampler is set. Defaults to the usage window (60s). Only used when --vitals-sampler is on.")
	flag.StringVar(&exposeProxyAdminURL, "expose-proxy-admin-url", "", "Expose proxy admin endpoint base URL for route-sync (POST /internal/routes); empty disables")
	flag.BoolVar(&enableOrgTenancy, "enable-org-tenancy", false, "Hosted-SaaS multi-tenancy (issue #288): run the OrgReconciler, which provisions a per-org isolation namespace (mitos-org-<id>) for every Org CR with PSA privileged labels, a per-org ResourceQuota ceiling, a LimitRange, a default-deny NetworkPolicy with a DNS egress allow, and the mitos-pool-secrets RoleBinding, all owner-referenced to the Org so org deletion cascades. OFF by default so a self-host single-tenant install is unaffected; hosted sets it true. Cross-org isolation is the separate namespace + default-deny NetworkPolicy + ResourceQuota + the microVM (a NetworkPolicy-enforcing CNI is required in a multi-tenant cluster; see docs/threat-model.md).")
	flag.IntVar(&orgDefaultMaxSandboxes, "org-default-max-sandboxes", 50, "Default per-org sandbox/pod count ceiling applied to an Org's ResourceQuota when the Org sets no spec.quota override. The per-org abuse-control primitive: it bounds how much one tenant can schedule. Only used with --enable-org-tenancy.")
	flag.StringVar(&orgDefaultCPU, "org-default-cpu", "32", "Default per-org aggregate CPU limit ceiling (a Kubernetes quantity, for example 32 or 32000m) applied to an Org's ResourceQuota when the Org sets no spec.quota override. Only used with --enable-org-tenancy.")
	flag.StringVar(&orgDefaultMemory, "org-default-memory", "64Gi", "Default per-org aggregate memory limit ceiling (a Kubernetes quantity, for example 64Gi) applied to an Org's ResourceQuota when the Org sets no spec.quota override. Only used with --enable-org-tenancy.")
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
		MultiVM:                   multiVMFork,
		LiveCowFork:               liveCowFork,
		LiveCowChildImport:        liveCowChildImport,
		PrewarmChild:              prewarmChild,
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
	// husk control-plane connection reuse (--husk-conn-reuse): when set, hand the
	// reconciler a connection pool so the activate/fork-snapshot/spawn-vm seams
	// reuse one authenticated mTLS connection per husk pod instead of dialing a
	// fresh handshake per RPC. Nil (the default) keeps the byte-for-byte one-shot
	// dial-per-RPC path.
	var huskConnPool *controller.HuskConnPool
	if huskConnReuse {
		huskConnPool = controller.NewHuskConnPool()
		logger.Info("husk control connection reuse: ENABLED; the controller reuses one authenticated mTLS control connection per husk pod across RPCs (a co-located fork's fork-snapshot + spawn-vm share one handshake). mTLS identity is verified on every dial")
	}

	sandboxReconciler := &controller.SandboxReconciler{
		Client:             mgr.GetClient(),
		Scheme:             mgr.GetScheme(),
		APIReader:          mgr.GetAPIReader(),
		NodeRegistry:       nodeRegistry,
		MaxPendingDuration: maxPendingDuration,
		EnableHuskPods:     enableHuskPods,
		MultiVMFork:        multiVMFork,
		LiveCowFork:        liveCowFork,
		HuskConns:          huskConnPool,
		LiveCowChildImport: liveCowChildImport,
		PrewarmChild:       prewarmChild,
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

	if multiVMFork && !enableHuskPods {
		logger.Info("WARNING: --multi-vm-fork is on but --enable-husk-pods is off; multi-vm fork routing only applies to husk pods, so the flag is a no-op on the raw-forkd path")
	} else if multiVMFork {
		logger.Info("multi-vm fork routing: ENABLED (experimental); a fork child co-locates as an additional VM inside a multi-VM-capable source pod (spawn-vm op) instead of a new pod, capped conservatively until per-pod memory accounting lands; a non-capable source falls back to a new pod")
	} else {
		logger.Info("multi-vm fork routing: disabled (default); every fork child gets its own pod. Pass --multi-vm-fork to co-locate fork children in a multi-VM-capable source pod")
	}

	if liveCowFork && !enableHuskPods {
		logger.Info("WARNING: --live-cow-fork is on but --enable-husk-pods is off; live-cow fork rides the husk pod spec, so the flag is a no-op on the raw-forkd path")
	} else if liveCowFork {
		logger.Info("live-cow fork: ENABLED (experimental); warm husk pods run --live-cow-fork so a co-located fork child shares the parent's resident guest memory instead of restoring from the disk fork snapshot, failing closed to the disk restore where the child-side memfd import is not yet complete. Separate from --multi-vm-fork; canary it independently")
	} else {
		logger.Info("live-cow fork: disabled (default); co-located fork children restore from the disk fork snapshot. Pass --live-cow-fork to canary parent-memory sharing")
	}

	if huskConnReuse && !enableHuskPods {
		logger.Info("WARNING: --husk-conn-reuse is on but --enable-husk-pods is off; connection reuse targets husk control RPCs, which only exist on the husk-pods path, so the flag is a no-op on the raw-forkd path")
	} else if !huskConnReuse {
		logger.Info("husk control connection reuse: disabled (default); each husk control RPC dials a fresh mTLS connection. Pass --husk-conn-reuse to reuse one connection per husk pod and cut per-RPC handshake overhead")
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

	// Per-org namespace tenancy (issue #288, the hosted-SaaS multi-tenant
	// boundary). OFF by default (--enable-org-tenancy) so a self-host single-tenant
	// install is unaffected: with the flag off the OrgReconciler is never wired, so
	// the controller does not touch namespaces/quotas/policies for org provisioning.
	if enableOrgTenancy {
		orgCPU, perr := resourceapi.ParseQuantity(orgDefaultCPU)
		if perr != nil {
			logger.Error(perr, "invalid --org-default-cpu", "value", orgDefaultCPU)
			os.Exit(1)
		}
		orgMem, perr := resourceapi.ParseQuantity(orgDefaultMemory)
		if perr != nil {
			logger.Error(perr, "invalid --org-default-memory", "value", orgDefaultMemory)
			os.Exit(1)
		}
		orgReconciler := &controller.OrgReconciler{
			Client:               mgr.GetClient(),
			PoolSecretsSubject:   "mitos-controller",
			PoolSecretsNamespace: poolControllerNamespace,
			DefaultMaxSandboxes:  int32(orgDefaultMaxSandboxes), //nolint:gosec // flag value, operator-controlled, bounded by ResourceQuota semantics
			DefaultCPU:           orgCPU,
			DefaultMemory:        orgMem,
		}
		if err := orgReconciler.SetupWithManager(mgr); err != nil {
			logger.Error(err, "unable to create controller", "controller", "Org")
			os.Exit(1)
		}
		logger.Info("org tenancy: ENABLED; provisioning per-org isolation namespaces (mitos-org-<id>) with PSA privileged labels, a ResourceQuota ceiling, a LimitRange, a default-deny NetworkPolicy + DNS egress, and the mitos-pool-secrets RoleBinding. Cross-org isolation is the separate namespace + default-deny NetworkPolicy + ResourceQuota + the microVM; a NetworkPolicy-enforcing CNI is required",
			"default-max-sandboxes", orgDefaultMaxSandboxes, "default-cpu", orgDefaultCPU, "default-memory", orgDefaultMemory)
	} else {
		logger.Info("org tenancy: disabled (default); self-host single-tenant. Pass --enable-org-tenancy for hosted multi-tenant per-org namespaces")
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
		// One shared store: the collector writes per-org usage records into it, and
		// the internal usage API serves reads of the SAME store to the console, so
		// the console shows exactly what was collected without a shared database.
		//
		// When a usage DSN is configured (issue #211) the store is durable Postgres,
		// so metered consumption survives a controller restart; otherwise it is the
		// in-memory store (DEV ONLY, lost on restart). The DSN is a secret and is
		// never logged: only the chosen backend is.
		var usageStore usage.UsageStore = usage.NewMemUsageStore()
		usageDSN := usageDatabaseDSN
		if usageDSN == "" {
			usageDSN = os.Getenv(pgstore.EnvDSN)
		}
		if usageDSN != "" {
			pg, err := pgstore.Open(context.Background(), usageDSN)
			if err != nil {
				// Never include the DSN in the error.
				logger.Error(err, "unable to open durable usage store; refusing to fall back to in-memory so usage is not silently lost")
				os.Exit(1)
			}
			defer pg.Close()
			usageStore = pgstore.NewPgUsageStore(pg.Pool())
			logger.Info("usage store: durable Postgres (records survive restart)")
		} else {
			logger.Info("usage store: in-memory (DEV ONLY, usage is lost on restart; set --usage-database-dsn for durable Postgres)")
		}
		// Claim-release usage terminations (issue #682): the sandbox reconciler
		// records an event at claim release/lifetime terminate, and the
		// collector's husk source drains it to bill the [last scrape, terminate]
		// tail window. The log is created only when the collector runs (the
		// reconciler's nil default records nothing, so self-host pays nothing),
		// and this assignment happens before mgr.Start, so no reconcile races it.
		usageTerminations := usage.NewTerminationLog()
		sandboxReconciler.UsageTerminations = usageTerminations
		if err := mgr.Add(&controller.UsageCollectorRunnable{
			Registry:     nodeRegistry,
			Client:       mgr.GetClient(),
			Cadence:      usageCollectorInterval,
			HTTPScheme:   "http",
			Store:        usageStore,
			Terminations: usageTerminations,
		}); err != nil {
			logger.Error(err, "unable to add usage collector")
			os.Exit(1)
		}
		logger.Info("usage collector: enabled", "interval", usageCollectorInterval.String())

		// Internal usage API (machine-to-machine, bearer-gated) so the hosted
		// console can read the collected per-org usage. The token is read from the
		// environment and never logged; an empty token makes the handler fail closed.
		if usageAPIAddr != "" {
			usageAPIToken := os.Getenv("MITOS_USAGE_API_TOKEN")
			if usageAPIToken == "" {
				logger.Info("WARNING: --usage-api-address is set but MITOS_USAGE_API_TOKEN is empty; the internal usage API will refuse every request (fail closed)")
			}
			// Display price list: MITOS_USAGE_PRICELIST (a JSON object mapping
			// onto usage.PriceList, dollars per unit; Helm value
			// controller.usage.priceList) REPLACES the illustrative defaults
			// when set, so the usage API's cost estimate can match the
			// deployment's configured billing rates instead of drifting from
			// them. A malformed value fails startup (fail closed); the parse
			// error carries the remediation text. See docs/saas/pricing.md.
			prices, err := usage.ParsePriceListConfig(os.Getenv("MITOS_USAGE_PRICELIST"))
			if err != nil {
				logger.Error(err, "invalid MITOS_USAGE_PRICELIST")
				os.Exit(1)
			}
			if err := mgr.Add(&controller.UsageAPIRunnable{
				Store:  usageStore,
				Prices: prices,
				Addr:   usageAPIAddr,
				Token:  usageAPIToken,
			}); err != nil {
				logger.Error(err, "unable to add internal usage API")
				os.Exit(1)
			}
			logger.Info("internal usage API: enabled", "addr", usageAPIAddr, "tokenConfigured", usageAPIToken != "")
		}
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
