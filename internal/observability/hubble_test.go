package observability

import "testing"

// A minimal Hubble flow shape: the fields the per-sandbox resolver needs from a
// Cilium Hubble flow record. Hubble identifies an endpoint by its pod namespace,
// pod name, and pod labels; the resolver maps those onto the mitos claim the
// sandbox belongs to via the mitos.run/claim and mitos.run/pool labels the husk
// pod (and any pod-backed sandbox) carries.
func huskFlow(verdict string, labels map[string]string) HubbleFlow {
	return HubbleFlow{
		Verdict: verdict,
		Source: HubbleEndpoint{
			Namespace: "tenant-a",
			PodName:   "pool-x-husk-abcde",
			Labels:    labels,
		},
		L4Protocol: "TCP",
		DestPort:   443,
	}
}

func TestResolveClaim_FromPodLabels(t *testing.T) {
	f := huskFlow("FORWARDED", map[string]string{
		"mitos.run/claim": "claim-a",
		"mitos.run/pool":  "pool-x",
		"app":             "agent",
	})
	id, ok := ResolveSandbox(f.Source)
	if !ok {
		t.Fatal("expected a sandbox identity from the claim label")
	}
	if id.Claim != "claim-a" || id.Pool != "pool-x" || id.Namespace != "tenant-a" {
		t.Errorf("identity = %+v, want claim-a/pool-x/tenant-a", id)
	}
}

func TestResolveClaim_NoClaimLabel(t *testing.T) {
	// A flow from a pod with no mitos claim label is not a sandbox flow; the
	// resolver must report that rather than inventing a claim.
	f := huskFlow("FORWARDED", map[string]string{"app": "kube-dns"})
	if _, ok := ResolveSandbox(f.Source); ok {
		t.Error("expected no sandbox identity for a non-sandbox pod")
	}
}

func TestResolveClaim_NilLabels(t *testing.T) {
	if _, ok := ResolveSandbox(HubbleEndpoint{Namespace: "x"}); ok {
		t.Error("expected no identity for nil labels")
	}
}

// DeniedEgress is the test the issue calls out: a denied egress flow must
// surface with the claim label so an operator can see which sandbox tried it.
// The verdict mapping is pure and testable; the LIVE Hubble emission is
// cluster-gated.
func TestDeniedEgress_CarriesClaim(t *testing.T) {
	f := huskFlow("DROPPED", map[string]string{
		"mitos.run/claim": "claim-b",
		"mitos.run/pool":  "pool-y",
	})
	ev, ok := ClassifyEgress(f)
	if !ok {
		t.Fatal("expected an egress event")
	}
	if !ev.Denied {
		t.Error("DROPPED verdict must classify as denied")
	}
	if ev.Sandbox.Claim != "claim-b" {
		t.Errorf("denied egress claim = %q, want claim-b", ev.Sandbox.Claim)
	}
	if ev.DestPort != 443 {
		t.Errorf("dest port = %d, want 443", ev.DestPort)
	}
}

func TestClassifyEgress_ForwardedNotDenied(t *testing.T) {
	f := huskFlow("FORWARDED", map[string]string{"mitos.run/claim": "claim-c"})
	ev, ok := ClassifyEgress(f)
	if !ok {
		t.Fatal("expected an egress event")
	}
	if ev.Denied {
		t.Error("FORWARDED verdict must not be denied")
	}
}

func TestClassifyEgress_NonSandboxSkipped(t *testing.T) {
	f := huskFlow("DROPPED", map[string]string{"app": "other"})
	if _, ok := ClassifyEgress(f); ok {
		t.Error("a non-sandbox flow must not produce a sandbox egress event")
	}
}
