package controller

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/husk"
	"mitos.run/mitos/internal/kms"
	"mitos.run/mitos/internal/observability"
	"mitos.run/mitos/internal/tenant"
	"mitos.run/mitos/internal/usage"
	"mitos.run/mitos/internal/vsock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// tracer is the controller component tracer; no-op unless tracing is configured.
var tracer = observability.Tracer("mitos-controller")

// DefaultMaxPendingDuration bounds how long a claim may stay Pending for lack of
// node capacity before the reconciler gives up and fails it with an actionable
// capacity-exhaustion message. Override per-deployment with --max-pending-duration.
const DefaultMaxPendingDuration = 5 * time.Minute

// pendingSinceAnnotation stamps, in RFC3339, the instant a claim first went
// Pending for lack of capacity. It is the durable source of truth for the
// bounded-wait deadline: a status condition's LastTransitionTime would reset on
// any unrelated condition churn, whereas this annotation only changes when the
// claim enters or leaves the capacity-pending state. Cleared on successful
// placement so a later capacity shortage starts a fresh clock.
const pendingSinceAnnotation = "mitos.run/capacity-pending-since"

// capacityPendingRequeue is the backoff between capacity-pending retries: long
// enough not to hot-loop a full cluster, short enough to place a claim promptly
// once a node frees up or a new node joins.
const capacityPendingRequeue = 5 * time.Second

// poolMissingSinceAnnotation stamps, in RFC3339, the instant the reconciler
// first observed the claim's referenced SandboxPool missing. Like
// pendingSinceAnnotation it is the durable anchor for a bounded-wait deadline:
// the pool may simply not have been applied yet (a manifest ordering race), so
// the claim pends for a grace period before failing terminally (issue #630).
// Cleared once the pool exists, so a later pool deletion starts a fresh clock.
const poolMissingSinceAnnotation = "mitos.run/pool-missing-since"

// huskHealthRequeue is how often a Ready husk claim re-checks its backing pod's
// readiness so its Ready condition reflects a node/pod outage (the endpoint is
// unreachable) instead of falsely staying Ready (#177). A pod watch would detect
// it sooner; this bounded poll is the contained fix.
const huskHealthRequeue = 15 * time.Second

// huskActivator is the seam the claim reconciler dials a husk stub through. The
// production value is ActivateHuskPod (huskclient.go); tests inject a fake to
// record requests without a real mTLS server.
type huskActivator func(ctx context.Context, addr string, tlsConf *tls.Config, req husk.ActivateRequest) (husk.ActivateResult, error)

// huskDialTLS resolves the controller client mTLS config to dial a husk pod in
// namespace ns, pinning the per-namespace husk identity (husk.<ns>.mitos) when a
// builder is configured; otherwise it falls back to the prebuilt HuskTLS. Every
// husk control-channel dial in this reconciler (activate, dehydrate, hydrate)
// must go through it so the dial pins the namespace whose leaf the husk pod
// serves.
func (r *SandboxReconciler) huskDialTLS(ctx context.Context, ns string) (*tls.Config, error) {
	if r.HuskTLSFor != nil {
		return r.HuskTLSFor(ctx, ns)
	}
	return r.HuskTLS, nil
}

// SandboxReconciler reconciles a v1 Sandbox (the consolidated run-axis kind, ADR
// 0007, issue #23). It OWNS the engine directly: a source.poolRef Sandbox drives
// the claim engine (node selection, forkd Fork, secret delivery, bearer token
// issue, endpoint/status, idle/lifetime, workspace binding, terminate-with-
// outputs); a source.fromSandbox Sandbox drives the fork engine (live fork,
// replicas:N, per-child status, secret-inheritance policy); a source.fromRevision
// Sandbox reports a clear not-served condition. No intermediate SandboxClaim or
// SandboxFork object is created (those kinds were removed in the v1 consolidation).
type SandboxReconciler struct {
	client.Client
	NodeRegistry *NodeRegistry

	// Scheme is the runtime scheme used to set controller owner references on the
	// per-sandbox token Secret and the fork child pods.
	Scheme *runtime.Scheme

	// HuskStubImage / HuskDNSUpstream / DataDir / KVMResourceName / HuskTLSSecretName
	// / HuskCASecretName configure the fork child husk pods on the fromSandbox path
	// (the husk fork engine). Only used when EnableHuskPods is true.
	HuskStubImage     string
	HuskDNSUpstream   string
	DataDir           string
	KVMResourceName   string
	HuskTLSSecretName string
	HuskCASecretName  string

	// forkSnapshot / removeForkSnapshot are the husk fork-snapshot control seams on
	// the fromSandbox path. Nil defaults to ForkSnapshotOnHusk / RemoveForkSnapshotOnHusk.
	// Tests inject fakes.
	forkSnapshot       huskForkSnapshotter
	removeForkSnapshot huskForkSnapshotRemover

	// MultiVMFork routes a husk fork child into an ADDITIONAL VM spawned INSIDE the
	// SOURCE pod (SpawnVMOnHusk) instead of a brand-new child pod, when the source
	// pod is multi-VM capable. EXPERIMENTAL, DEFAULT OFF: off is byte-for-byte the
	// buildForkChildPod new-pod path, so nothing changes unless an operator opts in
	// AND the source pod runs a --multi-vm husk stub (huskPodMultiVMCapable).
	// Co-location is gated by the per-pod MEMORY ACCOUNTING (L1.7b, guarantee A,
	// coLocatedForkVMBudget): a child co-locates only while the source pod's memory
	// budget (floor(memory.max / per-VM guest RAM) minus the source VM's own slot)
	// has room at the CoW worst case, and every child past the budget spills to a
	// new pod so a fork never overcommits the pod. Only used when EnableHuskPods is
	// true.
	MultiVMFork bool

	// LiveCowFork starts warm husk pods with --live-cow-fork so a CO-LOCATED fork
	// child shares the PARENT's resident guest memory (patched Firecracker memfd +
	// userfaultfd write-protect) instead of restoring from the disk fork snapshot
	// (milestone m4b). EXPERIMENTAL, DEFAULT OFF and SEPARATE from MultiVMFork so it
	// can be deployed off and canaried independently: off is byte-for-byte the disk
	// co-location. Only meaningful with EnableHuskPods (the flag rides the husk pod
	// spec); the co-located child still falls back to the disk restore until the
	// child-side memfd import lands, so turning it on never breaks a fork.
	LiveCowFork bool

	// spawnVM is the controller->husk spawn-vm seam used by the MultiVMFork routing.
	// Nil defaults to SpawnVMOnHusk; tests inject a fake.
	spawnVM huskVMSpawner

	// multiVMForkGate, when non-nil, overrides MultiVMFork so a test can toggle the
	// routing race-safely on the shared reconciler (the field itself is never
	// mutated after setup). Nil (the production default) reads MultiVMFork. Tests only.
	multiVMForkGate func() bool

	// KMS is the envelope-encryption Wrapper used to wrap a template's at-rest DEK
	// on the fork path (an idempotent read of the controller-owned Secret created
	// at build time). REQUIRED when any reconciled template is Encrypted;
	// EnsureEncKey fails closed if it is nil. Built from the controller
	// --kek-file (local AES-256-GCM KEK in dev/CI; cloud KMS is a follow-up).
	KMS kms.Wrapper

	// APIReader is an uncached reader straight to the apiserver
	// (mgr.GetAPIReader()). The deferred phase.changed emit reads the
	// post-reconcile phase through it so it always observes the status this
	// reconcile just persisted: the controller-runtime cache lags the apiserver
	// write, so a cached re-Get can read the stale phase, see no change, and drop
	// the event. A nil APIReader (a bare unit-test struct with no manager) falls
	// back to the cached client so those tests do not nil-panic.
	APIReader client.Reader

	// Feed is the workspace/sandbox change feed: the always-on Kubernetes Event
	// channel plus the opt-in CloudEvents sink. The claim reconciler emits a
	// sandbox.phase.changed event on every persisted phase transition. A zero Feed
	// (a bare reconciler in a unit test) records to a no-op recorder and NopSink.
	Feed emitFeed

	// MaxPendingDuration bounds the capacity-pending wait; zero falls back to
	// DefaultMaxPendingDuration. Set from the --max-pending-duration flag.
	MaxPendingDuration time.Duration

	// Now is the reconciler's clock, injectable for tests. Nil uses time.Now.
	Now func() time.Time

	// UsageTerminations, when set (hosted, usage collector on), receives one
	// usage.Termination per claimed, org-labeled husk pod at claim release or
	// lifetime terminate, so the usage collector can bill the half-open window
	// between the last scrape and the terminate instant (issue #682, was #664).
	// Nil (the self-host default) records nothing; every call is nil-safe and
	// best-effort: usage recording never blocks or fails a terminate.
	UsageTerminations *usage.TerminationLog

	// Demand is the shared per-pool claim-arrival tracker the warm-pool
	// autoscaler reads. The claim reconciler records an arrival whenever a claim
	// reaches the husk-claim path (whether it finds a warm pod or pends), so the
	// pool reconciler keeps the warm buffer up under load and resets the
	// scale-down cooldown. The SAME *PoolDemand instance must be shared with the
	// SandboxPoolReconciler. Nil disables demand recording (autoscaler then falls
	// back to never-recent, which only relaxes scale-down). Only used when
	// EnableHuskPods is true.
	Demand *PoolDemand

	// EnableHuskPods selects the husk-pod activation path (issue #18, slice 2):
	// the claim activates a dormant warm husk pod in place over the mTLS control
	// channel instead of SelectNode+forkOnNode. Default false: the raw-forkd path
	// is unchanged.
	EnableHuskPods bool

	// HuskTLS is the controller client mTLS config used to dial a husk stub's
	// network control (the SAME config that dials forkd, EnsurePKI's controller
	// leaf). Required when EnableHuskPods is true; a nil config makes
	// ActivateHuskPod refuse to send secrets.
	HuskTLS *tls.Config

	// HuskTLSFor, when set, builds the controller client mTLS config to dial a
	// husk pod in a given pool namespace, pinning that namespace's husk identity
	// (husk.<ns>.mitos). Production sets it to a closure over
	// controller.HuskDialTLSConfig bound to the controller namespace; when nil the
	// reconciler falls back to HuskTLS (the forkd.mitos-pinned config) so tests
	// that inject a fake activator are unaffected.
	HuskTLSFor func(ctx context.Context, poolNamespace string) (*tls.Config, error)

	// HuskControlPort is the TCP port the husk stub serves the mTLS control on.
	// Zero defaults to HuskControlPort. Only used when EnableHuskPods is true.
	HuskControlPort int

	// HuskSandboxPort is the in-pod port the activated VM's sandbox HTTP API is
	// reachable on; the claim's Status.Endpoint is podIP:this. Zero defaults to
	// the husk sandbox port (9091). Only used when EnableHuskPods is true.
	HuskSandboxPort int

	// Activate is the husk-activation seam. Nil defaults to ActivateHuskPod.
	// Tests inject a fake.
	Activate huskActivator

	// Checkpoint is the live-VM checkpoint seam used under a Checkpoint
	// DrainPolicy when an active claim's husk pod is lost. Nil defaults to
	// defaultHuskCheckpointer. Tests inject a fake to record the call. Only used
	// when EnableHuskPods is true.
	Checkpoint huskCheckpointer

	// HydrateWorkspace and DehydrateWorkspace are the workspace-binding transfer
	// seams (W4 slice 2). A claim with spec.workspaceRef hydrates its workspace
	// head into the sandbox on activate and dehydrates the sandbox /workspace into
	// a new committed WorkspaceRevision on terminate. Nil defaults to the real
	// node-side transport path; envtest injects fakes that record the manifest /
	// return a scripted digest without a VM.
	HydrateWorkspace   hydrateFunc
	DehydrateWorkspace dehydrateFunc

	// WorkspaceHydrateDelegate and WorkspaceDehydrateDelegate are the husk-mode
	// transport delegates the default hydrate/dehydrate paths route through when
	// EnableHuskPods is set. The controller is not on the node and cannot reach the
	// guest vsock or the node CAS, so it delegates the actual transfer to the
	// husk-stub control op that owns both (dial the claim's husk pod, like the fork
	// path). Nil defaults to the real path that dials the husk pod
	// (defaultHuskHydrate / defaultHuskDehydrate); envtest injects a recording
	// delegate. Only used when EnableHuskPods is true; the controller still owns the
	// WorkspaceRevision commit + head advance once the delegate returns the digest.
	WorkspaceHydrateDelegate   hydrateFunc
	WorkspaceDehydrateDelegate dehydrateFunc

	// WorkspaceDehydrateDiffDelegate is the husk-mode terminate delegate that
	// captures the sandbox /workspace into the node CAS AND, when a {diff: true}
	// output requested it, computes the content-hash diff of the new revision
	// against the parent head in the SAME node-CAS-side op (the controller is not on
	// the node and cannot read either manifest, so the diff must run where the node
	// CAS lives). It returns the new manifest digest and the optional diff. Nil
	// defaults to the real path that dials the husk pod (defaultHuskDehydrateWithDiff);
	// envtest injects a recording delegate. Only used when EnableHuskPods is true.
	WorkspaceDehydrateDiffDelegate huskDehydrateDiffFunc

	// DiffWorkspace computes a new revision's content-hash diff against the
	// workspace head before it, for a terminate {diff: true} output on the
	// raw-forkd path (the documented in-controller seam). Nil defaults to the real
	// store-backed path; envtest injects a fake. The husk path computes the diff on
	// the node via WorkspaceDehydrateDiffDelegate instead, since the controller
	// cannot read the node-CAS manifests.
	DiffWorkspace diffFunc

	// RendezvousGit pushes the workspace repo paths to a git rendezvous remote on
	// a per-attempt branch, for a terminate {git} output. Nil defaults to the real
	// path (workspace.Rendezvous via the git CLI); envtest and unit tests inject a
	// fake.
	RendezvousGit rendezvousFunc

	// RepoFilesForGit resolves the workspace spec.git.paths content from a
	// dehydrated revision manifest for a {git} output. Nil defaults to the real
	// store-backed path; envtest injects a fake.
	RepoFilesForGit repoFilesFunc

	// CheckpointMemory captures the sandbox VM memory snapshot on a
	// checkpoint-on-terminate (W4 Task 2), pairing it with the new revision.
	// ResumeMemory requests the memory-snapshot restore on activating a resumable
	// head. MemorySnapshotExists verifies a paired snapshot still exists and is
	// principal-bound (for the resumable status and the resume decision). Nil
	// defaults to a fail-closed real path; envtest injects fakes.
	CheckpointMemory     checkpointMemoryFunc
	ResumeMemory         resumeMemoryFunc
	MemorySnapshotExists memorySnapshotExistsFunc

	// eventFilter optionally restricts which claims this reconciler watches. Nil
	// watches all claims (the production default: a deployment runs exactly one
	// claim reconciler, husk or raw). It exists so a test harness can run a raw
	// and a husk reconciler on the same manager without the two fighting over the
	// same object.
	eventFilter predicate.Predicate

	// controllerName overrides the controller-runtime controller name. Empty uses
	// the kind-derived default. Only set by the test harness so two claim
	// reconcilers can coexist on one manager.
	controllerName string
}

