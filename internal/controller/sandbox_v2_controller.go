package controller

import (
	"context"
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/apierr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Reconcile drives the v1 Sandbox lifecycle by dispatching on its source. The
// SandboxReconciler OWNS the engine directly (no intermediate SandboxClaim or
// SandboxFork object): source.poolRef drives the claim engine
// (reconcilePoolRef), source.fromSandbox drives the fork engine
// (reconcileFromSandbox, gated on the never-widen fork budget), and
// source.fromRevision reports a clear not-served condition (the lineage-resume
// engine path is the continuation, never silently dropped).
//
// +kubebuilder:rbac:groups=mitos.run,resources=sandboxes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mitos.run,resources=sandboxes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mitos.run,resources=sandboxes/finalizers,verbs=update
func (r *SandboxReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var sb v1.Sandbox
	if err := r.Get(ctx, req.NamespacedName, &sb); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Emit a sandbox.phase.changed feed event for the NET phase transition this
	// reconcile persisted. The phase observed at entry is compared against the
	// phase persisted in the API after the reconcile, so the event fires only when
	// the change actually landed. The uncached APIReader is read post-reconcile so
	// the just-persisted phase is observed (the cache lags the apiserver write).
	entryPhase := sb.Status.Phase
	defer func() {
		reader := r.APIReader
		if reader == nil {
			reader = r.Client
		}
		var fresh v1.Sandbox
		if err := reader.Get(ctx, req.NamespacedName, &fresh); err != nil {
			return
		}
		if fresh.Status.Phase != "" && fresh.Status.Phase != entryPhase {
			r.Feed.emitPhaseChanged(ctx, &fresh, entryPhase, fresh.Status.Phase)
		}
	}()

	// A Sandbox under deletion: the poolRef engine reaps its backing VM via the
	// terminate finalizer; the fromSandbox engine reaps its fork snapshot + child
	// pods via the husk fork finalizer (and the child pods carry owner refs).
	if !sb.DeletionTimestamp.IsZero() {
		switch {
		case sb.Spec.Source.PoolRef != nil:
			return r.reconcileDelete(ctx, &sb)
		case sb.Spec.Source.FromSandbox != nil:
			return r.reconcileFromSandbox(ctx, &sb)
		default:
			return ctrl.Result{}, nil
		}
	}

	switch {
	case sb.Spec.Source.PoolRef != nil:
		return r.reconcilePoolRef(ctx, &sb)
	case sb.Spec.Source.FromSandbox != nil:
		// Capability-budget enforcement (issue #25): a self-initiated fork is
		// admitted only while the source's effective-remaining fork budget has room.
		// An over-budget fork is rejected terminally with a typed BudgetExhausted
		// condition BEFORE the fork is materialized.
		decision, err := r.enforceForkBudget(ctx, &sb)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !decision.admitted {
			return ctrl.Result{}, r.mirrorBudgetRejection(ctx, &sb, decision)
		}
		// Record the child's attenuated effective budget (status only) so the NEXT
		// level attenuates against it; this is what makes the bound depth-aggregate.
		if decision.childEffective != nil {
			if err := r.recordChildEffectiveBudget(ctx, &sb, decision.childEffective); err != nil {
				return ctrl.Result{}, err
			}
		}
		return r.reconcileFromSandbox(ctx, &sb)
	case sb.Spec.Source.FromRevision != nil:
		// NEW v2 surface; the lineage-resume engine path is the continuation. Be
		// honest: report a clear not-served condition rather than silently doing
		// nothing or panicking.
		return ctrl.Result{}, r.reportFromRevisionNotServed(ctx, &sb)
	default:
		logger.Info("sandbox has no source set", "sandbox", sb.Name)
		return ctrl.Result{}, r.reportNoSource(ctx, &sb)
	}
}

// effectiveReplicas treats a zero (unset) replicas as one, matching the
// kubebuilder default so an unset replicas counts as a single sandbox.
func effectiveReplicas(sb *v1.Sandbox) int32 {
	if sb.Spec.Replicas <= 0 {
		return 1
	}
	return sb.Spec.Replicas
}

// reportFromRevisionNotServed sets phase Pending and a Ready=False condition with
// reason RevisionResumeNotImplemented (docs/conditions.md), never silently
// dropping a fromRevision Sandbox.
func (r *SandboxReconciler) reportFromRevisionNotServed(ctx context.Context, sb *v1.Sandbox) error {
	src := sb.Spec.Source.FromRevision
	before := sb.DeepCopy()
	sb.Status.Phase = v1.SandboxPending
	apimeta.SetStatusCondition(&sb.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "RevisionResumeNotImplemented",
		Message:            fmt.Sprintf("lineage resume from a workspace revision is declared in v1 but not yet served; tracked as the fromRevision engine path (workspace %q revision %q)", src.Workspace, src.Revision),
		ObservedGeneration: sb.Generation,
	})
	if before.Status.Phase == sb.Status.Phase &&
		conditionEqual(before.Status.Conditions, sb.Status.Conditions, "Ready") {
		return nil
	}
	if err := r.Status().Update(ctx, sb); err != nil {
		return fmt.Errorf("report fromRevision not served on sandbox %s/%s: %w", sb.Namespace, sb.Name, err)
	}
	return nil
}

