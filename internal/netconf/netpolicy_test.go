package netconf

import (
	"net"
	"strings"
	"testing"

	"mitos.run/mitos/api/v1alpha1"
)

// TestRenderSandboxChainSpecBackwardCompat asserts the new spec-based renderer
// produces the same baseline chain (metadata block, established/related, the
// allowlist accepts, the dynamic sets, and the terminal drop) as the legacy
// RenderSandboxChain for an equivalent policy, so existing callers are
// unaffected when they migrate.
func TestRenderSandboxChainSpecBackwardCompat(t *testing.T) {
	allow := []HostPort{{IP: net.ParseIP("10.0.0.5"), Port: 443}}
	legacy := RenderSandboxChain("sbtap0", net.ParseIP("10.200.0.2"),
		v1alpha1.EgressDeny, allow, net.ParseIP("10.200.0.1"))
	spec := RenderSandboxChainSpec(ChainSpec{
		Tap:        "sbtap0",
		GuestIP:    net.ParseIP("10.200.0.2"),
		Egress:     v1alpha1.EgressDeny,
		Allow:      allow,
		ResolverIP: net.ParseIP("10.200.0.1"),
	})
	if legacy != spec {
		t.Errorf("spec render differs from legacy for an equivalent policy\n--- legacy ---\n%s\n--- spec ---\n%s", legacy, spec)
	}
}

// TestRenderSandboxChainBlockNetwork asserts that BlockNetwork drops ALL egress:
// no allow accept survives, and the chain still drops v4 and v6. The metadata
// block and the trailing drop remain; even an allowlist entry is inert because
// the chain terminates in drop before reaching any accept under block.
func TestRenderSandboxChainBlockNetwork(t *testing.T) {
	out := RenderSandboxChainSpec(ChainSpec{
		Tap:          "sbtap0",
		GuestIP:      net.ParseIP("10.200.0.2"),
		Egress:       v1alpha1.EgressAllow, // even allow is overridden by block
		Allow:        []HostPort{{IP: net.ParseIP("10.0.0.5"), Port: 443}},
		AllowCIDRs:   []string{"10.0.0.0/8"},
		ResolverIP:   net.ParseIP("10.200.0.1"),
		BlockNetwork: true,
	})
	// No destination accept may appear: not the static allow, not the CIDR allow,
	// not the DNS-to-resolver accept. The only legitimate accept under block is
	// none; everything egress-bound is dropped.
	for _, banned := range []string{
		"ip daddr 10.0.0.5 tcp dport 443 accept",
		"ip daddr 10.0.0.0/8 accept",
		"ip daddr 10.200.0.1 udp dport 53 accept",
	} {
		if strings.Contains(out, banned) {
			t.Errorf("block_network chain must not contain accept %q\n%s", banned, out)
		}
	}
	// The chain ends in a v4 drop and a v6 drop.
	if !strings.Contains(out, "ip saddr 10.200.0.2 drop") {
		t.Errorf("block_network chain missing v4 drop\n%s", out)
	}
	if !strings.Contains(out, "meta nfproto ipv6 drop") {
		t.Errorf("block_network chain missing v6 drop\n%s", out)
	}
}

// TestRenderSandboxChainCIDRAllowlist asserts that each AllowCIDR becomes a
// saddr-pinned daddr accept, placed before the terminal verdict.
func TestRenderSandboxChainCIDRAllowlist(t *testing.T) {
	out := RenderSandboxChainSpec(ChainSpec{
		Tap:        "sbtap0",
		GuestIP:    net.ParseIP("10.200.0.2"),
		Egress:     v1alpha1.EgressDeny,
		AllowCIDRs: []string{"203.0.113.0/24", "198.51.100.0/24"},
		ResolverIP: net.ParseIP("10.200.0.1"),
	})
	for _, want := range []string{
		"ip saddr 10.200.0.2 ip daddr 203.0.113.0/24 accept",
		"ip saddr 10.200.0.2 ip daddr 198.51.100.0/24 accept",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("CIDR allowlist chain missing %q\n%s", want, out)
		}
	}
	// The CIDR accept must precede the final drop.
	idxAccept := strings.Index(out, "ip daddr 203.0.113.0/24 accept")
	idxDrop := strings.LastIndex(out, "ip saddr 10.200.0.2 drop")
	if idxAccept < 0 || idxDrop < 0 || idxAccept > idxDrop {
		t.Errorf("CIDR accept must precede the terminal drop\n%s", out)
	}
}

// TestRenderSandboxChainCIDRAllowlistV6 asserts an IPv6 CIDR is rendered as an
// ip6 daddr accept (not saddr-pinned, matching the v6 dynamic-set posture: the
// guest has no v6 source identity to anti-spoof against).
func TestRenderSandboxChainCIDRAllowlistV6(t *testing.T) {
	out := RenderSandboxChainSpec(ChainSpec{
		Tap:        "sbtap0",
		GuestIP:    net.ParseIP("10.200.0.2"),
		Egress:     v1alpha1.EgressDeny,
		AllowCIDRs: []string{"2001:db8::/32"},
	})
	if !strings.Contains(out, "ip6 daddr 2001:db8::/32 accept") {
		t.Errorf("v6 CIDR allowlist chain missing ip6 daddr accept\n%s", out)
	}
	if strings.Contains(out, "ip saddr 10.200.0.2 ip6 daddr") {
		t.Errorf("v6 CIDR accept must not be v4-saddr-pinned\n%s", out)
	}
}

