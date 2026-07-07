package controller

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/husk"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// huskForkSnapshotter is the controller->husk fork-snapshot transport seam. Nil
// defaults to ForkSnapshotOnHusk; tests inject a fake.
type huskForkSnapshotter func(ctx context.Context, addr string, tlsConf *tls.Config, req husk.ForkSnapshotRequest) (husk.ForkSnapshotResult, error)

// huskForkSnapshotRemover is the controller->husk remove-fork-snapshot seam. Nil
// defaults to RemoveForkSnapshotOnHusk; tests inject a fake.
type huskForkSnapshotRemover func(ctx context.Context, addr string, tlsConf *tls.Config, req husk.RemoveForkSnapshotRequest) (husk.ForkSnapshotResult, error)

// huskVMSpawner is the controller->husk spawn-vm seam (the MultiVMFork routing).
// Nil defaults to SpawnVMOnHusk; tests inject a fake.
type huskVMSpawner func(ctx context.Context, addr string, tlsConf *tls.Config, req husk.SpawnVMRequest) (husk.SpawnVMResult, error)

// coLocatedForkVMBudget reports how many ADDITIONAL fork-child VMs the MultiVMFork
// routing may safely co-locate INSIDE the source pod alongside the source VM
// already resident there (L1.7b, guarantee A: a fork never overcommits a pod).
//
// The reservation is safe at the CoW WORST case, not the CoW-typical case. A
// co-located VM shares the parent memory image copy-on-write, so its TYPICAL
// resident cost is only its private-dirty set (a few MiB). But it MAY dirty up to
// its whole guest RAM, and memory is non-compressible: if the co-located guests
// together dirty more than what the pod reserved, a sibling OOM-kills a sibling
// (the user's own live work). So each VM (the source plus every co-located child)
// is accounted its FULL guest-memory footprint, not the tiny shared-page floor.
//
// Two source-pod shapes, one guarantee:
//
//   - A MULTI-VM husk pod is BUILT to host co-located fork VMs: it reserves node
//     memory up front for the source VM plus a bounded number of fork VMs
//     (buildHuskPod sizes Requests[memory] = (1 + reserved) * per-VM guest RAM) and
//     records the per-VM guest RAM in the huskForkVMGuestMemoryAnnotation. The
//     budget is floor(request / per-VM) - 1: the reserved request holds that many
//     VMs and one slot is the source VM. Reserving on the REQUEST is node-honest,
//     the scheduler placed the pod where every reserved VM's guest RAM fits, so a
//     co-located fork never overcommits the node. This is what makes co-location
//     actually engage for a realistically sized pool; without the reservation a
//     multi-VM pod was sized for ONE VM and this budget was always 0, so every fork
//     spilled to the new-pod path (where the production canary failed).
//
//   - A LEGACY / single-VM pod carries no reservation stamp, so its memory REQUEST
//     is one VM's guest RAM and its memory LIMIT (memory.max the kubelet enforces)
//     is the intra-pod ceiling. The budget is floor(limit / request) - 1: the pod
//     holds that many VMs before a sibling would OOM a sibling, one slot reserved
//     for the source VM. A pod sized for one VM (the honest single-VM default)
//     co-locates nothing, so a fork of it spills to a new pod. spawnInSourcePod is
//     false for a single-VM source anyway, so this branch only ever governs a pod
//     stamped multi-VM-capable by other means (a test fixture).
//
// A budget of 0 means every fork child spills to a new pod. When the source pod
// does not declare the memory accounting its shape needs (a malformed or
// resource-free pod), the budget is 0: with nothing provable the fork co-locates
// nothing, the conservative safe-at-worst-case default.
//
// This replaces the former hardcoded co-location count. It governs the per-pod
// dimension of guarantee A. The node budget is already honored by the spill path:
// a spilled child takes buildForkChildPod, which the node scheduler and the
// capacity-admission gate account honestly, so a fork that cannot fit the node
// pends there exactly as an independent claim does. The node-level spill-vs-pend
// refinement for the co-located dimension is deferred to a follow-up increment.
//
// SCOPE: this budget is the PER-POD ceiling computed from the source pod's static
// resources. It is the WHOLE-POD count, not a per-fork slice: the reconcile loop
// subtracts the co-located VMs OTHER SandboxForks targeting the SAME source pod
// have ALREADY placed there (coLocatedVMsInPodByOtherForks) before admitting this
// fork's own children, so the effective budget for a fork is this ceiling minus the
// live cross-fork occupancy. That closes the former per-fork gap where N concurrent
// forks of one source each independently admitted up to this ceiling and
// over-admitted VMs into the pod: the sum across all forks now stays within the
// ceiling and an over-budget child SPILLS to a new pod (buildForkChildPod) instead
// of risking an intra-pod OOM. The occupancy read is uncached (APIReader) and the
// controller reconciles Sandbox objects serially (default MaxConcurrentReconciles
// 1), so two same-source forks never evaluate occupancy against a stale view; the
// read is also conservative on error (treat the pod as full and spill), and rounds
// toward spilling so the guarantee is UNDER-admit, never over-admit. Even had the
// NODE stayed safe without this (the pod cgroup memory LIMIT is a hard ceiling the
// scheduler reserved, so an over-admission fails closed as an intra-pod OOM, not a
// node overcommit), spilling the over-budget child is the correct outcome and is
// now what happens.
func coLocatedForkVMBudget(pod *corev1.Pod) int {
	if pod == nil {
		return 0
	}
	var req, limit resource.Quantity
	found := false
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		if c.Name != huskContainerName {
			continue
		}
		req = c.Resources.Requests[corev1.ResourceMemory]
		limit = c.Resources.Limits[corev1.ResourceMemory]
		found = true
		break
	}
	if !found {
		return 0
	}
	// A MULTI-VM pod records ONE fork VM's guest RAM in an annotation and RESERVES
	// its memory request for the source VM plus a bounded number of co-located fork
	// VMs (buildHuskPod). The reserved request holds floor(request / per-VM) VMs on
	// the node, one slot reserved for the source VM already resident, so the pod
	// grants floor(request / per-VM) - 1 co-located fork slots. Reserving on the
	// REQUEST is node-honest: the scheduler placed the pod where all those VMs fit,
	// so a co-located fork never overcommits the node (guarantee A).
	if g, ok := pod.Annotations[huskForkVMGuestMemoryAnnotation]; ok {
		perVM, err := resource.ParseQuantity(g)
		if err == nil && !perVM.IsZero() && !req.IsZero() {
			coLocated := req.Value()/perVM.Value() - 1
			if coLocated < 0 {
				return 0
			}
			return int(coLocated)
		}
	}
	// Legacy / single-VM pod: no reservation stamp, so the memory REQUEST is one
	// VM's guest RAM and the pod's memory LIMIT (memory.max) is the intra-pod
	// ceiling. It holds floor(limit / request) VMs at the CoW worst case before a
	// sibling would OOM a sibling, one slot reserved for the source VM, so at most
	// floor(limit / request) - 1 children co-locate. A pod sized for one VM (the
	// honest single-VM default) co-locates nothing.
	if req.IsZero() || limit.IsZero() {
		return 0
	}
	coLocated := limit.Value()/req.Value() - 1
	if coLocated < 0 {
		return 0
	}
	return int(coLocated)
}

// huskMultiVMLabel marks a husk pod whose stub was started with --multi-vm, so the
// controller may drive a spawn-vm op against it (bring up an ADDITIONAL same-tenant
// VM in that pod). Only a pod carrying this label is a candidate for the MultiVMFork
// routing; every other source pod falls back to the new-pod path. The label is the
// controller-visible record that the pod is multi-VM capable, paired with the
// --multi-vm stub arg (a single-VM stub fails a spawn-vm op closed regardless).
const huskMultiVMLabel = "mitos.run/multi-vm"

