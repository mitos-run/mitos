package onboarding

import (
	"testing"
	"time"
)

func TestVelocityAllowsSlidingWindow(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	v := NewVelocity(3, time.Hour)

	// First 3 calls within the same window must be allowed.
	for i := 0; i < 3; i++ {
		if !v.Allow("ip", t0) {
			t.Fatalf("call %d: want Allow true, got false", i+1)
		}
	}
	// 4th call in the same window must be denied.
	if v.Allow("ip", t0) {
		t.Fatal("4th call in the same window should be denied, got true")
	}
}

func TestVelocityKeyIsIndependent(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	v := NewVelocity(3, time.Hour)

	// Exhaust the limit for "ip".
	for i := 0; i < 3; i++ {
		v.Allow("ip", t0)
	}

	// A completely different key must be unaffected.
	if !v.Allow("other-ip", t0) {
		t.Fatal("a different key should be allowed independently")
	}
}

func TestVelocityWindowResets(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	v := NewVelocity(3, time.Hour)

	// Exhaust the limit.
	for i := 0; i < 3; i++ {
		v.Allow("ip", t0)
	}
	if v.Allow("ip", t0) {
		t.Fatal("expected denial immediately after cap; got allow")
	}

	// After the window elapses the key must be allowed again.
	later := t0.Add(time.Hour + time.Minute)
	if !v.Allow("ip", later) {
		t.Fatal("after the window elapses the key should be allowed again")
	}
}

func TestVelocityDisabledWhenLimitZero(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	v := NewVelocity(0, time.Hour)
	for i := 0; i < 100; i++ {
		if !v.Allow("ip", t0) {
			t.Fatalf("disabled limiter (limit=0) must always return true; failed on call %d", i+1)
		}
	}
}

func TestVelocityNilAlwaysAllows(t *testing.T) {
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	var v *Velocity
	if !v.Allow("ip", t0) {
		t.Fatal("nil Velocity must return true (disabled)")
	}
}
