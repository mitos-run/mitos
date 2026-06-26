package clusterinstruments

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/saas/console"
	"mitos.run/mitos/internal/tenant"
)

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1.AddToScheme(s); err != nil {
		t.Fatalf("add v1 scheme: %v", err)
	}
	return s
}

// sbWith builds a v1.Sandbox owned by org with the given fork source and
// startup latency.
func sbWith(org, name string, forked bool, latencyMs int64) *v1.Sandbox {
	src := v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "python"}}
	if forked {
		src = v1.SandboxSource{FromSandbox: &v1.FromSandboxSource{Name: "parent"}}
	}
	return &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: tenant.NamespaceForOrg(org),
			Labels:    tenant.OrgLabels(org),
		},
		Spec:   v1.SandboxSpec{Source: src},
		Status: v1.SandboxStatus{Phase: v1.SandboxPhase("Ready"), StartupLatencyMs: latencyMs},
	}
}

func newSource(t *testing.T, objs ...client.Object) *Source {
	t.Helper()
	c := fakeclient.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objs...).Build()
	return New(c)
}

// TestSnapshotMeasuresCallerOrg asserts the snapshot counts the org's forks and
// computes its activate-latency percentiles from its OWN sandboxes.
func TestSnapshotMeasuresCallerOrg(t *testing.T) {
	c := newSource(t,
		sbWith("alice", "sb-a1", false, 20),
		sbWith("alice", "sb-a2", true, 27),
		sbWith("alice", "sb-a3", true, 41),
	)
	snap, err := c.Snapshot(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.OrgID != "alice" {
		t.Fatalf("org = %q, want alice", snap.OrgID)
	}
	if snap.ForksServed != 2 {
		t.Fatalf("forks_served = %d, want 2 (the two fromSandbox sandboxes)", snap.ForksServed)
	}
	if snap.ActivateP50Millis != 27 {
		t.Fatalf("activate_p50_ms = %v, want 27 (median of 20,27,41)", snap.ActivateP50Millis)
	}
	if snap.ActivateP99Millis != 41 {
		t.Fatalf("activate_p99_ms = %v, want 41 (top of 20,27,41)", snap.ActivateP99Millis)
	}
}

// TestSnapshotScopedToCallerOrg asserts alice's metrics never include bob's
// sandboxes: the proof read is org-scoped to the namespace + label.
func TestSnapshotScopedToCallerOrg(t *testing.T) {
	c := newSource(t,
		sbWith("alice", "sb-a1", true, 27),
		sbWith("bob", "sb-b1", true, 99),
		sbWith("bob", "sb-b2", true, 99),
	)
	alice, err := c.Snapshot(context.Background(), "alice")
	if err != nil {
		t.Fatalf("alice Snapshot: %v", err)
	}
	if alice.ForksServed != 1 {
		t.Fatalf("alice forks_served = %d, want 1 (bob's 2 forks must not leak)", alice.ForksServed)
	}
	if alice.ActivateP50Millis == 99 {
		t.Fatalf("alice saw bob's latency 99; cross-org leak")
	}
}

// TestSnapshotEmptyOrgIsZero asserts an org with no sandboxes gets a zero,
// org-scoped snapshot rather than another org's numbers.
func TestSnapshotEmptyOrgIsZero(t *testing.T) {
	c := newSource(t, sbWith("bob", "sb-b1", true, 99))
	snap, err := c.Snapshot(context.Background(), "alice")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.OrgID != "alice" || snap.ForksServed != 0 || snap.ActivateP50Millis != 0 {
		t.Fatalf("empty-org snapshot = %+v, want zero scoped to alice", snap)
	}
}

// TestImplementsInstrumentsSource is a compile-time seam assertion.
func TestImplementsInstrumentsSource(t *testing.T) {
	var _ console.InstrumentsSource = (*Source)(nil)
}