// now returns the reconciler's current time, honoring the injectable clock.
func (r *SandboxReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// recordHuskDemand stamps a claim arrival for the claim's pool into the shared
// demand tracker, the autoscaler's signal that warm capacity is being consumed.
// It is best-effort: a nil tracker is a no-op. It records only the pool key and a
// timestamp, never any claim payload.
func (r *SandboxReconciler) recordHuskDemand(claim *v1.Sandbox) {
	if r.Demand == nil {
		return
	}
	key := claim.Namespace + "/" + claim.Spec.Source.PoolRef.Name
	r.Demand.RecordArrival(key, r.now())
}

// maxPendingDuration returns the configured bound or the default.
func (r *SandboxReconciler) maxPendingDuration() time.Duration {
	if r.MaxPendingDuration > 0 {
		return r.MaxPendingDuration
	}
	return DefaultMaxPendingDuration
}

// SandboxClaim ownership: get/list/watch to reconcile, update to write the
// terminate finalizer, delete for the garbage collector's TTL sweep of
// finished claims. status writes phase, conditions, and FinishedAt.
// +kubebuilder:rbac:groups=mitos.run,resources=sandboxclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mitos.run,resources=sandboxclaims/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mitos.run,resources=sandboxclaims/finalizers,verbs=update
// SandboxTemplate and SandboxPool are read-only inputs to claim placement.
// +kubebuilder:rbac:groups=mitos.run,resources=sandboxtemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups=mitos.run,resources=sandboxpools,verbs=get;list;watch
// Secrets: get/list to read mounted secrets referenced by a sandbox and to
// reconcile the per-sandbox token Secret; create/update to mint and heal that
// token Secret (and the controller's PKI Secrets, see EnsurePKI); delete to
// crypto-shred a template's at-rest encryption key Secret on teardown
// (DeleteEncKey).
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;delete
// The change feed mirrors each event as a Kubernetes Event on the source object
// (the always-on channel); the EventRecorder needs create;patch on events.
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// reconcilePoolRef owns the claim engine for a source.poolRef Sandbox: node
// selection, forkd Fork, secret delivery, per-sandbox bearer token issue,
// endpoint/status, idle/lifetime, workspace binding, and terminate-with-outputs.
// The Sandbox IS the running sandbox (no intermediate SandboxClaim object). The
// shared dispatcher (Reconcile) has already fetched the Sandbox and owns deletion
// and the phase.changed feed emit.
func (r *SandboxReconciler) reconcilePoolRef(ctx context.Context, claim *v1.Sandbox) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// controller.reconcileClaim spans the poolRef engine. Only the sandbox
	// name/namespace and the pool name (config, no secrets) are attributes; the
	// fork RPC below is a child span carrying the trace to forkd over gRPC.
	ctx, span := tracer.Start(ctx, "controller.reconcileClaim", trace.WithAttributes(
		attribute.String("claim.name", claim.Name),
		attribute.String("claim.namespace", claim.Namespace),
		attribute.String("pool", claim.Spec.Source.PoolRef.Name),
	))
	defer span.End()

	// Already assigned. In husk mode, FIRST check whether the backing husk pod
	// was lost (a node drain, an eviction, a deletion): a Ready claim must not
	// keep advertising a dead endpoint. A lost pod re-pends the claim per the
	// pool's DrainPolicy (Kill re-pends; Checkpoint snapshots the live VM first
	// where reachable, then re-pends). This runs before the lifetime path so an
	// enqueued pod-delete event promptly re-pends. When the pod is still healthy,
	// fall through to the normal lifetime reaping.
	if claim.Status.Phase == v1.SandboxReady {
		if r.EnableHuskPods {
			lost, lostPod, err := r.checkHuskPodLost(ctx, claim)
			if err != nil {
				return ctrl.Result{}, err
			}
			if lost {
				var pool v1.SandboxPool
				if perr := r.Get(ctx, client.ObjectKey{Namespace: claim.Namespace, Name: claim.Spec.Source.PoolRef.Name}, &pool); perr != nil {
					// The pool is gone too: re-pend with Kill semantics (an empty
					// DrainPolicy defaults to Kill in rependOnHuskPodLost).
					if client.IgnoreNotFound(perr) != nil {
						return ctrl.Result{}, perr
					}
				}
				return r.rependOnHuskPodLost(ctx, claim, &pool, lostPod)
			}
			// The pod is not lost, but it may be NotReady (its node is rebooting or
			// unreachable): the sandbox endpoint is then unreachable, so the claim
			// must not keep reporting Ready. Reflect the backing pod's readiness in
			// the Ready condition (flips back to True when the pod recovers). Actual
			// loss/eviction is still handled by checkHuskPodLost above (#177).
			if reflectHuskBackingReadiness(claim, lostPod, r.now()) {
				if err := r.Status().Update(ctx, claim); err != nil {
					return ctrl.Result{}, err
				}
			}
		}
		// A Ready claim bound to a workspace hydrates its head into the sandbox
		// exactly once (idempotent via the hydrated annotation). A transient
		// transfer error requeues without failing the Ready claim; the sandbox
		// stays usable (an unpopulated workspace) until the next attempt succeeds.
		if err := r.hydrateOnActivate(ctx, claim); err != nil {
			logger.Error(err, "hydrate workspace into sandbox; will retry", "claim", claim.Name)
			return ctrl.Result{RequeueAfter: capacityPendingRequeue}, nil
		}
		res, err := r.reconcileLifetime(ctx, claim)
		// Re-check backing-pod health periodically even when the claim has no
		// lifetime/idle timeout (reconcileLifetime returns no requeue then), so a
		// node/pod outage is reflected within huskHealthRequeue (#177).
		if r.EnableHuskPods && err == nil && res.RequeueAfter == 0 {
			res.RequeueAfter = huskHealthRequeue
		}
		return res, err
	}

	// Terminal phases: don't retry provisioning. A terminal claim must never
	// leave its backing husk pod running (issue #688): terminateLifetime deletes
	// the pod after stamping Terminated, and this reap is the self-heal for a
	// crash or a failed delete between the stamp and the reap. The husk
	// activation path can also fail the claim (SandboxFailed) AFTER the pod is
	// already claimed and running, for example the token-secret-write failure in
	// reconcileHuskClaim, with no usage tail ever recorded for that pod; reaping
	// here closes that gap too: for a Failed claim the reap records the one tail
	// event, for a Terminated claim the phase guard records nothing (its event
	// was recorded at the terminate instant). Idempotent and usually a NotFound
	// no-op: one label-selector List, empty in raw-forkd mode (no pod carries
	// the label). A static lingering Running pod emits no new watch event on its
	// own, so what actually converges a transient list/delete error here is the
	// returned error driving the controller-runtime workqueue's backoff retry
	// (plus a real pod event, if one occurs); the error is logged so a repeating
	// loop is diagnosable.
	if claim.Status.Phase == v1.SandboxFailed || claim.Status.Phase == v1.SandboxTerminated {
		if err := r.reapClaimHuskPods(ctx, claim); err != nil {
			logger.Error(err, "reap lingering claimed husk pods for terminal claim; will retry", "claim", claim.Name)
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Find the pool. A missing pool is NOT a reconcile error: erroring here
	// retried forever under the controller-runtime backoff with no terminal
	// state (issue #630). Instead the claim pends for a bounded grace period
	// (the pool may be applied moments after the sandbox), then fails
	// terminally with an actionable PoolNotFound condition.
	var pool v1.SandboxPool
	if err := r.Get(ctx, client.ObjectKey{
		Namespace: claim.Namespace,
		Name:      claim.Spec.Source.PoolRef.Name,
	}, &pool); err != nil {
		if apierrors.IsNotFound(err) {
			return r.reconcilePoolMissing(ctx, claim)
		}
		logger.Error(err, "get pool", "pool", claim.Spec.Source.PoolRef.Name)
		return ctrl.Result{}, err
	}

	// The pool exists: clear any pool-missing stamp so a later deletion of the
	// pool starts a fresh grace clock.
	if claim.Annotations[poolMissingSinceAnnotation] != "" {
		delete(claim.Annotations, poolMissingSinceAnnotation)
		if err := r.Update(ctx, claim); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Resolve the inline template (ADR 0007). The pool's spec.template is the
	// common path; spec.templateRef resolves a shared template-shaped pool.
	template, err := r.resolvePoolTemplate(ctx, &pool)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Single-writer-per-workspace: a claim bound to a workspace that is already
	// bound to another active claim must not acquire a VM. It pends with a clear
	// WorkspaceBusy reason and retries, so the second writer waits for the first
	// to release rather than two sandboxes racing to dehydrate the same workspace.
	if claim.Spec.WorkspaceRef != nil {
		busy, err := r.workspaceBusyClaim(ctx, claim)
		if err != nil {
			return ctrl.Result{}, err
		}
		if busy != "" {
			claim.Status.Phase = v1.SandboxPending
			setCondition(&claim.Status.Conditions, metav1.Condition{
				Type:               "Ready",
				Status:             metav1.ConditionFalse,
				LastTransitionTime: metav1.NewTime(r.now()),
				Reason:             "WorkspaceBusy",
				Message: fmt.Sprintf(
					"workspace %q is already bound to active claim %q; this claim will bind once that claim releases the workspace (single-writer-per-workspace)",
					claim.Spec.WorkspaceRef.Name, busy,
				),
			})
			// Best-effort status write; the return requeues regardless.
			_ = r.Status().Update(ctx, claim)
			return ctrl.Result{RequeueAfter: capacityPendingRequeue}, nil
		}
	}

	// Add the terminate finalizer before the claim acquires a backing VM, so
	// no Ready claim can ever be deleted without forkd reaping its sandbox.
	// This is a metadata Update, distinct from the status writes below.
	if controllerutil.AddFinalizer(claim, FinalizerTerminate) {
		if err := r.Update(ctx, claim); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Husk-pod activation path (issue #18, slice 2). When enabled, the claim
	// activates a dormant warm husk pod in place over the mTLS control channel
	// instead of SelectNode+forkOnNode. The default (flag off) leaves the
	// raw-forkd path below unchanged. The husk path stamps Restoring itself only
	// once it has actually selected a dormant pod to activate: a claim that cannot
	// place (NoHuskPod) must stay Pending and settle, not cycle
	// Pending -> Restoring -> Pending on every reconcile. That cycle is a hot loop
	// (each write re-triggers the claim's own watch) whose continuous full-status
	// writes from a stale read clobber an externally applied status (the husk e2e's
	// merge-patch Ready stamp), so the claim never settles.
	if r.EnableHuskPods {
		return r.reconcileHuskClaim(ctx, claim, &pool, template)
	}

	// Raw-forkd path: mark as restoring before attempting placement.
	claim.Status.Phase = v1.SandboxRestoring
	if err := r.Status().Update(ctx, claim); err != nil {
		return ctrl.Result{}, err
	}

	// Pick a node with a ready snapshot
	node, snapshotID, err := r.selectNode(ctx, &pool, template, claim.Spec.NodeName)
	if err != nil {
		// No node admits the fork under the overcommit policy: this is a real
		// capacity shortage, not a missing snapshot. Pend with backpressure and
		// fail cleanly after a bounded wait rather than hammering a full cluster
		// forever (or, worse, forcing a placement that OOMs a node).
		if errors.Is(err, ErrNoCapacity) {
			return r.reconcileNoCapacity(ctx, claim, err)
		}
		// No registered/healthy node, or no node holds the snapshot yet: a
		// transient placement precondition the pool reconciler is expected to
		// resolve. Pend and retry indefinitely (no bounded fail).
		logger.Info("no node available for placement, pending", "error", err.Error())
		beforeStatus := claim.Status.DeepCopy()
		r.clearPendingSince(claim)
		claim.Status.Phase = v1.SandboxPending
		recordClaimPending()
		// Best-effort status write, elided on a no-op re-pend; the return below
		// already requeues or surfaces the error.
		_ = r.writeClaimStatusIfChanged(ctx, claim, beforeStatus)
		return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
	}

	// Placement succeeded: clear any capacity-pending stamp so a later shortage
	// starts a fresh bounded-wait clock.
	if claim.Annotations[pendingSinceAnnotation] != "" {
		r.clearPendingSince(claim)
		if err := r.Update(ctx, claim); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Translate the template's volumes (with this claim's VolumeOverrides
	// applied) into the Fork RPC's VolumeMounts. The node prepares and attaches
	// the backing drives per policy; the controller only forwards the spec.
	volumes := volumeMounts(template.Volumes, claim.Spec.VolumeOverrides)

	// Resolve secrets
	env, secretVals, err := r.resolveSecrets(ctx, claim.Namespace, claim.Labels[tenant.OrgLabelKey], claim.Spec.Env, claim.Spec.Secrets)
	if err != nil {
		logger.Error(err, "secret resolution failed")
		recordClaimError(claim.Spec.Source.PoolRef.Name, "secret")
		now := metav1.Now()
		claim.Status.Phase = v1.SandboxFailed
		// Stamp FinishedAt so the GC TTL pass can reap this terminal claim;
		// without it ttlFinished skips the claim forever (etcd leak).
		claim.Status.FinishedAt = &now
		// Best-effort status write; the return below already requeues or surfaces the error.
		_ = r.Status().Update(ctx, claim)
		return ctrl.Result{}, err
	}

	// Mint the sandbox API bearer token before forking; forkd registers it
	// at fork time. The value reaches exactly two places: the ForkRequest
	// and the owned token Secret below. Never status, conditions, events,
	// or logs.
	apiToken, err := mintAPIToken()
	if err != nil {
		logger.Error(err, "token minting failed")
		now := metav1.Now()
		claim.Status.Phase = v1.SandboxFailed
		// Stamp FinishedAt so the GC TTL pass can reap this terminal claim;
		// without it ttlFinished skips the claim forever (etcd leak).
		claim.Status.FinishedAt = &now
		// Best-effort status write; the return below already requeues or surfaces the error.
		_ = r.Status().Update(ctx, claim)
		return ctrl.Result{}, err
	}

	// When the source template is encrypted, read its WRAPPED DEK from the
	// controller-owned Secret (idempotent read; created by the pool reconciler at
	// build time) and deliver it plus the KEK id so the node can unwrap and open
	// the encrypted container before restoring. The controller never holds the
	// plaintext DEK on this path; the wrapped DEK is never logged. snapshotID
	// equals the template id, so it names the key Secret.
	var wrappedDEK []byte
	var kekID string
	if template.Encrypted {
		wrappedDEK, kekID, err = EnsureEncKey(ctx, r.Client, r.KMS, claim.Namespace, snapshotID, &pool)
		if err != nil {
			logger.Error(err, "read encryption key for template", "template", snapshotID)
			now := metav1.Now()
			claim.Status.Phase = v1.SandboxFailed
			claim.Status.FinishedAt = &now
			_ = r.Status().Update(ctx, claim)
			return ctrl.Result{}, err
		}
	}

	// Call forkd on the selected node: this is the <2ms hot path. The vitals
	// labels (claim/pool/workspace/namespace, all object names, never secrets)
	// ride along so the node can label the sandbox's Layer 3 guest telemetry
	// (issue #164).
	result, err := r.forkOnNode(ctx, node, snapshotID, claim.Name, env, secretVals, template.Network, volumes, apiToken, wrappedDEK, kekID, claimVitalsLabels(claim))
	if err != nil {
		// A NotFound from forkd usually means the snapshot is not built on
		// that node yet; transient while the pool reconciler catches up.
		if isNotFound(err) {
			logger.Info("snapshot not yet on node, retrying", "node", node.Name, "error", err.Error())
			beforeStatus := claim.Status.DeepCopy()
			claim.Status.Phase = v1.SandboxPending
			// Best-effort status write, elided on a no-op re-pend; the return below
			// already requeues or surfaces the error.
			_ = r.writeClaimStatusIfChanged(ctx, claim, beforeStatus)
			return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
		}
		// The node rejected the fork on its sandbox-count cap (ResourceExhausted,
		// PR #110) or went away mid-fork (Unavailable). Both are transient: the
		// claim was placed onto a node that filled or died between SelectNode and
		// the Fork RPC (a schedule-time race the count-admission check shrinks but
		// cannot eliminate). Re-pend through the bounded NoCapacity machinery
		// instead of failing terminally, so the claim retries on a node with
		// headroom and only fails after the bounded wait.
		if isRetryableCapacity(err) {
			logger.Info("node rejected fork (capacity/unavailable), re-pending", "node", node.Name, "error", err.Error())
			return r.reconcileNoCapacity(ctx, claim, err)
		}
		logger.Error(err, "fork failed", "node", node.Name)
		recordClaimError(claim.Spec.Source.PoolRef.Name, "fork")
		now := metav1.Now()
		claim.Status.Phase = v1.SandboxFailed
		// Stamp FinishedAt so the GC TTL pass can reap this terminal claim;
		// without it ttlFinished skips the claim forever (etcd leak).
		claim.Status.FinishedAt = &now
		// Best-effort status write; the return below already requeues or surfaces the error.
		_ = r.Status().Update(ctx, claim)
		return ctrl.Result{}, err
	}

	// Hand the token to the claim's consumer via an owned Secret, BEFORE
	// the Ready status write: a Ready claim whose token Secret does not
	// exist would be unusable, and the Ready early-return above would never
	// retry the Secret. The token exists only in this Secret.
	if err := ensureSandboxTokenSecret(ctx, r.Client, claim, claim.Name+tokenSecretSuffix, apiToken, result.Endpoint); err != nil {
		logger.Error(err, "token secret write failed")
		recordClaimError(claim.Spec.Source.PoolRef.Name, "token")
		now := metav1.Now()
		claim.Status.Phase = v1.SandboxFailed
		// Stamp FinishedAt so the GC TTL pass can reap this terminal claim;
		// without it ttlFinished skips the claim forever (etcd leak).
		claim.Status.FinishedAt = &now
		// Best-effort status write; the return below already requeues or surfaces the error.
		_ = r.Status().Update(ctx, claim)
		return ctrl.Result{}, err
	}

	// Update status
	now := metav1.Now()
	claim.Status.Phase = v1.SandboxReady
	claim.Status.Endpoint = result.Endpoint
	claim.Status.Node = node.Name
	claim.Status.SandboxID = result.SandboxID
	claim.Status.StartupLatencyMs = int64(result.ForkTimeMs)
	claim.Status.StartedAt = &now
	setCondition(&claim.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
		Reason:             "Forked",
		Message:            fmt.Sprintf("forked in %.2fms on node %s", result.ForkTimeMs, node.Name),
	})

	if err := r.Status().Update(ctx, claim); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("sandbox claimed",
		"sandbox", claim.Name,
		"node", node.Name,
		"forkTime", fmt.Sprintf("%.2fms", result.ForkTimeMs),
	)

	return ctrl.Result{}, nil
}

// reconcileHuskClaim activates a dormant warm husk pod for the claim in place
// over the mTLS control channel. It selects a Running+Ready, unclaimed husk pod
// for the pool; if none is available it pends the claim (recordClaimPending) and
// requeues, mirroring the no-node placement-precondition path. Otherwise it
// resolves env+secrets (the same resolveSecrets as the forkd path), builds an
// ActivateRequest, dials the pod's control port, and on success sets the claim's
// Endpoint (podIP:sandboxPort) and Node (pod.Spec.NodeName), marks the pod
// claimed, mints + writes the per-sandbox API token Secret, and goes Ready.
//
// FAILS CLOSED: an activate transport error or a not-OK result NEVER goes Ready;
// it pends with backpressure and an actionable message so a transient husk
// (snapshot not yet materialized, stub still starting) can recover. Secret
// VALUES are never logged or put in status/conditions.
func (r *SandboxReconciler) reconcileHuskClaim(ctx context.Context, claim *v1.Sandbox, pool *v1.SandboxPool, template *v1.PoolTemplateSpec) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Every arrival at the husk-claim path is demand on the pool's warm buffer,
	// whether or not a warm pod is available right now. Recording it here keeps
	// the autoscaler scaling up under sustained load and resets the scale-down
	// cooldown so an idle-down does not race a live burst.
	r.recordHuskDemand(claim)

	// Select a fresh dormant pod to claim. A pod already claimed by any claim
	// (including this one on a prior pass) carries the claim label and is skipped
	// by selectDormantHuskPod, so an evicted claim whose old pod is gone or dying
	// always moves on to a new dormant pod rather than re-activating the dead one.
	// Leader election (one active reconciler) is what bounds concurrent claiming.
	pod, err := r.selectDormantHuskPod(ctx, pool)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pod == nil {
		// No warm husk slot: pend and retry. The pool reconciler is expected to
		// scale the warm pool up; this is a transient placement precondition. The
		// status write is IDEMPOTENT: a claim already Pending with the same
		// NoHuskPod condition is left untouched, so a re-reconcile of an unplaceable
		// claim does not bump the object and re-trigger its own watch (a hot loop
		// whose stale-read full-status writes would clobber an externally applied
		// status). recordClaimPending is the pending-gauge signal and runs each
		// pass regardless.
		logger.Info("no dormant husk pod available, pending", "pool", pool.Name)
		recordClaimPending()
		changed := claim.Status.Phase != v1.SandboxPending ||
			claim.Status.Endpoint != "" || claim.Status.Node != "" || claim.Status.SandboxID != ""
		claim.Status.Phase = v1.SandboxPending
		claim.Status.Endpoint = ""
		claim.Status.Node = ""
		claim.Status.SandboxID = ""
		if setCondition(&claim.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.NewTime(r.now()),
			Reason:             "NoHuskPod",
			Message:            "no warm husk pod is ready in the pool; the claim will retry once the pool scales a dormant slot up",
		}) {
			changed = true
		}
		if changed {
			_ = r.Status().Update(ctx, claim)
		}
		return ctrl.Result{RequeueAfter: capacityPendingRequeue}, nil
	}

	// A dormant pod is available: mark the claim Restoring before activating it.
	// This is stamped here (not before pod selection) so a claim that cannot place
	// stays Pending and settles, never cycling Pending -> Restoring -> Pending.
	if claim.Status.Phase != v1.SandboxRestoring {
		claim.Status.Phase = v1.SandboxRestoring
		if err := r.Status().Update(ctx, claim); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Resolve env + secrets (same path as the forkd fork). Secret VALUES live
	// only in memory here and ride the mTLS control channel; never logged.
	env, secretVals, err := r.resolveSecrets(ctx, claim.Namespace, claim.Labels[tenant.OrgLabelKey], claim.Spec.Env, claim.Spec.Secrets)
	if err != nil {
		logger.Error(err, "secret resolution failed")
		recordClaimError(claim.Spec.Source.PoolRef.Name, "secret")
		now := metav1.Now()
		claim.Status.Phase = v1.SandboxFailed
		claim.Status.FinishedAt = &now
		_ = r.Status().Update(ctx, claim)
		return ctrl.Result{}, err
	}

	// Mint the per-sandbox API bearer token before activating. It reaches exactly
	// the owned token Secret below; never status, conditions, events, or logs.
	apiToken, err := mintAPIToken()
	if err != nil {
		logger.Error(err, "token minting failed")
		now := metav1.Now()
		claim.Status.Phase = v1.SandboxFailed
		claim.Status.FinishedAt = &now
		_ = r.Status().Update(ctx, claim)
		return ctrl.Result{}, err
	}

	controlPort := r.HuskControlPort
	if controlPort == 0 {
		controlPort = HuskControlPort
	}
	sandboxPort := r.HuskSandboxPort
	if sandboxPort == 0 {
		sandboxPort = huskSandboxPort
	}

	activate := r.Activate
	if activate == nil {
		activate = ActivateHuskPod
	}

	// Claim the dormant pod BEFORE activating it: stamp the mitos.run/claim
	// label under an OPTIMISTIC LOCK. This is the mutual-exclusion commit. Two
	// concurrent claims may both select the same dormant pod, but the
	// resourceVersion-guarded patch lets exactly one win; the loser gets a 409
	// Conflict and must NOT activate this pod (a second tenant on the same VM).
	// Winning the label patch is the gate to Activate, so a pod is activated by
	// exactly one claim. On conflict we requeue so the next reconcile picks a
	// different dormant pod.
	if err := r.markHuskPodClaimed(ctx, pod, claim); err != nil {
		if apierrors.IsConflict(err) {
			logger.Info("husk pod claimed concurrently, requeueing to pick another", "pod", pod.Name)
			beforeStatus := claim.Status.DeepCopy()
			claim.Status.Phase = v1.SandboxPending
			setCondition(&claim.Status.Conditions, metav1.Condition{
				Type:               "Ready",
				Status:             metav1.ConditionFalse,
				LastTransitionTime: metav1.NewTime(r.now()),
				Reason:             "HuskPodRaced",
				Message:            "the selected dormant husk pod was claimed by another claim concurrently; the claim will retry and pick a different dormant pod",
			})
			_ = r.writeClaimStatusIfChanged(ctx, claim, beforeStatus)
			return ctrl.Result{RequeueAfter: capacityPendingRequeue}, nil
		}
		logger.Error(err, "mark husk pod claimed failed", "pod", pod.Name)
		return ctrl.Result{}, err
	}

	// The recorded snapshot manifest digest the husk stub re-verifies the on-disk
	// snapshot against before loading it (the husk mirror of forkd's verify-on-load
	// gate). forkd reported it via GetCapacity; the NodeRegistry holds it. It is a
	// content address, not a secret. An empty digest (no node has reported it yet)
	// makes the stub refuse to activate unless it runs with the development escape
	// hatch, which is exactly the fail-closed behavior we want in production.
	expectedDigest := ""
	if r.NodeRegistry != nil {
		// Verify against THIS pod's node's digest, not a cluster-wide pick: nodes
		// build their template snapshots independently, so the recorded digests
		// differ per node (#175). Using another node's digest fails the stub's
		// prepare-time verification, which is what blocked cross-node failover
		// (the claim could only ever re-activate on its origin node) (#177).
		if d, ok := r.NodeRegistry.TemplateDigestOnNode(pod.Spec.NodeName, poolTemplateID(pool)); ok {
			expectedDigest = d
		}
	}

	addr := net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(controlPort))
	netCfg := huskEgressConfig(template)
	req := husk.ActivateRequest{
		SnapshotDir:    HuskSnapshotDir,
		ExpectedDigest: expectedDigest,
		Env:            env,
		Secrets:        secretVals,
		Network:        huskNotifyNetwork(template),
		Egress:         netCfg.Egress,
		Allow:          netCfg.Allow,
		BlockNetwork:   netCfg.BlockNetwork,
		AllowCIDRs:     netCfg.AllowCIDRs,
		Inbound:        netCfg.Inbound,
		InboundCIDRs:   netCfg.InboundCIDRs,
		Token:          apiToken,
	}
	tlsConf, err := r.huskDialTLS(ctx, pod.Namespace)
	var res husk.ActivateResult
	if err == nil {
		res, err = activate(ctx, addr, tlsConf, req)
	}
	if err != nil || !res.OK {
		// FAIL CLOSED: do not go Ready. Pend so a transient husk can recover.
		msg := "husk activation did not complete"
		if err != nil {
			msg = fmt.Sprintf("husk activation transport error: %v", err)
		} else if res.Error != "" {
			msg = "husk activation failed: " + res.Error
		}
		logger.Info("husk activation failed, pending", "pod", pod.Name, "node", pod.Spec.NodeName, "detail", msg)
		recordClaimError(claim.Spec.Source.PoolRef.Name, "activate")
		// Release the claim label stamped before activation so this pod returns to
		// the dormant pool and can be retried (by this claim or another). Without
		// this a stamped-but-not-activated pod is orphaned forever, leaking warm
		// capacity and blocking failover (#177).
		if uerr := r.unmarkHuskPodClaimed(ctx, pod); uerr != nil {
			logger.Error(uerr, "release husk pod claim label after activation failure", "pod", pod.Name)
		}
		beforeStatus := claim.Status.DeepCopy()
		claim.Status.Phase = v1.SandboxPending
		setCondition(&claim.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.NewTime(r.now()),
			Reason:             "ActivateFailed",
			Message:            msg + "; the claim will retry",
		})
		_ = r.writeClaimStatusIfChanged(ctx, claim, beforeStatus)
		return ctrl.Result{RequeueAfter: capacityPendingRequeue}, nil
	}

	endpoint := net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(sandboxPort))

	// The pod was already claimed (optimistic-lock label patch) BEFORE activation,
	// so this VM belongs to exactly this claim. Hand the token to the claim's
	// consumer via an owned Secret BEFORE the Ready
	// write (same ordering as the forkd path).
	if err := ensureSandboxTokenSecret(ctx, r.Client, claim, claim.Name+tokenSecretSuffix, apiToken, endpoint); err != nil {
		logger.Error(err, "token secret write failed")
		recordClaimError(claim.Spec.Source.PoolRef.Name, "token")
		now := metav1.Now()
		claim.Status.Phase = v1.SandboxFailed
		claim.Status.FinishedAt = &now
		_ = r.Status().Update(ctx, claim)
		return ctrl.Result{}, err
	}

	// Record how long this claim waited for a warm pod: creation to activate.
	// A burst absorbed by warm capacity lands near the activate cost; a claim
	// that waited for a cold-started refill lands in the seconds.
	if !claim.CreationTimestamp.IsZero() {
		observeClaimWaitForWarm(r.now().Sub(claim.CreationTimestamp.Time).Seconds())
	}

	now := metav1.Now()
	claim.Status.Phase = v1.SandboxReady
	claim.Status.Endpoint = endpoint
	claim.Status.Node = pod.Spec.NodeName
	// Surface the shared-host mapping on the CRD (fork-primitive-multinode plan,
	// "the k8s interface and observability"): Pod is the husk HOST pod backing
	// this sandbox and VMID is the intra-pod VM identity. On today's single-VM
	// path VMID is the default primary identity, so the (Pod, VMID) pair is
	// populated and correct for the 1:1 case; multi-VM co-location (fork routing)
	// lands in a later increment. This is a purely additive status write.
	claim.Status.Pod = pod.Name
	claim.Status.VMID = v1.DefaultVMID
	claim.Status.SandboxID = pod.Name
	claim.Status.StartedAt = &now
	setCondition(&claim.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
		Reason:             "HuskActivated",
		Message:            fmt.Sprintf("activated husk pod %s on node %s in %.2fms", pod.Name, pod.Spec.NodeName, res.LatencyMs),
	})
	if err := r.Status().Update(ctx, claim); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("sandbox claimed via husk activation", "sandbox", claim.Name, "pod", pod.Name, "node", pod.Spec.NodeName)
	return ctrl.Result{}, nil
}

