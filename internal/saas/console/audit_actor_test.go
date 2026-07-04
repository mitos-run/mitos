package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mitos.run/mitos/internal/saas"
)

// TestAuditRecordsActorNameFromEmailFallback asserts the audit() helper looks
// up the actor's account once and fills ActorName; newFixture's alice/bob
// accounts never set a DisplayName, so the fallback to Email is what proves
// the lookup actually ran (an empty ActorName would also "pass" a lookup that
// silently no-ops).
func TestAuditRecordsActorNameFromEmailFallback(t *testing.T) {
	f := newFixture(t)
	w := f.req(t, "DELETE", "/console/sandboxes/sb-alice-1", "", f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusOK {
		t.Fatalf("terminate status = %d body=%s", w.Code, w.Body.String())
	}
	events, _ := f.audit.List(context.Background(), f.aliceOrg)
	if len(events) == 0 {
		t.Fatal("expected an audit event")
	}
	ev := events[0]
	if ev.ActorName != "alice@example.com" {
		t.Errorf("ActorName = %q, want alice@example.com", ev.ActorName)
	}
	if ev.ActorType != "user" {
		t.Errorf("ActorType = %q, want user", ev.ActorType)
	}
	if ev.TargetType != "sandbox" {
		t.Errorf("TargetType = %q, want sandbox", ev.TargetType)
	}
}

// TestAuditActorLookupFailureLeavesActorNameEmpty asserts a lookup miss (an
// actor id the account store does not know) never fails the recorded action:
// the event is still recorded, just with an empty ActorName.
func TestAuditActorLookupFailureLeavesActorNameEmpty(t *testing.T) {
	f := newFixture(t)
	c := f.con
	c.audit(context.Background(), AuditEvent{
		OrgID:   f.aliceOrg,
		ActorID: "no-such-account",
		Action:  "key.create",
		Target:  "k1",
	})
	events, err := f.audit.List(context.Background(), f.aliceOrg)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var got *AuditEvent
	for i := range events {
		if events[i].ActorID == "no-such-account" {
			got = &events[i]
			break
		}
	}
	if got == nil {
		t.Fatal("expected the event to be recorded even though the actor lookup failed")
	}
	if got.ActorName != "" {
		t.Errorf("ActorName = %q, want empty on lookup failure", got.ActorName)
	}
	if got.ActorType != "user" {
		t.Errorf("ActorType = %q, want user (defaulted)", got.ActorType)
	}
}

// TestKeyCreateAndRevokeAuditCarryKeyNameAndType asserts key.create and
// key.revoke both set TargetType "key" and TargetName to the key's own name
// (create has it in hand; revoke resolves it via ListKeys before revoking).
func TestKeyCreateAndRevokeAuditCarryKeyNameAndType(t *testing.T) {
	f := newFixture(t)
	body := `{"name":"ci-key","scopes":["sandboxes"],"ttl_seconds":0}`
	w := f.req(t, "POST", "/console/keys", body, f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", w.Code, w.Body.String())
	}
	var created struct {
		Key struct{ ID string } `json:"key"`
	}
	decode(t, w, &created)

	events, _ := f.audit.List(context.Background(), f.aliceOrg)
	var createEv *AuditEvent
	for i := range events {
		if events[i].Action == "key.create" && events[i].Target == created.Key.ID {
			createEv = &events[i]
			break
		}
	}
	if createEv == nil {
		t.Fatalf("no key.create event for %s in %+v", created.Key.ID, events)
	}
	if createEv.TargetType != "key" || createEv.TargetName != "ci-key" {
		t.Errorf("key.create TargetType/TargetName = %q/%q, want key/ci-key", createEv.TargetType, createEv.TargetName)
	}

	w2 := f.req(t, "POST", "/console/keys/"+created.Key.ID+"/revoke", "", f.aliceAcct, f.aliceOrg)
	if w2.Code != http.StatusOK {
		t.Fatalf("revoke status = %d body=%s", w2.Code, w2.Body.String())
	}
	events2, _ := f.audit.List(context.Background(), f.aliceOrg)
	var revokeEv *AuditEvent
	for i := range events2 {
		if events2[i].Action == "key.revoke" && events2[i].Target == created.Key.ID {
			revokeEv = &events2[i]
			break
		}
	}
	if revokeEv == nil {
		t.Fatalf("no key.revoke event for %s in %+v", created.Key.ID, events2)
	}
	if revokeEv.TargetType != "key" || revokeEv.TargetName != "ci-key" {
		t.Errorf("key.revoke TargetType/TargetName = %q/%q, want key/ci-key", revokeEv.TargetType, revokeEv.TargetName)
	}
}

// TestProjectCreateAuditCarriesProjectName asserts project.create sets
// TargetType "project" and TargetName to the project's own name.
func TestProjectCreateAuditCarriesProjectName(t *testing.T) {
	f := newFixture(t)
	w := f.req(t, "POST", "/console/projects", `{"name":"Payments","description":"d"}`, f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusCreated {
		t.Fatalf("create project status = %d body=%s", w.Code, w.Body.String())
	}
	events, _ := f.audit.List(context.Background(), f.aliceOrg)
	if len(events) == 0 || events[0].Action != "project.create" {
		t.Fatalf("expected a project.create event, got %+v", events)
	}
	if events[0].TargetType != "project" || events[0].TargetName != "Payments" {
		t.Errorf("TargetType/TargetName = %q/%q, want project/Payments", events[0].TargetType, events[0].TargetName)
	}
}

// TestMemberRoleAuditCarriesTargetAccountName asserts member.role resolves the
// TARGET account's own name (not the actor's) via a best-effort lookup, so the
// audit sentence can read "changed Carol's role" rather than a bare account id.
// rf.targetAcct (carol) is the member being promoted; rf.ownerAcct (alice) is
// the actor making the change.
func TestMemberRoleAuditCarriesTargetAccountName(t *testing.T) {
	rf := newRoleFixture(t)
	if _, err := rf.accounts.UpdateProfile(context.Background(), rf.targetAcct, saas.ProfileUpdate{DisplayName: "Carol Q"}); err != nil {
		t.Fatalf("update profile: %v", err)
	}

	body := `{"role":"admin"}`
	r := httptest.NewRequest(http.MethodPost, "/console/members/"+rf.targetAcct+"/role", strings.NewReader(body))
	r = r.WithContext(WithCaller(r.Context(), rf.ownerAcct, rf.orgID))
	w := httptest.NewRecorder()
	rf.con.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("set role status = %d body=%s", w.Code, w.Body.String())
	}

	events, err := rf.con.deps.Audit.List(context.Background(), rf.orgID)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	var ev *AuditEvent
	for i := range events {
		if events[i].Action == "member.role" {
			ev = &events[i]
			break
		}
	}
	if ev == nil {
		t.Fatalf("no member.role event in %+v", events)
	}
	if ev.TargetType != "member" || ev.TargetName != "Carol Q" {
		t.Errorf("TargetType/TargetName = %q/%q, want member/Carol Q", ev.TargetType, ev.TargetName)
	}
	if ev.ActorName != "role-alice@example.com" {
		t.Errorf("ActorName = %q, want role-alice@example.com", ev.ActorName)
	}
}
