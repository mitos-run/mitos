package v1alpha2

import (
	"fmt"

	v1alpha1 "mitos.run/mitos/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/conversion"
)

// SandboxPool conversion (ADR 0007, issue #23, docs/api/v2-migration.md).
//
// v1alpha1 SandboxPool is the Hub (the storage version). The conversion
// functions are PURE: they read no API objects. That constraint shapes the
// template axis: the v1alpha1 model splits the template body into a separate
// SandboxTemplate object referenced by templateRef, while v1alpha2 inlines it.
// A pure conversion cannot fetch the separate SandboxTemplate to inline it, nor
// materialize a separate SandboxTemplate from an inline body. So:
//
//   - ConvertFrom (v1 -> v2): a v1 templateRef maps to a v2 templateRef (the
//     inline template stays nil; the operator/storage migration is what inlines
//     the referenced SandboxTemplate body, see the migration notes).
//   - ConvertTo (v2 -> v1): a v2 templateRef maps to a v1 templateRef
//     unchanged; a v2 INLINE template maps to a v1 templateRef whose name is
//     derived from the pool (the documented default: the storage migration
//     materializes the inline body into a SandboxTemplate named for the pool).
//
// Every pool-LEVEL field (warm/snapshots/drainPolicy/placement/cpuPinning) is
// round-trippable and is preserved exactly; only the template-body inlining is
// the staged storage-migration step. No pool-level v1 value is lost.

var _ conversion.Convertible = &SandboxPool{}

// inlineTemplateRefSuffix names the SandboxTemplate a v2 inline pool template is
// materialized into by the storage migration. The conversion derives the name
// deterministically from the pool so a re-conversion is stable.
const inlineTemplateRefSuffix = "-template"

// ConvertTo converts this v1alpha2 SandboxPool to the v1alpha1 Hub.
func (src *SandboxPool) ConvertTo(dstRaw conversion.Hub) error {
	dst, ok := dstRaw.(*v1alpha1.SandboxPool)
	if !ok {
		return fmt.Errorf("ConvertTo: expected *v1alpha1.SandboxPool, got %T", dstRaw)
	}

	dst.ObjectMeta = src.ObjectMeta
	dst.Status = src.Status

	// Template axis: a v2 templateRef carries straight across; a v2 inline
	// template maps to a synthetic, pool-derived templateRef (the storage
	// migration materializes the inline body into that SandboxTemplate).
	switch {
	case src.Spec.TemplateRef != nil:
		dst.Spec.TemplateRef = *src.Spec.TemplateRef
	case src.Spec.Template != nil:
		dst.Spec.TemplateRef = v1alpha1.LocalObjectReference{Name: src.Name + inlineTemplateRefSuffix}
	default:
		dst.Spec.TemplateRef = v1alpha1.LocalObjectReference{}
	}

	// Snapshots axis: unfold the v2 snapshots block back into the v1 flat fields.
	if s := src.Spec.Snapshots; s != nil {
		dst.Spec.Replicas = s.ReplicasPerNode
		dst.Spec.SnapshotAfter = s.SnapshotAfter
		dst.Spec.SnapshotDelay = s.SnapshotDelay
		dst.Spec.ScaleDownAfterSnapshot = s.ScaleDownAfterSnapshot
		dst.Spec.SnapshotStorage = s.Storage
	}

	// Warm axis: warm.min carries the fixed replicas for back-compat. When the v2
	// warm block describes an autoscaler (a non-zero max), unfold it into the v1
	// autoscale block; otherwise it is just the fixed replicas floor and no
	// autoscale is fabricated (the documented fixed-pool shape).
	if w := src.Spec.Warm; w != nil {
		if dst.Spec.Replicas == 0 {
			dst.Spec.Replicas = w.Min
		}
		if w.Max > 0 {
			dst.Spec.Autoscale = &v1alpha1.PoolAutoscaleSpec{
				MinWarm:                  w.Min,
				MaxWarm:                  w.Max,
				TargetSpare:              w.TargetPending,
				ScaleDownCooldownSeconds: w.CooldownSeconds,
			}
		}
	}

	dst.Spec.DrainPolicy = src.Spec.DrainPolicy
	dst.Spec.Placement = src.Spec.Placement
	dst.Spec.CPUPinning = src.Spec.CPUPinning
	return nil
}

// ConvertFrom converts the v1alpha1 Hub into this v1alpha2 SandboxPool.
func (dst *SandboxPool) ConvertFrom(srcRaw conversion.Hub) error {
	src, ok := srcRaw.(*v1alpha1.SandboxPool)
	if !ok {
		return fmt.Errorf("ConvertFrom: expected *v1alpha1.SandboxPool, got %T", srcRaw)
	}

	dst.ObjectMeta = src.ObjectMeta
	dst.Status = src.Status

	// Template axis: a pure conversion cannot inline the separate SandboxTemplate
	// body, so the v1 templateRef maps to a v2 templateRef (the inline template
	// stays nil; the storage migration inlines the referenced body).
	ref := src.Spec.TemplateRef
	dst.Spec.TemplateRef = &v1alpha1.LocalObjectReference{Name: ref.Name}
	dst.Spec.Template = nil

	// Snapshots axis: fold the v1 flat snapshot fields into the v2 block. Always
	// build the block so the per-node replica count (v1 replicas) is carried.
	dst.Spec.Snapshots = &PoolSnapshots{
		ReplicasPerNode:        src.Spec.Replicas,
		SnapshotAfter:          src.Spec.SnapshotAfter,
		SnapshotDelay:          src.Spec.SnapshotDelay,
		ScaleDownAfterSnapshot: src.Spec.ScaleDownAfterSnapshot,
		Storage:                src.Spec.SnapshotStorage,
	}

	// Warm axis: a v1 autoscale block maps onto warm; otherwise the fixed
	// replicas maps onto warm.min (the back-compat mapping).
	if a := src.Spec.Autoscale; a != nil {
		dst.Spec.Warm = &PoolWarm{
			Min:             a.MinWarm,
			Max:             a.MaxWarm,
			TargetPending:   a.TargetSpare,
			CooldownSeconds: a.ScaleDownCooldownSeconds,
		}
	} else {
		dst.Spec.Warm = &PoolWarm{Min: src.Spec.Replicas}
	}

	dst.Spec.DrainPolicy = src.Spec.DrainPolicy
	dst.Spec.Placement = src.Spec.Placement
	dst.Spec.CPUPinning = src.Spec.CPUPinning
	return nil
}