// huskPodMultiVMCapable reports whether a husk pod's stub runs with --multi-vm, so
// a spawn-vm op against it can succeed. A nil pod or a pod without the label is not
// capable, so the MultiVMFork routing falls back to a new child pod.
func huskPodMultiVMCapable(pod *corev1.Pod) bool {
	return pod != nil && pod.Labels[huskMultiVMLabel] == "true"
}

// forkChildVMID derives the node-local vmID for a fork child co-located in the
// source pod from the child's stable slot name. The husk stub validates a spawned
// vmID against checkVMID (^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$) and derives this VM's
// per-VM socket, tap, and workdir from it, so the id must be a safe node-local
// identifier and unique WITHIN the source pod. The child slot name
// ("<fork>-fork-<i>") is already node-local-unique and uses only [a-z0-9-]; cap it
// with a short content-hash suffix so a long source sandbox name still yields a
// <=64-char id that never collides across forks sharing one source pod.
func forkChildVMID(childName string) string {
	const maxLen = 64
	if len(childName) <= maxLen {
		return childName
	}
	sum := sha256.Sum256([]byte(childName))
	suffix := hex.EncodeToString(sum[:])[:8]
	return childName[:maxLen-1-len(suffix)] + "-" + suffix
}

// multiVMForkEnabled reports whether the MultiVMFork routing is on, honoring the
// test gate override so a test can toggle it race-safely on the shared reconciler.
func (r *SandboxReconciler) multiVMForkEnabled() bool {
	if r.multiVMForkGate != nil {
		return r.multiVMForkGate()
	}
	return r.MultiVMFork
}

// huskForkFinalizer guards a husk fork so its node-local fork snapshot is
// removed from the source pod before the Sandbox object is deleted.
const huskForkFinalizer = "mitos.run/husk-fork-snapshot"

// logForkStage emits one hosted-fork stage boundary: it logs the controller-side
// RPC round-trip (network + husk) plus the husk-reported per-stage breakdown of
// the work inside that RPC, and observes every stage into the
// mitos_fork_stage_duration_seconds histogram. It is how a SINGLE hosted fork
// yields an attributable breakdown: at each control op (fork-snapshot, spawn-vm,
// activate) the round-trip is one series (stage) and the stub's sub-stages
// (fc_boot, vmstate_restore, guest_ready, ...) are their own series, so the gap
// between the round-trip and the sub-stage sum is the control-plane / network
// overhead. name is the fixed round-trip stage label; huskLatencyMs is the stub's
// own total for the op; huskStages is the stub's sub-stage split (nil for a stub
// that did not report it, e.g. an older peer or an AlreadyActive re-drive). Only
// fixed stage names and millisecond durations are logged or labeled: no secret,
// token, entropy, or id value is ever emitted. Timing/observability only.
func logForkStage(logger logr.Logger, id, name string, rpc time.Duration, huskLatencyMs float64, huskStages map[string]float64) {
	observeForkStage(name, rpc.Seconds())
	kv := []any{
		"fork", id,
		"stage", name,
		"rpcMs", float64(rpc.Microseconds()) / 1000.0,
		"huskLatencyMs", huskLatencyMs,
	}
	names := make([]string, 0, len(huskStages))
	for stage := range huskStages {
		names = append(names, stage)
	}
	sort.Strings(names)
	for _, stage := range names {
		observeForkStage(stage, huskStages[stage]/1000.0)
		kv = append(kv, stage+"Ms", huskStages[stage])
	}
	logger.Info("fork stage timing", kv...)
}

