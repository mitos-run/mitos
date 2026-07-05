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

// TestMemSandboxControlCreateAssignsUniqueIDAndOrg asserts Create stamps the
// new sandbox with the caller's org and the requested template/sizing, and
// that two calls never collide on id.
func TestMemSandboxControlCreateAssignsUniqueIDAndOrg(t *testing.T) {
	ctx := context.Background()
	m := NewMemSandboxControl()
	a, err := m.Create(ctx, "orgA", CreateSandboxRequest{Template: "python", VCPUs: 2, MemGiB: 4})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if a.OrgID != "orgA" || a.Template != "python" || a.VCPUs != 2 || a.MemBytes != int64(4)<<30 {
		t.Fatalf("created view = %+v, want org/template/sizing to match the request", a)
	}
	b, err := m.Create(ctx, "orgA", CreateSandboxRequest{Template: "python", VCPUs: 1, MemGiB: 1})
	if err != nil {
		t.Fatalf("Create #2: %v", err)
	}
	if a.ID == b.ID {
		t.Fatalf("two Create calls returned the same id %q", a.ID)
	}
	// The created sandbox must actually be listed/gettable, not just returned.
	if got, err := m.Get(ctx, "orgA", a.ID); err != nil || got.ID != a.ID {
		t.Fatalf("Get(orgA, %s) = %+v, %v; want the created sandbox", a.ID, got, err)
	}
}

// TestMemSandboxControlForkRefusesCrossOrgSource asserts Fork refuses to fork
// a sandbox belonging to a different org.
func TestMemSandboxControlForkRefusesCrossOrgSource(t *testing.T) {
	ctx := context.Background()
	m := NewMemSandboxControl()
	m.Add(SandboxView{ID: "b1", OrgID: "orgB"})
	if _, err := m.Fork(ctx, "orgA", "b1", 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Fork(orgA, b1) err = %v, want ErrNotFound", err)
	}
}

// TestMemSandboxControlForkReturnsDistinctIDsOwnedByOrg asserts Fork creates
// exactly count new sandboxes, each with a unique id, owned by the caller's
// org, and independently gettable/terminable (they are first-class sandboxes,
// not just entries on the source).
func TestMemSandboxControlForkReturnsDistinctIDsOwnedByOrg(t *testing.T) {
	ctx := context.Background()
	m := NewMemSandboxControl()
	m.Add(SandboxView{ID: "src", OrgID: "orgA", Template: "python", VCPUs: 2})
	ids, err := m.Fork(ctx, "orgA", "src", 3)
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("Fork returned %d ids, want 3", len(ids))
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("duplicate fork id %q", id)
		}
		seen[id] = true
		v, err := m.Get(ctx, "orgA", id)
		if err != nil {
			t.Fatalf("Get(orgA, %s): %v", id, err)
		}
		if v.Template != "python" || v.VCPUs != 2 {
			t.Fatalf("fork child %+v did not inherit source template/sizing", v)
		}
	}
}

// TestMemSandboxControlExecRefusesCrossOrgAndReturnsScriptedResult asserts Exec
// is org-scoped and returns whatever was scripted via SetExecResult/SetExecErr.
func TestMemSandboxControlExecRefusesCrossOrgAndReturnsScriptedResult(t *testing.T) {
	ctx := context.Background()
	m := NewMemSandboxControl()
	m.Add(SandboxView{ID: "a1", OrgID: "orgA"})
	if _, err := m.Exec(ctx, "orgB", "a1", "echo hi", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Exec(orgB, a1) err = %v, want ErrNotFound", err)
	}
	m.SetExecResult("a1", ExecResult{Stdout: "hi\n", ExitCode: 0})
	res, err := m.Exec(ctx, "orgA", "a1", "echo hi", 0)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Stdout != "hi\n" || res.ExitCode != 0 {
		t.Fatalf("Exec result = %+v, want the scripted result", res)
	}
	m.SetExecErr("a1", ErrUnsupported)
	if _, err := m.Exec(ctx, "orgA", "a1", "echo hi", 0); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Exec after SetExecErr = %v, want ErrUnsupported", err)
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
