// Handler-level tests for the operate verbs (create, fork, exec, live logs)
// added in sandbox_ops.go: org-scoping (a cross-org sandbox id is refused),
// server-side bounds enforcement (vcpus/mem/count/timeout), audit events (and
// that exec's audit detail never carries more than a preview of the command),
// and the SSE log-stream handler's flush/heartbeat/ctx-cancel behavior.
package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func (f *fixture) reqBody(t *testing.T, method, target, body, acct, org string) *httptest.ResponseRecorder {
	t.Helper()
	return f.req(t, method, target, body, acct, org)
}

// --- Create ---

func TestCreateSandboxSucceedsAndAudits(t *testing.T) {
	f := newFixture(t)
	w := f.reqBody(t, "POST", "/console/sandboxes", `{"template":"alice-tmpl","vcpus":2,"mem_gib":4}`, f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var sb SandboxView
	decode(t, w, &sb)
	if sb.OrgID != f.aliceOrg || sb.Template != "alice-tmpl" || sb.VCPUs != 2 {
		t.Fatalf("created sandbox = %+v, want org/template/vcpus to match request", sb)
	}
	if sb.ID == "" {
		t.Fatal("created sandbox has no id")
	}
	// The new sandbox must be listable afterward, not just returned once.
	if _, err := f.sandboxes.Get(context.Background(), f.aliceOrg, sb.ID); err != nil {
		t.Fatalf("created sandbox is not gettable: %v", err)
	}
	events, _ := f.audit.List(context.Background(), f.aliceOrg, 0)
	if len(events) == 0 || events[0].Action != "sandbox.create" || events[0].Target != sb.ID {
		t.Fatalf("expected a sandbox.create audit event for %s, got %+v", sb.ID, events)
	}
}

func TestCreateSandboxRejectsMissingTemplate(t *testing.T) {
	f := newFixture(t)
	w := f.reqBody(t, "POST", "/console/sandboxes", `{"vcpus":1,"mem_gib":1}`, f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestCreateSandboxRejectsOutOfBoundsVCPUsAndMem(t *testing.T) {
	f := newFixture(t)
	cases := []string{
		`{"template":"t","vcpus":3,"mem_gib":1}`,  // 3 is not an allowed vcpu option
		`{"template":"t","vcpus":1,"mem_gib":16}`, // 16 exceeds the mem_gib option set
		`{"template":"t","vcpus":0,"mem_gib":1}`,
	}
	for _, body := range cases {
		w := f.reqBody(t, "POST", "/console/sandboxes", body, f.aliceAcct, f.aliceOrg)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400; resp=%s", body, w.Code, w.Body.String())
		}
	}
}

func TestCreateSandboxRejectsInvalidJSON(t *testing.T) {
	f := newFixture(t)
	w := f.reqBody(t, "POST", "/console/sandboxes", `{not json`, f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestCreateSandboxWithProjectRequiresProjectAccess is the load-bearing
// permission-escalation test: a caller with org-wide PermUseResources but no
// PermManageProjects and no membership in the target project must NOT be able
// to use create-with-project_id to plant a sandbox into a project they cannot
// otherwise touch (that would route around the stricter PermManageProjects
// gate on PUT .../project). A caller who IS a member of that project (even
// with a lesser org-wide role) succeeds.
func TestCreateSandboxWithProjectRequiresProjectAccess(t *testing.T) {
	pf := newProjectAccessFixture(t)

	// MEMBER: org-wide PermUseResources, no project P membership -> forbidden.
	body := `{"template":"t","vcpus":1,"mem_gib":1,"project_id":"` + pf.projectP + `"}`
	r := httptest.NewRequest("POST", "/console/sandboxes", strings.NewReader(body))
	r = r.WithContext(WithCaller(r.Context(), pf.memberAcct, pf.orgID))
	w := httptest.NewRecorder()
	pf.con.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("member create-into-project status = %d, want 403; body=%s", w.Code, w.Body.String())
	}

	// PVIEWER: org-wide Viewer (read-only) but project P Admin -> allowed.
	r2 := httptest.NewRequest("POST", "/console/sandboxes", strings.NewReader(body))
	r2 = r2.WithContext(WithCaller(r2.Context(), pf.pviewerAcct, pf.orgID))
	w2 := httptest.NewRecorder()
	pf.con.ServeHTTP(w2, r2)
	if w2.Code != http.StatusCreated {
		t.Fatalf("pviewer create-into-project status = %d, want 201; body=%s", w2.Code, w2.Body.String())
	}
	var sb SandboxView
	if err := json.Unmarshal(w2.Body.Bytes(), &sb); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sb.ProjectID != pf.projectP {
		t.Fatalf("created sandbox project_id = %q, want %q", sb.ProjectID, pf.projectP)
	}
}

// --- Fork ---

func TestForkSandboxRefusesCrossOrg(t *testing.T) {
	f := newFixture(t)
	w := f.reqBody(t, "POST", "/console/sandboxes/sb-bob-1/fork", `{"count":2}`, f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-org fork status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestForkSandboxRejectsOutOfBoundsCount(t *testing.T) {
	f := newFixture(t)
	for _, body := range []string{`{"count":0}`, `{"count":17}`, `{"count":-1}`} {
		w := f.reqBody(t, "POST", "/console/sandboxes/sb-alice-1/fork", body, f.aliceAcct, f.aliceOrg)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400; resp=%s", body, w.Code, w.Body.String())
		}
	}
}

func TestForkSandboxSucceedsReturnsIDsAndAudits(t *testing.T) {
	f := newFixture(t)
	w := f.reqBody(t, "POST", "/console/sandboxes/sb-alice-1/fork", `{"count":3}`, f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Source string   `json:"source"`
		IDs    []string `json:"ids"`
	}
	decode(t, w, &resp)
	if resp.Source != "sb-alice-1" || len(resp.IDs) != 3 {
		t.Fatalf("fork response = %+v, want source sb-alice-1 and 3 ids", resp)
	}
	for _, id := range resp.IDs {
		if _, err := f.sandboxes.Get(context.Background(), f.aliceOrg, id); err != nil {
			t.Errorf("forked sandbox %s not gettable in aliceOrg: %v", id, err)
		}
	}
	events, _ := f.audit.List(context.Background(), f.aliceOrg, 0)
	if len(events) == 0 || events[0].Action != "sandbox.fork" || events[0].Target != "sb-alice-1" {
		t.Fatalf("expected a sandbox.fork audit event, got %+v", events)
	}
}

// --- Exec ---

func TestExecSandboxRefusesCrossOrg(t *testing.T) {
	f := newFixture(t)
	w := f.reqBody(t, "POST", "/console/sandboxes/sb-bob-1/exec", `{"cmd":"echo hi"}`, f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-org exec status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestExecSandboxRejectsEmptyCmdAndBadTimeout(t *testing.T) {
	f := newFixture(t)
	for _, body := range []string{`{"cmd":""}`, `{"cmd":"echo hi","timeout_s":61}`, `{"cmd":"echo hi","timeout_s":-1}`} {
		w := f.reqBody(t, "POST", "/console/sandboxes/sb-alice-1/exec", body, f.aliceAcct, f.aliceOrg)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400; resp=%s", body, w.Code, w.Body.String())
		}
	}
}

func TestExecSandboxSucceedsAndAuditsTruncatedCommandOnly(t *testing.T) {
	f := newFixture(t)
	f.sandboxes.SetExecResult("sb-alice-1", ExecResult{Stdout: "hi\n", ExitCode: 0})
	longCmd := "echo " + strings.Repeat("x", 200) + " && export SECRET=do-not-log-me"
	body := `{"cmd":` + jsonString(longCmd) + `,"timeout_s":5}`
	w := f.reqBody(t, "POST", "/console/sandboxes/sb-alice-1/exec", body, f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp execSandboxResponse
	decode(t, w, &resp)
	if resp.Stdout != "hi\n" || resp.ExitCode != 0 {
		t.Fatalf("exec response = %+v, want the scripted result", resp)
	}
	events, _ := f.audit.List(context.Background(), f.aliceOrg, 0)
	if len(events) == 0 || events[0].Action != "sandbox.exec" {
		t.Fatalf("expected a sandbox.exec audit event, got %+v", events)
	}
	detail := events[0].Detail
	if strings.Contains(detail, "SECRET") || strings.Contains(detail, "do-not-log-me") {
		t.Fatalf("audit detail leaked past the command preview: %q", detail)
	}
	if len(detail) > len("executed: ")+auditCmdPreviewLen {
		t.Fatalf("audit detail longer than the 80-char preview budget: %q", detail)
	}
}

func TestExecSandboxUnsupportedMapsTo501(t *testing.T) {
	f := newFixture(t)
	f.sandboxes.SetExecErr("sb-alice-1", ErrUnsupported)
	w := f.reqBody(t, "POST", "/console/sandboxes/sb-alice-1/exec", `{"cmd":"echo hi"}`, f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", w.Code, w.Body.String())
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// --- SSE log stream ---

func TestSandboxLogsStreamRefusesCrossOrg(t *testing.T) {
	f := newFixture(t)
	w := f.req(t, "GET", "/console/sandboxes/sb-alice-1/logs/stream", "", f.bobAcct, f.bobOrg)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-org stream status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "alice-log") {
		t.Fatalf("cross-org stream leaked alice's log content: %s", w.Body.String())
	}
}

// TestSandboxLogsStreamSendsSSEAndClosesOnCtxCancel is the load-bearing SSE
// test: it asserts the response is text/event-stream, the seeded lines arrive
// as "data:" events, at least one heartbeat comment is sent while the
// connection is held open, and the handler actually returns (does not hang
// forever) once the request context is canceled.
func TestSandboxLogsStreamSendsSSEAndClosesOnCtxCancel(t *testing.T) {
	f := newFixture(t)
	origInterval := sseHeartbeatInterval
	sseHeartbeatInterval = 5 * time.Millisecond
	t.Cleanup(func() { sseHeartbeatInterval = origInterval })

	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("GET", "/console/sandboxes/sb-alice-1/logs/stream", nil)
	r = r.WithContext(WithCaller(ctx, f.aliceAcct, f.aliceOrg))
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		f.con.ServeHTTP(w, r)
		close(done)
	}()

	// Give the heartbeat ticker time to fire at least once before we cancel.
	time.Sleep(40 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after context cancellation; SSE loop leaked")
	}

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	body := w.Body.String()
	if !strings.Contains(body, "data: alice-log-line-1") {
		t.Fatalf("missing seeded log line as an SSE data event: %q", body)
	}
	if !strings.Contains(body, ": heartbeat") {
		t.Fatalf("missing heartbeat comment: %q", body)
	}
}