// reconcileFromSandbox owns the fork engine for a source.fromSandbox Sandbox: a
// live fork of the named source Sandbox into replicas indexed sibling children,
// reported in status.children with status.readyReplicas. It enforces the
// secret-inheritance policy (default reissue: each child gets a fresh per-fork
// bearer token and the source's in-memory secrets are NOT inherited;
// SecretInheritanceMode=inherit requires explicit opt-in and records the
// SecretInheritanceDenied/ExplicitOptIn conditions). The Sandbox IS the fan-out
// (no intermediate SandboxFork object). The shared dispatcher (Reconcile) has
// already fetched the Sandbox and admitted it against the source's fork budget.
func (r *SandboxReconciler) reconcileFromSandbox(ctx context.Context, fork *v1.Sandbox) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	replicas := effectiveReplicas(fork)

	// Husk fork deletion + finalizer handling MUST come before the terminal and
	// already-satisfied short-circuits below: a fork being deleted (or one that
	// has reached its replicas) still needs its node-local fork snapshot GC'd.
	if r.EnableHuskPods {
		if !fork.DeletionTimestamp.IsZero() {
			return r.finalizeHuskFork(ctx, fork)
		}
		if !controllerutil.ContainsFinalizer(fork, huskForkFinalizer) {
			controllerutil.AddFinalizer(fork, huskForkFinalizer)
			if err := r.Update(ctx, fork); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
	}

	// A rejected fork is terminal: never reconcile it again.
	if meta.IsStatusConditionTrue(fork.Status.Conditions, v1.ConditionRejected) {
		return ctrl.Result{}, nil
	}

	// A fork whose source terminated or vanished is terminal (issue #698):
	// never reconcile it again, so repeated reconciles are no-ops.
	if meta.IsStatusConditionTrue(fork.Status.Conditions, "SourceTerminated") {
		return ctrl.Result{}, nil
	}

	if fork.Status.ReadyReplicas >= replicas {
		return ctrl.Result{}, nil
	}

	// Find the source sandbox (another v1 Sandbox, by source.fromSandbox.Name).
	sourceKey := client.ObjectKey{
		Namespace: fork.Namespace,
		Name:      fork.Spec.Source.FromSandbox.Name,
	}
	var source v1.Sandbox
	err := r.Get(ctx, sourceKey, &source)
	if apierrors.IsNotFound(err) && r.APIReader != nil {
		// Authoritative re-check: the informer cache can lag a source created
		// moments before its fork, and a cache miss must not terminally fail a
		// fork whose source really exists.
		err = r.APIReader.Get(ctx, sourceKey, &source)
	}
	if apierrors.IsNotFound(err) {
		// The source object is gone (or never existed). Waiting can never
		// succeed: a live fork copies the source VM's running memory. Stop the
		// fan-out terminally instead of error-requeueing forever (issue #698).
		return r.failForkSourceTerminal(ctx, fork, "SourceGone", fmt.Sprintf(
			"source sandbox %q does not exist, so this fork's fan-out can never complete: a live fork copies the source VM's running memory, which is gone with the source.",
			fork.Spec.Source.FromSandbox.Name))
	}
	if err != nil {
		logger.Error(err, "get source sandbox failed", "source", fork.Spec.Source.FromSandbox.Name)
		return ctrl.Result{}, err
	}

	// A source in a terminal phase can never become Ready again: its VM is
	// reaped (Terminated) or never came up (Failed). A fork mid-Prepare when
	// the parent dies converges here on its next pass. Stop the fan-out
	// terminally rather than parking in the not-Ready wait below forever
	// (issue #698).
	if source.Status.Phase == v1.SandboxTerminated || source.Status.Phase == v1.SandboxFailed {
		reason := "SourceTerminated"
		if source.Status.Phase == v1.SandboxFailed {
			reason = "SourceFailed"
		}
		return r.failForkSourceTerminal(ctx, fork, reason, fmt.Sprintf(
			"source sandbox %q is in the terminal phase %s, so this fork's fan-out can never complete: a live fork copies the source VM's running memory, which no longer exists.",
			source.Name, source.Status.Phase))
	}

	// Live-fork secret gate: duplicating guest memory duplicates any delivered
	// secrets into every fork. Default-deny (secretInheritance=reissue) without
	// explicit opt-in. Spec-level check: fires regardless of source readiness.
	inherit := fork.Spec.SecretInheritance == v1.SecretInherit
	if len(source.Spec.Secrets) > 0 {
		now := metav1.Now()
		if !inherit {
			setCondition(&fork.Status.Conditions, metav1.Condition{
				Type:               v1.ConditionRejected,
				Status:             metav1.ConditionTrue,
				LastTransitionTime: now,
				Reason:             v1.ReasonSecretInheritanceDenied,
				Message:            "source sandbox holds secrets; recreate the fork with spec.secretInheritance=inherit to permit it (forks duplicate guest memory, including secret values)",
			})
			if err := r.Status().Update(ctx, fork); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil // terminal: no requeue
		}
		// Audit trail for the explicit opt-in. Only write status when the
		// condition is not already recorded, so the status-update-triggered
		// re-reconcile does not loop on itself.
		if c := meta.FindStatusCondition(fork.Status.Conditions, "SecretInheritance"); c == nil || c.Status != metav1.ConditionTrue || c.Reason != "ExplicitOptIn" {
			setCondition(&fork.Status.Conditions, metav1.Condition{
				Type:               "SecretInheritance",
				Status:             metav1.ConditionTrue,
				LastTransitionTime: now,
				Reason:             "ExplicitOptIn",
				Message:            "fork inherits the source's in-memory secrets by explicit opt-in",
			})
			if err := r.Status().Update(ctx, fork); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	if source.Status.Phase != v1.SandboxReady {
		logger.Info("source sandbox not ready, waiting", "source", source.Name, "phase", source.Status.Phase)
		return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
	}

	// Husk fork path: the source VM is owned by the source husk pod's stub, not
	// forkd's engine, so the only way to live-fork it is for the owning stub to
	// snapshot it and N child husk pods to restore that snapshot. The raw-forkd
	// ForkRunning path below is left unchanged for the non-husk default.
	if r.EnableHuskPods {
		return r.reconcileHuskFork(ctx, fork, &source)
	}

	// Find the node running the source sandbox. A live/standard fork is pinned
	// to the source sandbox's node by construction: ForkRunning copies the
	// source VM's already-resident guest memory in place, so the fork cannot be
	// placed on any other node and the capacity-aware SelectNode does not apply
	// here (it governs cold claim placement, where a node is genuinely chosen).
	// The node's own admission still guards the live fork at the forkd layer.
	node, ok := r.NodeRegistry.GetNode(source.Status.Node)
	if !ok {
		return ctrl.Result{}, fmt.Errorf("node %s not found in registry", source.Status.Node)
	}

	// Resolve the source's pool for the inline template (volume fork policies). The
	// source is a poolRef Sandbox; its pool carries the inline template (ADR 0007).
	var template *v1.PoolTemplateSpec
	if source.Spec.Source.PoolRef != nil {
		var pool v1.SandboxPool
		if err := r.Get(ctx, client.ObjectKey{
			Namespace: fork.Namespace,
			Name:      source.Spec.Source.PoolRef.Name,
		}, &pool); err != nil {
			return ctrl.Result{}, err
		}
		t, err := r.resolvePoolTemplate(ctx, &pool)
		if err != nil {
			return ctrl.Result{}, err
		}
		template = t
	} else {
		template = &v1.PoolTemplateSpec{}
	}

	// Create forks
	total := int32(len(fork.Status.Children))
	needed := replicas - fork.Status.ReadyReplicas
	for i := int32(0); i < needed; i++ {
		forkID := fmt.Sprintf("%s-fork-%d", fork.Name, total+i)

		// Translate the template's volumes (with this fork's VolumeOverrides
		// applied) into the Fork RPC's VolumeMounts. A live fork (ForkRunning)
		// inherits the source's already-attached drives, so these are carried
		// for the node to reconcile rather than re-prepared here.
		volumes := volumeMounts(template.Volumes, fork.Spec.VolumeOverrides)

		// Per-fork bearer token: the source's token never opens the fork (the
		// reissue default). The value reaches exactly two places: the
		// ForkRunningRequest and the owned token Secret below. Never status,
		// conditions, events, or logs.
		apiToken, err := mintAPIToken()
		if err != nil {
			logger.Error(err, "token minting failed", "fork", forkID)
			continue
		}

		// Call forkd.ForkRunning on the source node
		result, err := r.forkRunningOnNode(ctx, node, source.Status.SandboxID, forkID, fork.Spec.Source.FromSandbox.PauseSource, volumes, apiToken)
		if err != nil {
			logger.Error(err, "fork failed", "fork", forkID)
			continue
		}

		// Hand the token to the fork's consumer via a Secret owned by the Sandbox
		// (GC'd with it). A fork without its token Secret is unusable, so it is not
		// recorded as ready.
		if err := ensureSandboxTokenSecret(ctx, r.Client, fork, forkID+tokenSecretSuffix, apiToken, result.Endpoint); err != nil {
			logger.Error(err, "token secret write failed", "fork", forkID)
			continue
		}

		fork.Status.Children = append(fork.Status.Children, v1.SandboxChild{
			Name:             forkID,
			SandboxID:        result.SandboxID,
			Endpoint:         result.Endpoint,
			Node:             node.Name,
			Phase:            v1.SandboxReady,
			StartupLatencyMs: int64(result.ForkTimeMs),
		})
		fork.Status.ReadyReplicas++

		logger.Info("fork created",
			"fork", forkID,
			"node", node.Name,
			"forkTime", fmt.Sprintf("%.2fms", result.ForkTimeMs),
		)
	}

	now := metav1.Now()
	fork.Status.CheckpointTime = &now
	ready := fork.Status.ReadyReplicas >= replicas
	fork.Status.Phase = forkPhase(ready)
	setCondition(&fork.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             conditionStatus(ready),
		LastTransitionTime: now,
		Reason:             "ForksCreated",
		Message:            fmt.Sprintf("%d/%d forks ready", fork.Status.ReadyReplicas, replicas),
	})

	if err := r.Status().Update(ctx, fork); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// failForkSourceTerminal fails a fork TERMINALLY because its source sandbox is
// in a terminal phase or gone (issue #698). Terminal applies to the FAN-OUT,
// never to the born children: a child once activated is an INDEPENDENT
// sandbox (its memory was copied at fork time and its VM runs regardless of
// the source), so only the never-activated pending child pods are deleted
// (they are owner-ref'd to the FORK, not the pool, so no pool machinery ever
// replaces or releases them; left in place they hold their mitos.run/kvm and
// memory requests forever). Then the terminal status is recorded, in one of
// two honest shapes:
//
//   - No surviving children: the fork failed outright. Phase Failed, a True
//     SourceTerminated condition plus a mirrored Ready=False (the gateway's
//     failureReason reads the Ready condition message on a Failed sandbox, so
//     the cause reaches the SDK caller instead of an eternal pending), and
//     FinishedAt so the GC TTL pass reaps the fork like every other Failed
//     sandbox.
//   - Surviving children: the fan-out stopped short, but live sandboxes
//     remain. The phase is NOT forced Failed and FinishedAt is NOT stamped:
//     the GC TTL pass deletes Failed+FinishedAt sandboxes, and deleting the
//     fork object would take the surviving children down through their owner
//     refs. The phase keeps its fan-out meaning (Restoring: not every
//     requested replica exists) and the SourceTerminated condition (mirrored
//     on Ready=False, which was already False mid fan-out) is the
//     authoritative "fan-out stopped" signal. ReadyReplicas and
//     Status.Children keep the survivors.
//
// In both shapes the SourceTerminated condition short-circuits every later
// reconcile, so this is written once and never requeued.
func (r *SandboxReconciler) failForkSourceTerminal(ctx context.Context, fork *v1.Sandbox, reason, cause string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Release the pending children BEFORE recording the terminal condition: if
	// a delete fails the error requeues this whole path, and the
	// condition-gated short-circuit above must never skip a partially done
	// cleanup. Activated children (those backing a Status.Children entry, the
	// same source of truth the fan-out loop records a completed child in) are
	// never touched.
	if err := r.deletePendingForkChildPods(ctx, fork); err != nil {
		return ctrl.Result{}, err
	}

	survivors := int32(len(fork.Status.Children))
	replicas := effectiveReplicas(fork)
	now := metav1.Now()

	var message string
	if survivors > 0 {
		message = cause + fmt.Sprintf(
			" The fan-out stopped at %d of %d children; the existing children keep running and are unaffected (an activated child is an independent sandbox holding its own copy of the source memory). The never-activated pending child pods were deleted to release their node resources. Fork a Ready sandbox to create more children.",
			survivors, replicas)
		fork.Status.ReadyReplicas = survivors
	} else {
		message = cause + " No child had been activated; this fork's pending child pods were deleted to release their node resources. Create a fresh sandbox from the pool and fork that instead."
		fork.Status.Phase = v1.SandboxFailed
		fork.Status.ReadyReplicas = 0
		fork.Status.Children = nil
		if fork.Status.FinishedAt == nil {
			fork.Status.FinishedAt = &now
		}
	}
	setCondition(&fork.Status.Conditions, metav1.Condition{
		Type:               "SourceTerminated",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: fork.Generation,
	})
	setCondition(&fork.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: fork.Generation,
	})
	if err := r.Status().Update(ctx, fork); err != nil {
		return ctrl.Result{}, err
	}
	logger.Info("fork fan-out stopped terminally: source is terminal or gone",
		"fork", fork.Name, "source", fork.Spec.Source.FromSandbox.Name, "reason", reason, "survivors", survivors)
	return ctrl.Result{}, nil // terminal: no requeue
}

// deletePendingForkChildPods deletes the fork's NEVER-ACTIVATED child husk
// pods (matched by the huskForkLabel and re-checked against the fork's
// controller owner ref) so their scheduler resources are released. A pod
// backing a Status.Children entry is an activated, independent sandbox and is
// never deleted here. Idempotent: a pod already terminating or gone is
// skipped. The raw-forkd fork path creates no pods, so this is a no-op there.
func (r *SandboxReconciler) deletePendingForkChildPods(ctx context.Context, fork *v1.Sandbox) error {
	activated := make(map[string]bool, 2*len(fork.Status.Children))
	for i := range fork.Status.Children {
		// A husk fork child records Name = the stable slot name and SandboxID =
		// the pod name (they coincide today); guard both so a future divergence
		// can never mark an active pod pending.
		activated[fork.Status.Children[i].Name] = true
		activated[fork.Status.Children[i].SandboxID] = true
	}
	// List through the UNCACHED reader when available, mirroring the source
	// lookup's cache-staleness guard above: a just-created pending pod the
	// informer cache has not observed yet would be silently skipped by a
	// cached list, and the SourceTerminated short-circuit means this cleanup
	// never runs again, leaking the pod's KVM and memory requests forever.
	lister := client.Reader(r.Client)
	if r.APIReader != nil {
		lister = r.APIReader
	}
	var pods corev1.PodList
	if err := lister.List(ctx, &pods,
		client.InNamespace(fork.Namespace),
		client.MatchingLabels{huskForkLabel: fork.Name},
	); err != nil {
		return fmt.Errorf("list fork child pods of %s/%s: %w", fork.Namespace, fork.Name, err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if activated[pod.Name] {
			continue
		}
		if !metav1.IsControlledBy(pod, fork) || pod.DeletionTimestamp != nil {
			continue
		}
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete fork child pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
	}
	return nil
}

// forkPhase maps the fan-out readiness to a Sandbox phase: Ready when all
// children are up, Restoring while they are still coming up.
func forkPhase(ready bool) v1.SandboxPhase {
	if ready {
		return v1.SandboxReady
	}
	return v1.SandboxRestoring
}

// huskForksInPodDir is the in-pod path a husk stub writes a fork snapshot to for
// forkID; it matches the --forks-dir mount the pod builder set (huskForksMountPath).
func huskForksInPodDir(forkID string) string { return filepath.Join(huskForksMountPath, forkID) }

// reconcileHuskFork forks a husk-backed source: it snapshots the source pod's
// running VM ONCE (fork-snapshot control op), then creates and activates N child
// husk pods from the fork snapshot, recording each Ready child in the fork
// status. It is the husk analog of the forkd ForkRunning loop. The fork snapshot
// is node-local and shared read-only by the children on the same node, while each
// child gets its own pod + VM + per-activation rootfs CoW clone (independence) and
// runs the same fail-closed RNG/clock reseed handshake a warm pod does.
func (r *SandboxReconciler) reconcileHuskFork(ctx context.Context, fork *v1.Sandbox, source *v1.Sandbox) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Resolve the source husk pod: a husk claim records Status.SandboxID = pod
	// name and Status.Node = the pod's node.
	srcPod, err := r.findHuskPod(ctx, fork.Namespace, source.Status.SandboxID)
	if err != nil {
		return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
	}
	if srcPod.Status.PodIP == "" {
		return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
	}

	// Attribute the end-to-end fork latency across the level-triggered reconcile
	// passes. A hosted co-location fork reaches Ready over several ~1s requeue
	// passes, so no single pass sees the whole wall-clock; stamp the start on the
	// first working pass (source resolved Ready, source pod reachable) and count
	// each pass that advances the fan-out to a status write. Both are persisted by
	// the Status().Update calls this pass already makes, so the total (from
	// ForkStartedAt to Ready) and the pass count are attributable even though the
	// work is split across passes. Timing/observability only; drives no behavior.
	if fork.Status.ForkStartedAt == nil {
		startNow := metav1.Now()
		fork.Status.ForkStartedAt = &startNow
	}
	fork.Status.ForkReconcilePasses++

	controlPort := r.HuskControlPort
	if controlPort == 0 {
		controlPort = HuskControlPort
	}
	sandboxPort := r.HuskSandboxPort
	if sandboxPort == 0 {
		sandboxPort = huskSandboxPort
	}
	forkSnap := r.forkSnapshot
	if forkSnap == nil {
		forkSnap = ForkSnapshotOnHusk
	}
	activate := r.Activate
	if activate == nil {
		activate = ActivateHuskPod
	}
	spawnVM := r.spawnVM
	if spawnVM == nil {
		spawnVM = SpawnVMOnHusk
	}

	// MultiVMFork routing decision, computed ONCE for the whole fan-out. The
	// spawn-in-source-pod path is taken only when the flag is on AND the source pod
	// is multi-VM capable (its stub runs --multi-vm); otherwise every child takes
	// the byte-for-byte-unchanged new-pod path below. A non-capable source is the
	// silent, safe fallback the design requires.
	spawnInSourcePod := r.multiVMForkEnabled() && huskPodMultiVMCapable(srcPod)

	// Per-pod memory budget for co-located fork VMs (L1.7b, guarantee A), computed
	// ONCE for the whole fan-out from the source pod's ACTUAL memory accounting: the
	// pod holds only floor(memory.max / per-VM guest RAM) VMs safely at the CoW
	// worst case, one slot reserved for the source VM already resident in the pod.
	// This replaces the former hardcoded co-location count.
	coLocationBudget := coLocatedForkVMBudget(srcPod)

	// Cross-fork reservation (guarantee A across CONCURRENT same-source forks): the
	// per-pod budget above is the WHOLE-POD ceiling, so a fork may only co-locate up
	// to the ceiling MINUS the VMs OTHER SandboxForks already placed in this source
	// pod. Without this subtraction N concurrent forks of one source each admit up to
	// the full ceiling and over-admit VMs into the pod (an intra-pod OOM risk under
	// memory.max). Reading the live occupancy here and admitting this fork's children
	// only against the REMAINING room makes the sum across all forks stay within the
	// ceiling; the overflow spills to a new pod exactly as an over-budget single fork
	// already does. Computed ONCE for the fan-out. Only consulted when this fork is
	// actually a co-location candidate (flag on, source capable, ceiling > 0); the
	// flag-off / non-capable / zero-budget paths never list and stay byte-for-byte
	// unchanged.
	coLocationBudgetRemaining := coLocationBudget
	if spawnInSourcePod && coLocationBudget > 0 {
		otherOccupancy, occErr := r.coLocatedVMsInPodByOtherForks(ctx, fork, srcPod)
		if occErr != nil {
			// Conservative on error: treat the pod as full and co-locate nothing this
			// pass (spill), never over-admit. The children take the honestly
			// node-scheduled new-pod path and the reconcile requeues.
			logger.Info("cross-fork co-location occupancy unavailable; co-locating nothing this pass (conservative spill)", "source", srcPod.Name, "detail", occErr.Error())
			coLocationBudgetRemaining = 0
		} else {
			coLocationBudgetRemaining = coLocationBudget - otherOccupancy
			if coLocationBudgetRemaining < 0 {
				coLocationBudgetRemaining = 0
			}
		}
	}

	// One fork snapshot per SandboxFork, keyed by the fork name, taken EXACTLY
	// ONCE and reused for every child across reconcile passes. Children take
	// several passes to reach Ready; re-snapshotting on each pass would re-pause
	// the source and OVERWRITE the fork mem/vmstate, so a child activated in a
	// later pass would restore a NEWER source memory state than an earlier child:
	// the N children would not be a coherent single fork point. The guard is the
	// persisted Status.ForkSnapshotTaken flag, so it survives a controller restart
	// mid-fork (the source is never re-paused once the snapshot exists).
	forkID := fork.Name
	if !fork.Status.ForkSnapshotTaken {
		srcAddr := net.JoinHostPort(srcPod.Status.PodIP, strconv.Itoa(controlPort))
		snapCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		// The source stub writes the snapshot inside its OWN in-pod forks dir mount
		// (huskForksMountPath/<fork-id>); the child reads the same node dir mounted
		// read-only at HuskSnapshotDir.
		tlsConf, err := r.huskDialTLS(snapCtx, fork.Namespace)
		var snapRes husk.ForkSnapshotResult
		snapStart := time.Now()
		if err == nil {
			snapRes, err = forkSnap(snapCtx, srcAddr, tlsConf, husk.ForkSnapshotRequest{
				ForkID:      forkID,
				SnapshotDir: huskForksInPodDir(forkID),
				PauseSource: fork.Spec.Source.FromSandbox.PauseSource,
			})
		}
		// Time the fork-snapshot RPC round-trip and record the husk-reported
		// paused-window sub-stages (pause, create_snapshot, rootfs_freeze, resume),
		// so the one-time source pause cost is attributable in the fork breakdown.
		if err == nil && snapRes.OK {
			logForkStage(logger, forkID, "fork_snapshot_rpc", time.Since(snapStart), snapRes.LatencyMs, snapRes.Stages)
		}
		if err != nil || !snapRes.OK {
			msg := "fork snapshot did not complete"
			if err != nil {
				msg = fmt.Sprintf("fork snapshot transport error: %v", err)
			} else if snapRes.Error != "" {
				msg = "fork snapshot failed: " + snapRes.Error
			}
			logger.Info("husk fork snapshot failed, requeueing", "source", srcPod.Name, "detail", msg)
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		// Record the snapshot was taken BEFORE creating any child, so a crash
		// between here and the child loop does not re-snapshot (re-pause) the
		// source on the next pass. The children always re-read the same fork
		// snapshot dir, so persisting the flag first is safe.
		fork.Status.ForkSnapshotTaken = true
		if err := r.Status().Update(ctx, fork); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Resolve the SOURCE's pool template so the fork child inherits the SAME
	// network posture, egress policy, and resource caps a warm-claimed sandbox of
	// this pool gets (issue #760), mirroring the warm-claim activate path. The
	// warm-claim path builds the ActivateRequest from huskNotifyNetwork(template) +
	// huskEgressConfig(template); the fork child previously sent only SnapshotDir +
	// Token, so a deny-all child came up NETWORKLESS (a socket connect returned an
	// immediate OSError instead of a real deny-all sandbox's tap + nft-drop TIMEOUT)
	// and a networked/allowlisted tier's child lost the parent's access entirely.
	// Best-effort: a source whose pool object cannot be resolved (deleted, or a
	// non-poolRef source) falls back to the empty template, which still yields the
	// fail-closed default-deny network the warm path uses for a no-policy pool
	// (huskNotifyNetwork always returns a non-nil config and huskEgressConfig
	// defaults Egress to deny), so the child is never LESS isolated than the fix
	// intends.
	template := &v1.PoolTemplateSpec{}
	if source.Spec.Source.PoolRef != nil {
		var pool v1.SandboxPool
		if err := r.Get(ctx, client.ObjectKey{Namespace: fork.Namespace, Name: source.Spec.Source.PoolRef.Name}, &pool); err != nil {
			logger.Info("fork child source pool unresolved; using fail-closed default-deny network and default resources", "pool", source.Spec.Source.PoolRef.Name, "detail", err.Error())
		} else if t, terr := r.resolvePoolTemplate(ctx, &pool); terr != nil {
			logger.Info("fork child source pool template unresolved; using fail-closed default-deny network and default resources", "pool", source.Spec.Source.PoolRef.Name, "detail", terr.Error())
		} else {
			template = t
		}
	}
	netCfg := huskEgressConfig(template)

	opts := HuskPodOptions{
		StubImage:       r.HuskStubImage,
		DNSUpstream:     r.HuskDNSUpstream,
		KVMResourceName: r.KVMResourceName,
		SnapshotID:      sourcePoolName(source), // template id, for resource/kernel mounts
		DataDir:         r.DataDir,
		ForkSnapshotID:  forkID,
		ForkSourceNode:  source.Status.Node,
		// The resolved source pool template: the fork child pod inherits its cpu
		// burst cap + memory (buildForkChildPod threads it into buildHuskPod),
		// instead of the default caps an empty PoolTemplateSpec yields (issue #760).
		Template: template,
		// The husk PKI Secrets the child stub mounts for its --control-listen mTLS
		// channel (leaf at /etc/husk/tls, CA at /etc/husk/ca). buildHuskPod only
		// adds the TLS/CA volumes when these are set; omitting them (the previous
		// bug) leaves the child without its TLS material and the stub crash-loops
		// reading --tls-cert. They are the SAME Secrets the warm-pool path uses.
		TLSSecretName: r.HuskTLSSecretName,
		CASecretName:  r.HuskCASecretName,
		// The child's per-activation rootfs CoW clone is sourced from the FROZEN
		// source rootfs the source stub captured inside the fork snapshot's paused
		// window (SnapshotDir/rootfs.ext4), NOT the source's LIVE rootfs and NOT the
		// pristine template rootfs. The frozen copy is a point-in-time pair with the
		// mem+vmstate checkpoint, so the child's restored memory matches its disk.
		// Cloning from the source's live rootfs would let the resumed source drift
		// the disk out of sync with the checkpoint; the template rootfs would lose
		// every write the source made. The frozen copy rides the SAME read-only
		// snapshot mount the child restores mem+vmstate from.
		ForkSourceRootfsPath: huskForkRootfsInPodPath(),
	}

	// Fixed-slot, idempotent child set. The child pods are EXACTLY Replicas, with
	// STABLE names ("<fork>-fork-<i>" for i in [0, Replicas)) that never change
	// across reconcile passes. The previous count-driven loop derived the name from
	// (TotalForks + i) and the iteration count from (Replicas - ReadyForks): once a
	// child in a pass became Ready it bumped TotalForks mid-loop, so the next i
	// produced a NEW name (fork-2, fork-3, ...) and ensureForkChildPod created an
	// EXTRA pod instead of reusing an existing slot, overcommitting the node. With
	// fixed names the number of child pods can never exceed Replicas regardless of
	// how many passes run or how slowly children become Ready: ensureForkChildPod
	// is get-or-create by the stable name, so each slot maps to exactly one pod.
	//
	// ReadyForks is recomputed from scratch each pass (counting Ready slots) rather
	// than incremented, so a slow or transient child does not permanently inflate
	// the count.
	//
	// A slot already recorded Ready (and so already activated with its token Secret
	// written) is carried forward as-is and NOT re-activated: re-activating a live
	// child VM each pass would mint a fresh token and thrash the restored VM.
	recorded := make(map[string]v1.SandboxChild, len(fork.Status.Children))
	for _, f := range fork.Status.Children {
		recorded[f.Name] = f
	}

	var ready int32
	var forks []v1.SandboxChild
	for i := int32(0); i < effectiveReplicas(fork); i++ {
		childName := fmt.Sprintf("%s-fork-%d", fork.Name, i)

		// A slot activated in a prior pass is carried forward as-is BEFORE any
		// routing decision, so its fate is decided once and independently of the
		// current flag state. Without this hoist, a slot recorded via the
		// spawn-in-source-pod path would fall to the new-pod branch after a
		// --multi-vm-fork flip-to-off (controller restart) and ensureForkChildPod
		// would create a brand-new pod for a childName that status never references
		// (the carried-forward source-pod record wins), leaking a KVM slot until the
		// fork is GC'd. Idempotent per slot; never re-activates a live child VM.
		if info, ok := recorded[childName]; ok {
			forks = append(forks, info)
			ready++
			continue
		}

		// MultiVMFork routing: for the first coLocationBudgetRemaining slots of a
		// multi-VM-capable source, spawn the child as an ADDITIONAL VM INSIDE the
		// source pod (spawn-vm op) instead of creating a brand-new child pod. The
		// budget is the whole-pod ceiling MINUS the VMs other same-source forks
		// already placed there (the cross-fork reservation), so concurrent forks of
		// one source never over-admit past the pod's memory budget. Slots past the
		// remaining budget fall through to the new-pod path below, which spills the
		// child to a fresh, honestly node-scheduled pod so a fork never overcommits
		// the source pod (L1.7b, guarantee A). When the flag is off or the source is
		// not multi-VM capable (spawnInSourcePod is false), or the remaining budget is
		// 0, this branch is never entered and the path below is byte-for-byte
		// unchanged.
		if spawnInSourcePod && int(i) < coLocationBudgetRemaining {
			spawnedChild, ok := r.spawnForkChildInSourcePod(ctx, spawnForkChildArgs{
				fork:        fork,
				srcPod:      srcPod,
				source:      source,
				childName:   childName,
				template:    template,
				netCfg:      netCfg,
				sandboxPort: sandboxPort,
				controlPort: controlPort,
				spawnVM:     spawnVM,
			})
			if ok {
				forks = append(forks, spawnedChild)
				ready++
				// Persist the co-located child to status IMMEDIATELY, before spawning the
				// next slot or returning. The cross-fork reservation
				// (coLocatedVMsInPodByOtherForks) counts co-located VMs from RECORDED
				// status, so a live VM that exists in srcPod but is not yet recorded would
				// let a concurrent same-source fork read stale occupancy and over-admit.
				// Writing after each spawn shrinks that window from the whole fan-out to a
				// single spawn-then-write. The persisted set is the full child ledger, not
				// the prefix processed so far: `forks` only holds slots 0..i, so any
				// already-recorded HIGHER-index child (carried forward, appended later in
				// this loop) must be merged in or an early exit would persist a TRUNCATED
				// ledger and cross-fork occupancy would under-count.
				persisted := append([]v1.SandboxChild(nil), forks...)
				inForks := make(map[string]bool, len(forks))
				for _, f := range forks {
					inForks[f.Name] = true
				}
				for name, rec := range recorded {
					if !inForks[name] {
						persisted = append(persisted, rec)
					}
				}
				fork.Status.Children = persisted
				if err := r.Status().Update(ctx, fork); err != nil {
					// A conflict means a concurrent writer advanced the fork; returning the
					// error requeues so the next pass re-reads and recounts occupancy fresh,
					// never over-admitting.
					return ctrl.Result{}, err
				}
			}
			// A failed spawn is NOT wedged: the slot is left not-ready with the cause
			// logged (issue #28), and the reconcile requeues below because
			// ReadyReplicas < Replicas. It never silently hangs and never weakens the
			// new-pod fallback for the OFF path.
			continue
		}

		// Get-or-create the child pod for this slot (idempotent by the stable name).
		// The recorded-slot carry-forward is hoisted above the routing branches, so a
		// slot already activated (in-source-pod or in its own pod) never reaches here
		// and this only runs for a genuinely new slot on the new-pod path.
		child, err := r.ensureForkChildPod(ctx, fork, srcPod, childName, opts)
		if err != nil {
			logger.Error(err, "create fork child pod failed", "child", childName)
			continue
		}

		// The child must be Running+Ready before it can be activated. Not ready yet:
		// requeue this slot next pass WITHOUT creating any extra pod.
		if child.Status.PodIP == "" || !huskPodReady(child) {
			continue
		}

		endpoint := net.JoinHostPort(child.Status.PodIP, strconv.Itoa(sandboxPort))
		// Persist a STABLE token BEFORE activating (issue #183). Activation is not
		// transactional: the activate ack can be lost, or the post-activate
		// bookkeeping below can fail, leaving the VM ACTIVE while the controller
		// did not record it. If the token were minted fresh per pass and written
		// only AFTER activate, the next pass would re-activate an already-active VM
		// (the stub refuses: "must be dormant") with a different token, forever.
		// Persisting first and reusing the same token means a re-drive activates
		// with the token the VM already holds, and AlreadyActive lets us adopt it.
		apiToken, err := ensureForkChildToken(ctx, r.Client, fork, childName+tokenSecretSuffix, endpoint)
		if err != nil {
			logger.Error(err, "fork child token secret write failed", "child", childName)
			continue
		}

		addr := net.JoinHostPort(child.Status.PodIP, strconv.Itoa(controlPort))
		tlsConf, err := r.huskDialTLS(ctx, fork.Namespace)
		var actRes husk.ActivateResult
		actStart := time.Now()
		if err == nil {
			actRes, err = activate(ctx, addr, tlsConf, husk.ActivateRequest{
				// The child reads the FORK snapshot here (its <dataDir>/forks/<fork-id>
				// hostPath is mounted at HuskSnapshotDir). No ExpectedDigest: the fork
				// snapshot is node-local, not content-addressed.
				SnapshotDir: HuskSnapshotDir,
				// Inherit the SAME network posture the warm-claim activate delivers
				// (issue #760): the guest is pinned to the in-pod /30 + DNS proxy
				// (huskNotifyNetwork, never nil), and the full egress policy (default
				// verdict, allowlist, block_network, CIDR allows, inbound) comes from
				// the resolved source pool template. Without these the child came up
				// networkless / with no access; an omitted egress chain is an
				// isolation gap if the baked NIC were ever routable.
				Network:      huskNotifyNetwork(template),
				Egress:       netCfg.Egress,
				Allow:        netCfg.Allow,
				BlockNetwork: netCfg.BlockNetwork,
				AllowCIDRs:   netCfg.AllowCIDRs,
				Inbound:      netCfg.Inbound,
				InboundCIDRs: netCfg.InboundCIDRs,
				Token:        apiToken,
			})
		}
		if err != nil {
			logger.Info("fork child activation failed, will retry", "child", childName, "detail", err.Error())
			continue
		}
		if !actRes.OK && !actRes.AlreadyActive {
			// Surface WHY (issue #28 LLM-legible errors): the bare "will retry" hid
			// the cause of stuck fork children.
			logger.Info("fork child activation failed, will retry", "child", childName, "detail", actRes.Error)
			continue
		}
		// OK, or AlreadyActive: a prior Activate brought this child up but its ack
		// or bookkeeping was lost (issue #183). Either way the VM is active with the
		// stable token persisted above, so ADOPT it as ready rather than retrying a
		// non-dormant VM forever.

		// New-pod fork child: time the activate RPC round-trip and record the
		// husk-reported activate sub-stages (vmstate_restore, guest_ready,
		// handshake, ...). AlreadyActive re-drives carry no fresh stub-side timing
		// (LatencyMs 0, empty Stages), so this attributes only the pass that did the
		// real activation work.
		if actRes.OK {
			logForkStage(logger, childName, "activate_rpc", time.Since(actStart), actRes.LatencyMs, actRes.Stages)
		}

		forks = append(forks, v1.SandboxChild{
			Name:      childName,
			SandboxID: child.Name,
			Endpoint:  endpoint,
			Node:      child.Spec.NodeName,
			Phase:     v1.SandboxReady,
		})
		ready++
	}
	fork.Status.Children = forks
	fork.Status.ReadyReplicas = ready
	fork.Status.Phase = forkPhase(fork.Status.ReadyReplicas >= effectiveReplicas(fork))

	now := metav1.Now()
	fork.Status.CheckpointTime = &now
	setCondition(&fork.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             conditionStatus(fork.Status.ReadyReplicas >= effectiveReplicas(fork)),
		LastTransitionTime: now,
		Reason:             "ForksCreated",
		Message:            fmt.Sprintf("%d/%d husk forks ready", fork.Status.ReadyReplicas, effectiveReplicas(fork)),
	})
	if err := r.Status().Update(ctx, fork); err != nil {
		return ctrl.Result{}, err
	}
	if fork.Status.ReadyReplicas < effectiveReplicas(fork) {
		// Children still coming up; requeue to drive them Ready.
		return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
	}
	// Fan-out complete: emit the single-fork end-to-end breakdown. The total is
	// the wall-clock from the first working pass (ForkStartedAt) to Ready NOW,
	// attributed across the level-triggered reconcile passes, so a level of the
	// ~728 ms hosted fork that lives in requeue wait between passes (rather than in
	// any one RPC) is visible as total >> the sum of the per-RPC stages. The
	// per-stage RPC and husk sub-stage lines were emitted at each boundary above.
	if fork.Status.ForkStartedAt != nil {
		total := time.Since(fork.Status.ForkStartedAt.Time)
		observeForkStage("total", total.Seconds())
		logger.Info("fork timing complete",
			"fork", fork.Name,
			"replicas", effectiveReplicas(fork),
			"reconcilePasses", fork.Status.ForkReconcilePasses,
			"totalMs", float64(total.Microseconds())/1000.0,
		)
	}
	return ctrl.Result{}, nil
}

// coLocatedVMsInPodByOtherForks counts the fork-child VMs already co-located
// INSIDE srcPod by SandboxForks OTHER than self. It is the live cross-fork
// occupancy the co-location admission subtracts from the per-pod budget so
// concurrent same-source forks never over-admit past the pod's memory ceiling
// (guarantee A across forks).
//
// A co-located child records status.Children[i].Pod == its host pod and a
// non-empty status.Children[i].VMID (the new-pod spill path leaves both empty), so
// those two fields ARE the cross-fork occupancy ledger; no extra bookkeeping is
// needed. self is excluded because this fork's own co-located slots are governed by
// the slot loop against the remaining budget (double-counting self would make a
// fork spill its own already-placed children on a requeue).
//
// The list is UNCACHED (APIReader) when available, mirroring deletePendingForkChildPods:
// a peer fork that co-located a VM one reconcile earlier has persisted its
// status.Children, but the informer cache may not have observed it yet, and a stale
// read is exactly the over-admission this guards against. With the controller's
// serial reconcile (default MaxConcurrentReconciles 1) two same-source forks never
// evaluate occupancy at the same instant, so the uncached read is an accurate
// point-in-time count; a caller treats a list error as "full" and spills.
func (r *SandboxReconciler) coLocatedVMsInPodByOtherForks(ctx context.Context, self *v1.Sandbox, srcPod *corev1.Pod) (int, error) {
	lister := client.Reader(r.Client)
	if r.APIReader != nil {
		lister = r.APIReader
	}
	var sandboxes v1.SandboxList
	if err := lister.List(ctx, &sandboxes, client.InNamespace(self.Namespace)); err != nil {
		return 0, fmt.Errorf("list sandboxes for cross-fork co-location occupancy of %s/%s: %w", srcPod.Namespace, srcPod.Name, err)
	}
	count := 0
	for i := range sandboxes.Items {
		s := &sandboxes.Items[i]
		if s.UID == self.UID {
			continue
		}
		for j := range s.Status.Children {
			c := &s.Status.Children[j]
			if c.Pod == srcPod.Name && c.VMID != "" {
				count++
			}
		}
	}
	return count, nil
}

// findHuskPod returns the husk pod named name in ns (a husk claim's
// Status.SandboxID is the pod name). It returns an error when not found so the
// caller can requeue.
func (r *SandboxReconciler) findHuskPod(ctx context.Context, ns, name string) (*corev1.Pod, error) {
	var pod corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &pod); err != nil {
		return nil, fmt.Errorf("get source husk pod %s/%s: %w", ns, name, err)
	}
	return &pod, nil
}

// ensureForkChildPod creates the fork child pod if it does not exist and returns
// the current pod object. Idempotent across requeues (a child already created is
// fetched and returned). srcPod is the SOURCE husk pod whose scheduling
// constraints the child inherits (buildForkChildPod).
func (r *SandboxReconciler) ensureForkChildPod(ctx context.Context, fork *v1.Sandbox, srcPod *corev1.Pod, name string, opts HuskPodOptions) (*corev1.Pod, error) {
	var existing corev1.Pod
	err := r.Get(ctx, client.ObjectKey{Namespace: fork.Namespace, Name: name}, &existing)
	if err == nil {
		return &existing, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get fork child pod %s: %w", name, err)
	}
	pod := buildForkChildPod(fork, srcPod, name, opts, r.Scheme)
	if err := r.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("create fork child pod %s: %w", name, err)
	}
	// Re-get so the caller sees the server object (with any defaults applied).
	if err := r.Get(ctx, client.ObjectKey{Namespace: fork.Namespace, Name: name}, &existing); err != nil {
		return nil, fmt.Errorf("re-get fork child pod %s: %w", name, err)
	}
	return &existing, nil
}

