package console

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestMemSandboxControlIsOrgScoped asserts the sandbox control seam enforces
// org-scoping at the seam: List returns only the org's sandboxes, and Get /
// Terminate report a foreign sandbox as ErrNotFound (indistinguishable from
// missing) so a caller cannot probe or act on another org's sandbox.
func TestMemSandboxControlIsOrgScoped(t *testing.T) {
	ctx := context.Background()
	m := NewMemSandboxControl()
	m.Add(SandboxView{ID: "a1", OrgID: "orgA"})
	m.Add(SandboxView{ID: "b1", OrgID: "orgB"})

	list, _ := m.List(ctx, "orgA")
	if len(list) != 1 || list[0].ID != "a1" {
		t.Fatalf("List(orgA) = %+v, want only a1", list)
	}
	if _, err := m.Get(ctx, "orgA", "b1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(orgA, b1) err = %v, want ErrNotFound", err)
	}
	if err := m.Terminate(ctx, "orgA", "b1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Terminate(orgA, b1) err = %v, want ErrNotFound", err)
	}
	// b1 must survive an attempted cross-org terminate.
	if _, err := m.Get(ctx, "orgB", "b1"); err != nil {
		t.Errorf("b1 was terminated cross-org: %v", err)
	}
}

// TestMemAuditLogIsOrgScopedAndReverseChronological asserts the audit log never
// returns another org's events and orders most-recent-first. limit 0 means
// "use the default", which is well above this test's two events.
func TestMemAuditLogIsOrgScopedAndReverseChronological(t *testing.T) {
	ctx := context.Background()
	m := NewMemAuditLog()
	t0 := time.Unix(0, 0)
	_ = m.Record(ctx, AuditEvent{OrgID: "orgA", Action: "first", At: t0})
	_ = m.Record(ctx, AuditEvent{OrgID: "orgA", Action: "second", At: t0.Add(time.Second)})
	_ = m.Record(ctx, AuditEvent{OrgID: "orgB", Action: "other", At: t0})

	got, _ := m.List(ctx, "orgA", 0)
	if len(got) != 2 || got[0].Action != "second" || got[1].Action != "first" {
		t.Fatalf("List(orgA) = %+v, want [second, first]", got)
	}
	for _, e := range got {
		if e.OrgID != "orgA" {
			t.Errorf("foreign org event leaked: %+v", e)
		}
	}
}

// TestMemAuditLogListRespectsLimit asserts a caller-supplied limit truncates
// to the most recent N events, matching PgAuditLog's LIMIT semantics.
func TestMemAuditLogListRespectsLimit(t *testing.T) {
	ctx := context.Background()
	m := NewMemAuditLog()
	t0 := time.Unix(0, 0)
	for i := 0; i < 5; i++ {
		_ = m.Record(ctx, AuditEvent{OrgID: "orgA", Action: string(rune('a' + i)), At: t0.Add(time.Duration(i) * time.Second)})
	}
	got, err := m.List(ctx, "orgA", 2)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 || got[0].Action != "e" || got[1].Action != "d" {
		t.Fatalf("List(orgA, 2) = %+v, want the 2 most recent events [e, d]", got)
	}
}

// TestMemAuditLogCapsPerOrgHistory asserts Record drops the oldest event once
// an org crosses maxAuditEventsPerOrg, so the in-memory fallback cannot grow
// without bound on a long-running dev/self-host process without Postgres
// configured.
func TestMemAuditLogCapsPerOrgHistory(t *testing.T) {
	ctx := context.Background()
	m := NewMemAuditLog()
	t0 := time.Unix(0, 0)
	for i := 0; i < maxAuditEventsPerOrg+10; i++ {
		_ = m.Record(ctx, AuditEvent{OrgID: "orgA", Action: "ev", At: t0.Add(time.Duration(i) * time.Second)})
	}
	got, err := m.List(ctx, "orgA", maxAuditEventsPerOrg+100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != maxAuditEventsPerOrg {
		t.Fatalf("stored events = %d, want capped at %d", len(got), maxAuditEventsPerOrg)
	}
	// The most recent event must be the LAST one recorded (the 10 oldest were
	// dropped, not the 10 newest).
	want := t0.Add(time.Duration(maxAuditEventsPerOrg+9) * time.Second)
	if !got[0].At.Equal(want) {
		t.Errorf("newest retained event At = %v, want %v (oldest events should be dropped first)", got[0].At, want)
	}
}

// TestMemTemplateListerIsOrgScoped asserts the template seam returns only the
// org's templates.
func TestMemTemplateListerIsOrgScoped(t *testing.T) {
	ctx := context.Background()
	m := NewMemTemplateLister()
	m.Add(TemplateView{Name: "a", OrgID: "orgA"})
	m.Add(TemplateView{Name: "b", OrgID: "orgB"})
	got, _ := m.List(ctx, "orgA")
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("List(orgA) = %+v, want only a", got)
	}
}