// huskInPodResolverIP is the fixed pod-local address the husk-stub binds the
// per-pod DNS proxy on and points the guest at via NotifyForkedNetwork. It is a
// link-local address inside the pod netns, the same default the raw-forkd path
// uses (cmd/forkd defaultDNSResolverIP).
const huskInPodResolverIP = "169.254.1.1"

// huskGuestIP and huskGatewayIP are the fixed point-to-point /30 the husk VM
// uses inside its OWN pod netns. Because each VM is alone in its pod netns there
// is no cross-VM collision, so a single fixed pair is correct (unlike raw-forkd,
// which carves a shared subnet). The stub derives the tap from huskGuestIP via
// netconf.DeriveTapName, assigns huskGatewayIP to that tap, and the guest keeps
// its baked huskGuestIP; that is the NIC-binding contract both sides agree on.
const (
	huskGuestIP   = "10.200.0.2"
	huskGatewayIP = "10.200.0.1"
)

// huskNotifyNetwork maps the template's network policy to the guest
// NotifyForkedNetwork delivered in the activate handshake. It always returns a
// config (never nil now): the guest is pinned to the fixed in-pod /30 and
// pointed at the in-pod DNS proxy resolver, so the in-pod egress filter and DNS
// proxy enforce the template allowlist. A template with no NetworkPolicy still
// gets the fail-closed default-deny config (the stub defaults Egress to deny).
func huskNotifyNetwork(_ *v1.PoolTemplateSpec) *vsock.NotifyForkedNetwork {
	return &vsock.NotifyForkedNetwork{
		GuestIP:    huskGuestIP,
		GatewayIP:  huskGatewayIP,
		PrefixLen:  30,
		ResolverIP: huskInPodResolverIP,
	}
}