// reportNoSource fails a Sandbox with no source set, surfacing it rather than
// crash-looping (validation should forbid this shape).
func (r *SandboxReconciler) reportNoSource(ctx context.Context, sb *v1.Sandbox) error {
	sb.Status.Phase = v1.SandboxFailed
	apimeta.SetStatusCondition(&sb.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "NoSource",
		Message:            "spec.source must set exactly one of poolRef, fromSandbox, or fromRevision",
		ObservedGeneration: sb.Generation,
	})
	if err := r.Status().Update(ctx, sb); err != nil {
		return fmt.Errorf("report no source on sandbox %s/%s: %w", sb.Namespace, sb.Name, err)
	}
	return nil
}

// conditionEqual reports whether the named condition is field-equal (status,
// reason, message, observedGeneration) between two condition slices.
func conditionEqual(a, b []metav1.Condition, t string) bool {
	ca := apimeta.FindStatusCondition(a, t)
	cb := apimeta.FindStatusCondition(b, t)
	if ca == nil || cb == nil {
		return ca == cb
	}
	return ca.Status == cb.Status && ca.Reason == cb.Reason && ca.Message == cb.Message &&
		ca.ObservedGeneration == cb.ObservedGeneration
}

// mirrorBudgetRejection records a terminal Failed phase with the BudgetExhausted
// reason for a fork the source's budget forbids, plus the child's (zero-room)
// effective budget so the status stays honest.
func (r *SandboxReconciler) mirrorBudgetRejection(ctx context.Context, sb *v1.Sandbox, d forkBudgetDecision) error {
	sb.Status.Phase = v1.SandboxFailed
	if d.childEffective != nil {
		sb.Status.EffectiveBudget = d.childEffective
	}
	apimeta.SetStatusCondition(&sb.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             d.reason,
		Message:            d.message,
		ObservedGeneration: sb.Generation,
	})
	if err := r.Status().Update(ctx, sb); err != nil {
		return fmt.Errorf("mirror budget rejection on sandbox %s/%s: %w", sb.Namespace, sb.Name, err)
	}
	return nil
}

// recordChildEffectiveBudget records the controller-computed attenuated budget
// (status only, never the user-owned spec) for a fork child, so the next level
// attenuates against it (the depth-aggregate never-widen invariant, issue #25).
func (r *SandboxReconciler) recordChildEffectiveBudget(ctx context.Context, sb *v1.Sandbox, eff *v1.SandboxBudget) error {
	if equalBudget(sb.Status.EffectiveBudget, eff) {
		return nil
	}
	sb.Status.EffectiveBudget = eff
	if err := r.Status().Update(ctx, sb); err != nil {
		return fmt.Errorf("record effective budget on sandbox %s/%s: %w", sb.Namespace, sb.Name, err)
	}
	return nil
}

// forkBudgetDecision is the outcome of the capability-budget gate for one
// self-initiated fork. childEffective is the attenuated effective budget the
// fork-child would hold (recorded in its status so the next level attenuates
// against it); it is set even on a rejection so the status stays honest.
type forkBudgetDecision struct {
	admitted       bool
	reason         string
	message        string
	childEffective *v1.SandboxBudget
}

