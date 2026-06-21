package quota

import (
	"testing"

	"mitos.run/mitos/api/v1alpha1"
)

// TestFreeTierIsDenyByDefaultEgress asserts the anti-abuse floor: a free-tier
// sandbox gets deny-by-default egress (no network without an explicit allow), and
// the mapping translates that to a netconf.SandboxPolicy the #219 datapath
// enforces (EgressDeny, not blocked, not open).
func TestFreeTierIsDenyByDefaultEgress(t *testing.T) {
	tier := DefaultTiers()[TierFree]
	pol := tier.Egress.Policy()
	if pol.BlockNetwork {
		t.Fatal("free tier must not fully block (deny-by-default allows an explicit allowlist), got BlockNetwork")
	}
	if pol.Egress != v1alpha1.EgressDeny {
		t.Fatalf("free tier egress = %q, want %q (deny-by-default)", pol.Egress, v1alpha1.EgressDeny)
	}
	if pol.Inbound != v1alpha1.InboundDeny {
		t.Fatalf("free tier inbound = %q, want %q (never dialable)", pol.Inbound, v1alpha1.InboundDeny)
	}
}

// TestBlockedEgressTierDropsAllTraffic asserts the most restrictive posture
// (BlockNetwork) maps to a SandboxPolicy with BlockNetwork set, which the #219
// datapath renders as a total egress drop.
func TestBlockedEgressTierDropsAllTraffic(t *testing.T) {
	e := EgressTier{BlockNetwork: true}
	pol := e.Policy()
	if !pol.BlockNetwork {
		t.Fatal("blocked tier must map to BlockNetwork=true")
	}
}

// TestOpenTierAllowsEgressButStillBlocksAbusePorts asserts the pro/open tier maps
// to EgressAllow, but the abuse-port block (SMTP, issue #36) is still threaded via
// the resolved egress, so even an open tier cannot send mail spam.
func TestOpenTierAllowsEgressButStillBlocksAbusePorts(t *testing.T) {
	tier := DefaultTiers()[TierPro]
	resolved := tier.ResolveEgress()
	if resolved.Policy.Egress != v1alpha1.EgressAllow {
		t.Fatalf("pro tier egress = %q, want %q", resolved.Policy.Egress, v1alpha1.EgressAllow)
	}
	if !containsPort(resolved.BlockPorts, 25) {
		t.Fatal("open tier must still block outbound SMTP port 25 (issue #36)")
	}
	for _, p := range AbusePorts {
		if !containsPort(resolved.BlockPorts, p) {
			t.Errorf("abuse port %d not in the open tier block set", p)
		}
	}
}

// TestEveryTierBlocksAbusePorts asserts the abuse-port block is fleet-wide: no
// tier, however permissive, omits it.
func TestEveryTierBlocksAbusePorts(t *testing.T) {
	for name, tier := range DefaultTiers() {
		resolved := tier.ResolveEgress()
		if !containsPort(resolved.BlockPorts, 25) {
			t.Errorf("tier %q does not block SMTP port 25", name)
		}
	}
}

// TestTierLadderWidensMonotonically asserts the prepaid ladder is monotonic: each
// step up has at least as much concurrency and aggregate vCPU as the step below,
// which is the property that makes "pay to climb" the abuse lever.
func TestTierLadderWidensMonotonically(t *testing.T) {
	tiers := DefaultTiers()
	free, starter, pro := tiers[TierFree], tiers[TierStarter], tiers[TierPro]
	if !(free.MaxConcurrentSandboxes < starter.MaxConcurrentSandboxes &&
		starter.MaxConcurrentSandboxes < pro.MaxConcurrentSandboxes) {
		t.Fatalf("concurrency not monotonic: free=%d starter=%d pro=%d",
			free.MaxConcurrentSandboxes, starter.MaxConcurrentSandboxes, pro.MaxConcurrentSandboxes)
	}
	if !(free.MaxAggregateVCPUs < starter.MaxAggregateVCPUs &&
		starter.MaxAggregateVCPUs < pro.MaxAggregateVCPUs) {
		t.Fatalf("aggregate vCPU not monotonic: free=%d starter=%d pro=%d",
			free.MaxAggregateVCPUs, starter.MaxAggregateVCPUs, pro.MaxAggregateVCPUs)
	}
}

func containsPort(ports []int, p int) bool {
	for _, x := range ports {
		if x == p {
			return true
		}
	}
	return false
}
