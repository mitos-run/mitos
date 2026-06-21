package netconf

import (
	"fmt"
	"net"
	"strings"

	"mitos.run/mitos/api/v1alpha1"
)

// This file holds the spec-based egress/ingress chain renderers that express
// the full network-posture model (docs/networking.md, issue #219): the legacy
// egress-deny + IP:port/name allowlist, plus block_network total-deny, CIDR
// allowlists, deny-by-default inbound, and the per-sandbox egress byte counter
// the metering pipeline (#211) reads. All output is deterministic for the same
// input and is a string of `nft` statements, so it is unit-testable on any
// platform without running nft (the real packet enforcement is KVM-gated).

// SandboxEgressCounterName returns the per-sandbox nftables counter object name
// for a tap. The chain increments it on every egress packet sourced by the
// guest, so the metering pipeline (#211) can read this sandbox's egress bytes
// from `nft list counter inet <table> <name>`. Named like SandboxChainName so a
// tap's chain, set, and counter share a stable, collision-free identity.
func SandboxEgressCounterName(tap string) string {
	return "sb_" + tap + "_egress"
}

// SandboxPolicy is the resolved per-sandbox network posture the network Manager
// applies: the egress default verdict, the static IP:port allowlist, the CIDR
// allowlists, block_network, the inbound policy, and whether the egress byte
// counter is wired. It is the single value threaded from the engine into the
// Manager so the Setup signature does not grow one parameter per dimension.
type SandboxPolicy struct {
	Egress       v1alpha1.EgressPolicy
	Allow        []HostPort
	AllowCIDRs   []string
	BlockNetwork bool
	Inbound      v1alpha1.InboundPolicy
	InboundCIDRs []string
	// Counter requests the per-sandbox egress byte counter (#211 metering seam).
	Counter bool
}

// ChainSpec is the full per-sandbox egress chain input. The zero value renders a
// deny-by-default chain with no allows and no counter, which is the secure
// default for an untrusted sandbox.
type ChainSpec struct {
	// Tap is the sandbox's tap device; it is the dispatch key and chain suffix.
	Tap string
	// GuestIP is the guest source address every accept and the metadata block are
	// saddr-pinned to (anti-spoof defense in depth).
	GuestIP net.IP
	// Egress is the default verdict for traffic that matches no allow rule: drop
	// under EgressDeny (the secure default), accept under EgressAllow. Inert when
	// BlockNetwork is set.
	Egress v1alpha1.EgressPolicy
	// Allow is the static IP:port allowlist (name entries are enforced by the DNS
	// proxy, not here).
	Allow []HostPort
	// AllowCIDRs are pre-parsed v4 and v6 destination CIDR blocks to accept
	// (Modal outbound_cidr_allowlist). Use ParseCIDRList to build them.
	AllowCIDRsV4 []*net.IPNet
	AllowCIDRsV6 []*net.IPNet
	// ResolverIP is the controlled DNS resolver the chain allows on port 53. Nil
	// omits the DNS allow rule.
	ResolverIP net.IP
	// BlockNetwork drops ALL egress regardless of Egress and the allowlists: the
	// total-deny knob (Modal block_network=True). When true, no accept is emitted
	// and the chain drops every guest-sourced packet (v4 and v6).
	BlockNetwork bool
	// Counter, when true, emits a per-sandbox egress byte counter and a counting
	// rule at the top of the chain so the metering pipeline (#211) can read this
	// sandbox's egress bytes. Opt-in so the legacy chain shape is unchanged by
	// default.
	Counter bool

	// AllowCIDRs is the raw CIDR allowlist as written in the policy. When set and
	// the pre-parsed slices are empty, RenderSandboxChainSpec parses it (ignoring
	// malformed entries, which the caller is expected to have validated). Prefer
	// setting AllowCIDRsV4/V6 directly from ParseCIDRList so a malformed entry
	// fails the call before any datapath is touched.
	AllowCIDRs []string
}

