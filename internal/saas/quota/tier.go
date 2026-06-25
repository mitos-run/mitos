// Package quota is the abuse-control envelope for the hosted offering (issue
// #213): per-organization quotas, per-org and per-IP rate limiting, live
// concurrency and aggregate-resource caps, per-tier egress policy selection, and
// the kill-switch (org suspension). It plugs into the public gateway's
// QuotaEnforcer seam (internal/saas, issue #210) so a request is checked AFTER
// the key is authenticated and the org resolved, BEFORE it is forwarded to the
// control plane.
//
// This is the hard gate on public self-serve untrusted multi-tenancy: without a
// real enforcer the hosted cloud is a free crypto-mining and outbound-attack
// platform. The verifiable core (the tier model, the rate limiter, the
// tier->egress-policy mapping, and the suspension logic) is unit-tested and
// pure; the live multi-node enforcement and the automated abuse-detection
// signals are documented seams (the LiveUsage and the AbuseSignal interfaces).
//
// Security: this package never logs a key value or any secret. It logs org ids,
// op names, tier names, and counts only. Every denial maps to an apierr code the
// gateway already exposes (quota_exceeded, rate_limited, forbidden).
package quota

import (
	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/netconf"
)

// TierName identifies a plan/top-up level. Abuse gating is a prepaid ladder
// (Daytona-style): a brand-new anonymous-ish signup lands on the most restricted
// tier with the tightest concurrency, smallest sizes, and a deny-by-default
// egress posture; a verified, paid, or topped-up org climbs the ladder to wider
// limits. The ladder is the abuse lever: untrusted code never gets a wide
// blast radius until the org has paid its way up.
type TierName string

const (
	// TierFree is the default tier for a new, unverified signup. It is the
	// tightest: minimal concurrency, smallest sandbox size, and a deny-by-default
	// egress posture so untrusted code cannot reach the network at all without an
	// explicit allowlist. This is the anti-abuse floor.
	TierFree TierName = "free"
	// TierStarter is the first paid step: more concurrency and a minimal egress
	// allowlist, still well short of an unrestricted network.
	TierStarter TierName = "starter"
	// TierPro is an established, paid org: generous concurrency and aggregate caps
	// and, by default, open egress with the abuse-port block still in force.
	TierPro TierName = "pro"
)

// Tier is the resolved set of limits and the default egress posture for a plan
// level. Every limit is a hard ceiling the enforcer checks against live usage.
// A zero limit means the dimension is not capped for that tier (used only on the
// widest tiers); the free tier caps every dimension.
type Tier struct {
	Name TierName

	// MaxConcurrentSandboxes caps how many sandboxes the org may have running at
	// once. The single most important anti-abuse lever: it bounds the blast radius
	// of a compromised or malicious key.
	MaxConcurrentSandboxes int

	// MaxAggregateVCPUs, MaxAggregateMemBytes, and MaxAggregateStorageBytes cap the
	// org's total live footprint summed across all its running sandboxes, so an org
	// cannot evade the concurrency cap by running a few enormous sandboxes.
	MaxAggregateVCPUs        int32
	MaxAggregateMemBytes     int64
	MaxAggregateStorageBytes int64

	// MaxSandboxVCPUs, MaxSandboxMemBytes, and MaxSandboxStorageBytes cap a single
	// sandbox's size, so a free-tier org cannot request one giant VM.
	MaxSandboxVCPUs        int32
	MaxSandboxMemBytes     int64
	MaxSandboxStorageBytes int64

	// CreationRatePerMinute caps how many sandboxes the org may create per minute
	// (a creation-rate bucket, distinct from the API request rate). This throttles
	// a churn-attack that creates and destroys sandboxes to amortize the
	// concurrency cap.
	CreationRatePerMinute float64

	// APIRequestsPerMinute caps the org's API request rate at the gateway (a
	// request-rate bucket). This throttles brute-force and scraping.
	APIRequestsPerMinute float64

	// Egress is the DEFAULT network posture a sandbox in this tier gets unless the
	// org overrides it within what the tier allows. The free tier is
	// deny-by-default (block or a minimal allowlist); wider tiers open up. This is
	// policy SELECTION; the real packet enforcement is the #219 KVM datapath.
	Egress EgressTier
}

// EgressTier is a tier's default network posture, expressed at the policy-
// selection layer. It maps deterministically to a netconf.SandboxPolicy (the
// #219 enforcement primitive) via Policy. Three postures cover the ladder:
// blocked (no network at all), allowlist (deny-by-default with a minimal CIDR
// allowlist), and open (network allowed, but the abuse-port block still applies).
type EgressTier struct {
	// BlockNetwork drops ALL egress for the tier (the free-tier floor option).
	// When true the allowlist and Open are inert.
	BlockNetwork bool
	// Open, when true and BlockNetwork is false, allows egress by default
	// (EgressAllow). When false the tier is deny-by-default (EgressDeny): only the
	// AllowCIDRs are reachable.
	Open bool
	// AllowCIDRs is the tier's default minimal CIDR allowlist used under
	// deny-by-default. Empty under a fully blocked or fully open tier.
	AllowCIDRs []string
}

