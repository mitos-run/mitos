package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/tenant"
)

// sandboxInPhase builds a sandbox owned by org in the org's hard-isolation
// namespace with the given status phase.
func sandboxInPhase(org, name string, phase v1.SandboxPhase) *v1.Sandbox {
	return sandboxInPhaseNS(org, tenant.NamespaceForOrg(org), name, phase)
}

// sandboxInPhaseNS builds an org-labeled sandbox in an explicit namespace (the
// single-tenant-mode shape: shared namespace, org label still authoritative).
func sandboxInPhaseNS(org, ns, name string, phase v1.SandboxPhase) *v1.Sandbox {
	return &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    tenant.OrgLabels(org),
		},
		Status: v1.SandboxStatus{Phase: phase},
	}
}

// TestLiveCounterCountsNonTerminalPhases asserts the counter reports every
// sandbox in a non-terminal phase (Pending, Restoring, Ready, Terminating, and
// the empty just-created phase) and excludes Terminated and Failed: only
// sandboxes that hold or are about to hold capacity count against the
// concurrency cap.
func TestLiveCounterCountsNonTerminalPhases(t *testing.T) {
	c := newFakeClient(t,
		sandboxInPhase(orgA, "sb-pending", v1.SandboxPending),
		sandboxInPhase(orgA, "sb-restoring", v1.SandboxRestoring),
		sandboxInPhase(orgA, "sb-ready", v1.SandboxReady),
		sandboxInPhase(orgA, "sb-terminating", v1.SandboxTerminating),
		sandboxInPhase(orgA, "sb-new", ""), // created, controller has not set a phase yet.
		sandboxInPhase(orgA, "sb-done", v1.SandboxTerminated),
		sandboxInPhase(orgA, "sb-broken", v1.SandboxFailed),
	)
	lc := NewLiveCounter(c, "")
	got, err := lc.Count(context.Background(), orgA)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got.ConcurrentSandboxes != 5 {
		t.Errorf("ConcurrentSandboxes = %d, want 5 (Terminated and Failed excluded)", got.ConcurrentSandboxes)
	}
	// Aggregate resource fields stay zero until the pool-resolved follow-up:
	// only the concurrency cap is live from this counter.
	if got.VCPUs != 0 || got.MemBytes != 0 || got.StorageBytes != 0 {
		t.Errorf("aggregate fields = %+v, want all zero (pool-resolved aggregates are a deferred follow-up)", got)
	}
}

// TestLiveCounterCountsReplicas asserts a Sandbox with Spec.Replicas = N
// counts as N live sandboxes, not 1: replicas is the fork fan-out, so one
// object with replicas=100 is 100 running VMs. Counting objects instead of
// replicas would let a free-tier org (cap 2) run ~200 VMs via 2 objects,
// bypassing exactly the lever the concurrency cap exists for.
func TestLiveCounterCountsReplicas(t *testing.T) {
	fanned := sandboxInPhase(orgA, "sb-fan", v1.SandboxReady)
	fanned.Spec.Replicas = 100
	single := sandboxInPhase(orgA, "sb-one", v1.SandboxReady) // Replicas unset = 1.
	terminatedFan := sandboxInPhase(orgA, "sb-dead-fan", v1.SandboxTerminated)
	terminatedFan.Spec.Replicas = 50 // terminal: contributes nothing.
	c := newFakeClient(t, fanned, single, terminatedFan)
	lc := NewLiveCounter(c, "")
	got, err := lc.Count(context.Background(), orgA)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got.ConcurrentSandboxes != 101 {
		t.Errorf("ConcurrentSandboxes = %d, want 101 (100 replicas + 1 single, terminated fan excluded)", got.ConcurrentSandboxes)
	}
}

// TestLiveCounterScopedToOrg asserts another org's sandboxes never count, even
// when a mislabeled object sits inside the org's own namespace (defense in
// depth: namespace AND label must both match).
func TestLiveCounterScopedToOrg(t *testing.T) {
	c := newFakeClient(t,
		sandboxInPhase(orgA, "sb-a1", v1.SandboxReady),
		sandboxInPhase(orgB, "sb-b1", v1.SandboxReady),
		// A foreign-labeled object in org A's namespace must not count for A.
		sandboxInPhaseNS(orgB, tenant.NamespaceForOrg(orgA), "sb-b-in-a", v1.SandboxReady),
	)
	lc := NewLiveCounter(c, "")
	got, err := lc.Count(context.Background(), orgA)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got.ConcurrentSandboxes != 1 {
		t.Errorf("ConcurrentSandboxes = %d, want 1 (only org A's own sandbox)", got.ConcurrentSandboxes)
	}
}

// TestLiveCounterSingleTenantNamespace asserts single-tenant mode counts in the
// pinned shared namespace and still separates orgs by label, matching the
// control plane's WithSingleTenantNamespace semantics.
func TestLiveCounterSingleTenantNamespace(t *testing.T) {
	const shared = "mitos"
	c := newFakeClient(t,
		sandboxInPhaseNS(orgA, shared, "sb-a1", v1.SandboxReady),
		sandboxInPhaseNS(orgA, shared, "sb-a2", v1.SandboxPending),
		sandboxInPhaseNS(orgB, shared, "sb-b1", v1.SandboxReady),
		// Org A also has a stray object in its per-org namespace; single-tenant
		// mode must not look there.
		sandboxInPhase(orgA, "sb-elsewhere", v1.SandboxReady),
	)
	lc := NewLiveCounter(c, shared)
	got, err := lc.Count(context.Background(), orgA)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got.ConcurrentSandboxes != 2 {
		t.Errorf("ConcurrentSandboxes = %d, want 2 (shared namespace, org A label only)", got.ConcurrentSandboxes)
	}
}

// TestLiveCounterEmptyOrg asserts an org with nothing running (or with a
// not-yet-provisioned namespace, which lists empty) reports zero, not an error.
func TestLiveCounterEmptyOrg(t *testing.T) {
	c := newFakeClient(t)
	lc := NewLiveCounter(c, "")
	got, err := lc.Count(context.Background(), "nobody")
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got.ConcurrentSandboxes != 0 {
		t.Errorf("ConcurrentSandboxes = %d, want 0", got.ConcurrentSandboxes)
	}
}

// TestLiveCounterListErrorFailsClosed asserts a List failure surfaces as an
// error (which the quota enforcer maps to a deny): a transient apiserver blip
// must never read as "zero live sandboxes" on the anti-abuse path.
func TestLiveCounterListErrorFailsClosed(t *testing.T) {
	boom := errors.New("apiserver unavailable")
	c := fakeclient.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return boom
			},
		}).
		Build()
	lc := NewLiveCounter(c, "")
	_, err := lc.Count(context.Background(), orgA)
	if err == nil {
		t.Fatal("Count with a failing List returned nil error; the counter must fail closed")
	}
	if !errors.Is(err, boom) {
		t.Errorf("Count error = %v, want it to wrap the List error", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("error text mentions secrets: %q", err.Error())
	}
}