// huskNetworkConfig is the resolved per-sandbox network posture the controller
// threads into the husk ActivateRequest: the egress default verdict, the raw
// allowlist, the block_network total-deny, the CIDR allowlists, and the inbound
// policy. All fields are config, not secrets, and are safe to log.
type huskNetworkConfig struct {
	Egress       string
	Allow        []string
	BlockNetwork bool
	AllowCIDRs   []string
	Inbound      string
	InboundCIDRs []string
}

// huskEgressConfig extracts the full network posture from the template,
// defaulting to the secure fail-closed posture when the template carries no
// NetworkPolicy: egress deny with no allows, and inbound deny-by-default. The
// inbound default is left empty (the stub and renderer treat empty as deny), so
// an untrusted sandbox with no policy gets deny-by-default in both directions.
func huskEgressConfig(template *v1.PoolTemplateSpec) huskNetworkConfig {
	if template == nil || template.Network == nil {
		return huskNetworkConfig{Egress: string(v1.EgressDeny)}
	}
	n := template.Network
	e := n.Egress
	if e == "" {
		e = v1.EgressDeny
	}
	return huskNetworkConfig{
		Egress:       string(e),
		Allow:        n.Allow,
		BlockNetwork: n.BlockNetwork,
		AllowCIDRs:   n.AllowCIDRs,
		Inbound:      string(n.Inbound),
		InboundCIDRs: n.InboundCIDRs,
	}
}

