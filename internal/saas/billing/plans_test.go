package billing

import (
	"context"
	"testing"
)

// TestEntitlementsForCommunityAlwaysFull asserts the self-hosted community
// edition returns every entitlement enabled with unlimited retention (0 days,
// the DataRetentionPolicy "keep forever" convention) REGARDLESS of the
// resolved plan: the Apache-2.0 engine is never gated by a plan, only the
// hosted conveniences are.
func TestEntitlementsForCommunityAlwaysFull(t *testing.T) {
	for _, plan := range []Plan{PlanFree, PlanTeam, Plan("unknown")} {
		got := EntitlementsFor(plan, "community")
		want := Entitlements{
			SSOEnforced:        true,
			SCIM:               true,
			AuditStreaming:     true,
			AuditRetentionDays: 0,
			SeatPriceCents:     0,
		}
		if got != want {
			t.Errorf("EntitlementsFor(%q, community) = %+v, want %+v", plan, got, want)
		}
	}
}

// TestEntitlementsForHostedFree asserts the hosted Free plan gates every
// hosted-only convenience off and caps retention at 30 days.
func TestEntitlementsForHostedFree(t *testing.T) {
	got := EntitlementsFor(PlanFree, "hosted")
	want := Entitlements{
		SSOEnforced:        false,
		SCIM:               false,
		AuditStreaming:     false,
		AuditRetentionDays: 30,
		SeatPriceCents:     0,
	}
	if got != want {
		t.Errorf("EntitlementsFor(free, hosted) = %+v, want %+v", got, want)
	}
}

// TestEntitlementsForHostedTeam asserts the hosted Team plan turns on every
// gated convenience, extends retention to 365 days, and carries the $20/user
// seat price.
func TestEntitlementsForHostedTeam(t *testing.T) {
	got := EntitlementsFor(PlanTeam, "hosted")
	want := Entitlements{
		SSOEnforced:        true,
		SCIM:               true,
		AuditStreaming:     true,
		AuditRetentionDays: 365,
		SeatPriceCents:     2000,
	}
	if got != want {
		t.Errorf("EntitlementsFor(team, hosted) = %+v, want %+v", got, want)
	}
}

// TestEntitlementsForHostedUnknownPlanFallsBackToFree asserts an unrecognized
// plan string on a hosted deployment fails CLOSED to the Free entitlements
// rather than granting a hosted convenience nobody paid for.
func TestEntitlementsForHostedUnknownPlanFallsBackToFree(t *testing.T) {
	got := EntitlementsFor(Plan("bogus"), "hosted")
	want := EntitlementsFor(PlanFree, "hosted")
	if got != want {
		t.Errorf("EntitlementsFor(bogus, hosted) = %+v, want the Free entitlements %+v", got, want)
	}
}

// TestStaticPlanSourceDefaultsToFree asserts an org not in the team allowlist
// resolves to PlanFree.
func TestStaticPlanSourceDefaultsToFree(t *testing.T) {
	src := NewStaticPlanSource(nil)
	plan, err := src.GetPlan(context.Background(), "org-unknown")
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	if plan != PlanFree {
		t.Errorf("plan = %q, want free", plan)
	}
}

// TestStaticPlanSourceGrantsListedOrgsTeam asserts the manual-grant allowlist
// (the MITOS_CONSOLE_TEAM_ORGS wiring) resolves a listed org id to PlanTeam
// and leaves every other org on the default.
func TestStaticPlanSourceGrantsListedOrgsTeam(t *testing.T) {
	src := NewStaticPlanSource([]string{"org-a", "org-b"})

	for _, orgID := range []string{"org-a", "org-b"} {
		plan, err := src.GetPlan(context.Background(), orgID)
		if err != nil {
			t.Fatalf("GetPlan(%s): %v", orgID, err)
		}
		if plan != PlanTeam {
			t.Errorf("GetPlan(%s) = %q, want team", orgID, plan)
		}
	}

	plan, err := src.GetPlan(context.Background(), "org-c")
	if err != nil {
		t.Fatalf("GetPlan(org-c): %v", err)
	}
	if plan != PlanFree {
		t.Errorf("GetPlan(org-c) = %q, want free (not in the allowlist)", plan)
	}
}
