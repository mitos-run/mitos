package controller

import (
	"context"
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	v1alpha1 "mitos.run/mitos/api/v1alpha1"
	v1alpha2 "mitos.run/mitos/api/v1alpha2"
	"mitos.run/mitos/internal/apierr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// SandboxReconciler reconciles a v1alpha2 Sandbox (the consolidated run-axis
// kind, ADR 0007, issue #23) onto the EXISTING engine, additively. It maps a
// Sandbox onto exactly one of the surviving run-axis kinds and mirrors that
// child's status back:
//
//   - source.poolRef (replicas 1): owns a SandboxClaim bound to the pool. This
//     drives the same fork-from-pool path a SandboxClaim drives today (the claim
//     equivalent).
//   - source.fromSandbox (replicas N): owns a SandboxFork of the named source.
//     This drives the live-fork path the SandboxFork reconciler drives today
//     (the fork equivalent), reporting per-child status.
//
// The mapping is the same shape the agents.x-k8s.io facade uses to map the
// upstream Sandbox onto a SandboxClaim: a consolidated surface served on top of
// the unchanged engine. The existing SandboxClaim, SandboxFork, SandboxTemplate,
// and SandboxPool kinds and controllers are UNTOUCHED; both surfaces serve
// during the transition. The full cutover (this reconciler owning the engine
// directly, the old kinds removed) is the staged continuation (ADR 0007 OPEN).
//
// source.fromRevision is NEW v2 surface with no v1 engine path yet; it is
// reported as a clear not-ready condition (the lineage-resume engine path is the
// continuation), never silently dropped.
type SandboxReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// childClaimName / childForkName derive the owned child object name from the
// Sandbox. They are deterministic so the child is get-or-created idempotently.
func childClaimName(sb *v1alpha2.Sandbox) string { return sb.Name }
func childForkName(sb *v1alpha2.Sandbox) string  { return sb.Name }

// +kubebuilder:rbac:groups=mitos.run,resources=sandboxes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mitos.run,resources=sandboxes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mitos.run,resources=sandboxes/finalizers,verbs=update

// Reconcile drives the v1alpha2 Sandbox lifecycle onto the engine. Deletion is
// handled by the owner-reference garbage collector (the owned SandboxClaim /
// SandboxFork carries an owner reference to the Sandbox).
func (r *SandboxReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var sb v1alpha2.Sandbox
	if err := r.Get(ctx, req.NamespacedName, &sb); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !sb.DeletionTimestamp.IsZero() {
		// The owned child carries an owner reference, so the apiserver GCs it.
		return ctrl.Result{}, nil
	}

	switch {
	case sb.Spec.Source.PoolRef != nil:
		return r.reconcilePoolRef(ctx, &sb)
	case sb.Spec.Source.FromSandbox != nil:
		return r.reconcileFromSandbox(ctx, &sb)
	case sb.Spec.Source.FromRevision != nil:
		// NEW v2 surface; the lineage-resume engine path is the continuation. Be
		// honest: surface a clear not-ready condition with actionable text rather
		// than silently doing nothing.
		return ctrl.Result{}, r.mirror(ctx, &sb, v1alpha1.SandboxPending, sandboxMirror{
			reason: "FromRevisionNotImplemented",
			message: fmt.Sprintf(
				"source.fromRevision (workspace %q revision %q) is the v2 lineage-resume surface; its engine path is the issue #23 continuation, not yet served. Use source.poolRef or source.fromSandbox",
				sb.Spec.Source.FromRevision.Workspace, sb.Spec.Source.FromRevision.Revision),
		})
	default:
		// A Sandbox with no source is invalid; surface it rather than crash-looping.
		logger.Info("sandbox has no source set", "sandbox", sb.Name)
		return ctrl.Result{}, r.mirror(ctx, &sb, v1alpha1.SandboxFailed, sandboxMirror{
			reason:  "NoSource",
			message: "spec.source must set exactly one of poolRef, fromSandbox, or fromRevision",
		})
	}
}

// reconcilePoolRef maps source.poolRef onto an owned SandboxClaim (the claim
// equivalent) and mirrors the claim phase/endpoint/pod back onto the Sandbox.
func (r *SandboxReconciler) reconcilePoolRef(ctx context.Context, sb *v1alpha2.Sandbox) (ctrl.Result, error) {
	claim, err := r.ensureClaim(ctx, sb)
	if err != nil {
		return ctrl.Result{}, err
	}

	m := sandboxMirror{
		endpoint:         claim.Status.Endpoint,
		pod:              claim.Status.SandboxID,
		sandboxID:        claim.Status.SandboxID,
		startupLatencyMs: claim.Status.ForkTimeMicros / 1000,
		startedAt:        claim.Status.StartedAt,
		finishedAt:       claim.Status.FinishedAt,
	}
	switch claim.Status.Phase {
	case v1alpha1.SandboxReady:
		m.reason = "Forked"
		m.message = fmt.Sprintf("sandbox is Ready on pool %q", sb.Spec.Source.PoolRef.Name)
	default:
		m.reason = "Claim" + nonEmptyPhase(claim.Status.Phase)
		m.message = fmt.Sprintf("fork-from-pool is in phase %q on pool %q", nonEmptyPhase(claim.Status.Phase), sb.Spec.Source.PoolRef.Name)
	}
	return ctrl.Result{}, r.mirror(ctx, sb, mapPhase(claim.Status.Phase), m)
}

// reconcileFromSandbox maps source.fromSandbox onto an owned SandboxFork (the
// fork equivalent) and mirrors readyForks / per-child status back.
func (r *SandboxReconciler) reconcileFromSandbox(ctx context.Context, sb *v1alpha2.Sandbox) (ctrl.Result, error) {
	// Capability-budget enforcement (issue #25): a self-initiated fork
	// (source.fromSandbox = P) is admitted only while P's capability budget has
	// room. An over-budget fork is rejected terminally with a typed
	// BudgetExhausted condition BEFORE the fork is ever materialized, so no engine
	// work is spent on a fork the creator's budget forbids.
	admitted, reason, message, err := r.enforceForkBudget(ctx, sb)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !admitted {
		return ctrl.Result{}, r.mirror(ctx, sb, v1alpha1.SandboxFailed, sandboxMirror{
			reason:  reason,
			message: message,
		})
	}

	fork, err := r.ensureFork(ctx, sb)
	if err != nil {
		return ctrl.Result{}, err
	}

	children := make([]v1alpha2.SandboxChild, 0, len(fork.Status.Forks))
	for _, f := range fork.Status.Forks {
		children = append(children, v1alpha2.SandboxChild{
			Name:             f.Name,
			SandboxID:        f.SandboxID,
			Endpoint:         f.Endpoint,
			Node:             f.Node,
			Phase:            f.Phase,
			StartupLatencyMs: f.ForkTimeMicros / 1000,
		})
	}

	m := sandboxMirror{
		readyReplicas:     fork.Status.ReadyForks,
		children:          children,
		forkSnapshotTaken: fork.Status.ForkSnapshotTaken,
		checkpointTime:    fork.Status.CheckpointTime,
	}

	// A rejected fork (for example a secret-inheritance denial) is terminal: the
	// Sandbox reports Failed with the fork's reason carried across.
	if rejected := apimeta.FindStatusCondition(fork.Status.Conditions, "Rejected"); rejected != nil && rejected.Status == metav1.ConditionTrue {
		m.reason = rejected.Reason
		m.message = rejected.Message
		return ctrl.Result{}, r.mirror(ctx, sb, v1alpha1.SandboxFailed, m)
	}

	ready := fork.Status.ReadyForks >= fork.Spec.Replicas && fork.Spec.Replicas > 0
	if ready {
		m.reason = "ForksCreated"
		m.message = fmt.Sprintf("%d/%d children ready (forked from %q)", fork.Status.ReadyForks, fork.Spec.Replicas, sb.Spec.Source.FromSandbox.Name)
		return ctrl.Result{}, r.mirror(ctx, sb, v1alpha1.SandboxReady, m)
	}
	m.reason = "ForksPending"
	m.message = fmt.Sprintf("%d/%d children ready (forking from %q)", fork.Status.ReadyForks, fork.Spec.Replicas, sb.Spec.Source.FromSandbox.Name)
	return ctrl.Result{}, r.mirror(ctx, sb, v1alpha1.SandboxRestoring, m)
}

// effectiveReplicas treats a zero (unset) replicas as one, matching the
// kubebuilder default so an unset replicas counts as a single fork.
func effectiveReplicas(sb *v1alpha2.Sandbox) int32 {
	if sb.Spec.Replicas <= 0 {
		return 1
	}
	return sb.Spec.Replicas
}

// enforceForkBudget enforces the source Sandbox P's capability budget for a
// self-initiated fork (sb.Spec.Source.FromSandbox = P), issue #25. It admits
// forks up to P.spec.budget.maxForks (depth-aggregate by replicas) and rejects
// the ones beyond, ranking P's fork-children deterministically by
// (creationTimestamp, name) so the decision is the same regardless of reconcile
// order. It best-effort records P.status.budgetSpend.forks (the admitted count,
// capped at the limit).
//
// It returns admitted=true (and proceeds the normal flow) when P does not exist,
// when P has no budget, or when P.budget.maxForks is unset (unlimited): there is
// no Sandbox-scoped fork budget to enforce in those cases.
func (r *SandboxReconciler) enforceForkBudget(ctx context.Context, sb *v1alpha2.Sandbox) (bool, string, string, error) {
	src := sb.Spec.Source.FromSandbox
	if src == nil {
		return true, "", "", nil
	}

	var parent v1alpha2.Sandbox
	if err := r.Get(ctx, client.ObjectKey{Name: src.Name, Namespace: sb.Namespace}, &parent); err != nil {
		if apierrors.IsNotFound(err) {
			// No source Sandbox to enforce a budget against; let the normal flow
			// proceed (the fork reconciler surfaces a missing-source condition).
			return true, "", "", nil
		}
		return false, "", "", fmt.Errorf("get source sandbox %s/%s for budget: %w", sb.Namespace, src.Name, err)
	}

	if parent.Spec.Budget == nil || parent.Spec.Budget.MaxForks == nil {
		// Unlimited fork budget; admit.
		return true, "", "", nil
	}
	limit := *parent.Spec.Budget.MaxForks

	// All fork-children of P in the namespace (including sb), ranked
	// deterministically by (creationTimestamp, name).
	var siblings v1alpha2.SandboxList
	if err := r.List(ctx, &siblings, client.InNamespace(sb.Namespace)); err != nil {
		return false, "", "", fmt.Errorf("list fork-children of %s/%s for budget: %w", sb.Namespace, parent.Name, err)
	}
	children := make([]v1alpha2.Sandbox, 0, len(siblings.Items))
	for i := range siblings.Items {
		c := siblings.Items[i]
		if c.Spec.Source.FromSandbox != nil && c.Spec.Source.FromSandbox.Name == parent.Name {
			children = append(children, c)
		}
	}
	sort.SliceStable(children, func(i, j int) bool {
		ti, tj := children[i].CreationTimestamp, children[j].CreationTimestamp
		if ti.Equal(&tj) {
			return children[i].Name < children[j].Name
		}
		return ti.Before(&tj)
	})

	// Walk in rank order. priorForks accumulates the replicas of strictly-earlier
	// children; sb is over budget when its prior allocation already meets the
	// limit. totalAdmitted is the cumulative replicas admitted (capped at limit),
	// the value mirrored into P.status.budgetSpend.forks.
	var totalAdmitted int32
	var sbPriorForks int32
	var sbOverBudget bool
	for i := range children {
		c := &children[i]
		prior := totalAdmitted
		if c.UID == sb.UID {
			sbPriorForks = prior
			sbOverBudget = prior >= limit
		}
		if prior < limit {
			room := limit - prior
			reps := effectiveReplicas(c)
			if reps > room {
				reps = room
			}
			totalAdmitted += reps
		}
	}

	// Best-effort record P.status.budgetSpend.forks (idempotent). A conflict is
	// not fatal: the next reconcile of any child re-derives and re-records it.
	if err := r.recordParentForkSpend(ctx, &parent, totalAdmitted); err != nil && !apierrors.IsConflict(err) {
		return false, "", "", err
	}

	if sbOverBudget {
		base := apierr.Get(apierr.CodeBudgetExhausted)
		message := fmt.Sprintf(
			"%s The forks budget dimension is exhausted: the limit is %d and 0 remain (%d forks of %q were already admitted ahead of this one). %s",
			base.Message, limit, sbPriorForks, parent.Name, base.Remediation,
		)
		return false, "BudgetExhausted", message, nil
	}
	return true, "", "", nil
}

// recordParentForkSpend writes parent.status.budgetSpend.forks = forks
// idempotently onto the source Sandbox status subresource. It returns nil when
// the value already matches so a steady state does not write.
func (r *SandboxReconciler) recordParentForkSpend(ctx context.Context, parent *v1alpha2.Sandbox, forks int32) error {
	if parent.Status.BudgetSpend != nil && parent.Status.BudgetSpend.Forks == forks {
		return nil
	}
	if parent.Status.BudgetSpend == nil {
		parent.Status.BudgetSpend = &v1alpha2.SandboxBudgetSpend{}
	}
	parent.Status.BudgetSpend.Forks = forks
	if err := r.Status().Update(ctx, parent); err != nil {
		return fmt.Errorf("record budgetSpend.forks on sandbox %s/%s: %w", parent.Namespace, parent.Name, err)
	}
	return nil
}

// ensureClaim get-or-creates the SandboxClaim owned by a poolRef Sandbox,
// translating the carried-across fields (env, secrets, volumeOverrides,
// workspaceRef, serviceAccount, nodeName, lifetime) per the migration table.
func (r *SandboxReconciler) ensureClaim(ctx context.Context, sb *v1alpha2.Sandbox) (*v1alpha1.SandboxClaim, error) {
	claim := &v1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{Name: childClaimName(sb), Namespace: sb.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, claim, func() error {
		claim.Spec.PoolRef = v1alpha1.LocalObjectReference{Name: sb.Spec.Source.PoolRef.Name}
		claim.Spec.Env = sb.Spec.Env
		claim.Spec.Secrets = sb.Spec.Secrets
		claim.Spec.VolumeOverrides = sb.Spec.VolumeOverrides
		claim.Spec.WorkspaceRef = sb.Spec.WorkspaceRef
		claim.Spec.ServiceAccount = sb.Spec.ServiceAccount
		claim.Spec.NodeName = sb.Spec.NodeName
		applySandboxLifetime(claim, sb.Spec.Lifetime)
		return controllerutil.SetControllerReference(sb, claim, r.Scheme)
	})
	if err != nil {
		return nil, fmt.Errorf("ensure SandboxClaim for sandbox %s/%s: %w", sb.Namespace, sb.Name, err)
	}
	return claim, nil
}

// ensureFork get-or-creates the SandboxFork owned by a fromSandbox Sandbox. The
// fan-out replicas and the secretInheritance mode (inverted back to the
// allowSecretInheritance boolean per the migration table) carry across.
func (r *SandboxReconciler) ensureFork(ctx context.Context, sb *v1alpha2.Sandbox) (*v1alpha1.SandboxFork, error) {
	fork := &v1alpha1.SandboxFork{
		ObjectMeta: metav1.ObjectMeta{Name: childForkName(sb), Namespace: sb.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, fork, func() error {
		fork.Spec.SourceRef = v1alpha1.LocalObjectReference{Name: sb.Spec.Source.FromSandbox.Name}
		fork.Spec.Replicas = sb.Spec.Replicas
		fork.Spec.VolumeOverrides = sb.Spec.VolumeOverrides
		fork.Spec.PauseSource = sb.Spec.Source.FromSandbox.PauseSource
		// secretInheritance: inherit -> allowSecretInheritance true; reissue (the
		// default) -> false (each fork gets fresh credentials).
		fork.Spec.AllowSecretInheritance = sb.Spec.SecretInheritance == v1alpha2.SecretInherit
		return controllerutil.SetControllerReference(sb, fork, r.Scheme)
	})
	if err != nil {
		return nil, fmt.Errorf("ensure SandboxFork for sandbox %s/%s: %w", sb.Namespace, sb.Name, err)
	}
	return fork, nil
}

// applySandboxLifetime maps the v2 lifetime block onto the v1 SandboxClaim
// lifetime fields per the migration table.
func applySandboxLifetime(claim *v1alpha1.SandboxClaim, lt *v1alpha2.SandboxLifetime) {
	if lt == nil {
		return
	}
	claim.Spec.Timeout = lt.TTL
	claim.Spec.IdleTimeout = lt.IdleTimeout
	claim.Spec.TTLSecondsAfterFinished = lt.TTLSecondsAfterFinished
	if ot := lt.OnTerminate; ot != nil {
		claim.Spec.Outputs = ot.Outputs
		// A non-empty snapshot retention directive generalizes the boolean
		// checkpoint-on-terminate.
		claim.Spec.CheckpointOnTerminate = ot.Snapshot != ""
	}
}

// sandboxMirror is the set of v2 Sandbox status facts mirrored in one reconcile.
type sandboxMirror struct {
	reason            string
	message           string
	endpoint          string
	pod               string
	sandboxID         string
	startupLatencyMs  int64
	startedAt         *metav1.Time
	finishedAt        *metav1.Time
	revision          string
	readyReplicas     int32
	children          []v1alpha2.SandboxChild
	forkSnapshotTaken bool
	checkpointTime    *metav1.Time
}

// mirror writes one sandboxMirror onto the Sandbox status subresource,
// idempotently (no write when nothing changed). It always sets the phase, the
// Ready condition (True only on the Ready phase), and observedGeneration.
func (r *SandboxReconciler) mirror(ctx context.Context, sb *v1alpha2.Sandbox, phase v1alpha1.SandboxPhase, m sandboxMirror) error {
	before := sb.DeepCopy()

	sb.Status.Phase = phase
	sb.Status.Endpoint = m.endpoint
	sb.Status.Pod = m.pod
	sb.Status.SandboxID = m.sandboxID
	sb.Status.StartupLatencyMs = m.startupLatencyMs
	if m.startedAt != nil {
		sb.Status.StartedAt = m.startedAt
	}
	if m.finishedAt != nil {
		sb.Status.FinishedAt = m.finishedAt
	}
	if m.revision != "" {
		sb.Status.Revision = m.revision
	}
	sb.Status.ReadyReplicas = m.readyReplicas
	sb.Status.Children = m.children
	sb.Status.ForkSnapshotTaken = m.forkSnapshotTaken
	if m.checkpointTime != nil {
		sb.Status.CheckpointTime = m.checkpointTime
	}

	apimeta.SetStatusCondition(&sb.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             conditionStatus(phase == v1alpha1.SandboxReady),
		Reason:             m.reason,
		Message:            m.message,
		ObservedGeneration: sb.Generation,
	})

	if equalSandboxStatus(&before.Status, &sb.Status) {
		return nil
	}
	if err := r.Status().Update(ctx, sb); err != nil {
		return fmt.Errorf("mirror status into sandbox %s/%s: %w", sb.Namespace, sb.Name, err)
	}
	return nil
}

// equalSandboxStatus reports whether two statuses are equal for the elision of a
// no-op status write. It compares the phase, the mirrored observables, and the
// Ready condition's status/reason (ignoring the condition's transition time so
// an unchanged condition does not force a write).
func equalSandboxStatus(a, b *v1alpha2.SandboxStatus) bool {
	if a.Phase != b.Phase || a.Endpoint != b.Endpoint || a.Pod != b.Pod ||
		a.SandboxID != b.SandboxID || a.StartupLatencyMs != b.StartupLatencyMs ||
		a.ReadyReplicas != b.ReadyReplicas || a.Revision != b.Revision ||
		a.ForkSnapshotTaken != b.ForkSnapshotTaken || len(a.Children) != len(b.Children) {
		return false
	}
	for i := range a.Children {
		if a.Children[i] != b.Children[i] {
			return false
		}
	}
	ca := apimeta.FindStatusCondition(a.Conditions, "Ready")
	cb := apimeta.FindStatusCondition(b.Conditions, "Ready")
	if (ca == nil) != (cb == nil) {
		return false
	}
	if ca != nil && (ca.Status != cb.Status || ca.Reason != cb.Reason || ca.Message != cb.Message) {
		return false
	}
	return true
}

// mapPhase maps a v1 claim phase onto a v2 Sandbox phase per the migration table
// (Restoring -> Hydrating; everything else carries across by value). An empty
// phase maps to Pending.
func mapPhase(p v1alpha1.SandboxPhase) v1alpha1.SandboxPhase {
	switch p {
	case v1alpha1.SandboxRestoring:
		// The v2 phase name for Restoring is Hydrating; the shared SandboxPhase
		// type does not yet carry the v2-only constants (they are added in the
		// continuation), so Restoring is carried across unchanged here and the
		// rename lands with the status-convention re-homing.
		return v1alpha1.SandboxRestoring
	case "":
		return v1alpha1.SandboxPending
	default:
		return p
	}
}

// nonEmptyPhase returns the phase string or "Pending" when empty, for a stable
// condition reason.
func nonEmptyPhase(p v1alpha1.SandboxPhase) string {
	if p == "" {
		return string(v1alpha1.SandboxPending)
	}
	return string(p)
}

// SetupWithManager wires the reconciler to watch v1alpha2 Sandboxes and own the
// SandboxClaim / SandboxFork it maps each onto, so a child status change
// re-queues the Sandbox.
func (r *SandboxReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha2.Sandbox{}).
		Owns(&v1alpha1.SandboxClaim{}).
		Owns(&v1alpha1.SandboxFork{}).
		Complete(r)
}
