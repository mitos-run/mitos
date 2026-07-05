package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/billing"
)

// TestCapabilitiesDefaultPlanIsFreeWithCommunityEntitlements asserts a Console
// built without an explicit Capabilities or Plans seam advertises the
// community-edition default: plan "free", but entitlements ALL true with
// unlimited retention (the self-host-keeps-everything override), matching
// EntitlementsFor(free, community).
func TestCapabilitiesDefaultPlanIsFreeWithCommunityEntitlements(t *testing.T) {
	c := New(Deps{})
	_, caps := getCaps(t, c)
	if caps.Plan != billing.PlanFree {
		t.Errorf("plan = %q, want free", caps.Plan)
	}
	want := billing.EntitlementsFor(billing.PlanFree, "community")
	if caps.Entitlements != want {
		t.Errorf("entitlements = %+v, want %+v", caps.Entitlements, want)
	}
}

// TestCapabilitiesResolvesPlanForCallerOrg asserts that when the request
// carries an authenticated org context, handleCapabilities re-resolves
// Plan/Entitlements for THAT org via Deps.Plans, rather than serving the
// boot-time default: a hosted deployment with the org manually granted Team
// must see the Team entitlements, and a different org on the same deployment
// must still see Free.
func TestCapabilitiesResolvesPlanForCallerOrg(t *testing.T) {
	c := New(Deps{
		Capabilities: Capabilities{Edition: "hosted", Ownership: "hosted"},
		Plans:        billing.NewStaticPlanSource([]string{"org-team"}),
	})

	hit := func(orgID string) Capabilities {
		r := httptest.NewRequest("GET", "/console/capabilities", nil)
		r = r.WithContext(WithCaller(context.Background(), "acct-1", orgID))
		w := httptest.NewRecorder()
		c.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
		}
		var caps Capabilities
		decode(t, w, &caps)
		return caps
	}

	teamCaps := hit("org-team")
	if teamCaps.Plan != billing.PlanTeam {
		t.Errorf("org-team plan = %q, want team", teamCaps.Plan)
	}
	if want := billing.EntitlementsFor(billing.PlanTeam, "hosted"); teamCaps.Entitlements != want {
		t.Errorf("org-team entitlements = %+v, want %+v", teamCaps.Entitlements, want)
	}

	freeCaps := hit("org-other")
	if freeCaps.Plan != billing.PlanFree {
		t.Errorf("org-other plan = %q, want free", freeCaps.Plan)
	}
	if want := billing.EntitlementsFor(billing.PlanFree, "hosted"); freeCaps.Entitlements != want {
		t.Errorf("org-other entitlements = %+v, want %+v", freeCaps.Entitlements, want)
	}
}

// TestCapabilitiesUnauthenticatedRequestKeepsBootDefault asserts an
// unauthenticated GET /console/capabilities (no org context, the pre-login
// path the SPA relies on) is served the boot-time default plan, never an
// error, so the pre-auth SPA boot never breaks.
func TestCapabilitiesUnauthenticatedRequestKeepsBootDefault(t *testing.T) {
	c := New(Deps{
		Capabilities: Capabilities{Edition: "hosted", Ownership: "hosted"},
		Plans:        billing.NewStaticPlanSource([]string{"org-team"}),
	})
	w, caps := getCaps(t, c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if caps.Plan != billing.PlanFree {
		t.Errorf("unauthenticated plan = %q, want the boot-time default (free)", caps.Plan)
	}
}

// --- Sink-creation gating on the AuditStreaming entitlement ---

// gatedConsoleFixture builds a Console with a real AccountService (an owner
// signed up so PermManageSettings resolves) plus the given edition and Plans
// seam, and returns the caller's account/org ids to authenticate as.
func gatedConsoleFixture(t *testing.T, edition string, plans billing.PlanSource) (c *Console, acctID, orgID string) {
	t.Helper()
	store := saas.NewMemStore()
	keys := saas.NewKeyService(store)
	accounts := saas.NewAccountService(store, keys)
	owner, org, err := accounts.SignUp(context.Background(), "plan-gate-owner@example.com")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	c = New(Deps{
		Accounts:     accounts,
		Capabilities: Capabilities{Edition: edition, Ownership: edition},
		Plans:        plans,
	})
	return c, owner.ID, org.ID
}

func postSink(c *Console, acctID, orgID string) *httptest.ResponseRecorder {
	body := `{"type":"webhook","endpoint":"https://example.com/hook"}`
	r := httptest.NewRequest("POST", "/console/audit/sinks", strings.NewReader(body)).
		WithContext(WithCaller(context.Background(), acctID, orgID))
	w := httptest.NewRecorder()
	c.ServeHTTP(w, r)
	return w
}

// TestCreateSinkGatedForHostedFreePlan asserts that a hosted org on the Free
// plan (the default, no manual Team grant) gets a 402 naming the Team plan
// when it tries to create an audit sink, and that no sink is actually stored.
func TestCreateSinkGatedForHostedFreePlan(t *testing.T) {
	c, acctID, orgID := gatedConsoleFixture(t, "hosted", billing.NewStaticPlanSource(nil))
	w := postSink(c, acctID, orgID)
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402; body=%s", w.Code, w.Body.String())
	}
	var body struct {
		Error struct {
			Code        string `json:"code"`
			Message     string `json:"message"`
			Remediation string `json:"remediation"`
		} `json:"error"`
	}
	decode(t, w, &body)
	if body.Error.Code == "" {
		t.Error("error code must not be empty")
	}
	if body.Error.Remediation == "" {
		t.Error("remediation must not be empty")
	}
	if !strings.Contains(strings.ToLower(body.Error.Message+body.Error.Remediation), "team") {
		t.Errorf("gated error must name the Team plan: %+v", body.Error)
	}
	if sinks := c.deps.Sinks.List(context.Background(), orgID); len(sinks) != 0 {
		t.Errorf("sinks = %v, want none created when gated", sinks)
	}
}

// TestCreateSinkAllowedForHostedTeamPlan asserts a hosted org manually granted
// Team can create an audit sink.
func TestCreateSinkAllowedForHostedTeamPlan(t *testing.T) {
	c, acctID, orgID := gatedConsoleFixture(t, "hosted", nil) // set below once org id is known
	c.deps.Plans = billing.NewStaticPlanSource([]string{orgID})
	w := postSink(c, acctID, orgID)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	if sinks := c.deps.Sinks.List(context.Background(), orgID); len(sinks) != 1 {
		t.Errorf("sinks = %v, want exactly one created", sinks)
	}
}

// TestCreateSinkUngatedForCommunityEdition asserts the self-hosted community
// edition NEVER gates sink creation, even with no Plans grant at all: the
// engine keeps every feature (Apache-2.0).
func TestCreateSinkUngatedForCommunityEdition(t *testing.T) {
	c, acctID, orgID := gatedConsoleFixture(t, "community", nil)
	w := postSink(c, acctID, orgID)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (community must never be gated); body=%s", w.Code, w.Body.String())
	}
}
