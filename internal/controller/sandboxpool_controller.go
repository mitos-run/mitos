package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/kms"
	forkdpb "mitos.run/mitos/proto/forkd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type SandboxPoolReconciler struct {
	client.Client
	NodeRegistry *NodeRegistry
	// PeerToken is the shared bearer credential forkd accepts on its token-gated
	// CAS surface. The controller passes it in every PullTemplate so a deficit
	// node can pull a template from a holder. It must match forkd's --peer-token.
	// Empty disables distribution by pull (every deficit node builds its own
	// snapshot, the prior behavior). A SECRET VALUE: it is never logged. A
	// per-pull minted token is a follow-up; the shared-token model matches the
	// forkd side (Task 1).
	PeerToken string

	// EnableHuskPods selects the husk pod warm-pool path (issue #18), the
	// pod-native default. When true the pool does BOTH: it builds the template
	// snapshot on the target nodes (createSnapshotsOnNodes) AND maintains a warm
	// pool of pre-scheduled husk pods, pinned to the snapshot-holding nodes, that
	// run the dormant-VMM stub. When false the raw-forkd fork-per-claim path runs:
	// the snapshot is built and each claim forks on a holder, no husk pods. In
	// cmd/controller this is true by default and turned off by --enable-raw-forkd.
	EnableHuskPods bool
	// HuskStubImage is the container image that runs cmd/husk-stub in a husk
	// pod. Only used when EnableHuskPods is true.
	HuskStubImage string
	// HuskDNSUpstream is the comma-separated resolver list (failover order) the
	// husk-stub's per-pod DNS proxy forwards allowlisted name queries to. Empty
	// leaves name-based egress off (IP-only allowlists still enforced).
	HuskDNSUpstream string
	// KVMResourceName is the extended resource a husk pod requests for KVM
	// access (the device plugin slot, not privileged: true). Empty defaults to
	// mitos.run/kvm. Only used when EnableHuskPods is true.
	KVMResourceName string
	// DataDir is the forkd data directory on the node; the husk pod's snapshot
	// hostPath is rooted here (<DataDir>/templates/<id>/snapshot). Empty defaults
	// to /var/lib/mitos. Only used when EnableHuskPods is true.
	DataDir string
	// HuskMemoryHeadroom is the FIXED-FLOOR memory headroom added on top of a
	// husk pod's memory request to size its memory LIMIT (production-blocker #2,
	// cap 1). The limit must include headroom because the cgroup that the limit
	// caps holds MORE than the guest's configured RAM: the Firecracker VMM
	// itself, the husk-stub process, and copy-on-write dirty-page slack as the
	// restored VM faults in and writes pages. A limit equal to the request would
	// OOM-kill a VM running normally at its configured RAM (destroying the
	// activate latency); the headroom is what makes the limit transparent to a
	// legitimate VM while still capping a runaway. The effective headroom is
	// max(this floor, HuskMemoryHeadroomPercent% of the request) so a large VM
	// gets proportional slack and a small VM gets at least the floor. Zero
	// selects the default floor (defaultHuskMemoryHeadroom, 256Mi). Tunable via
	// the controller --husk-memory-headroom flag.
	HuskMemoryHeadroom resource.Quantity
	// HuskMemoryHeadroomPercent is the PROPORTIONAL memory headroom (percent of
	// the memory request) considered alongside HuskMemoryHeadroom; the larger of
	// the two is used. Zero selects the default (defaultHuskMemoryHeadroomPercent,
	// 25). Tunable via the controller --husk-memory-headroom-percent flag.
	HuskMemoryHeadroomPercent int
	// HuskTLSSecretName is the Secret holding the husk stub's mTLS server leaf
	// (tls.crt, tls.key), mounted into each husk pod so the stub can serve the
	// mTLS network control. Only used when EnableHuskPods is true.
	HuskTLSSecretName string
	// HuskCASecretName is the Secret holding the control plane CA (ca.crt),
	// mounted into each husk pod so the stub verifies the controller client cert.
	// Only used when EnableHuskPods is true.
	HuskCASecretName string
	// ControllerNamespace is the namespace EnsurePKI materialized the control
	// plane PKI Secrets in (the controller's own namespace, default "mitos").
	// reconcileHuskPods replicates mitos-ca + mitos-forkd-tls FROM here INTO the
	// pool namespace so husk pods, which run in the pool namespace, can mount
	// them. Empty disables replication (the husk pods then require the secrets
	// to already exist in their namespace). Only used when EnableHuskPods.
	ControllerNamespace string

	// KMS is the envelope-encryption Wrapper that wraps a template's at-rest DEK
	// (the controller never persists the plaintext DEK). It is REQUIRED when any
	// reconciled template is Encrypted; EnsureEncKey fails closed if it is nil.
	// Built from the controller --kek-file (local AES-256-GCM KEK in dev/CI; a
	// cloud KMS provider is a documented follow-up).
	KMS kms.Wrapper

	// Now is the reconciler's clock, injectable for tests; nil uses time.Now. It
	// is read when deciding whether the warm-pool scale-down cooldown has elapsed.
	Now func() time.Time

	// Demand is the shared per-pool claim-arrival tracker the warm-pool
	// autoscaler reads to decide whether the scale-down cooldown has elapsed. The
	// SAME *PoolDemand instance must be shared with the SandboxClaimReconciler so
	// claim arrivals recorded there are visible here. Nil disables demand-aware
	// cooldown (the pool then treats every reconcile as "no recent claim", which
	// only relaxes scale-down; scale-up is unaffected). Only used when
	// EnableHuskPods and Autoscale are set.
	Demand *PoolDemand
}