// TestRenderSandboxChainEgressCounter asserts the egress counter is rendered as
// a named per-sandbox nftables counter plus a counting rule, so the metering
// pipeline (#211) can read per-sandbox egress bytes. The counter is saddr-pinned
// so it counts only this guest's traffic, and is placed at the top of the chain
// so it counts every egress packet (allowed or dropped) for the sandbox.
func TestRenderSandboxChainEgressCounter(t *testing.T) {
	out := RenderSandboxChainSpec(ChainSpec{
		Tap:        "sbtap0",
		GuestIP:    net.ParseIP("10.200.0.2"),
		Egress:     v1alpha1.EgressDeny,
		ResolverIP: net.ParseIP("10.200.0.1"),
		Counter:    true,
	})
	counter := SandboxEgressCounterName("sbtap0")
	if !strings.Contains(out, "add counter inet "+SharedTableName()+" "+counter) {
		t.Errorf("egress counter declaration missing\n%s", out)
	}
	if !strings.Contains(out, "ip saddr 10.200.0.2 counter name "+counter) {
		t.Errorf("egress counting rule missing or not saddr-pinned\n%s", out)
	}
	// The counting rule must come before the established/related accept so it
	// counts every egress packet, including those that are later dropped.
	idxCount := strings.Index(out, "counter name "+counter)
	idxEstablished := strings.Index(out, "ct state established,related accept")
	if idxCount < 0 || idxEstablished < 0 || idxCount > idxEstablished {
		t.Errorf("counter must be placed before the established/related accept\n%s", out)
	}
}

// TestRenderSandboxChainNoCounterByDefault asserts the counter is opt-in: a spec
// without Counter renders no counter object, preserving the legacy chain shape.
func TestRenderSandboxChainNoCounterByDefault(t *testing.T) {
	out := RenderSandboxChainSpec(ChainSpec{
		Tap:        "sbtap0",
		GuestIP:    net.ParseIP("10.200.0.2"),
		Egress:     v1alpha1.EgressDeny,
		ResolverIP: net.ParseIP("10.200.0.1"),
	})
	if strings.Contains(out, "add counter") || strings.Contains(out, "counter name") {
		t.Errorf("counter must be opt-in; default render must contain no counter\n%s", out)
	}
}

// TestRenderSandboxInputChainDenyByDefault asserts the input chain denies all
// unsolicited inbound by default (the existing behavior: only DNS to the
// resolver, then drop), confirming deny-by-default inbound is the baseline.
func TestRenderSandboxInputChainDenyByDefault(t *testing.T) {
	out := RenderSandboxInputChainSpec(InputChainSpec{
		Tap:        "sbtap0",
		GuestIP:    net.ParseIP("10.200.0.2"),
		ResolverIP: net.ParseIP("10.200.0.1"),
		Inbound:    v1alpha1.InboundDeny,
	})
	if !strings.HasSuffix(strings.TrimRight(out, "\n"), "drop }\nadd element inet "+SharedTableName()+" "+InputDispatchMapName()+" { \"sbtap0\" : jump "+SandboxInputChainName("sbtap0")+" }") &&
		!strings.Contains(out, "ip saddr 10.200.0.2 drop") {
		t.Errorf("deny-by-default input chain must drop guest-sourced pod-local traffic\n%s", out)
	}
}

// TestRenderSandboxInputChainAllowCIDR asserts that InboundAllow with source
// CIDRs accepts inbound from those CIDRs on the input hook (a listener inside the
// guest is reachable only from the allowed sources). The terminal drop remains
// for any source not in the allowlist.
func TestRenderSandboxInputChainAllowCIDR(t *testing.T) {
	out := RenderSandboxInputChainSpec(InputChainSpec{
		Tap:          "sbtap0",
		GuestIP:      net.ParseIP("10.200.0.2"),
		ResolverIP:   net.ParseIP("10.200.0.1"),
		Inbound:      v1alpha1.InboundAllow,
		InboundCIDRs: []string{"203.0.113.0/24"},
	})
	if !strings.Contains(out, "ip daddr 10.200.0.2 ip saddr 203.0.113.0/24 accept") {
		t.Errorf("inbound-allow chain missing source-CIDR accept to guest\n%s", out)
	}
	// Still ends in a drop for sources not in the allowlist.
	if !strings.Contains(out, "drop") {
		t.Errorf("inbound chain must still drop non-allowlisted sources\n%s", out)
	}
}

// TestRenderSandboxInputChainAllowAny asserts that InboundAllow with no CIDRs
// accepts inbound to the guest from any source.
func TestRenderSandboxInputChainAllowAny(t *testing.T) {
	out := RenderSandboxInputChainSpec(InputChainSpec{
		Tap:        "sbtap0",
		GuestIP:    net.ParseIP("10.200.0.2"),
		ResolverIP: net.ParseIP("10.200.0.1"),
		Inbound:    v1alpha1.InboundAllow,
	})
	if !strings.Contains(out, "ip daddr 10.200.0.2 accept") {
		t.Errorf("inbound-allow-any chain missing accept to guest\n%s", out)
	}
}

// TestParseCIDRList asserts the CIDR allowlist parser accepts valid v4/v6 CIDRs,
// splits them by family, and rejects malformed entries (fail-closed).
func TestParseCIDRList(t *testing.T) {
	v4, v6, err := ParseCIDRList([]string{"10.0.0.0/8", "2001:db8::/32", "192.168.0.0/16"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(v4) != 2 || len(v6) != 1 {
		t.Fatalf("expected 2 v4 and 1 v6, got %d v4 %d v6", len(v4), len(v6))
	}
	if _, _, err := ParseCIDRList([]string{"not-a-cidr"}); err == nil {
		t.Error("expected error for malformed CIDR")
	}
	if _, _, err := ParseCIDRList([]string{"10.0.0.1"}); err == nil {
		t.Error("expected error for bare IP without prefix")
	}
}
