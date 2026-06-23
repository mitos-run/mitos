package controller_test

// Envtest coverage for Task 4: the claim reconciler reads the template's
// NetworkPolicy (egress + allow) and passes it through the Fork RPC's
// NetworkConfig. A claim from a template with egress=deny and a mixed allowlist
// (one IP:port, one name:port) reaches Ready, and the fake forkd's engine
// records the egress policy and the full allowlist. The name-based entry is
// accepted by the API and does NOT fail the claim; forkd treats it as
// not-yet-enforced.

import (
	v1 "mitos.run/mitos/api/v1"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"mitos.run/mitos/internal/controller"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestClaimPlumbsNetworkPolicyToForkd(t *testing.T) {
	stop, engine, _, err := controller.StartFakeForkdNodeRecording(testRegistry, "netpol-node", "netpol-pool")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	pool := &v1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "netpol-pool", Namespace: "default"},
		Spec: v1.SandboxPoolSpec{
			Template: &v1.PoolTemplateSpec{
				Image: "python:3.12-slim",
				Network: &v1.NetworkPolicy{
					Egress: v1.EgressDeny,
					Allow:  []string{"10.0.0.5:443", "api.example.com:443"},
				},
			},
			Warm: &v1.PoolWarm{Min: 1},
		},
	}
	claim := &v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "netpol-claim", Namespace: "default"},
		Spec:       v1.SandboxSpec{Source: v1.SandboxSource{PoolRef: &v1.LocalObjectReference{Name: "netpol-pool"}}},
	}
	for _, obj := range []client.Object{pool, claim} {
		if err := k8sClient.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, claim)
		_ = k8sClient.Delete(ctx, pool)
	})

	// The claim must reach Ready: a name-based allow entry does not fail it.
	waitClaimReady(t, "netpol-claim")

	// The fake forkd's engine must have recorded the NetworkConfig the claim
	// path sent. Poll briefly because Fork is recorded just before the claim
	// flips Ready.
	var got *struct {
		EgressPolicy string
		AllowList    []string
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if n := engine.LastForkNetwork(); n != nil {
			got = &struct {
				EgressPolicy string
				AllowList    []string
			}{EgressPolicy: n.EgressPolicy, AllowList: n.AllowList}
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got == nil {
		t.Fatal("forkd recorded no NetworkConfig; template NetworkPolicy was not plumbed through the claim path")
	}
	if got.EgressPolicy != string(v1.EgressDeny) {
		t.Fatalf("recorded egress policy = %q, want deny", got.EgressPolicy)
	}
	want := []string{"10.0.0.5:443", "api.example.com:443"}
	if len(got.AllowList) != len(want) {
		t.Fatalf("recorded allow list = %v, want %v", got.AllowList, want)
	}
	for i, e := range want {
		if got.AllowList[i] != e {
			t.Fatalf("allow[%d] = %q, want %q", i, got.AllowList[i], e)
		}
	}
}
