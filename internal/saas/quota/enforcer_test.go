package quota

import (
	"context"
	"errors"
	"testing"
	"time"

	"mitos.run/mitos/internal/saas"
)

// fakeLiveUsage is a static live-usage/live-count seam for tests: it reports a
// fixed footprint per org so the enforcer's caps can be exercised without a live
// cluster.
type fakeLiveUsage struct {
	usage map[string]LiveUsage
}

func (f fakeLiveUsage) Live(_ context.Context, orgID string) (LiveUsage, error) {
	return f.usage[orgID], nil
}

func newEnforcer(t *testing.T, lu LiveUsage, tier TierName, now func() time.Time) (*Enforcer, *MemSuspensionStore) {
	t.Helper()
	sus := NewMemSuspensionStore()
	e := NewEnforcer(Deps{
		Tiers:       DefaultTiers(),
		TierOf:      func(_ context.Context, _ string) (TierName, error) { return tier, nil },
		LiveUsage:   fakeLiveUsage{usage: map[string]LiveUsage{"org-1": lu, "org-a": lu, "org-b": {}}},
		Suspensions: sus,
		RateLimiter: NewRateLimiter(now),
		Now:         now,
	})
	return e, sus
}

func clock() func() time.Time {
	now := time.Unix(0, 0)
	return func() time.Time { return now }
}

// TestWithinQuotaAllowed asserts a create under every cap is allowed.
func TestWithinQuotaAllowed(t *testing.T) {
	e, _ := newEnforcer(t, LiveUsage{ConcurrentSandboxes: 0, VCPUs: 0}, TierFree, clock())
	req := Request{Op: "sandbox.create", IP: "1.2.3.4", NewSandbox: SandboxSpec{VCPUs: 1, MemBytes: 1 << 30, StorageBytes: 1 << 30}}
	if err := e.Check(context.Background(), "org-1", req); err != nil {
		t.Fatalf("within-quota create denied: %v", err)
	}
}

// TestOverConcurrencyRejected asserts a create is denied when the org is already
// at its concurrent-sandbox cap (checked against LIVE usage, issue #211 seam).
func TestOverConcurrencyRejected(t *testing.T) {
	// Free tier caps concurrency at 2; org already has 2 running.
	e, _ := newEnforcer(t, LiveUsage{ConcurrentSandboxes: 2}, TierFree, clock())
	req := Request{Op: "sandbox.create", IP: "1.2.3.4", NewSandbox: SandboxSpec{VCPUs: 1, MemBytes: 1 << 30, StorageBytes: 1 << 30}}
	err := e.Check(context.Background(), "org-1", req)
	if !errors.Is(err, ErrConcurrencyExceeded) {
		t.Fatalf("over-concurrency error = %v, want ErrConcurrencyExceeded", err)
	}
}

// TestOverAggregateRejected asserts a create is denied when it would push the
// org's aggregate vCPU over the tier cap, even if concurrency is fine.
func TestOverAggregateRejected(t *testing.T) {
	// Free aggregate vCPU cap is 4; org already uses 4, a new 1-vCPU sandbox would
	// be 5.
	e, _ := newEnforcer(t, LiveUsage{ConcurrentSandboxes: 1, VCPUs: 4}, TierFree, clock())
	req := Request{Op: "sandbox.create", IP: "1.2.3.4", NewSandbox: SandboxSpec{VCPUs: 1, MemBytes: 1 << 30, StorageBytes: 1 << 30}}
	err := e.Check(context.Background(), "org-1", req)
	if !errors.Is(err, ErrAggregateExceeded) {
		t.Fatalf("over-aggregate error = %v, want ErrAggregateExceeded", err)
	}
}

