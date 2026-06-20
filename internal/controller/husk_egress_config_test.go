package controller

import (
	"testing"

	v1alpha1 "mitos.run/mitos/api/v1alpha1"
)

// TestHuskNotifyNetworkMapsTemplatePolicy asserts huskNotifyNetwork always
// delivers a network config (never nil now) so the in-pod egress filter and DNS
// proxy are driven, with the guest pointed at the in-pod resolver.
func TestHuskNotifyNetworkMapsTemplatePolicy(t *testing.T) {
	tmpl := &v1alpha1.SandboxTemplate{
		Spec: v1alpha1.SandboxTemplateSpec{
			Network: &v1alpha1.NetworkPolicy{
				Egress: v1alpha1.EgressDeny,
				Allow:  []string{"api.example.com:443"},
			},
		},
	}
	got := huskNotifyNetwork(tmpl)
	if got == nil {
		t.Fatal("huskNotifyNetwork returned nil; expected a network config so the in-pod filter is driven")
	}
	if got.ResolverIP == "" {
		t.Error("expected a resolver IP so the guest is pointed at the in-pod DNS proxy")
	}
	// The controller-delivered /30 MUST match what the stub binds: the stub
	// derives the tap from GuestIP and assigns GatewayIP to it, so both sides
	// agree on the fixed in-pod /30.
	if got.GuestIP != huskGuestIP || got.GatewayIP != huskGatewayIP || got.PrefixLen != 30 {
		t.Errorf("network /30 = %s/%d gw %s, want %s/30 gw %s", got.GuestIP, got.PrefixLen, got.GatewayIP, huskGuestIP, huskGatewayIP)
	}
}

// TestHuskNotifyNetworkNilTemplateStillEnforces asserts a template with no
// NetworkPolicy still gets the fail-closed default-deny config (the stub
// defaults Egress to deny).
func TestHuskNotifyNetworkNilTemplateStillEnforces(t *testing.T) {
	got := huskNotifyNetwork(&v1alpha1.SandboxTemplate{})
	if got == nil {
		t.Fatal("huskNotifyNetwork returned nil for a template with no NetworkPolicy; the filter must still run fail-closed")
	}
}

// TestHuskEgressAllowFromTemplate asserts the template egress policy + allowlist
// are extracted for threading into the activate request.
func TestHuskEgressAllowFromTemplate(t *testing.T) {
	tmpl := &v1alpha1.SandboxTemplate{
		Spec: v1alpha1.SandboxTemplateSpec{
			Network: &v1alpha1.NetworkPolicy{Egress: v1alpha1.EgressDeny, Allow: []string{"x:1"}},
		},
	}
	egress, allow := huskEgressConfig(tmpl)
	if egress != "deny" || len(allow) != 1 || allow[0] != "x:1" {
		t.Errorf("egress=%q allow=%v, want deny [x:1]", egress, allow)
	}
}

// TestHuskEgressConfigDefaultsDeny asserts a nil template or one with no policy
// fails closed to deny with no allows.
func TestHuskEgressConfigDefaultsDeny(t *testing.T) {
	for _, tmpl := range []*v1alpha1.SandboxTemplate{nil, {}} {
		egress, allow := huskEgressConfig(tmpl)
		if egress != "deny" || allow != nil {
			t.Errorf("egress=%q allow=%v, want deny nil", egress, allow)
		}
	}
}
