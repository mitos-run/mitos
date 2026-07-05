// Handler-level tests for the operate verbs (create, fork, exec, live logs)
// added in sandbox_ops.go: org-scoping (a cross-org sandbox id is refused),
// server-side bounds enforcement (vcpus/mem/count/timeout), audit events (and
// that exec's audit detail never carries more than a preview of the command),
// and the SSE log-stream handler's flush/heartbeat/ctx-cancel behavior.
package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
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

// partialForkSandboxControl wraps a real SandboxControl but makes Fork
// return a fixed (survivors, err) pair unconditionally, standing in for a
// cluster partial failure: the underlying seam creates each fork
// independently, so an error partway through still leaves however many
// landed so far (see clustersandbox.Control.Fork's doc); survivors == nil
// stands in for a TOTAL failure (nothing landed at all).
type partialForkSandboxControl struct {
	SandboxControl
	survivors []string
	err       error
}

func (p *partialForkSandboxControl) Fork(context.Context, string, string, int) ([]string, error) {
	return p.survivors, p.err
}

// TestForkSandboxPartialFailureReturns207WithSurvivorsAndError asserts a
// partial fork failure (issue #716) reports the survivor ids and the error
// via a 207-style body, rather than discarding the ids the previous
// behavior did (a bare c.failSandbox(w, err) call that dropped them).
func TestForkSandboxPartialFailureReturns207WithSurvivorsAndError(t *testing.T) {
	f := newFixture(t)
	forkErr := errors.New("create fork 3/5 of sb-alice-1: cluster api timeout")
	f.con.deps.Sandboxes = &partialForkSandboxControl{
		SandboxControl: f.sandboxes,
		survivors:      []string{"sb-alice-1-fork-1", "sb-alice-1-fork-2"},
		err:            forkErr,
	}

	w := f.reqBody(t, "POST", "/console/sandboxes/sb-alice-1/fork", `{"count":5}`, f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusMultiStatus {
		t.Fatalf("status = %d, want 207; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Source string   `json:"source"`
		IDs    []string `json:"ids"`
		Error  string   `json:"error"`
	}
	decode(t, w, &resp)
	if resp.Source != "sb-alice-1" {
		t.Errorf("source = %q, want sb-alice-1", resp.Source)
	}
	if len(resp.IDs) != 2 || resp.IDs[0] != "sb-alice-1-fork-1" || resp.IDs[1] != "sb-alice-1-fork-2" {
		t.Fatalf("ids = %+v, want the two survivor ids preserved", resp.IDs)
	}
	if resp.Error != forkErr.Error() {
		t.Errorf("error = %q, want %q", resp.Error, forkErr.Error())
	}

	events, _ := f.audit.List(context.Background(), f.aliceOrg, 0)
	if len(events) == 0 {
		t.Fatal("expected a sandbox.fork audit event for the partial failure")
	}
	ev := events[0]
	if ev.Action != "sandbox.fork" || ev.Target != "sb-alice-1" {
		t.Fatalf("audit event = %+v, want action sandbox.fork target sb-alice-1", ev)
	}
	if !strings.Contains(ev.Detail, "2 of 5") {
		t.Errorf("audit detail = %q, want it to record the survivor count (2 of 5)", ev.Detail)
	}
}

// TestForkSandboxTotalFailureReturnsPlainError asserts a TOTAL fork failure
// (no survivor ids at all) still goes through the ordinary failSandbox path
// (no ids/error 207 body): there is nothing to report as a partial success.
func TestForkSandboxTotalFailureReturnsPlainError(t *testing.T) {
	f := newFixture(t)
	f.con.deps.Sandboxes = &partialForkSandboxControl{
		SandboxControl: f.sandboxes,
		survivors:      nil,
		err:            errors.New("cluster api unavailable"),
	}

	w := f.reqBody(t, "POST", "/console/sandboxes/sb-alice-1/fork", `{"count":5}`, f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"ids"`) {
		t.Errorf("total-failure body must not carry an ids field: %s", w.Body.String())
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

// TestExecSandboxZeroTimeoutDefaultsToThirty asserts the handler applies a
// default 30s timeout before calling the SandboxControl seam when the
// caller's timeout_s is 0, instead of forwarding 0 (which would mean
// "unbounded" against a real backend and could let a single command run
// forever holding shared BFF resources).
func TestExecSandboxZeroTimeoutDefaultsToThirty(t *testing.T) {
	f := newFixture(t)
	w := f.reqBody(t, "POST", "/console/sandboxes/sb-alice-1/exec", `{"cmd":"echo hi","timeout_s":0}`, f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if got := f.sandboxes.LastTimeoutSec("sb-alice-1"); got != 30 {
		t.Fatalf("backend received timeout_s = %d, want the 30s default", got)
	}
}

// TestExecSandboxNonZeroTimeoutPassesThrough asserts an explicit, in-bounds
// timeout_s is forwarded unchanged (the default only kicks in for 0).
func TestExecSandboxNonZeroTimeoutPassesThrough(t *testing.T) {
	f := newFixture(t)
	w := f.reqBody(t, "POST", "/console/sandboxes/sb-alice-1/exec", `{"cmd":"echo hi","timeout_s":5}`, f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if got := f.sandboxes.LastTimeoutSec("sb-alice-1"); got != 5 {
		t.Fatalf("backend received timeout_s = %d, want the caller's explicit 5", got)
	}
}

// TestExecSandboxTruncatesLargeOutputWithMarker is the load-bearing
// unbounded-output test: a backend that returns more than 256 KiB of stdout
// or stderr must have its response truncated with a trailing marker line, so
// a high-output command cannot exhaust the shared BFF's memory when it is
// serialized and held in the response body.
func TestExecSandboxTruncatesLargeOutputWithMarker(t *testing.T) {
	f := newFixture(t)
	big := strings.Repeat("x", maxExecOutputBytes+100)
	f.sandboxes.SetExecResult("sb-alice-1", ExecResult{Stdout: big, Stderr: big, ExitCode: 0})
	w := f.reqBody(t, "POST", "/console/sandboxes/sb-alice-1/exec", `{"cmd":"echo hi"}`, f.aliceAcct, f.aliceOrg)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp execSandboxResponse
	decode(t, w, &resp)
	if len(resp.Stdout) > maxExecOutputBytes+len(truncatedOutputMarker) {
		t.Fatalf("stdout not truncated: got %d bytes", len(resp.Stdout))
	}
	if !strings.HasSuffix(resp.Stdout, truncatedOutputMarker) {
		t.Fatalf("stdout missing the truncation marker: tail=%q", resp.Stdout[max(0, len(resp.Stdout)-60):])
	}
	if len(resp.Stderr) > maxExecOutputBytes+len(truncatedOutputMarker) {
		t.Fatalf("stderr not truncated: got %d bytes", len(resp.Stderr))
	}
	if !strings.HasSuffix(resp.Stderr, truncatedOutputMarker) {
		t.Fatalf("stderr missing the truncation marker: tail=%q", resp.Stderr[max(0, len(resp.Stderr)-60):])
	}
}

// TestExecSandboxOutputUnderLimitIsUnchanged asserts short output is
// returned verbatim, with no marker appended.
func TestExecSandboxOutputUnderLimitIsUnchanged(t *testing.T) {
	f := newFixture(t)
	f.sandboxes.SetExecResult("sb-alice-1", ExecResult{Stdout: "hi\n", Stderr: "", ExitCode: 0})
	w := f.reqBody(t, "POST", "/console/sandboxes/sb-alice-1/exec", `{"cmd":"echo hi"}`, f.aliceAcct, f.aliceOrg)
	var resp execSandboxResponse
	decode(t, w, &resp)
	if resp.Stdout != "hi\n" || resp.Stderr != "" {
		t.Fatalf("small output was mutated: %+v", resp)
	}
}

// TestExecSandboxTruncationIsRuneSafe asserts the truncation never splits a
// multi-byte UTF-8 rune, even when the byte cutoff lands in the middle of
// one.
func TestExecSandboxTruncationIsRuneSafe(t *testing.T) {
	f := newFixture(t)
	// Filler puts the maxExecOutputBytes-th byte in the middle of a run of
	// 2-byte runes, which a byte-naive truncation would split.
	filler := strings.Repeat("a", maxExecOutputBytes-1)
	big := filler + strings.Repeat("é", 8) // e-acute, 2 bytes each in UTF-8
	f.sandboxes.SetExecResult("sb-alice-1", ExecResult{Stdout: big, ExitCode: 0})
	w := f.reqBody(t, "POST", "/console/sandboxes/sb-alice-1/exec", `{"cmd":"echo hi"}`, f.aliceAcct, f.aliceOrg)
	var resp execSandboxResponse
	decode(t, w, &resp)
	trimmed := strings.TrimSuffix(resp.Stdout, truncatedOutputMarker)
	if !utf8.ValidString(trimmed) {
		t.Fatalf("truncated stdout is not valid UTF-8: a multi-byte rune was split")
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

// blockingLogStreamer models a real follow transport (the cluster adapter's
// husk-pod log follow): it writes one line, then blocks until ctx is done,
// exactly like clustersandbox.Control.StreamLogs does while following a live
// pod. It is the load-bearing stand-in for proving the 501-honest path
// "flips to working": once a real, blocking LogStreamer is wired in, the SSE
// handler must still heartbeat (not starve) and still return promptly on
// disconnect, not just for the finite in-memory/fake streamer the other SSE
// test above exercises.
type blockingLogStreamer struct {
	line string
}

func (b *blockingLogStreamer) StreamLogs(ctx context.Context, _, _ string, sink LogSink) error {
	if err := sink.Write([]byte(b.line)); err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}

// TestSandboxLogsStreamHeartbeatsDuringBlockingFollowTransport asserts that
// when the LogStreamer is a real, blocking follow (not a finite fake that
// returns immediately), the SSE handler still sends heartbeats WHILE the
// transport is active, and still returns promptly once the client
// disconnects: this is the concurrency the real cluster transport actually
// needs (see sandbox_ops.go's handleSandboxLogsStream doc), which the
// finite-streamer test above cannot exercise since it returns before any
// heartbeat is due.
func TestSandboxLogsStreamHeartbeatsDuringBlockingFollowTransport(t *testing.T) {
	origInterval := sseHeartbeatInterval
	sseHeartbeatInterval = 5 * time.Millisecond
	t.Cleanup(func() { sseHeartbeatInterval = origInterval })

	sandboxes := NewMemSandboxControl()
	sandboxes.Add(SandboxView{ID: "sb-1", OrgID: "org-alice"})
	con := New(Deps{
		Sandboxes: sandboxes,
		Logs:      &blockingLogStreamer{line: "live-line\n"},
	})

	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("GET", "/console/sandboxes/sb-1/logs/stream", nil)
	r = r.WithContext(WithCaller(ctx, "acct-1", "org-alice"))
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		con.ServeHTTP(w, r)
		close(done)
	}()

	// Give the concurrent heartbeat loop time to fire more than once WHILE
	// the transport is still blocked mid-follow, before we disconnect.
	time.Sleep(40 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after ctx cancel while a blocking follow transport was active")
	}

	body := w.Body.String()
	if !strings.Contains(body, "data: live-line") {
		t.Fatalf("missing the blocking transport's live line: %q", body)
	}
	if strings.Count(body, ": heartbeat") < 2 {
		t.Fatalf("want at least 2 heartbeats while the follow transport blocked, got: %q", body)
	}
}

// recordingPeriodicLogStreamer models a real follow transport that keeps
// writing lines on its own goroutine for as long as ctx is alive (a live
// husk-pod follow with steady output), unlike blockingLogStreamer above which
// writes exactly once then goes silent. It records whether it observed ctx
// cancellation (vs. returning for some other reason) so a test can prove the
// SSE handler actually stops the upstream on the heartbeat-failure path, not
// just abandons the goroutine.
type recordingPeriodicLogStreamer struct {
	interval  time.Duration
	sawCancel chan bool
}

func (p *recordingPeriodicLogStreamer) StreamLogs(ctx context.Context, _, _ string, sink LogSink) error {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			p.sawCancel <- true
			return ctx.Err()
		case <-ticker.C:
			if err := sink.Write([]byte("tick-line\n")); err != nil {
				p.sawCancel <- false
				return err
			}
		}
	}
}

// timedWrite records one Write to raceDetectResponseWriter along with when it
// happened, so a test can assert no write landed after the SSE handler had
// already returned to net/http.
type timedWrite struct {
	at   time.Time
	data string
}

// raceDetectResponseWriter is a minimal http.ResponseWriter + http.Flusher
// fake that fails exactly the heartbeat payload sseLogSink.heartbeat sends
// (": heartbeat\n\n"), simulating a broken connection discovered only on a
// heartbeat write, while succeeding on every other write. It timestamps every
// write it sees.
type raceDetectResponseWriter struct {
	mu     sync.Mutex
	header http.Header
	code   int
	writes []timedWrite
}

func newRaceDetectResponseWriter() *raceDetectResponseWriter {
	return &raceDetectResponseWriter{header: http.Header{}}
}

func (w *raceDetectResponseWriter) Header() http.Header { return w.header }

func (w *raceDetectResponseWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.code = code
}

func (w *raceDetectResponseWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, timedWrite{at: time.Now(), data: string(p)})
	if string(p) == ": heartbeat\n\n" {
		return 0, errors.New("simulated broken connection on heartbeat write")
	}
	return len(p), nil
}

func (w *raceDetectResponseWriter) Flush() {}

func (w *raceDetectResponseWriter) writesAfter(t time.Time) []timedWrite {
	w.mu.Lock()
	defer w.mu.Unlock()
	var late []timedWrite
	for _, tw := range w.writes {
		if tw.at.After(t) {
			late = append(late, tw)
		}
	}
	return late
}

// TestSandboxLogsStreamNoWriteAfterHandlerReturnsOnHeartbeatFailure is the
// regression test for the write-after-ServeHTTP-return hazard: when the
// ticker.C branch's heartbeat write fails while a real, blocking follow
// transport is still active, the handler must stop the upstream (cancel a
// derived context) and wait for it to actually return BEFORE the handler
// itself returns, so net/http never observes a write to the ResponseWriter
// after ServeHTTP has already returned. Before the fix, this branch returned
// immediately without draining the background StreamLogs goroutine, so the
// transport could keep calling sink.Write after the handler had returned.
func TestSandboxLogsStreamNoWriteAfterHandlerReturnsOnHeartbeatFailure(t *testing.T) {
	origInterval := sseHeartbeatInterval
	sseHeartbeatInterval = 5 * time.Millisecond
	t.Cleanup(func() { sseHeartbeatInterval = origInterval })

	sandboxes := NewMemSandboxControl()
	sandboxes.Add(SandboxView{ID: "sb-1", OrgID: "org-alice"})
	streamer := &recordingPeriodicLogStreamer{interval: 2 * time.Millisecond, sawCancel: make(chan bool, 1)}
	con := New(Deps{
		Sandboxes: sandboxes,
		Logs:      streamer,
	})

	r := httptest.NewRequest("GET", "/console/sandboxes/sb-1/logs/stream", nil)
	r = r.WithContext(WithCaller(context.Background(), "acct-1", "org-alice"))
	w := newRaceDetectResponseWriter()

	done := make(chan struct{})
	go func() {
		con.ServeHTTP(w, r)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after the heartbeat write failed; upstream was not stopped")
	}
	returnedAt := time.Now()

	select {
	case sawCancel := <-streamer.sawCancel:
		if !sawCancel {
			t.Fatal("upstream StreamLogs did not observe ctx cancellation; it returned for another reason")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream StreamLogs never returned; the handler leaked the transport goroutine")
	}

	// Give a still-buggy handler a chance to sneak in a late write before checking.
	time.Sleep(50 * time.Millisecond)

	if late := w.writesAfter(returnedAt); len(late) > 0 {
		t.Fatalf("write(s) to the ResponseWriter after the handler returned: %+v", late)
	}
}