// now returns the reconciler's clock, honoring the injectable Now seam.
func (r *SandboxPoolReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// desiredWarm computes the autoscaler's desired dormant count for the pool given
// the current dormant and in-use counts, reading the shared demand tracker for
// the last claim arrival so the scale-down cooldown is honored. It is the single
// place the formula and the cooldown clock meet.
func (r *SandboxPoolReconciler) desiredWarm(pool *v1.SandboxPool, dormant, inUse int32) int32 {
	var lastClaim *metav1.Time
	if r.Demand != nil {
		if t, ok := r.Demand.LastArrival(poolKey(pool)); ok {
			mt := metav1.NewTime(t)
			lastClaim = &mt
		}
	}
	desired, _ := computeDesiredWarm(pool, dormant, inUse, lastClaim, r.now())
	return desired
}

// poolKey is the namespace/name demand-tracker key for a pool.
func poolKey(pool *v1.SandboxPool) string {
	return pool.Namespace + "/" + pool.Name
}

// SandboxPool ownership: get/list/watch to reconcile, status to write warmed
// counts and conditions. SandboxTemplate is read-only (covered above). The
// husk pod warm-pool path (issue #18) creates and deletes Pods, so the
// reconciler needs create;delete on pods on top of the get;list;watch the forkd
// discovery already declares.
// The husk warm pool is bounded against voluntary disruption (node drain,
// eviction) by a PodDisruptionBudget the reconciler creates-or-updates per pool
// (issue #18, slice 4b), so it needs the policy/poddisruptionbudgets verbs.
// +kubebuilder:rbac:groups=mitos.run,resources=sandboxpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mitos.run,resources=sandboxpools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=create;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// The controller emits a best-effort husk NetworkPolicy (ensureHuskNetworkPolicy)
// via the CACHED client, which lazily starts a NetworkPolicy informer; without
// list/watch that informer never syncs and the cached Get BLOCKS the reconcile
// before it ever creates husk pods. get;list;watch are load-bearing for liveness,
// not just create. (Found by kind-e2e-husk: 0 husk pods created.)
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
func (r *SandboxPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var pool v1.SandboxPool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		if apierrors.IsNotFound(err) {
			// The pool is genuinely deleted (not a transient error): drop its
			// process-local demand-tracker entry and its per-pool metric label
			// series so neither accumulates one entry per distinct pool name over
			// the controller lifetime. req.NamespacedName is the same namespace/name
			// poolKey builds. Only ever runs on NotFound, never on a transient Get
			// error (which is returned for requeue below).
			key := req.Namespace + "/" + req.Name
			if r.Demand != nil {
				r.Demand.Forget(key)
			}
			forgetPoolMetrics(key)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Resolve the effective template: the inline spec.template is the common path
	// (ADR 0007, the Deployment-embeds-PodSpec pattern); when it is nil the pool
	// referenced a shared template-shaped SandboxPool by name via spec.templateRef,
	// and that pool's inline template is reused. A pool with neither is invalid
	// (validation forbids it); the accessor stays nil-safe.
	template := poolTemplate(&pool)
	if pool.Spec.Template == nil && pool.Spec.TemplateRef != nil {
		var ref v1.SandboxPool
		if err := r.Get(ctx, client.ObjectKey{
			Namespace: pool.Namespace,
			Name:      pool.Spec.TemplateRef.Name,
		}, &ref); err != nil {
			logger.Error(err, "templateRef not found", "templateRef", pool.Spec.TemplateRef.Name)
			return ctrl.Result{}, err
		}
		template = poolTemplate(&ref)
	}

	templateID := poolTemplateID(&pool)
	desired := poolReplicas(&pool)

	// dedicatedNodes (#172): resolve the pool's placement node set once so every
	// snapshot readiness count and the template build below are scoped to the
	// dedicated nodes. nil for an unplaced pool (no placement => any node).
	nodeFilter, err := r.placementFilter(ctx, &pool)
	if err != nil {
		logger.Error(err, "resolve placement nodes for pool")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Husk pod warm-pool path (issue #18, the pod-native default). When enabled,
	// the pool does TWO INDEPENDENT things: it maintains a warm pool of
	// pre-scheduled DORMANT husk pods, AND it builds the template snapshot on the
	// target nodes the pods activate against. These two are DECOUPLED: the warm
	// pool of husk pods is maintained to Replicas REGARDLESS of whether the
	// snapshot is built yet. The husk pods schedule dormant and cannot ACTIVATE
	// until the snapshot is present on their node, but the pool of pod objects
	// must exist and self-heal independent of the build (a deleted husk pod is
	// recreated even while the build is incomplete or failing).
	//
	// ORDERING: reconcileHuskPods runs FIRST and is NEVER gated by the build, so a
	// build that cannot complete (for example a node that cannot boot a VM to
	// snapshot) does not stall warm-pool maintenance or self-heal. The build then
	// runs best-effort: its result is reported in status and a failure only
	// requeues to keep trying; it never short-circuits before the warm pool is
	// maintained.
	//
	// PLACEMENT COUPLING: when a snapshot-holding node is known (NodesWithTemplate),
	// each husk pod is pinned to it via nodeAffinity; when no holder exists yet
	// (build incomplete), the husk pods fall back to the kvm nodeSelector and a
	// later reconcile tightens the affinity once the registry reports the holders.
	// The raw-forkd path below (flag off, behind --enable-raw-forkd) is the
	// fork-per-claim fallback and does NOT create husk pods.
	if r.EnableHuskPods {
		// Warm pool FIRST, unconditionally: maintain the husk pod count to the
		// autoscaler's desired count and self-heal a deleted slot. This is
		// decoupled from the snapshot build so a build that does not complete never
		// blocks the warm pool. A reconcileHuskPods error (an API failure
		// listing/creating pods) requeues; the build is then skipped this pass and
		// retried on the requeue.
		//
		// Autoscale the warm pool: first read the current dormant + in-use counts,
		// compute the desired dormant count from demand (in-use + spare, clamped to
		// [minWarm, maxWarm], scale-down gated by the cooldown), then drive the pool
		// toward it. When Autoscale is nil desired == Replicas (legacy behavior).
		dormantNow, inUseNow, err := r.countHuskPods(ctx, &pool)
		if err != nil {
			logger.Error(err, "failed to count husk pods")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		desiredWarm := r.desiredWarm(&pool, dormantNow, inUseNow)

		// Best-effort Kubernetes NetworkPolicy (default-deny egress) for this
		// pool's husk pods. CNI-dependent; the in-pod nft filter the husk-stub
		// programs is the guarantee. A failure is logged but does NOT block husk
		// pod creation, so a CNI without NetworkPolicy support never stalls the
		// warm pool.
		npAllow := huskEgressConfig(template).Allow
		if err := r.ensureHuskNetworkPolicy(ctx, &pool, npAllow); err != nil {
			logger.Error(err, "ensure husk network policy (best effort; in-pod filter is the guarantee)")
		}

		res, err := r.reconcileHuskPods(ctx, &pool, template, desiredWarm)
		if err != nil {
			logger.Error(err, "failed to reconcile husk pods")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		warm := res.dormant

		// Metrics + scale-event counters.
		setWarmPoolGauges(poolKey(&pool), res.dormant, res.inUse, desiredWarm)
		if res.scaledUp {
			recordWarmScaleUp(poolKey(&pool))
		}
		if res.scaledDn {
			recordWarmScaleDown(poolKey(&pool))
		}

		// Build the snapshot the husk pods activate against, BEST-EFFORT. A build
		// error is logged and reported in status, and we requeue to keep trying,
		// but it does NOT return before the warm pool was maintained above. On a
		// node that cannot snapshot (the documented nested-VMM boundary on kind)
		// the build never completes, yet the warm pool above is still maintained
		// and self-heals.
		buildErr := r.ensureTemplateBuilt(ctx, &pool, template, nodeFilter)
		if buildErr != nil {
			logger.Error(buildErr, "failed to build template snapshot for husk pool (warm pool still maintained)")
		}

		// Bound voluntary disruption of the warm pool: a PodDisruptionBudget so a
		// node drain evicts husk pods one at a time instead of collapsing the pool.
		// A PDB error is logged and we requeue; it does not block the warm-pool
		// status update (the pool is still functional, just unbounded on drain).
		if err := r.ensureHuskPDB(ctx, &pool); err != nil {
			logger.Error(err, "failed to ensure husk PDB")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		readySnapshots := r.readySnapshotCountOn(templateID, nodeFilter)
		setPoolReadySnapshots(pool.Name, readySnapshots)
		// Snapshot the stored status before mutating so a no-op reconcile can skip
		// the status write (issue #163). Captured AFTER the metric setter (which
		// does not touch status) and BEFORE any pool.Status mutation.
		before := pool.Status.DeepCopy()
		pool.Status.ReadySnapshots = readySnapshots
		pool.Status.TotalSnapshots = readySnapshots
		pool.Status.NodeDistribution = r.nodeDistribution(templateID)
		pool.Status.DesiredWarm = desiredWarm
		if r.NodeRegistry != nil {
			if digest, ok := r.NodeRegistry.TemplateDigest(templateID); ok {
				pool.Status.TemplateDigest = digest
			}
		}
		now := metav1.Now()
		if res.scaledDn {
			pool.Status.LastScaleDownTime = &now
		}
		setCondition(&pool.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             conditionStatus(warm >= desiredWarm && readySnapshots > 0),
			LastTransitionTime: now,
			Reason:             "HuskPodsReady",
			Message:            fmt.Sprintf("%d/%d warm husk pods (%d in use), %d snapshot node(s)", warm, desiredWarm, res.inUse, readySnapshots),
		})
		if err := r.writePoolStatusIfChanged(ctx, &pool, before, now); err != nil {
			return ctrl.Result{}, err
		}
		// Bounded periodic requeue so the warm pool re-converges to Replicas even
		// if a husk pod DELETE event is somehow missed (Owns(pods) normally
		// enqueues the pool on delete, but the requeue is the belt-and-suspenders
		// guarantee that self-heal is not event-dependent). When the snapshot is
		// not built yet (no holder, or the build errored) requeue sooner to keep
		// driving the build AND to tighten the husk pod nodeAffinity once a holder
		// appears; once everything is ready, fall back to the slower steady cadence.
		if buildErr != nil || readySnapshots < desired {
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Raw-forkd path (the --enable-raw-forkd fallback): build the snapshot on the
	// target nodes and let each claim fork on a holder. No husk pods.
	if rawReady := r.readySnapshotCountOn(templateID, nodeFilter); rawReady < desired {
		logger.Info("snapshot deficit", "ready", rawReady, "desired", desired)
		if err := r.ensureTemplateBuilt(ctx, &pool, template, nodeFilter); err != nil {
			logger.Error(err, "failed to create snapshots")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
	}
	readySnapshots := r.readySnapshotCountOn(templateID, nodeFilter)

	// Update status
	setPoolReadySnapshots(pool.Name, readySnapshots)
	// Snapshot before mutating so a no-op reconcile skips the status write (#163).
	before := pool.Status.DeepCopy()
	pool.Status.ReadySnapshots = readySnapshots
	pool.Status.TotalSnapshots = readySnapshots
	pool.Status.NodeDistribution = r.nodeDistribution(templateID)
	if digest, ok := r.NodeRegistry.TemplateDigest(templateID); ok {
		pool.Status.TemplateDigest = digest
	}

	now := metav1.Now()
	setCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             conditionStatus(readySnapshots >= desired),
		LastTransitionTime: now,
		Reason:             "SnapshotsReady",
		Message:            fmt.Sprintf("%d/%d snapshots ready", readySnapshots, desired),
	})

	if err := r.writePoolStatusIfChanged(ctx, &pool, before, now); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// readySnapshotCount counts healthy nodes that hold the pool's template
// snapshot. One snapshot per node per template, so replicas are capped by
// node count.
func (r *SandboxPoolReconciler) readySnapshotCount(templateID string) int32 {
	return r.readySnapshotCountOn(templateID, nil)
}

// readySnapshotCountOn counts healthy nodes holding templateID, restricted to
// nodeFilter when non-nil (dedicatedNodes, #172). A snapshot on a node outside a
// placed pool's placement set cannot back its placement-pinned husk pods, so it
// must not count toward readiness; otherwise the readySnapshots>=desired gate
// would report the deficit met while no dedicated node holds the snapshot. A nil
// filter counts every healthy holder (unplaced pool).
func (r *SandboxPoolReconciler) readySnapshotCountOn(templateID string, nodeFilter map[string]bool) int32 {
	nodes := r.NodeRegistry.NodesWithTemplate(templateID)
	if nodeFilter == nil {
		return int32(len(nodes))
	}
	var n int32
	for _, node := range nodes {
		if nodeFilter[node.Name] {
			n++
		}
	}
	return n
}

// placementFilter resolves a pool's dedicatedNodes placement (#172) to the set
// of node names its snapshots may live on. It returns nil when the pool declares
// no placement (every node is eligible), so callers treat nil as "unconstrained".
func (r *SandboxPoolReconciler) placementFilter(ctx context.Context, pool *v1.SandboxPool) (map[string]bool, error) {
	sel := huskPlacementNodeSelector(pool)
	if len(sel) == 0 {
		return nil, nil
	}
	matching, err := r.nodesMatchingSelector(ctx, sel)
	if err != nil {
		return nil, fmt.Errorf("list placement nodes for pool %s: %w", pool.Name, err)
	}
	return matching, nil
}

// poolStatusUnchanged reports whether two pool statuses are equal for the purpose
// of skipping a redundant status write. LastSnapshotTime is excluded: it is a
// reconcile heartbeat that would otherwise change every pass and defeat the
// comparison. Every field an operator or the autoscaler reads (counts, digest,
// node distribution, desiredWarm, conditions, scale-down time) IS compared, and
// setCondition carries an unchanged condition's LastTransitionTime forward, so a
// steady-state reconcile compares equal. apiequality.Semantic compares metav1.Time
// by value (handling the monotonic-clock and location pitfalls of DeepEqual).
func poolStatusUnchanged(a, b *v1.SandboxPoolStatus) bool {
	x, y := a.DeepCopy(), b.DeepCopy()
	x.LastSnapshotTime, y.LastSnapshotTime = nil, nil
	return apiequality.Semantic.DeepEqual(x, y)
}

// writePoolStatusIfChanged writes the pool status only when it differs from the
// before snapshot (ignoring the LastSnapshotTime heartbeat), stamping
// LastSnapshotTime to now on a real change. This elides the redundant status
// write the periodic 30s requeue would otherwise issue every pass even when
// nothing changed: the dominant source of pool status-write churn against etcd
// (issue #163, failure/GC status-update rate-limiting). On a no-op it leaves the
// stored object untouched (no write, no heartbeat bump), so the object's own
// watch is not re-triggered.
func (r *SandboxPoolReconciler) writePoolStatusIfChanged(ctx context.Context, pool *v1.SandboxPool, before *v1.SandboxPoolStatus, now metav1.Time) error {
	if poolStatusUnchanged(before, &pool.Status) {
		// Keep the in-memory heartbeat consistent with what is stored (we did not
		// write), then skip the API call.
		pool.Status.LastSnapshotTime = before.LastSnapshotTime
		return nil
	}
	pool.Status.LastSnapshotTime = &now
	return r.Status().Update(ctx, pool)
}

// snapshotNodeNames returns the hostnames of the healthy nodes that hold the
// template snapshot, the set a husk pod's nodeAffinity is pinned to. A nil
// registry (some unit tests) returns nil, which leaves the husk pod on the kvm
// nodeSelector alone.
func (r *SandboxPoolReconciler) snapshotNodeNames(templateID string) []string {
	if r.NodeRegistry == nil {
		return nil
	}
	holders := r.NodeRegistry.NodesWithTemplate(templateID)
	names := make([]string, 0, len(holders))
	for _, n := range holders {
		names = append(names, n.Name)
	}
	return names
}

// huskTemplateDigest returns the recorded CAS manifest digest for the template,
// as reported by any healthy node holding it (forkd's GetCapacity feeds the
// NodeRegistry). The husk pod mounts the matching manifest and the stub verifies
// the snapshot against it before loading. A nil registry or no reported digest
// returns "", which makes the husk pod fall back to the stub's development
// escape hatch (the warm pool still activates, the stub logs it loudly).
func (r *SandboxPoolReconciler) huskTemplateDigest(templateID string) string {
	if r.NodeRegistry == nil {
		return ""
	}
	if d, ok := r.NodeRegistry.TemplateDigest(templateID); ok {
		return d
	}
	return ""
}

// ensureTemplateBuilt drives the template snapshot toward pool.Spec.Replicas
// holder nodes using the same build/distribute path as the raw-forkd pool
// (createSnapshotsOnNodes). It is the FIRST half of a husk-mode reconcile: the
// husk pods that follow mount <dataDir>/templates/<id>/snapshot on a holder
// node, so the snapshot must exist there first. A no-op when the deficit is
// already met. The encrypted-template key handling matches the raw path: the
// per-template key Secret is owned here and delivered over mTLS, never logged.
func (r *SandboxPoolReconciler) ensureTemplateBuilt(ctx context.Context, pool *v1.SandboxPool, template *v1.PoolTemplateSpec, nodeFilter map[string]bool) error {
	templateID := poolTemplateID(pool)
	desired := poolReplicas(pool)
	// dedicatedNodes (#172): nodeFilter restricts the snapshot to a placed pool's
	// placement nodes for BOTH the deficit count and the build targets, so a
	// snapshot is never built off the dedicated set. nil => unconstrained.
	readySnapshots := r.readySnapshotCountOn(templateID, nodeFilter)
	if readySnapshots >= desired {
		return nil
	}
	deficit := desired - readySnapshots

	var wrappedDEK []byte
	var kekID string
	if template.Encrypted {
		var keyErr error
		// The pool owns the per-template key Secret now that the template inlines
		// into it (no standalone SandboxTemplate object to own it).
		wrappedDEK, kekID, keyErr = EnsureEncKey(ctx, r.Client, r.KMS, pool.Namespace, templateID, pool)
		if keyErr != nil {
			// The error names only the Secret and the non-secret KEK id, never key
			// bytes.
			return fmt.Errorf("ensure encryption key for template %s: %w", templateID, keyErr)
		}
	}
	// InitCommands flattens the declarative BuildSteps (issue #220) into the
	// in-VM init commands, falling back to the legacy Init list when no
	// BuildSteps are set, so a template authored either way builds identically.
	if _, err := r.createSnapshotsOnNodes(ctx, templateID, template.Image, v1.InitCommands(template.BuildSteps, template.Init), template.Volumes, wrappedDEK, kekID, deficit, nodeFilter, forkdWorkload(template.Workload), forkdResources(template.Resources)); err != nil {
		return fmt.Errorf("build template snapshot %s: %w", templateID, err)
	}
	return nil
}

// createSnapshotsOnNodes ensures the template is present on up to deficit
// additional healthy nodes and returns how many were added (built + pulled).
//
// Distribution policy (build once, distribute by pull):
//   - Encrypted template: every deficit node BUILDS its own snapshot
//     (CreateTemplate). The CAS chunks of a plaintext-on-the-wire pull would
//     defeat at-rest encryption, so encrypted templates are not distributed by
//     pull; this is the documented carve-out.
//   - Plaintext template: ensure the template is BUILT on at least one node
//     (CreateTemplate on the first eligible node when no node holds it yet),
//     then for the remaining deficit nodes that lack it, PULL the snapshot from
//     a holder's CAS surface instead of rebuilding it. A pull is O(network) and
//     reuses the one expensive build, so the fleet converges far faster than N
//     independent boots.
//
// The pull token is the shared peer credential the controller is configured
// with; it is delivered to the deficit node over its mTLS gRPC and is never
// logged.
//
// nodeFilter constrains which nodes may build or pull (dedicatedNodes, #172):
// when non-nil only nodes whose name is in the set are eligible, so a placed
// pool's snapshot lands ONLY on its dedicated nodes (a snapshot built elsewhere
// could never back a placement-pinned husk pod). A nil filter places on any
// healthy node, preserving the unplaced behavior. The pull SOURCE is not
// constrained: the digest is content-addressed, so pulling from any holder into
// a dedicated node is safe.
// forkdResources maps a pool template's resources to the forkd build VM sizing so
// the snapshot VM (and every fork) has the pool's cpu/memory, not the 512 MiB / 1
// vCPU default. A serving workload (issue #460) runs in the build VM, so it needs
// the pool's memory there. Nil when the pool sets no resources (keeps the default).
func forkdResources(r v1.SandboxResources) *forkdpb.ResourceSpec {
	out := &forkdpb.ResourceSpec{}
	if cpu := r.CPU.Value(); cpu > 0 {
		out.Vcpus = int32(cpu)
	}
	if mem := r.Memory.Value(); mem > 0 {
		out.MemoryMb = mem / (1024 * 1024)
	}
	if out.Vcpus == 0 && out.MemoryMb == 0 {
		return nil
	}
	return out
}

// forkdWorkload maps a pool template's serving workload (issue #460) to the
// forkd build request. Nil (or no command) means the template has no serving
// workload, so the node builds it exec-only as before. Env values are non-secret
// (secrets are injected per fork), so they are safe to bake into the snapshot.
func forkdWorkload(w *v1.WorkloadSpec) *forkdpb.WorkloadSpec {
	if w == nil || len(w.Command) == 0 {
		return nil
	}
	out := &forkdpb.WorkloadSpec{Command: w.Command}
	if len(w.Env) > 0 {
		out.Env = make(map[string]string, len(w.Env))
		for _, e := range w.Env {
			out.Env[e.Name] = e.Value
		}
	}
	if w.Ready != nil {
		out.Ready = &forkdpb.WorkloadHttpReady{
			Port:           uint32(w.Ready.Port),
			Path:           w.Ready.Path,
			Expect:         uint32(w.Ready.Expect),
			TimeoutSeconds: uint32(w.Ready.TimeoutSeconds),
		}
	}
	return out
}

func (r *SandboxPoolReconciler) createSnapshotsOnNodes(ctx context.Context, templateID, image string, initCommands []string, templateVolumes []v1.SandboxVolume, wrappedDEK []byte, kekID string, deficit int32, nodeFilter map[string]bool, workload *forkdpb.WorkloadSpec, resources *forkdpb.ResourceSpec) (int32, error) {
	var added int32
	var errs []error

	// Whether distribution by pull applies: plaintext template, a peer token is
	// configured, and a holder reporting a content-addressed digest exists. An
	// encrypted template always builds per node (the carve-out above).
	distribute := len(wrappedDEK) == 0 && r.PeerToken != ""

	for _, node := range r.NodeRegistry.ListNodes() {
		if added >= deficit {
			break
		}
		// dedicatedNodes (#172): skip a node outside the pool's placement set so
		// the snapshot is only built where the placement-pinned husk pods can run.
		if nodeFilter != nil && !nodeFilter[node.Name] {
			continue
		}
		if !node.isHealthy() || node.hasSnapshot(templateID) {
			continue
		}

		// Prefer a pull when distribution applies AND a holder exists. The build
		// on the first node (when no holder exists yet) falls through to
		// CreateTemplate below; once that build registers a digest, subsequent
		// deficit nodes in this same pass pull from it.
		if distribute {
			if holder, casURL, digest, ok := r.NodeRegistry.TemplateSource(templateID); ok && holder.Name != node.Name {
				pullStart := r.now()
				if err := r.pullTemplateOnNode(ctx, node, templateID, digest, casURL, r.PeerToken); err != nil {
					errs = append(errs, fmt.Errorf("node %s: %w", node.Name, err))
					continue
				}
				r.NodeRegistry.AddTemplateWithDigest(node.Name, templateID, digest)
				// Snapshot-distribution lag (#164): record the pull duration. This is
				// the multi-node distribution path (a peer token AND a holder), so the
				// metric series is populated only when real distribution happened.
				observeSnapshotDistributionLag(templateID, r.now().Sub(pullStart).Seconds())
				added++
				continue
			}
		}

		// Build path. Fail closed: an encrypted template's WRAPPED DEK travels in
		// CreateTemplate, so the node connection must be mTLS. Refuse to send the
		// wrapped DEK over an insecure channel (node.TLS nil and registry.TLS nil,
		// i.e. PKI bootstrap disabled); skip the node without setting it and record
		// the refusal. A plaintext template carries no DEK and is unaffected.
		if len(wrappedDEK) > 0 && !r.NodeRegistry.NodeMTLS(node.Name) {
			errs = append(errs, fmt.Errorf("node %s: refusing to deliver the wrapped DEK over an insecure gRPC channel: enable PKI bootstrap on the controller and mTLS on forkd, or disable template encryption", node.Name))
			continue
		}
		conn, err := r.NodeRegistry.GetConnection(node.Name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		// CreateTemplate on the real engine boots a VM and snapshots it:
		// O(minutes). This blocks the pool reconcile worker; bounded here so a
		// hung node cannot stall pool reconciliation forever. Moving builds to
		// a background queue is roadmap work (snapshot distribution).
		cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		// The template's declared volumes are baked into the snapshot as
		// placeholder drives. No fork-policy override applies at build time, so
		// volumeMounts is called with no overrides; each fork's ForkRequest must
		// match this set by name.
		resp, err := forkdpb.NewForkDaemonClient(conn).CreateTemplate(cctx, &forkdpb.CreateTemplateRequest{
			TemplateId:   templateID,
			Image:        image,
			InitCommands: initCommands,
			Workload:     workload,
			Resources:    resources,
			Volumes:      volumeMounts(templateVolumes, nil),
			// EncryptionKey carries the WRAPPED DEK for an Encrypted template,
			// delivered over mTLS; KekId names the KEK that wrapped it (non-secret)
			// so the node selects the matching KEK to unwrap. Both empty for a
			// plaintext template. The wrapped DEK is never logged.
			EncryptionKey: wrappedDEK,
			KekId:         kekID,
		})
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("node %s: %w", node.Name, err))
			continue
		}
		r.NodeRegistry.AddTemplateWithDigest(node.Name, templateID, resp.TemplateDigest)
		added++
	}
	if added == 0 && len(errs) > 0 {
		return 0, errors.Join(errs...)
	}
	return added, nil
}

func (r *SandboxPoolReconciler) nodeDistribution(templateID string) map[string]int32 {
	dist := make(map[string]int32)
	for _, n := range r.NodeRegistry.NodesWithTemplate(templateID) {
		// One snapshot per node in the current model; becomes a real count when
		// snapshot distribution lands (ROADMAP §3).
		dist[n.Name] = 1
	}
	return dist
}

func (r *SandboxPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Owns(pods): a husk pod is owner-referenced to its pool, so a husk pod
	// delete (a node drain, an eviction, an operator kubectl delete) enqueues the
	// owning pool. The deficit logic in reconcileHuskPods then recreates the
	// replacement, so the warm pool SELF-HEALS a lost dormant slot without
	// waiting for the periodic 30s requeue. Owns(pods) also covers the
	// owner-referenced husk PodDisruptionBudget for free via the same ownership
	// edge for pods; the PDB itself is reconciled on the pool's own events.
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.SandboxPool{}).
		Owns(&corev1.Pod{}).
		Complete(r)
}

// setCondition upserts a condition by Type, preserving LastTransitionTime when
// the condition's Status has not changed (the same semantics as apimachinery's
// meta.SetStatusCondition). This is load-bearing: a reconcile that re-asserts an
// unchanged condition (a Pending claim re-pended with the same reason every pass)
// must NOT stamp a fresh LastTransitionTime, because a fresh timestamp makes the
// status write a real change, which re-triggers the object's own watch, which
// re-reconciles, which writes again: a self-sustaining hot loop. Worse, that loop
// of full Status().Update writes from a stale read clobbers any externally
// applied status (for example the husk e2e's merge-patch Ready stamp), so the
// claim never settles. Preserving the timestamp on an unchanged Status makes the
// re-assert a no-op write, so the loop terminates and external stamps survive.
func setCondition(conditions *[]metav1.Condition, condition metav1.Condition) bool {
	for i, c := range *conditions {
		if c.Type == condition.Type {
			// Carry the existing transition time forward unless the Status flips,
			// so an unchanged condition does not churn the object.
			if c.Status == condition.Status && !c.LastTransitionTime.IsZero() {
				condition.LastTransitionTime = c.LastTransitionTime
			}
			// Report whether anything meaningful changed: an identical re-assert
			// (same Status, Reason, Message, and carried-forward transition time) is
			// a no-op, so the caller can skip a status write that would otherwise
			// re-trigger the object's own watch.
			unchanged := c.Status == condition.Status &&
				c.Reason == condition.Reason &&
				c.Message == condition.Message &&
				c.ObservedGeneration == condition.ObservedGeneration &&
				c.LastTransitionTime.Equal(&condition.LastTransitionTime)
			(*conditions)[i] = condition
			return !unchanged
		}
	}
	*conditions = append(*conditions, condition)
	return true
}

func conditionStatus(ok bool) metav1.ConditionStatus {
	if ok {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}