// spawnForkChildArgs bundles the inputs for spawnForkChildInSourcePod so the
// slot-loop call site stays readable.
type spawnForkChildArgs struct {
	fork        *v1.Sandbox
	srcPod      *corev1.Pod
	source      *v1.Sandbox
	childName   string
	template    *v1.PoolTemplateSpec
	netCfg      huskNetworkConfig
	sandboxPort int
	controlPort int
	spawnVM     huskVMSpawner
}

// spawnForkChildInSourcePod brings up a fork child as an ADDITIONAL VM inside the
// SOURCE husk pod (the spawn-vm control op) rather than creating a new child pod.
// It is the MultiVMFork routing's per-slot worker and the co-located analog of the
// new-pod ensureForkChildPod + activate pair: it mints and persists the child's
// stable bearer token FIRST (issue #183: a lost spawn ack must re-drive with the
// token the VM already holds, and AlreadyActive lets us adopt it), then asks the
// source stub to spawn a new vmID activating from the SAME fork snapshot the source
// already produced, threading the SAME network + egress posture the new-pod fork
// child inherits (issue #760) so a co-located child is never less isolated. The
// spawned VM runs the same fail-closed RNG/clock reseed handshake activate runs, so
// the fork-correctness handshake is preserved.
//
// It returns (child, true) with status.Pod = the source pod, status.VMID = the
// spawned vmID, and status.Node = the source node when the VM is up (OK or the
// idempotent AlreadyActive), and (_, false) when the spawn did not complete this
// pass. A false is a clean not-ready-yet signal the caller requeues on; it NEVER
// wedges and NEVER weakens the new-pod fallback. The mTLS control channel and the
// fork-snapshot flow are unchanged; SpawnVMOnHusk refuses a nil TLS config so the
// secret-bearing Activate never rides an unauthenticated channel.
func (r *SandboxReconciler) spawnForkChildInSourcePod(ctx context.Context, a spawnForkChildArgs) (v1.SandboxChild, bool) {
	logger := log.FromContext(ctx)

	vmID := forkChildVMID(a.childName)
	endpoint := net.JoinHostPort(a.srcPod.Status.PodIP, strconv.Itoa(a.sandboxPort))

	// Persist a STABLE token BEFORE spawning (issue #183): a spawn ack or the
	// post-spawn bookkeeping can be lost, leaving the VM ACTIVE while the controller
	// did not record it. Reusing the same token on a re-drive lets AlreadyActive
	// adopt the running VM instead of looping.
	apiToken, err := ensureForkChildToken(ctx, r.Client, a.fork, a.childName+tokenSecretSuffix, endpoint)
	if err != nil {
		logger.Error(err, "fork child token secret write failed", "child", a.childName)
		return v1.SandboxChild{}, false
	}

	addr := net.JoinHostPort(a.srcPod.Status.PodIP, strconv.Itoa(a.controlPort))
	tlsConf, err := r.huskDialTLS(ctx, a.fork.Namespace)
	var spRes husk.SpawnVMResult
	spawnStart := time.Now()
	if err == nil {
		spRes, err = a.spawnVM(ctx, addr, tlsConf, husk.SpawnVMRequest{
			VMID: vmID,
			Activate: husk.ActivateRequest{
				// The spawned VM restores from the PARENT FORK SNAPSHOT the source stub
				// wrote to its in-pod forks dir (huskForksInPodDir: mem + vmstate + the
				// frozen source rootfs at <dir>/rootfs.ext4), NOT the pool template.
				// HuskSnapshotDir in the SOURCE pod resolves to the pool TEMPLATE
				// snapshot (the source is a warm pod), so activating from it booted a
				// FRESH template VM that inherited NEITHER the parent's memory NOR its
				// disk: the co-located fork was fast precisely because it skipped the
				// restore a fork requires (the prod-canary correctness bug). Pointing at
				// the fork snapshot dir restores the parent's memory + filesystem, exactly
				// as the new-pod fork child does from <DataDir>/forks/<ForkSnapshotID>.
				SnapshotDir: huskForksInPodDir(a.fork.Name),
				// ForkSnapshot tells the source stub this is a CO-LOCATED fork child: clone
				// the child's rootfs from the FROZEN source rootfs the fork snapshot carries
				// (inherit the DISK) and skip the content-addressed verify a node-local fork
				// snapshot has no digest for, mirroring the new-pod fork child's
				// --allow-unverified-snapshots. Without it the child booted the template.
				ForkSnapshot: true,
				// Inherit the SAME network posture the new-pod fork child gets from the
				// source pool template (issue #760), so a co-located child is never less
				// isolated than a one-pod-per-child fork.
				Network:      huskNotifyNetwork(a.template),
				Egress:       a.netCfg.Egress,
				Allow:        a.netCfg.Allow,
				BlockNetwork: a.netCfg.BlockNetwork,
				AllowCIDRs:   a.netCfg.AllowCIDRs,
				Inbound:      a.netCfg.Inbound,
				InboundCIDRs: a.netCfg.InboundCIDRs,
				Token:        apiToken,
			},
		})
	}
	if err != nil {
		logger.Info("fork child spawn-in-source-pod failed, will retry", "child", a.childName, "detail", err.Error())
		return v1.SandboxChild{}, false
	}
	if !spRes.OK && !spRes.AlreadyActive {
		// Surface WHY (issue #28 LLM-legible errors) rather than a bare retry.
		logger.Info("fork child spawn-in-source-pod failed, will retry", "child", a.childName, "detail", spRes.Error)
		return v1.SandboxChild{}, false
	}
	// OK, or AlreadyActive: a prior spawn brought this VM up but its ack or
	// bookkeeping was lost. Either way the VM is active with the stable token
	// persisted above, so ADOPT it as ready.

	// Co-located fork child: time the spawn-vm RPC round-trip and record the
	// husk-reported prepare + activate sub-stages (fc_boot, rootfs_clone,
	// vmstate_restore, guest_ready, handshake, ...). This is the core of the
	// hosted co-location fork latency, so its breakdown is the primary signal for
	// targeting the bottleneck. AlreadyActive re-drives carry no fresh stub-side
	// timing (LatencyMs 0, empty Stages) and so add nothing to the breakdown.
	if spRes.OK {
		logForkStage(logger, a.childName, "spawn_vm_rpc", time.Since(spawnStart), spRes.LatencyMs, spRes.Stages)
	}
	return v1.SandboxChild{
		Name:      a.childName,
		SandboxID: a.srcPod.Name,
		Endpoint:  endpoint,
		Node:      a.source.Status.Node,
		Pod:       a.srcPod.Name,
		VMID:      vmID,
		Phase:     v1.SandboxReady,
	}, true
}

