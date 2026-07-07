package quota

import (
	"context"
	"errors"
	"testing"
)

// TestForkOpCountsAsCreateForCaps asserts the sandbox.fork op (the hosted
// per-sandbox live fork, issue #709) is enforced like a create: it adds a
// running VM to the org's footprint, so the live concurrency cap and the
// creation-rate bucket MUST apply. Before the gateway routed the flat SDK's
// fork to its own op it rode sandbox.create and was capped; routing it to
// sandbox.fork must not open a cap bypass where an org at its concurrency
// limit fans out via forks.
func TestForkOpCountsAsCreateForCaps(t *testing.T) {
	// Free tier caps concurrency at 2; org already has 2 running.
	e, _ := newEnforcer(t, LiveUsage{ConcurrentSandboxes: 2}, TierFree, clock())
	req := Request{Op: "sandbox.fork", IP: "1.2.3.4"}
	err := e.Check(context.Background(), "org-1", req)
	if !errors.Is(err, ErrConcurrencyExceeded) {
		t.Fatalf("over-concurrency fork error = %v, want ErrConcurrencyExceeded (fork must count as a create)", err)
	}
}

// TestForkOpUnderCapAllowed asserts a fork under every cap is allowed, so the
// create-classing above does not over-deny.
func TestForkOpUnderCapAllowed(t *testing.T) {
	e, _ := newEnforcer(t, LiveUsage{ConcurrentSandboxes: 0}, TierFree, clock())
	req := Request{Op: "sandbox.fork", IP: "1.2.3.4"}
	if err := e.Check(context.Background(), "org-1", req); err != nil {
		t.Fatalf("within-quota fork denied: %v", err)
	}
}

// TestForkOpOverSizeRejected asserts the per-sandbox size cap applies to a
// fork request that carries a sandbox spec, exactly like a create: classing
// forks as creates must cover EVERY create-path cap, not just concurrency.
func TestForkOpOverSizeRejected(t *testing.T) {
	e, _ := newEnforcer(t, LiveUsage{}, TierFree, clock())
	req := Request{Op: "sandbox.fork", IP: "1.2.3.4", NewSandbox: SandboxSpec{VCPUs: 999, MemBytes: 1 << 62, StorageBytes: 1 << 62}}
	err := e.Check(context.Background(), "org-1", req)
	if !errors.Is(err, ErrSandboxTooLarge) {
		t.Fatalf("over-size fork error = %v, want ErrSandboxTooLarge", err)
	}
}

// TestForkOpSizeOfAppliesThroughAdapter asserts the gateway adapter consults
// its SizeOf seam for sandbox.fork exactly as it does for sandbox.create, so a
// deployment that wires SizeOf enforces the size and aggregate caps on forks
// too instead of silently checking forks with a zero spec.
func TestForkOpSizeOfAppliesThroughAdapter(t *testing.T) {
	e, _ := newEnforcer(t, LiveUsage{}, TierFree, clock())
	a := GatewayAdapter{
		Enforcer: e,
		SizeOf: func(_ context.Context) (SandboxSpec, bool) {
			return SandboxSpec{VCPUs: 999, MemBytes: 1 << 62, StorageBytes: 1 << 62}, true
		},
	}
	err := a.Check(context.Background(), "org-1", "sandbox.fork")
	if !errors.Is(err, ErrSandboxTooLarge) {
		t.Fatalf("adapter fork error = %v, want ErrSandboxTooLarge (SizeOf must apply to forks)", err)
	}
}

// TestForkOpChargesCreationRateBucket asserts forks share the rate ladder with
// creates: churn-forking is throttled exactly like churn-creating (mirrors
// TestCreationRateLimited).
func TestForkOpChargesCreationRateBucket(t *testing.T) {
	e, _ := newEnforcer(t, LiveUsage{}, TierFree, clock())
	req := Request{Op: "sandbox.fork", IP: "1.2.3.4"}
	var lastErr error
	for i := 0; i < 10; i++ {
		lastErr = e.Check(context.Background(), "org-1", req)
		if errors.Is(lastErr, ErrRateLimited) {
			break
		}
	}
	if !errors.Is(lastErr, ErrRateLimited) {
		t.Fatalf("expected fork creation-rate ErrRateLimited, got %v", lastErr)
	}
}