// enforceForkBudget enforces the source Sandbox P's capability budget for a
// self-initiated fork (sb.Spec.Source.FromSandbox = P), issue #25. The bound is
// depth-aggregate: it enforces against P's EFFECTIVE-REMAINING budget, not P's
// raw spec budget. Because every level was intersected with its parent's
// remaining, a grandchild is transitively bounded by the root: a fork-of-a-fork
// cannot widen past what the root has left.
func (r *SandboxReconciler) enforceForkBudget(ctx context.Context, sb *v1.Sandbox) (forkBudgetDecision, error) {
	src := sb.Spec.Source.FromSandbox
	if src == nil {
		return forkBudgetDecision{admitted: true}, nil
	}

	var parent v1.Sandbox
	if err := r.Get(ctx, client.ObjectKey{Name: src.Name, Namespace: sb.Namespace}, &parent); err != nil {
		if apierrors.IsNotFound(err) {
			// No source Sandbox to enforce a budget against; let the normal flow
			// proceed (the fork engine surfaces a missing-source condition). The child
			// holds at most its own requested budget.
			return forkBudgetDecision{admitted: true, childEffective: sb.Spec.Budget.DeepCopy()}, nil
		}
		return forkBudgetDecision{}, fmt.Errorf("get source sandbox %s/%s for budget: %w", sb.Namespace, src.Name, err)
	}

	// P's effective budget: P's status.effectiveBudget (already attenuated against
	// P's own parent, so this is the depth-aggregate quantity), falling back to
	// P.spec.budget for a root.
	parentEffective := parent.Status.EffectiveBudget
	if parentEffective == nil {
		parentEffective = parent.Spec.Budget
	}

	if parentEffective == nil || parentEffective.MaxForks == nil {
		// Unlimited fork budget on the forks dimension; admit. The child still
		// attenuates its OTHER dimensions against the parent's effective budget so
		// the next level is bounded (unlimited INTERSECT x = x).
		childEffective := parentEffective.Intersect(sb.Spec.Budget)
		return forkBudgetDecision{admitted: true, childEffective: childEffective}, nil
	}
	limit := *parentEffective.MaxForks

	// All fork-children of P in the namespace (including sb), ranked
	// deterministically by (creationTimestamp, name).
	var siblings v1.SandboxList
	if err := r.List(ctx, &siblings, client.InNamespace(sb.Namespace)); err != nil {
		return forkBudgetDecision{}, fmt.Errorf("list fork-children of %s/%s for budget: %w", sb.Namespace, parent.Name, err)
	}
	children := make([]v1.Sandbox, 0, len(siblings.Items))
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

	// Best-effort record P.status.budgetSpend.forks (idempotent). A conflict is not
	// fatal: the next reconcile of any child re-derives and re-records it.
	if err := r.recordParentForkSpend(ctx, &parent, totalAdmitted); err != nil && !apierrors.IsConflict(err) {
		return forkBudgetDecision{}, err
	}

	parentRemaining := parentEffective.Remaining(v1.SandboxBudgetSpend{Forks: totalAdmitted})
	childEffective := parentRemaining.Intersect(sb.Spec.Budget)

	if sbOverBudget {
		base := apierr.Get(apierr.CodeBudgetExhausted)
		message := fmt.Sprintf(
			"%s The forks budget dimension is exhausted: %q's effective fork budget is %d and 0 remain (%d forks were already admitted ahead of this one). The bound is depth-aggregate, so the whole fork subtree is limited by the root. %s",
			base.Message, parent.Name, limit, sbPriorForks, base.Remediation,
		)
		return forkBudgetDecision{admitted: false, reason: "BudgetExhausted", message: message, childEffective: childEffective}, nil
	}
	return forkBudgetDecision{admitted: true, childEffective: childEffective}, nil
}

// recordParentForkSpend writes parent.status.budgetSpend.forks = forks
// idempotently onto the source Sandbox status subresource.
func (r *SandboxReconciler) recordParentForkSpend(ctx context.Context, parent *v1.Sandbox, forks int32) error {
	if parent.Status.BudgetSpend != nil && parent.Status.BudgetSpend.Forks == forks {
		return nil
	}
	if parent.Status.BudgetSpend == nil {
		parent.Status.BudgetSpend = &v1.SandboxBudgetSpend{}
	}
	parent.Status.BudgetSpend.Forks = forks
	if err := r.Status().Update(ctx, parent); err != nil {
		return fmt.Errorf("record budgetSpend.forks on sandbox %s/%s: %w", parent.Namespace, parent.Name, err)
	}
	return nil
}

// equalBudget reports whether two capability budgets are field-equal, treating a
// nil pointer as unlimited on every dimension, so a no-op status write is elided.
func equalBudget(a, b *v1.SandboxBudget) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return eqInt32Ptr(a.MaxForks, b.MaxForks) &&
		eqInt32Ptr(a.MaxCheckpoints, b.MaxCheckpoints) &&
		eqInt64Ptr(a.MaxCpuSeconds, b.MaxCpuSeconds) &&
		eqDurationPtr(a.MaxLifetimeExtension, b.MaxLifetimeExtension) &&
		eqQuantityPtr(a.MaxEgressBytes, b.MaxEgressBytes)
}

func eqInt32Ptr(a, b *int32) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func eqInt64Ptr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func eqDurationPtr(a, b *metav1.Duration) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Duration == b.Duration
}

func eqQuantityPtr(a, b *resource.Quantity) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Cmp(*b) == 0
}
