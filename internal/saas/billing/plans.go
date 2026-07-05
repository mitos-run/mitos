package billing

import "context"

// Plan identifies a billing plan/tier for an org. A plan gates hosted-only
// conveniences ONLY (SSO enforcement, SCIM, audit-sink streaming, extended
// audit retention); it never gates the engine itself, which stays Apache-2.0
// feature-complete for every self-hosted install regardless of plan (see
// EntitlementsFor's community-edition override).
type Plan string

const (
	// PlanFree is the default hosted plan: every hosted-only convenience is
	// off and the audit log retains 30 days of history.
	PlanFree Plan = "free"
	// PlanTeam is the paid, seat-priced plan: every hosted-only convenience is
	// on and the audit log retains 365 days of history.
	PlanTeam Plan = "team"
)

// Entitlements is the resolved set of plan-gated hosted conveniences for one
// org, derived from (Plan, edition) by EntitlementsFor. It is never stored
// directly: a plan or edition change takes effect immediately on the next
// resolution, with no migration.
type Entitlements struct {
	// SSOEnforced gates REQUIRING SSO login for the org (disabling the
	// email/password and social paths). Basic SSO login itself is free on
	// every edition and plan; this flag gates only the enforcement toggle.
	SSOEnforced bool `json:"ssoEnforced"`
	// SCIM gates SCIM user provisioning for the org.
	SCIM bool `json:"scim"`
	// AuditStreaming gates forwarding audit events to the org's configured
	// sinks (webhook, s3, splunk, datadog; see console.SinkRegistry). The
	// audit log itself is always recorded and always readable in the
	// console on every plan; this only gates the best-effort sink dispatch.
	AuditStreaming bool `json:"auditStreaming"`
	// AuditRetentionDays is the audit-log retention window in days. 0 means
	// unlimited (keep forever), matching the console.DataRetentionPolicy
	// "zero value = keep forever" convention.
	AuditRetentionDays int `json:"auditRetentionDays"`
	// SeatPriceCents is the plan's illustrative per-seat monthly price, in
	// cents, for display only; nothing in this slice charges it automatically.
	SeatPriceCents int64 `json:"seatPriceCents"`
}

// ILLUSTRATIVE, CONFIGURABLE plan constants (the no-unverified-claims rule):
// these are not published prices, they mirror docs/saas/pricing.md's shape.
const (
	hostedFreeAuditRetentionDays = 30
	hostedTeamAuditRetentionDays = 365
	// TeamSeatPriceCents is the illustrative Team plan seat price: $20/user/month.
	TeamSeatPriceCents int64 = 2000
)

// EntitlementsFor resolves the entitlements for a plan on a given deployment
// edition. edition is the same string console.Capabilities.Edition carries
// ("community" or "hosted").
//
// The self-hosted COMMUNITY edition keeps ALL features on with unlimited
// retention, no matter what plan is passed in: mitos is Apache-2.0 and
// self-host is never behind a paywall (CLAUDE.md operating principle #7).
// Only the hosted edition gates by plan.
func EntitlementsFor(plan Plan, edition string) Entitlements {
	if edition == "community" {
		return Entitlements{
			SSOEnforced:        true,
			SCIM:               true,
			AuditStreaming:     true,
			AuditRetentionDays: 0,
			SeatPriceCents:     0,
		}
	}
	switch plan {
	case PlanTeam:
		return Entitlements{
			SSOEnforced:        true,
			SCIM:               true,
			AuditStreaming:     true,
			AuditRetentionDays: hostedTeamAuditRetentionDays,
			SeatPriceCents:     TeamSeatPriceCents,
		}
	default:
		// PlanFree and any unrecognized plan string fail CLOSED to the Free
		// entitlements: an unknown plan never grants a hosted convenience the
		// org has not paid for.
		return Entitlements{
			SSOEnforced:        false,
			SCIM:               false,
			AuditStreaming:     false,
			AuditRetentionDays: hostedFreeAuditRetentionDays,
			SeatPriceCents:     0,
		}
	}
}

// PlanSource resolves an org's current plan. It is the seam between billing
// state and every plan-gated decision in the console (capabilities
// advertisement, sink-creation gating); a real subscription/payment
// integration is a documented follow-up behind this interface.
type PlanSource interface {
	GetPlan(ctx context.Context, orgID string) (Plan, error)
}

// StaticPlanSource is the default PlanSource: every org resolves to Default
// unless its id is in Teams, an early manual-grant mechanism (the
// MITOS_CONSOLE_TEAM_ORGS env override wired in cmd/console) used until a real
// subscription/payment integration exists. Safe for concurrent read-only use
// (Teams is never mutated after construction).
type StaticPlanSource struct {
	// Default is the plan returned for an org not present in Teams.
	Default Plan
	// Teams is the set of org ids manually granted PlanTeam.
	Teams map[string]bool
}

// NewStaticPlanSource returns a StaticPlanSource defaulting every org to
// PlanFree, with the given org ids granted PlanTeam.
func NewStaticPlanSource(teamOrgIDs []string) *StaticPlanSource {
	teams := make(map[string]bool, len(teamOrgIDs))
	for _, id := range teamOrgIDs {
		if id != "" {
			teams[id] = true
		}
	}
	return &StaticPlanSource{Default: PlanFree, Teams: teams}
}

// GetPlan returns PlanTeam for an org in the manual-grant allowlist, otherwise
// s.Default (falling back to PlanFree if Default was left unset).
func (s *StaticPlanSource) GetPlan(_ context.Context, orgID string) (Plan, error) {
	if s.Teams[orgID] {
		return PlanTeam, nil
	}
	if s.Default == "" {
		return PlanFree, nil
	}
	return s.Default, nil
}