// TestOverSizeRejected asserts a single sandbox larger than the tier's max
// sandbox size is denied regardless of aggregate headroom.
func TestOverSizeRejected(t *testing.T) {
	// Free max sandbox vCPU is 2; request 4.
	e, _ := newEnforcer(t, LiveUsage{}, TierFree, clock())
	req := Request{Op: "sandbox.create", IP: "1.2.3.4", NewSandbox: SandboxSpec{VCPUs: 4, MemBytes: 1 << 30, StorageBytes: 1 << 30}}
	err := e.Check(context.Background(), "org-1", req)
	if !errors.Is(err, ErrSandboxTooLarge) {
		t.Fatalf("over-size error = %v, want ErrSandboxTooLarge", err)
	}
}

// TestCrossOrgQuotaIsolation asserts org A being over quota does not deny org B:
// caps are read against EACH org's own live usage.
func TestCrossOrgQuotaIsolation(t *testing.T) {
	sus := NewMemSuspensionStore()
	e := NewEnforcer(Deps{
		Tiers:  DefaultTiers(),
		TierOf: func(_ context.Context, _ string) (TierName, error) { return TierFree, nil },
		LiveUsage: fakeLiveUsage{usage: map[string]LiveUsage{
			"org-a": {ConcurrentSandboxes: 2}, // at the free cap
			"org-b": {ConcurrentSandboxes: 0}, // empty
		}},
		Suspensions: sus,
		RateLimiter: NewRateLimiter(clock()),
		Now:         clock(),
	})
	req := Request{Op: "sandbox.create", IP: "9.9.9.9", NewSandbox: SandboxSpec{VCPUs: 1, MemBytes: 1 << 30, StorageBytes: 1 << 30}}
	if err := e.Check(context.Background(), "org-a", req); !errors.Is(err, ErrConcurrencyExceeded) {
		t.Fatalf("org-a should be over concurrency, got %v", err)
	}
	if err := e.Check(context.Background(), "org-b", req); err != nil {
		t.Fatalf("org-b is empty and must be allowed, got %v", err)
	}
}

// TestRateLimitedRequestRejected asserts the per-org request-rate bucket denies
// once the org exceeds its tier's API request rate, with the typed rate-limit
// error.
func TestRateLimitedRequestRejected(t *testing.T) {
	// Free tier API rate is 60/min, burst 60. A read op (sandbox.status) charges the
	// in-sandbox bucket. Drain it and the next is denied.
	e, _ := newEnforcer(t, LiveUsage{}, TierFree, clock())
	req := Request{Op: "sandbox.status", IP: "1.2.3.4"}
	var lastErr error
	for i := 0; i < 200; i++ {
		lastErr = e.Check(context.Background(), "org-1", req)
		if lastErr != nil {
			break
		}
	}
	if !errors.Is(lastErr, ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited after draining the bucket, got %v", lastErr)
	}
}

// TestCreationRateLimited asserts repeated creates trip the lifecycle creation-
// rate bucket independent of the API request rate.
func TestCreationRateLimited(t *testing.T) {
	// Free creation rate is 5/min, burst 5. Concurrency cap is 2, so to isolate the
	// creation-rate path use an org with zero live usage and a 6th create.
	e, _ := newEnforcer(t, LiveUsage{}, TierFree, clock())
	req := Request{Op: "sandbox.create", IP: "1.2.3.4", NewSandbox: SandboxSpec{VCPUs: 1, MemBytes: 1 << 30, StorageBytes: 1 << 30}}
	var lastErr error
	for i := 0; i < 10; i++ {
		lastErr = e.Check(context.Background(), "org-1", req)
		if errors.Is(lastErr, ErrRateLimited) {
			break
		}
	}
	if !errors.Is(lastErr, ErrRateLimited) {
		t.Fatalf("expected creation-rate ErrRateLimited, got %v", lastErr)
	}
}