// writeClaimStatusIfChanged writes the claim status only when it differs from
// the before snapshot, eliding the redundant status write a steady-state pend
// requeue would otherwise issue every pass. The claim reconciler re-reconciles a
// stuck claim every 1-5s (no node, snapshot-not-yet, NoCapacity, husk-raced,
// activate-failed), and each of those paths re-asserts an identical Phase=Pending
// plus condition. setCondition carries an unchanged condition's
// LastTransitionTime forward, so on a true no-op the status deep-compares equal
// and the write (and the etcd churn and the object's own watch re-trigger) is
// skipped. This is the claim-side counterpart to writePoolStatusIfChanged
// (issue #163, status-update rate-limiting under churn).
//
// Unlike the pool status there is no per-reconcile heartbeat field to exclude:
// the only claim status timestamps (StartedAt, FinishedAt) are stamped once on a
// genuine transition, never per pass, so a plain deep-compare is correct.
// apiequality.Semantic compares metav1.Time by value (handling the
// monotonic-clock and location pitfalls of a raw DeepEqual). It is used only on
// the best-effort pend re-assert paths; a genuine transition (Ready, a terminal
// Failed/Terminated) flips Phase and so always compares unequal and always
// writes.
func (r *SandboxReconciler) writeClaimStatusIfChanged(ctx context.Context, claim *v1.Sandbox, before *v1.SandboxStatus) error {
	if apiequality.Semantic.DeepEqual(before, &claim.Status) {
		return nil
	}
	return r.Status().Update(ctx, claim)
}