// RenderSandboxChainSpec renders the add block for ONE sandbox's egress chain
// from a full ChainSpec. It is the superset of the legacy RenderSandboxChain:
// the metadata block, established/related accept, the IP:port allowlist, the
// CIDR allowlist, the DNS-to-resolver accept, the dynamic name-pin sets, and the
// terminal verdict, plus the optional egress counter and the block_network
// total-deny.
//
// Priority: BlockNetwork wins over everything (no accept survives; the chain
// drops v4 and v6). Otherwise the metadata block is unconditional, then the
// allows, then the Egress default verdict. Every v4 accept is saddr-pinned; the
// v6 path is family-scoped (the guest has no v6 source identity to pin against,
// so the v6 default-deny is the boundary).
func RenderSandboxChainSpec(spec ChainSpec) string {
	table := SharedTableName()
	chain := SandboxChainName(spec.Tap)
	dispatch := DispatchMapName()
	saddr := fmt.Sprintf("ip saddr %s", spec.GuestIP.String())

	var b strings.Builder
	// Regular chain: no hook, no policy. A regular chain's final verdict is a
	// verdict for the matched packet, not a hook-wide default.
	fmt.Fprintf(&b, "add chain inet %s %s\n", table, chain)

	// Per-sandbox egress counter (opt-in): declare the named counter and count
	// every guest-sourced packet at the TOP of the chain, before any verdict, so
	// it measures total egress (allowed and dropped) for this sandbox. The
	// metering pipeline reads it by name. saddr-pinned so it counts only this
	// guest's traffic.
	if spec.Counter {
		counter := SandboxEgressCounterName(spec.Tap)
		fmt.Fprintf(&b, "add counter inet %s %s\n", table, counter)
		fmt.Fprintf(&b, "add rule inet %s %s %s counter name %s\n", table, chain, saddr, counter)
	}

	// Unconditional cloud-metadata block: emitted BEFORE every accept so a guest
	// can never reach the IMDS endpoint and steal node IAM credentials, regardless
	// of egress policy or allowlist. Applies even under block_network (defense in
	// depth) and under EgressAllow.
	b.WriteString(RenderMetadataBlock(table, chain, spec.GuestIP))

	if spec.BlockNetwork {
		// Total deny: no accept of any kind. Established/related is also dropped
		// because block_network means the sandbox may never reach the network at
		// all, even for return traffic of a connection it somehow opened. Drop v4
		// and v6.
		fmt.Fprintf(&b, "add rule inet %s %s %s drop\n", table, chain, saddr)
		fmt.Fprintf(&b, "add rule inet %s %s meta nfproto ipv6 drop\n", table, chain)
		fmt.Fprintf(&b, "add element inet %s %s { %q : jump %s }\n", table, dispatch, spec.Tap, chain)
		return b.String()
	}

	fmt.Fprintf(&b, "add rule inet %s %s %s ct state established,related accept\n", table, chain, saddr)

	for _, hp := range spec.Allow {
		fmt.Fprintf(&b, "add rule inet %s %s %s ip daddr %s tcp dport %d accept\n",
			table, chain, saddr, hp.IP.String(), hp.Port)
	}

	// CIDR allowlist: accept egress whose destination IP is inside an allowed
	// block (Modal outbound_cidr_allowlist). v4 blocks are saddr-pinned; v6 blocks
	// are family-scoped (no v4 saddr applies to a v6 packet, matching the v6
	// dynamic-set posture).
	v4, v6 := spec.AllowCIDRsV4, spec.AllowCIDRsV6
	if len(v4) == 0 && len(v6) == 0 && len(spec.AllowCIDRs) > 0 {
		// Best-effort parse for callers that pass raw entries; malformed entries
		// are skipped here (callers should pre-validate via ParseCIDRList).
		v4, v6, _ = ParseCIDRList(spec.AllowCIDRs)
	}
	for _, c := range v4 {
		fmt.Fprintf(&b, "add rule inet %s %s %s ip daddr %s accept\n", table, chain, saddr, c.String())
	}
	for _, c := range v6 {
		fmt.Fprintf(&b, "add rule inet %s %s ip6 daddr %s accept\n", table, chain, c.String())
	}

	if spec.ResolverIP != nil {
		fmt.Fprintf(&b, "add rule inet %s %s %s ip daddr %s udp dport 53 accept\n",
			table, chain, saddr, spec.ResolverIP.String())
		fmt.Fprintf(&b, "add rule inet %s %s %s ip daddr %s tcp dport 53 accept\n",
			table, chain, saddr, spec.ResolverIP.String())
	}

	// Dynamic allow set: the DNS proxy pins (resolved ip . port) elements with a
	// timeout here as it answers allowlisted name queries. Declare the set, then
	// accept traffic whose (daddr . dport) is currently present in it.
	set := SandboxAllowSetName(spec.Tap)
	fmt.Fprintf(&b, "add set inet %s %s { type ipv4_addr . inet_service ; flags timeout ; }\n", table, set)
	fmt.Fprintf(&b, "add rule inet %s %s %s ip daddr . tcp dport @%s accept\n", table, chain, saddr, set)

	// IPv6 dynamic allow set: the DNS proxy pins resolved AAAA addresses here.
	set6 := SandboxAllowSet6Name(spec.Tap)
	fmt.Fprintf(&b, "add set inet %s %s { type ipv6_addr . inet_service ; flags timeout ; }\n", table, set6)
	fmt.Fprintf(&b, "add rule inet %s %s ip6 daddr . tcp dport @%s accept\n", table, chain, set6)

	// Final verdict: drop under EgressDeny (the secure default), accept under
	// EgressAllow. Terminal within this regular chain for this packet only.
	final := "drop"
	if spec.Egress == v1alpha1.EgressAllow {
		final = "accept"
	}
	fmt.Fprintf(&b, "add rule inet %s %s %s %s\n", table, chain, saddr, final)
	fmt.Fprintf(&b, "add rule inet %s %s meta nfproto ipv6 %s\n", table, chain, final)

	// Dispatch element: route this tap into the chain.
	fmt.Fprintf(&b, "add element inet %s %s { %q : jump %s }\n", table, dispatch, spec.Tap, chain)
	return b.String()
}