// TestSuspendedOrgRejected asserts a suspended org's request is denied (fail
// closed), proving the kill-switch path: once an org is suspended, the enforcer
// denies it before any quota math, so an in-flight request from a suspended org
// is refused.
func TestSuspendedOrgRejected(t *testing.T) {
	now := clock()
	e, sus := newEnforcer(t, LiveUsage{}, TierFree, now)
	ks := NewKillSwitch(sus, now)
	if err := ks.Suspend(context.Background(), "org-1", ReasonManual, "abuse review", true); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	req := Request{Op: "sandbox.create", IP: "1.2.3.4", NewSandbox: SandboxSpec{VCPUs: 1, MemBytes: 1 << 30, StorageBytes: 1 << 30}}
	err := e.Check(context.Background(), "org-1", req)
	if !errors.Is(err, ErrSuspended) {
		t.Fatalf("suspended org error = %v, want ErrSuspended", err)
	}
	// Lifting the suspension restores access.
	if _, err := ks.Lift(context.Background(), "org-1"); err != nil {
		t.Fatalf("lift: %v", err)
	}
	if err := e.Check(context.Background(), "org-1", req); err != nil {
		t.Fatalf("after lift, request should be allowed, got %v", err)
	}
}

// TestEmergencyStopSuspendsAllListedOrgs asserts the pool-wide emergency stop
// suspends every named org so all fail closed.
func TestEmergencyStopSuspendsAllListedOrgs(t *testing.T) {
	now := clock()
	sus := NewMemSuspensionStore()
	ks := NewKillSwitch(sus, now)
	n, err := ks.EmergencyStop(context.Background(), []string{"org-a", "org-b", "org-c"}, "incident-42")
	if err != nil || n != 3 {
		t.Fatalf("emergency stop n=%d err=%v, want 3 and nil", n, err)
	}
	for _, org := range []string{"org-a", "org-b", "org-c"} {
		if _, ok, _ := sus.IsSuspended(context.Background(), org); !ok {
			t.Errorf("org %q not suspended after emergency stop", org)
		}
	}
}

// staticSignal flags a fixed set of orgs.
type staticSignal struct{ orgs map[string]string }

func (s staticSignal) FiredOrgs(_ context.Context) (map[string]string, error) { return s.orgs, nil }

// TestAbuseSignalDrivesAutomatedSuspension asserts the automated suspension path:
// an abuse signal that flags an org causes the kill switch to suspend it, after
// which the enforcer denies it.
func TestAbuseSignalDrivesAutomatedSuspension(t *testing.T) {
	now := clock()
	e, sus := newEnforcer(t, LiveUsage{}, TierFree, now)
	ks := NewKillSwitch(sus, now)
	sig := staticSignal{orgs: map[string]string{"org-1": "egress spike consistent with crypto mining"}}
	suspended, err := ks.ProcessSignals(context.Background(), sig)
	if err != nil || len(suspended) != 1 || suspended[0] != "org-1" {
		t.Fatalf("ProcessSignals = %v, %v; want [org-1], nil", suspended, err)
	}
	req := Request{Op: "sandbox.create", IP: "1.2.3.4", NewSandbox: SandboxSpec{VCPUs: 1, MemBytes: 1 << 30, StorageBytes: 1 << 30}}
	if err := e.Check(context.Background(), "org-1", req); !errors.Is(err, ErrSuspended) {
		t.Fatalf("auto-suspended org error = %v, want ErrSuspended", err)
	}
}

// TestEnforcerSatisfiesGatewaySeam asserts the enforcer plugs into the #210
// gateway QuotaEnforcer seam: it satisfies the interface and its Check maps to a
// deny the gateway turns into quota_exceeded. The gateway-side adapter is exercised
// in the gateway integration test.
func TestEnforcerSatisfiesGatewaySeam(t *testing.T) {
	e, _ := newEnforcer(t, LiveUsage{}, TierFree, clock())
	var _ saas.QuotaEnforcer = GatewayAdapter{Enforcer: e, IPOf: func(_ context.Context) string { return "0.0.0.0" }}
}