// reconcileNoCapacity handles a placement that no node admits under the
// overcommit policy. It pends the claim with an LLM-legible NoCapacity
// condition and backs off, stamping the first-pending instant. Once the claim
// has waited longer than the bounded max-pending duration without ever placing,
// it gives up and fails the claim with an actionable capacity-exhaustion
// message (and a claim_errors metric, reason "capacity"). A claim that becomes
// admittable before the deadline proceeds to Restoring on a later reconcile.
func (r *SandboxReconciler) reconcileNoCapacity(ctx context.Context, claim *v1.Sandbox, cause error) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	now := r.now()

	// Stamp the first-pending instant on the metadata annotation if absent.
	// This is the durable deadline anchor across reconciles.
	pendingSince := now
	if stamp := claim.Annotations[pendingSinceAnnotation]; stamp != "" {
		if parsed, perr := time.Parse(time.RFC3339, stamp); perr == nil {
			pendingSince = parsed
		}
	} else {
		if claim.Annotations == nil {
			claim.Annotations = map[string]string{}
		}
		claim.Annotations[pendingSinceAnnotation] = now.Format(time.RFC3339)
		if err := r.Update(ctx, claim); err != nil {
			return ctrl.Result{}, err
		}
	}

	waited := now.Sub(pendingSince)
	maxWait := r.maxPendingDuration()

	// Derive an accurate, cause-specific condition and remediation (issue #28).
	// reconcileNoCapacity now fronts three distinct re-pend causes, and the
	// memory-overcommit wording is only correct for one of them.
	causeClause, remediation := noCapacityCauseText(cause)

	// Bounded fail: capacity never freed within the allowed wait. Surface an
	// actionable terminal error and stamp FinishedAt so the GC TTL pass reaps it.
	if waited >= maxWait {
		recordClaimError(claim.Spec.Source.PoolRef.Name, "capacity")
		finished := metav1.NewTime(now)
		claim.Status.Phase = v1.SandboxFailed
		claim.Status.FinishedAt = &finished
		setCondition(&claim.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			LastTransitionTime: finished,
			Reason:             "CapacityExhausted",
			Message: fmt.Sprintf(
				"%s for %s (waited past the %s bound); %s, then recreate the claim",
				causeClause, waited.Round(time.Second), maxWait, remediation,
			),
		})
		_ = r.Status().Update(ctx, claim)
		logger.Info("claim failed: capacity exhausted past bounded wait", "claim", claim.Name, "waited", waited.Round(time.Second), "maxWait", maxWait)
		return ctrl.Result{}, nil
	}

	// Within the bounded wait: pend with backpressure and retry.
	beforeStatus := claim.Status.DeepCopy()
	claim.Status.Phase = v1.SandboxPending
	recordClaimPending()
	setCondition(&claim.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		LastTransitionTime: metav1.NewTime(now),
		Reason:             "NoCapacity",
		Message: fmt.Sprintf(
			"%s; the claim will retry (waited %s of %s), %s",
			causeClause, waited.Round(time.Second), maxWait, remediation,
		),
	})
	// Best-effort status write, elided on a no-op re-pend; the return below
	// requeues regardless. The waited-duration text in the message is rounded to
	// the second, so a re-pend within the same second compares equal and is
	// skipped, while a real progression of the wait writes.
	_ = r.writeClaimStatusIfChanged(ctx, claim, beforeStatus)
	return ctrl.Result{RequeueAfter: capacityPendingRequeue}, nil
}

// reconcilePoolMissing handles a claim whose referenced SandboxPool does not
// exist (issue #630). The pool may simply not have been applied yet (a
// manifest ordering race), so the claim pends with an actionable PoolNotFound
// condition and retries for a bounded grace period, reusing the same bound as
// the capacity wait (--max-pending-duration, default 5m). A pool that never
// appears within the bound fails the claim TERMINALLY: phase Failed, no
// further steady-state requeues (the terminal early-return in reconcilePoolRef
// short-circuits later reconciles), and FinishedAt stamped so the GC TTL pass
// reaps it like every other Failed sandbox (#163). The first-missing instant
// is anchored on the poolMissingSinceAnnotation, mirroring the
// capacity-pending machinery: durable across reconciles and controller
// restarts, and cleared by reconcilePoolRef once the pool exists.
func (r *SandboxReconciler) reconcilePoolMissing(ctx context.Context, claim *v1.Sandbox) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	now := r.now()
	poolName := claim.Spec.Source.PoolRef.Name

	// Stamp the first-missing instant on the metadata annotation if absent.
	missingSince := now
	if stamp := claim.Annotations[poolMissingSinceAnnotation]; stamp != "" {
		if parsed, perr := time.Parse(time.RFC3339, stamp); perr == nil {
			missingSince = parsed
		}
	} else {
		if claim.Annotations == nil {
			claim.Annotations = map[string]string{}
		}
		claim.Annotations[poolMissingSinceAnnotation] = now.Format(time.RFC3339)
		if err := r.Update(ctx, claim); err != nil {
			return ctrl.Result{}, err
		}
	}

	waited := now.Sub(missingSince)
	maxWait := r.maxPendingDuration()

	// Bounded fail: the pool never appeared within the grace period. Surface an
	// actionable terminal error (issue #28: LLM-legible) and stamp FinishedAt so
	// the GC TTL pass reaps the claim.
	if waited >= maxWait {
		recordClaimError(poolName, "pool")
		finished := metav1.NewTime(now)
		claim.Status.Phase = v1.SandboxFailed
		claim.Status.FinishedAt = &finished
		setCondition(&claim.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			LastTransitionTime: finished,
			Reason:             "PoolNotFound",
			Message: fmt.Sprintf(
				"the referenced SandboxPool %q does not exist in namespace %q (missing for %s, past the %s grace period); create the pool or delete this sandbox, then recreate the sandbox once the pool exists",
				poolName, claim.Namespace, waited.Round(time.Second), maxWait,
			),
		})
		_ = r.Status().Update(ctx, claim)
		logger.Info("claim failed: referenced pool not found past bounded wait", "claim", claim.Name, "pool", poolName, "waited", waited.Round(time.Second), "maxWait", maxWait)
		return ctrl.Result{}, nil
	}

	// Within the grace period: pend with backpressure and retry. The status
	// write is elided on a no-op re-pend (writeClaimStatusIfChanged).
	beforeStatus := claim.Status.DeepCopy()
	claim.Status.Phase = v1.SandboxPending
	recordClaimPending()
	setCondition(&claim.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		LastTransitionTime: metav1.NewTime(now),
		Reason:             "PoolNotFound",
		Message: fmt.Sprintf(
			"the referenced SandboxPool %q does not exist in namespace %q; the sandbox will retry (waited %s of the %s grace period); create the pool or delete this sandbox",
			poolName, claim.Namespace, waited.Round(time.Second), maxWait,
		),
	})
	_ = r.writeClaimStatusIfChanged(ctx, claim, beforeStatus)
	return ctrl.Result{RequeueAfter: capacityPendingRequeue}, nil
}

