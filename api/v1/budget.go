package v1

// Never-widen budget primitives for capability attenuation (issue #25). These
// mirror the internal/captoken Budget semantics (Remaining, Intersect) on the
// CRD-facing SandboxBudget type, where a nil pointer on any of the five
// dimensions means UNLIMITED (unset). The load-bearing property is that
// attenuation can never widen: a child's effective budget, derived as
// parentRemaining.Intersect(childRequest), is element-wise no greater than the
// parent's remaining budget on every dimension.
//
// Pointer/unlimited semantics, applied uniformly to all five dimensions:
//
//   - nil = unlimited. In Intersect, unlimited INTERSECT x = x (nil yields the
//     other operand; both nil stays nil). In Remaining, an unlimited dimension
//     stays unlimited regardless of spend (spend cannot make an unlimited
//     dimension finite).
//   - a set (non-nil) dimension is a finite ceiling. Intersect takes the
//     element-wise minimum of two set dimensions; Remaining subtracts the
//     recorded spend from a set dimension, floored at zero.
//
// Spend coverage: SandboxBudgetSpend records only Forks and CpuSeconds today, so
// Remaining only subtracts from MaxForks and MaxCpuSeconds; the three dimensions
// with no recorded spend (MaxCheckpoints, MaxLifetimeExtension, MaxEgressBytes)
// pass through unchanged. When those dimensions gain spend accounting, this is
// the single place that extends. The result of either method is always a fresh
// SandboxBudget (deep-copied pointers) so callers never alias the inputs.

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Remaining returns the budget left after subtracting spend, floored at zero on
// every set dimension. An unlimited (nil) dimension stays unlimited. A nil
// receiver (unlimited budget) returns nil. The result is the upper bound on what
// this sandbox may delegate to a self-initiated fork-child.
func (b *SandboxBudget) Remaining(spend SandboxBudgetSpend) *SandboxBudget {
	if b == nil {
		return nil
	}
	return &SandboxBudget{
		MaxForks:             subFloor32(b.MaxForks, spend.Forks),
		MaxCheckpoints:       copyInt32(b.MaxCheckpoints),
		MaxCpuSeconds:        subFloor64(b.MaxCpuSeconds, spend.CpuSeconds),
		MaxLifetimeExtension: copyDuration(b.MaxLifetimeExtension),
		MaxEgressBytes:       copyQuantity(b.MaxEgressBytes),
	}
}

// Intersect returns the element-wise minimum of two budgets with nil = unlimited
// on every dimension (unlimited INTERSECT x = x). The result is element-wise no
// greater than either operand on any set dimension, which is the never-widen
// guarantee: a child request can never widen the parent's remaining budget. A
// nil receiver returns a copy of other; a nil other returns a copy of the
// receiver; both nil returns nil.
func (b *SandboxBudget) Intersect(other *SandboxBudget) *SandboxBudget {
	if b == nil {
		return other.DeepCopy()
	}
	if other == nil {
		return b.DeepCopy()
	}
	return &SandboxBudget{
		MaxForks:             minInt32(b.MaxForks, other.MaxForks),
		MaxCheckpoints:       minInt32(b.MaxCheckpoints, other.MaxCheckpoints),
		MaxCpuSeconds:        minInt64(b.MaxCpuSeconds, other.MaxCpuSeconds),
		MaxLifetimeExtension: minDuration(b.MaxLifetimeExtension, other.MaxLifetimeExtension),
		MaxEgressBytes:       minQuantity(b.MaxEgressBytes, other.MaxEgressBytes),
	}
}

// subFloor32 subtracts spend from a set ceiling, floored at zero. A nil ceiling
// (unlimited) is returned unchanged: spend cannot make an unlimited dimension
// finite.
func subFloor32(ceiling *int32, spend int32) *int32 {
	if ceiling == nil {
		return nil
	}
	v := *ceiling - spend
	if v < 0 {
		v = 0
	}
	return &v
}

func subFloor64(ceiling *int64, spend int64) *int64 {
	if ceiling == nil {
		return nil
	}
	v := *ceiling - spend
	if v < 0 {
		v = 0
	}
	return &v
}

// minInt32 returns the element-wise minimum with nil = unlimited: nil yields the
// other operand, both nil yields nil.
func minInt32(a, b *int32) *int32 {
	if a == nil {
		return copyInt32(b)
	}
	if b == nil {
		return copyInt32(a)
	}
	v := *a
	if *b < v {
		v = *b
	}
	return &v
}

func minInt64(a, b *int64) *int64 {
	if a == nil {
		return copyInt64(b)
	}
	if b == nil {
		return copyInt64(a)
	}
	v := *a
	if *b < v {
		v = *b
	}
	return &v
}

func minDuration(a, b *metav1.Duration) *metav1.Duration {
	if a == nil {
		return copyDuration(b)
	}
	if b == nil {
		return copyDuration(a)
	}
	v := *a
	if b.Duration < v.Duration {
		v = *b
	}
	return &v
}

func minQuantity(a, b *resource.Quantity) *resource.Quantity {
	if a == nil {
		return copyQuantity(b)
	}
	if b == nil {
		return copyQuantity(a)
	}
	if b.Cmp(*a) < 0 {
		return copyQuantity(b)
	}
	return copyQuantity(a)
}

func copyInt32(p *int32) *int32 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func copyInt64(p *int64) *int64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func copyDuration(p *metav1.Duration) *metav1.Duration {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func copyQuantity(p *resource.Quantity) *resource.Quantity {
	if p == nil {
		return nil
	}
	v := p.DeepCopy()
	return &v
}
