package v1

// Unit tests for the never-widen budget primitives (issue #25): SandboxBudget
// Remaining and Intersect. These are the load-bearing capability-attenuation
// arithmetic; the *pointer = unlimited* semantics and the subtract-with-floor on
// spend are tested hard here because they are the security guarantee that a
// child's effective budget can never widen beyond its parent's remaining.

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func i32(v int32) *int32 { return &v }
func i64(v int64) *int64 { return &v }
func dur(d metav1.Duration) *metav1.Duration {
	return &d
}
func qty(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

// eqInt32Ptr compares two *int32 treating nil as unlimited (a distinct value
// from any set value).
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

func eqDurPtr(a, b *metav1.Duration) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Duration == b.Duration
}

func eqQtyPtr(a, b *resource.Quantity) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Cmp(*b) == 0
}

func eqBudget(t *testing.T, got, want *SandboxBudget) {
	t.Helper()
	if got == nil || want == nil {
		if got != want {
			t.Fatalf("budget nil mismatch: got %v want %v", got, want)
		}
		return
	}
	if !eqInt32Ptr(got.MaxForks, want.MaxForks) {
		t.Errorf("MaxForks: got %v want %v", deref32(got.MaxForks), deref32(want.MaxForks))
	}
	if !eqInt32Ptr(got.MaxCheckpoints, want.MaxCheckpoints) {
		t.Errorf("MaxCheckpoints: got %v want %v", deref32(got.MaxCheckpoints), deref32(want.MaxCheckpoints))
	}
	if !eqInt64Ptr(got.MaxCpuSeconds, want.MaxCpuSeconds) {
		t.Errorf("MaxCpuSeconds: got %v want %v", deref64(got.MaxCpuSeconds), deref64(want.MaxCpuSeconds))
	}
	if !eqDurPtr(got.MaxLifetimeExtension, want.MaxLifetimeExtension) {
		t.Errorf("MaxLifetimeExtension: got %v want %v", got.MaxLifetimeExtension, want.MaxLifetimeExtension)
	}
	if !eqQtyPtr(got.MaxEgressBytes, want.MaxEgressBytes) {
		t.Errorf("MaxEgressBytes: got %v want %v", got.MaxEgressBytes, want.MaxEgressBytes)
	}
}

func deref32(p *int32) any {
	if p == nil {
		return "nil"
	}
	return *p
}
func deref64(p *int64) any {
	if p == nil {
		return "nil"
	}
	return *p
}

// TestSandboxBudgetRemaining covers the subtract-with-floor on the two spend-
// tracked dimensions (forks, cpuSeconds), the floor at zero, the pass-through of
// the three dimensions with no recorded spend, and the unlimited (nil) handling.
func TestSandboxBudgetRemaining(t *testing.T) {
	t.Run("nil receiver stays nil", func(t *testing.T) {
		var b *SandboxBudget
		if got := b.Remaining(SandboxBudgetSpend{Forks: 3}); got != nil {
			t.Fatalf("nil budget Remaining = %v, want nil (unlimited)", got)
		}
	})

	t.Run("set dimensions subtract with floor", func(t *testing.T) {
		b := &SandboxBudget{MaxForks: i32(5), MaxCpuSeconds: i64(100)}
		got := b.Remaining(SandboxBudgetSpend{Forks: 2, CpuSeconds: 30})
		eqBudget(t, got, &SandboxBudget{MaxForks: i32(3), MaxCpuSeconds: i64(70)})
	})

	t.Run("floors at zero, never negative", func(t *testing.T) {
		b := &SandboxBudget{MaxForks: i32(2), MaxCpuSeconds: i64(10)}
		got := b.Remaining(SandboxBudgetSpend{Forks: 5, CpuSeconds: 1000})
		eqBudget(t, got, &SandboxBudget{MaxForks: i32(0), MaxCpuSeconds: i64(0)})
	})

	t.Run("unlimited (nil) dimension is not touched by spend", func(t *testing.T) {
		// MaxForks unset = unlimited: spend cannot make an unlimited dimension
		// finite. MaxCpuSeconds set is reduced.
		b := &SandboxBudget{MaxCpuSeconds: i64(50)}
		got := b.Remaining(SandboxBudgetSpend{Forks: 9, CpuSeconds: 20})
		eqBudget(t, got, &SandboxBudget{MaxForks: nil, MaxCpuSeconds: i64(30)})
	})

	t.Run("non-spend-tracked dimensions pass through unchanged", func(t *testing.T) {
		b := &SandboxBudget{
			MaxForks:             i32(4),
			MaxCheckpoints:       i32(7),
			MaxLifetimeExtension: dur(metav1.Duration{Duration: 60}),
			MaxEgressBytes:       qty("1Gi"),
		}
		got := b.Remaining(SandboxBudgetSpend{Forks: 1})
		eqBudget(t, got, &SandboxBudget{
			MaxForks:             i32(3),
			MaxCheckpoints:       i32(7),
			MaxLifetimeExtension: dur(metav1.Duration{Duration: 60}),
			MaxEgressBytes:       qty("1Gi"),
		})
	})
}