// noCapacityCauseText maps a re-pend cause to an accurate condition clause and
// an actionable remediation (issue #28: LLM-legible errors). reconcileNoCapacity
// fronts three distinct causes, only one of which is the memory-overcommit
// shortage; surfacing the memory wording for the count-ceiling or
// node-unreachable causes would be misleading. The returned clause carries no
// secret values.
func noCapacityCauseText(cause error) (clause, remediation string) {
	switch {
	case hasGRPCCode(cause, codes.ResourceExhausted):
		// The selected node hit its per-node MaxSandboxes count ceiling between
		// placement and the Fork RPC (the --max-sandbox cap, a schedule-time race).
		return "the selected node reached its per-node sandbox-count limit (--max-sandbox)",
			"add forkd nodes or raise the per-node --max-sandbox limit"
	case hasGRPCCode(cause, codes.Unavailable):
		// The selected node went unreachable between placement and the Fork RPC.
		return "the selected node is unreachable (it left or died mid-fork)",
			"the claim retries on a healthy node; add forkd nodes if the cluster is down"
	default:
		// scheduler ErrNoCapacity: no node admits the fork under the memory
		// overcommit policy. This is the original wording.
		return "no node has memory capacity under the overcommit policy",
			"scale out forkd nodes or raise the overcommit factor"
	}
}

// clearPendingSince removes the capacity-pending stamp from the claim's
// annotations (in memory; the caller persists if needed). Idempotent.
func (r *SandboxReconciler) clearPendingSince(claim *v1.Sandbox) {
	delete(claim.Annotations, pendingSinceAnnotation)
}

// reconcileDelete reaps the claim's backing VM via forkd Terminate, then
// removes the finalizer so the API object can be garbage collected. A claim
// that never acquired a sandbox (no Node or SandboxID) skips straight to
// finalizer removal. terminateOnNode treats a NotFound sandbox and a
// vanished node as already-terminated, so a node that left the registry never
// hangs deletion.
func (r *SandboxReconciler) reconcileDelete(ctx context.Context, claim *v1.Sandbox) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(claim, FinalizerTerminate) {
		return ctrl.Result{}, nil
	}

	// Dehydrate the sandbox /workspace into a new committed revision BEFORE
	// reaping the VM on delete (the guest must still be alive to tar its
	// workspace). A claim already dehydrated by a lifetime-expiry terminate is a
	// no-op (the dehydrated annotation). A dehydrate error requeues the delete so
	// the finalizer is not removed until the work is captured.
	if err := r.dehydrateOnTerminate(ctx, claim); err != nil {
		logger.Error(err, "dehydrate workspace on delete; will retry", "claim", claim.Name)
		return ctrl.Result{RequeueAfter: capacityPendingRequeue}, nil
	}

	// Husk path: the claim's backing VM lives in the husk pod this claim
	// activated; reap it (record the usage tail, then delete the pod). No-op in
	// raw-forkd mode (no pod carries the label); terminateOnNode below covers
	// that path.
	if err := r.reapClaimHuskPods(ctx, claim); err != nil {
		logger.Error(err, "reap claimed husk pods on delete; will retry", "claim", claim.Name)
		return ctrl.Result{}, err
	}

	if claim.Status.Node != "" && claim.Status.SandboxID != "" {
		if err := terminateOnNode(ctx, r.NodeRegistry, claim.Status.Node, claim.Status.SandboxID); err != nil {
			logger.Error(err, "terminate backing sandbox on delete", "node", claim.Status.Node, "sandbox", claim.Status.SandboxID)
			return ctrl.Result{}, err
		}
	}

	controllerutil.RemoveFinalizer(claim, FinalizerTerminate)
	if err := r.Update(ctx, claim); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// reapClaimHuskPods reaps the husk pods this claim activated (labeled
// mitos.run/claim=<name>): it records the usage termination tail for each
// org-labeled pod FIRST, then deletes the pods. The order matters: the
// collector needs the event to bill the half-open [last scrape, terminate]
// window (issue #682), and the event fields come from the pod labels the
// delete removes. Deleting the pod is what actually STOPS the in-pod VM
// (forkd never tracks husk pods, so terminateOnNode is a no-op for them,
// issue #688), drops the pod from the usage scrape lister's billable set, and
// FREES THE WARM-POOL SLOT: a husk pod is single-use (once a tenant ran in
// it, it cannot be re-dormanted safely), so the pool reconcile refills a
// fresh dormant pod.
//
// One claim, one event: a claim already Terminated recorded its event at the
// TRUE terminate instant and recordHuskTerminations skips it here, so the
// object-delete reap and any retried reap delete pods without recording
// again. A Failed claim never went through terminateLifetime, so the phase
// guard does NOT skip it: for a post-activation failure that left a claimed
// pod running, the record here is what closes the billing window. Idempotent:
// pods already gone are NotFound no-ops, and in raw-forkd mode no pod carries
// the label. The instant comes from the reconciler clock (r.now()) so tests
// can freeze it.
func (r *SandboxReconciler) reapClaimHuskPods(ctx context.Context, claim *v1.Sandbox) error {
	var claimedHusk corev1.PodList
	if err := r.List(ctx, &claimedHusk, client.InNamespace(claim.Namespace), client.MatchingLabels{huskClaimLabel: claim.Name}); err != nil {
		return fmt.Errorf("list claimed husk pods: %w", err)
	}
	r.recordHuskTerminations(claim, claimedHusk.Items, r.now())
	for i := range claimedHusk.Items {
		if err := r.Delete(ctx, &claimedHusk.Items[i], client.GracePeriodSeconds(0)); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete claimed husk pod %s: %w", claimedHusk.Items[i].Name, err)
		}
	}
	return nil
}