// InputChainSpec is the full per-sandbox INPUT chain input: it governs
// unsolicited inbound connections to the guest (packets the guest receives, and
// packets the guest sends to a pod-LOCAL address). The secure default is
// deny-by-default (InboundDeny): only the controlled resolver on 53 is reachable
// and nothing dials into the guest. InboundAllow opens the guest to inbound,
// optionally narrowed to InboundCIDRs source blocks (Modal inbound_cidr_allowlist).
type InputChainSpec struct {
	Tap          string
	GuestIP      net.IP
	ResolverIP   net.IP
	Inbound      v1alpha1.InboundPolicy
	InboundCIDRs []string
}

// RenderSandboxInputChainSpec renders the per-tap input chain from a full spec.
// Under InboundDeny (the secure default) it reproduces the legacy chain: allow
// the guest to reach the resolver on 53, then drop every other guest-sourced
// pod-local packet. Under InboundAllow it additionally accepts unsolicited
// inbound to the guest IP, scoped to InboundCIDRs source blocks when given (else
// from any source).
func RenderSandboxInputChainSpec(spec InputChainSpec) string {
	table := SharedTableName()
	chain := SandboxInputChainName(spec.Tap)
	dispatch := InputDispatchMapName()
	saddr := fmt.Sprintf("ip saddr %s", spec.GuestIP.String())
	guestDaddr := fmt.Sprintf("ip daddr %s", spec.GuestIP.String())

	var b strings.Builder
	fmt.Fprintf(&b, "add chain inet %s %s\n", table, chain)
	if spec.ResolverIP != nil {
		fmt.Fprintf(&b, "add rule inet %s %s %s ip daddr %s udp dport 53 accept\n",
			table, chain, saddr, spec.ResolverIP.String())
		fmt.Fprintf(&b, "add rule inet %s %s %s ip daddr %s tcp dport 53 accept\n",
			table, chain, saddr, spec.ResolverIP.String())
	}

	// Inbound allow: accept unsolicited inbound to the guest. Deny-by-default
	// (the empty / InboundDeny case) emits nothing here, so inbound falls through
	// to the terminal drop below. Return traffic for the guest's own egress is
	// matched by the forward chain's established,related accept, so deny-by-default
	// inbound never breaks the guest's outbound flows.
	if spec.Inbound == v1alpha1.InboundAllow {
		if len(spec.InboundCIDRs) == 0 {
			fmt.Fprintf(&b, "add rule inet %s %s %s accept\n", table, chain, guestDaddr)
		} else {
			v4, v6, _ := ParseCIDRList(spec.InboundCIDRs)
			for _, c := range v4 {
				fmt.Fprintf(&b, "add rule inet %s %s %s ip saddr %s accept\n", table, chain, guestDaddr, c.String())
			}
			for _, c := range v6 {
				fmt.Fprintf(&b, "add rule inet %s %s ip6 daddr %s ip6 saddr %s accept\n",
					table, chain, spec.GuestIP.String(), c.String())
			}
		}
	}

	// Drop every other guest-sourced packet (deny-by-default for pod-local
	// destinations and any inbound not explicitly allowed above).
	fmt.Fprintf(&b, "add rule inet %s %s %s drop\n", table, chain, saddr)
	fmt.Fprintf(&b, "add element inet %s %s { %q : jump %s }\n", table, dispatch, spec.Tap, chain)
	return b.String()
}

// ParseCIDRList parses a raw CIDR allowlist into v4 and v6 *net.IPNet blocks,
// split by family so the renderer can emit `ip daddr` and `ip6 daddr` rules
// respectively. A malformed entry, or a bare IP without a prefix, fails the whole
// call (fail-closed: a sandbox never comes up with a partially parsed CIDR
// allowlist). An empty input returns empty slices and no error.
func ParseCIDRList(entries []string) (v4 []*net.IPNet, v6 []*net.IPNet, err error) {
	for _, e := range entries {
		_, ipnet, perr := net.ParseCIDR(strings.TrimSpace(e))
		if perr != nil {
			return nil, nil, fmt.Errorf("parse CIDR allow entry %q: %w", e, perr)
		}
		if ipnet.IP.To4() != nil {
			v4 = append(v4, ipnet)
		} else {
			v6 = append(v6, ipnet)
		}
	}
	return v4, v6, nil
}