// DefaultTiers returns the built-in prepaid ladder. It is the policy table the
// enforcer resolves an org's tier against; a hosted deployment can override it,
// but these defaults are the safe anti-abuse baseline: free is deny-by-default
// with the tightest caps, and each step up widens the envelope.
func DefaultTiers() map[TierName]Tier {
	const (
		gib = int64(1) << 30
	)
	return map[TierName]Tier{
		TierFree: {
			Name:                     TierFree,
			MaxConcurrentSandboxes:   2,
			MaxAggregateVCPUs:        4,
			MaxAggregateMemBytes:     4 * gib,
			MaxAggregateStorageBytes: 20 * gib,
			MaxSandboxVCPUs:          2,
			MaxSandboxMemBytes:       2 * gib,
			MaxSandboxStorageBytes:   10 * gib,
			CreationRatePerMinute:    5,
			APIRequestsPerMinute:     60,
			// Free tier: deny-by-default. Untrusted code gets NO network unless the
			// org explicitly allowlists a destination within its tier. The minimal
			// allowlist is empty by default; a free org reaches nothing outbound.
			Egress: EgressTier{BlockNetwork: false, Open: false, AllowCIDRs: nil},
		},
		TierStarter: {
			Name:                     TierStarter,
			MaxConcurrentSandboxes:   10,
			MaxAggregateVCPUs:        20,
			MaxAggregateMemBytes:     40 * gib,
			MaxAggregateStorageBytes: 200 * gib,
			MaxSandboxVCPUs:          4,
			MaxSandboxMemBytes:       8 * gib,
			MaxSandboxStorageBytes:   50 * gib,
			CreationRatePerMinute:    30,
			APIRequestsPerMinute:     300,
			// Starter: still deny-by-default; the org adds its own allowlist within
			// this envelope.
			Egress: EgressTier{BlockNetwork: false, Open: false, AllowCIDRs: nil},
		},
		TierPro: {
			Name:                     TierPro,
			MaxConcurrentSandboxes:   100,
			MaxAggregateVCPUs:        400,
			MaxAggregateMemBytes:     800 * gib,
			MaxAggregateStorageBytes: 4000 * gib,
			MaxSandboxVCPUs:          16,
			MaxSandboxMemBytes:       64 * gib,
			MaxSandboxStorageBytes:   500 * gib,
			CreationRatePerMinute:    120,
			APIRequestsPerMinute:     1200,
			// Pro: open egress by default. The abuse-port block (SMTP and friends,
			// from issue #36) still applies via Policy regardless of Open, so even an
			// open tier cannot send mail-spam or hit the well-known abuse ports.
			Egress: EgressTier{Open: true},
		},
	}
}

// AbusePorts is the set of well-known abuse destination ports that are blocked
// for EVERY tier, even the fully open one (issue #36). It is dominated by
// outbound SMTP (25, 465, 587: mail spam is the classic free-sandbox abuse), plus
// the other ports a compromised sandbox is most often used to attack from.
var AbusePorts = []int{25, 465, 587}

// Policy maps a tier's EgressTier to the concrete netconf.SandboxPolicy the #219
// datapath enforces. This is the tier->egress-policy mapping: it is the single
// place the prepaid ladder is translated into a real network posture, so the
// mapping is unit-testable in isolation from the datapath.
//
//   - A blocked tier -> BlockNetwork (drop all egress).
//   - A deny-by-default tier -> EgressDeny with the tier's minimal CIDR allowlist.
//   - An open tier -> EgressAllow.
//
// The abuse-port block (issue #36) is NOT a field on netconf.SandboxPolicy; it is
// a fleet-wide drop the datapath applies to every sandbox before any tier allow
// (AbusePorts is the catalogue). It therefore applies under every posture this
// mapping selects, so even an open tier cannot reach SMTP. TierEgress pairs the
// selected SandboxPolicy with the abuse-port set so a caller threads both.
func (e EgressTier) Policy() netconf.SandboxPolicy {
	p := netconf.SandboxPolicy{
		// Inbound is deny-by-default for every tier: an untrusted sandbox is never
		// dialable from outside regardless of egress posture.
		Inbound: v1.InboundDeny,
	}
	switch {
	case e.BlockNetwork:
		p.BlockNetwork = true
	case e.Open:
		p.Egress = v1.EgressAllow
	default:
		p.Egress = v1.EgressDeny
		p.AllowCIDRs = append([]string(nil), e.AllowCIDRs...)
	}
	return p
}

// ResolvedEgress is the full per-tier network posture a sandbox launch threads
// into the datapath: the selected netconf.SandboxPolicy plus the fleet-wide
// abuse-port block. It is the complete output of the tier->egress-policy mapping.
type ResolvedEgress struct {
	Policy     netconf.SandboxPolicy
	BlockPorts []int
}

// ResolveEgress resolves a tier's full network posture (the policy plus the
// abuse-port block). This is the single call a sandbox-launch path makes to turn
// an org's tier into a #219 datapath configuration.
func (t Tier) ResolveEgress() ResolvedEgress {
	return ResolvedEgress{
		Policy:     t.Egress.Policy(),
		BlockPorts: append([]int(nil), AbusePorts...),
	}
}