// reconcileLifetime drives a Ready claim to the terminal Terminated phase when
// it exceeds maxLifetime (Spec.Timeout from StartedAt) or goes idle
// (Spec.IdleTimeout from the later of last-activity and StartedAt). Expiry
// terminates the backing VM directly via terminateOnNode and leaves the
// finalizer in place; the bounded, tolerant terminateOnNode keeps eventual
// delete safe. A claim already Terminated returns immediately (idempotent).
func (r *SandboxReconciler) reconcileLifetime(ctx context.Context, claim *v1.Sandbox) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if claim.Status.Phase == v1.SandboxTerminated {
		return ctrl.Result{}, nil
	}
	if claim.Status.StartedAt == nil {
		return ctrl.Result{}, nil
	}

	hasMaxLifetime := sandboxTTL(claim) != nil
	hasIdle := sandboxIdleTimeout(claim) != nil
	if !hasMaxLifetime && !hasIdle {
		return ctrl.Result{}, nil
	}

	now := time.Now()
	startedAt := claim.Status.StartedAt.Time

	// maxLifetime takes precedence: it does not depend on a reachable forkd.
	if hasMaxLifetime {
		deadline := startedAt.Add(sandboxTTL(claim).Duration)
		if !now.Before(deadline) {
			return r.terminateLifetime(ctx, claim, "MaxLifetimeExceeded",
				fmt.Sprintf("max lifetime %s exceeded", sandboxTTL(claim).Duration))
		}
	}

	// Idle and live-deadline checks need the work-aware activity signal from
	// forkd. An unreachable node means we cannot evaluate them this pass; requeue
	// and try again. We fetch the signal once when either is in play: a live
	// set_timeout deadline (issue #218) is honored even without Spec.IdleTimeout.
	requeue := time.Duration(0)
	if hasIdle {
		sig, ok := fetchActivitySignal(ctx, r.NodeRegistry, claim.Status.Node, claim.Status.SandboxID)
		if !ok {
			logger.Info("cannot evaluate idle, node unreachable; requeueing", "node", claim.Status.Node, "sandbox", claim.Status.SandboxID)
			return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
		}
		// A live set_timeout deadline takes authority over the idle clock
		// (issue #218): the caller explicitly bounded this running sandbox's TTL.
		// A deadline in the past reaps; a deadline in the future EXTENDS the TTL,
		// so the idle window is not consulted at all (this is how set_timeout
		// keeps an otherwise-idle sandbox alive, the #206 shim seam).
		if !sig.Deadline.IsZero() {
			if deadlineExpired(sig, now) {
				return r.terminateLifetime(ctx, claim, "TimeoutExpired",
					"live set_timeout deadline expired")
			}
			requeue = time.Until(sig.Deadline)
		} else {
			// Work-aware idle (issue #218): a paused sandbox or one with a live
			// background job (an open stream) is NOT idle, so an unattended job is
			// never reaped mid-run.
			if idleExpired(sig, startedAt, sandboxIdleTimeout(claim).Duration, now) {
				return r.terminateLifetime(ctx, claim, "IdleTimeout",
					fmt.Sprintf("idle for more than %s", sandboxIdleTimeout(claim).Duration))
			}
			last := startedAt
			if sig.LastActivity.After(last) {
				last = sig.LastActivity
			}
			requeue = time.Until(last.Add(sandboxIdleTimeout(claim).Duration))
		}
	}

	// Requeue at the nearest deadline.
	if hasMaxLifetime {
		untilMax := time.Until(startedAt.Add(sandboxTTL(claim).Duration))
		if requeue == 0 || untilMax < requeue {
			requeue = untilMax
		}
	}
	if requeue <= 0 {
		requeue = 1 * time.Second
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// terminateLifetime reaps the claim's backing VM and stamps the terminal
// Terminated phase with a FinishedAt time and a Terminated condition. The
// finalizer stays in place; the bounded terminateOnNode keeps later delete
// safe.
func (r *SandboxReconciler) terminateLifetime(ctx context.Context, claim *v1.Sandbox, reason, message string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Dehydrate the sandbox /workspace into a new committed revision BEFORE
	// reaping the VM (the guest must still be alive to tar its workspace). A
	// dehydrate error requeues without terminating, so the work is not lost; the
	// operation is idempotent via the dehydrated annotation.
	if err := r.dehydrateOnTerminate(ctx, claim); err != nil {
		logger.Error(err, "dehydrate workspace on lifetime expiry; will retry", "claim", claim.Name)
		return ctrl.Result{RequeueAfter: capacityPendingRequeue}, nil
	}

	if claim.Status.Node != "" && claim.Status.SandboxID != "" {
		if err := terminateOnNode(ctx, r.NodeRegistry, claim.Status.Node, claim.Status.SandboxID); err != nil {
			logger.Error(err, "terminate backing sandbox on lifetime expiry", "node", claim.Status.Node, "sandbox", claim.Status.SandboxID)
			return ctrl.Result{}, err
		}
	}

	// Husk path: the claim's backing VM lives in the claimed husk pod, which
	// terminateOnNode above never touches (forkd does not track husk pods), so
	// a lifetime terminate must delete the pod itself or the VM keeps running
	// and keeps being scraped and billed until object deletion (issue #688).
	// List the claimed husk pods and close the usage tail window on them FIRST,
	// while claim.Status.Phase is still pre-Terminated so the one-event guard
	// in recordHuskTerminations passes: this is the claim's ONE event, at the
	// TRUE terminate instant (issue #682). A duplicate record from a requeued
	// terminate (the stamp below failing) is deduplicated by the collector's
	// finalized guard. A list failure requeues rather than under-billing the
	// tail. No-op in raw-forkd mode: no pod carries the label.
	var claimedHusk corev1.PodList
	if err := r.List(ctx, &claimedHusk, client.InNamespace(claim.Namespace), client.MatchingLabels{huskClaimLabel: claim.Name}); err != nil {
		logger.Error(err, "list claimed husk pods on lifetime expiry; will retry", "claim", claim.Name)
		return ctrl.Result{RequeueAfter: capacityPendingRequeue}, nil
	}
	r.recordHuskTerminations(claim, claimedHusk.Items, r.now())

	now := metav1.Now()
	claim.Status.Phase = v1.SandboxTerminated
	claim.Status.FinishedAt = &now
	setCondition(&claim.Status.Conditions, metav1.Condition{
		Type:               "Terminated",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
	})
	if err := r.Status().Update(ctx, claim); err != nil {
		return ctrl.Result{}, err
	}

	// Reap the claimed husk pod AFTER the Terminated phase is durable, not
	// before: deleting it first fires the huskPodToClaim pod-delete watch while
	// the claim still reads Ready on the apiserver, and a concurrent reconcile's
	// checkHuskPodLost/rependOnHuskPodLost then mistakes this deliberate release
	// for unexpected pod loss and re-pends the claim (Phase Pending, endpoint
	// cleared) into a fresh VM, racing our own Status().Update; on a pool with
	// no spare warm slot that re-pend never resolves. rependOnHuskPodLost only
	// fires for a Ready claim, so persisting Terminated first closes the race.
	// The tail usage record above is what actually closes BILLING (it is what
	// stops the half-open scrape window from growing); this delete is what
	// stops the VM, releases the memory and the warm slot, and drops the pod
	// from live scrape billing (HuskPodScrapeLister filters only on pod state,
	// never claim phase, so it keeps counting the pod as running until it is
	// actually deleted). The reap's own record step is skipped here and on any
	// later reap: the claim is Terminated. A delete failure returns the error,
	// which requeues the reconcile; the requeued pass reads the now-durable
	// Terminated phase and takes reconcilePoolRef's terminal-phase branch,
	// whose reapClaimHuskPods call is the retry that guarantees this delete
	// eventually happens (issue #688) rather than relying solely on
	// reconcileDelete's identical cleanup at the claim's later GC-driven
	// object deletion.
	if err := r.reapClaimHuskPods(ctx, claim); err != nil {
		logger.Error(err, "reap claimed husk pods on lifetime expiry; will retry", "claim", claim.Name)
		return ctrl.Result{}, err
	}
	logger.Info("claim terminated by lifetime policy", "claim", claim.Name, "reason", reason)
	return ctrl.Result{}, nil
}

type forkResult struct {
	SandboxID  string
	Endpoint   string
	ForkTimeMs float64
}

func (r *SandboxReconciler) selectNode(ctx context.Context, pool *v1.SandboxPool, template *v1.PoolTemplateSpec, preferredNode string) (*NodeInfo, string, error) {
	templateName := poolTemplateID(pool)
	req := ForkRequest{TemplateID: templateName, PreferredNode: preferredNode}
	// Thread the template's explicit size and GPU demand into placement (issue
	// #221): a large memory size is admitted only on a node with the RAM, and a
	// GPU pool is pinned to GPU-capable nodes. A zero memory size falls back to
	// the per-template CoW estimate (the legacy behavior).
	if template != nil {
		if mem := template.Resources.Memory; !mem.IsZero() {
			if v, ok := mem.AsInt64(); ok {
				req.MemoryBytes = v
			}
		}
		if gpu := template.Resources.GPU; gpu != nil {
			req.GPUCount = gpu.Count
			req.GPUType = gpu.Type
		}
		// Thread the required isolation floor into placement (issue #40): a
		// security-sensitive template never lands on a lower-assurance node (for
		// example a hardware-kvm floor keeps the sandbox off a PVM node). The
		// requireHardwareKvm flag is folded in as the strongest possible floor.
		req.MinIsolationTier = MinIsolationTierFromSpec(template.MinIsolationTier, template.RequireHardwareKvm)
	}
	node, err := r.NodeRegistry.SelectNodeForFork(req)
	if err != nil {
		return nil, "", err
	}
	return node, templateName, nil
}

// resolveSecrets materializes a claim's env and secretRef mounts. wantOrg is the
// claim's org id (empty for non-SaaS/self-hosted claims that carry no org
// label). When wantOrg is set, a referenced Secret that carries a DIFFERENT
// org label is refused: this is the controller-side defense in depth for
// GHSA-pgv2-9w24-j7wh, so that even a Sandbox created off the gateway path
// cannot mount another org's Secret in a shared namespace. A Secret with no org
// label is allowed (per-org namespaces keep the boundary; the shared-namespace
// platform-secret path is blocked at the gateway).
func (r *SandboxReconciler) resolveSecrets(ctx context.Context, namespace, wantOrg string, env []corev1.EnvVar, secrets []v1.SecretMount) (envOut, secretsOut map[string]string, err error) {
	envOut = make(map[string]string)
	secretsOut = make(map[string]string)

	for _, e := range env {
		envOut[e.Name] = e.Value
	}

	coreClient := r.Client
	for _, s := range secrets {
		var secret corev1.Secret
		if err := coreClient.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      s.SecretRef.Name,
		}, &secret); err != nil {
			return nil, nil, fmt.Errorf("secret %s: %w", s.SecretRef.Name, err)
		}
		if gotOrg := secret.Labels[tenant.OrgLabelKey]; wantOrg != "" && gotOrg != "" && gotOrg != wantOrg {
			// Cross-org reference: refuse without echoing the secret name owner or
			// any value (secret hygiene).
			return nil, nil, fmt.Errorf("secret %s: cross-org reference refused", s.SecretRef.Name)
		}
		value, ok := secret.Data[s.SecretRef.Key]
		if !ok {
			return nil, nil, fmt.Errorf("key %s not found in secret %s", s.SecretRef.Key, s.SecretRef.Name)
		}
		envVar := s.EnvVar
		if envVar == "" {
			envVar = s.Name
		}
		secretsOut[envVar] = string(value)
	}

	return envOut, secretsOut, nil
}

func (r *SandboxReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr)
	// The claim event filter is scoped to the PRIMARY claim source only (not the
	// pod Watches below), so the test harness can run a raw and a husk reconciler
	// on one manager: each filters the CLAIMS it owns, while the husk pod watch
	// stays unfiltered (a pod carries no claim label, so a global filter would
	// drop every mapped pod event and break the re-pend trigger).
	if r.eventFilter != nil {
		b = b.For(&v1.Sandbox{}, builder.WithPredicates(r.eventFilter))
	} else {
		b = b.For(&v1.Sandbox{})
	}
	// In husk mode, watch husk pods and map a pod event to the claim named in its
	// mitos.run/claim label. A husk pod delete (drain, eviction, kubectl
	// delete) then promptly reconciles the active claim, which re-pends per the
	// pool's DrainPolicy instead of waiting for the claim's own periodic requeue.
	// The mapped reconcile re-Gets the claim and routes through checkHuskPodLost,
	// so a claim the husk reconciler does not own (no husk-test label, in the
	// test harness) is simply a no-op reconcile for it. The raw reconciler does
	// not register this watch (no husk pods).
	if r.EnableHuskPods {
		b = b.Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(huskPodToClaim),
		)
	}
	// The fromSandbox fork engine owns child husk pods (created + GC'd with the
	// Sandbox), so a child pod event re-queues the owning fan-out Sandbox.
	b = b.Owns(&corev1.Pod{})
	// A name lets the test harness run a raw and a husk reconciler on one manager
	// (controller-runtime auto-names by kind and would collide). The production
	// default (controllerName empty) keeps the kind-derived name.
	if r.controllerName != "" {
		b = b.Named(r.controllerName)
	}
	return b.Complete(r)
}