// TestSandboxBudgetIntersect covers per-field min with nil = unlimited so that
// unlimited INTERSECT x = x on every one of the five dimensions, and a wider
// request is clamped down to the narrower operand.
func TestSandboxBudgetIntersect(t *testing.T) {
	t.Run("nil receiver returns other", func(t *testing.T) {
		var b *SandboxBudget
		other := &SandboxBudget{MaxForks: i32(2)}
		eqBudget(t, b.Intersect(other), &SandboxBudget{MaxForks: i32(2)})
	})

	t.Run("nil other returns receiver", func(t *testing.T) {
		b := &SandboxBudget{MaxForks: i32(2)}
		eqBudget(t, b.Intersect(nil), &SandboxBudget{MaxForks: i32(2)})
	})

	t.Run("both nil stays nil (unlimited)", func(t *testing.T) {
		var b *SandboxBudget
		if got := b.Intersect(nil); got != nil {
			t.Fatalf("nil INTERSECT nil = %v, want nil", got)
		}
	})

	t.Run("per-field min picks the narrower", func(t *testing.T) {
		a := &SandboxBudget{MaxForks: i32(5), MaxCpuSeconds: i64(10)}
		c := &SandboxBudget{MaxForks: i32(3), MaxCpuSeconds: i64(99)}
		eqBudget(t, a.Intersect(c), &SandboxBudget{MaxForks: i32(3), MaxCpuSeconds: i64(10)})
	})

	t.Run("unlimited INTERSECT finite = finite on each dimension", func(t *testing.T) {
		// a has every dimension unlimited (nil); c sets each. The result must equal
		// c: unlimited INTERSECT x = x.
		a := &SandboxBudget{}
		c := &SandboxBudget{
			MaxForks:             i32(1),
			MaxCheckpoints:       i32(2),
			MaxCpuSeconds:        i64(3),
			MaxLifetimeExtension: dur(metav1.Duration{Duration: 4}),
			MaxEgressBytes:       qty("5"),
		}
		eqBudget(t, a.Intersect(c), c)
	})

	t.Run("a wider child request is clamped to the parent remaining", func(t *testing.T) {
		// Parent remaining maxForks=1; child requests a WIDER 9. The intersection
		// must be the narrower 1: the child can never widen.
		parentRemaining := &SandboxBudget{MaxForks: i32(1)}
		childRequest := &SandboxBudget{MaxForks: i32(9)}
		eqBudget(t, parentRemaining.Intersect(childRequest), &SandboxBudget{MaxForks: i32(1)})
	})

	t.Run("duration and quantity dimensions take the min", func(t *testing.T) {
		a := &SandboxBudget{
			MaxLifetimeExtension: dur(metav1.Duration{Duration: 100}),
			MaxEgressBytes:       qty("2Gi"),
		}
		c := &SandboxBudget{
			MaxLifetimeExtension: dur(metav1.Duration{Duration: 30}),
			MaxEgressBytes:       qty("1Gi"),
		}
		eqBudget(t, a.Intersect(c), &SandboxBudget{
			MaxLifetimeExtension: dur(metav1.Duration{Duration: 30}),
			MaxEgressBytes:       qty("1Gi"),
		})
	})
}

// TestSandboxBudgetNeverWiden is the cross-cutting invariant: for ANY parent
// remaining budget and ANY child request, the child's effective budget
// (parentRemaining INTERSECT childRequest) is never greater than the parent
// remaining on any of the five dimensions, even when the request is wider.
func TestSandboxBudgetNeverWiden(t *testing.T) {
	parentRemaining := &SandboxBudget{
		MaxForks:             i32(1),
		MaxCheckpoints:       i32(0),
		MaxCpuSeconds:        i64(5),
		MaxLifetimeExtension: dur(metav1.Duration{Duration: 10}),
		MaxEgressBytes:       qty("100"),
	}
	// Child requests a strictly wider budget on every dimension.
	childRequest := &SandboxBudget{
		MaxForks:             i32(100),
		MaxCheckpoints:       i32(100),
		MaxCpuSeconds:        i64(100),
		MaxLifetimeExtension: dur(metav1.Duration{Duration: 1000}),
		MaxEgressBytes:       qty("1Ti"),
	}
	eff := parentRemaining.Intersect(childRequest)

	if eff.MaxForks == nil || *eff.MaxForks > *parentRemaining.MaxForks {
		t.Errorf("MaxForks widened: %v > %v", deref32(eff.MaxForks), *parentRemaining.MaxForks)
	}
	if eff.MaxCheckpoints == nil || *eff.MaxCheckpoints > *parentRemaining.MaxCheckpoints {
		t.Errorf("MaxCheckpoints widened: %v > %v", deref32(eff.MaxCheckpoints), *parentRemaining.MaxCheckpoints)
	}
	if eff.MaxCpuSeconds == nil || *eff.MaxCpuSeconds > *parentRemaining.MaxCpuSeconds {
		t.Errorf("MaxCpuSeconds widened: %v > %v", deref64(eff.MaxCpuSeconds), *parentRemaining.MaxCpuSeconds)
	}
	if eff.MaxLifetimeExtension == nil || eff.MaxLifetimeExtension.Duration > parentRemaining.MaxLifetimeExtension.Duration {
		t.Errorf("MaxLifetimeExtension widened")
	}
	if eff.MaxEgressBytes == nil || eff.MaxEgressBytes.Cmp(*parentRemaining.MaxEgressBytes) > 0 {
		t.Errorf("MaxEgressBytes widened")
	}
}
