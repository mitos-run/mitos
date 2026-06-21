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
	cfg := huskEgressConfig(tmpl)
	if cfg.Egress != "deny" || len(cfg.Allow) != 1 || cfg.Allow[0] != "x:1" {
		t.Errorf("egress=%q allow=%v, want deny [x:1]", cfg.Egress, cfg.Allow)
	}
}

// TestHuskEgressConfigDefaultsDeny asserts a nil template or one with no policy
// fails closed to deny with no allows (and deny-by-default inbound, the empty
// Inbound string which the renderer and stub treat as deny).
func TestHuskEgressConfigDefaultsDeny(t *testing.T) {
	for _, tmpl := range []*v1alpha1.SandboxTemplate{nil, {}} {
		cfg := huskEgressConfig(tmpl)
		if cfg.Egress != "deny" || cfg.Allow != nil {
			t.Errorf("egress=%q allow=%v, want deny nil", cfg.Egress, cfg.Allow)
		}
		if cfg.BlockNetwork || cfg.Inbound != "" {
			t.Errorf("default posture must be no-block, deny-by-default inbound; got block=%v inbound=%q", cfg.BlockNetwork, cfg.Inbound)
		}
	}
}

// TestHuskEgressConfigThreadsNewDimensions asserts the block_network total-deny,
// the CIDR allowlist, and the inbound policy from the template's NetworkPolicy
// are extracted for threading into the activate request (issue #219).
func TestHuskEgressConfigThreadsNewDimensions(t *testing.T) {
	tmpl := &v1alpha1.SandboxTemplate{
		Spec: v1alpha1.SandboxTemplateSpec{
			Network: &v1alpha1.NetworkPolicy{
				Egress:       v1alpha1.EgressDeny,
				BlockNetwork: true,
				AllowCIDRs:   []string{"10.0.0.0/8"},
				Inbound:      v1alpha1.InboundAllow,
				InboundCIDRs: []string{"203.0.113.0/24"},
			},
		},
	}
	cfg := huskEgressConfig(tmpl)
	if !cfg.BlockNetwork {
		t.Error("block_network not threaded")
	}
	if len(cfg.AllowCIDRs) != 1 || cfg.AllowCIDRs[0] != "10.0.0.0/8" {
		t.Errorf("allow_cidrs not threaded: %v", cfg.AllowCIDRs)
	}
	if cfg.Inbound != "allow" || len(cfg.InboundCIDRs) != 1 {
		t.Errorf("inbound policy not threaded: inbound=%q cidrs=%v", cfg.Inbound, cfg.InboundCIDRs)
	}
}