// finalizeHuskFork removes the node-local fork snapshot from the source husk pod
// (best effort) and clears the finalizer so deletion proceeds. The child pods are
// owner-ref'd to the fork and reaped by Kubernetes GC; only the snapshot dir
// needs explicit cleanup. A transport failure does not block deletion: the dir is
// reclaimed when the source pod is recycled.
func (r *SandboxReconciler) finalizeHuskFork(ctx context.Context, fork *v1.Sandbox) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(fork, huskForkFinalizer) {
		return ctrl.Result{}, nil
	}

	remove := r.removeForkSnapshot
	if remove == nil {
		remove = RemoveForkSnapshotOnHusk
	}

	// Resolve the source pod to dial; if it is gone the snapshot went with it.
	var source v1.Sandbox
	if err := r.Get(ctx, client.ObjectKey{Namespace: fork.Namespace, Name: fork.Spec.Source.FromSandbox.Name}, &source); err == nil && source.Status.SandboxID != "" {
		if srcPod, err := r.findHuskPod(ctx, fork.Namespace, source.Status.SandboxID); err == nil && srcPod.Status.PodIP != "" {
			controlPort := r.HuskControlPort
			if controlPort == 0 {
				controlPort = HuskControlPort
			}
			addr := net.JoinHostPort(srcPod.Status.PodIP, strconv.Itoa(controlPort))
			rmCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			tlsConf, derr := r.huskDialTLS(rmCtx, fork.Namespace)
			if derr != nil {
				logger.Info("remove fork snapshot skipped; husk dial config unavailable", "fork", fork.Name, "detail", derr.Error())
			} else if _, err := remove(rmCtx, addr, tlsConf, husk.RemoveForkSnapshotRequest{
				ForkID:      fork.Name,
				SnapshotDir: huskForksInPodDir(fork.Name),
			}); err != nil {
				logger.Info("remove fork snapshot failed; proceeding with delete", "fork", fork.Name, "detail", err.Error())
			}
		}
	}

	controllerutil.RemoveFinalizer(fork, huskForkFinalizer)
	if err := r.Update(ctx, fork); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

type forkRunningResult struct {
	SandboxID    string
	Endpoint     string
	ForkTimeMs   float64
	CheckpointMs float64
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete

// sourcePoolName returns the pool name of a source Sandbox (its source.poolRef),
// used by the husk fork path as the template id for resource/kernel mounts.
// Empty when the source is not a poolRef sandbox.
func sourcePoolName(source *v1.Sandbox) string {
	if source.Spec.Source.PoolRef != nil {
		return source.Spec.Source.PoolRef.Name
	}
	return ""
}
