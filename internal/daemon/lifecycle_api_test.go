package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestSetTimeoutRecordsDeadline asserts POST /v1/set_timeout records a live
// deadline on a RUNNING sandbox so the idle/lifetime reaper can read the
// extended TTL. The deadline is now + timeout_seconds against the API clock.
func TestSetTimeoutRecordsDeadline(t *testing.T) {
	api := newEnvelopeTestAPI(t)
	fixed := time.Unix(1_700_000_000, 0)
	api.now = func() time.Time { return fixed }

	body, _ := json.Marshal(map[string]any{"sandbox": "sb-1", "timeout_seconds": 600})
	req := httptest.NewRequest(http.MethodPost, "/v1/set_timeout", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	api.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	deadline, ok := api.Deadline("sb-1")
	if !ok {
		t.Fatal("Deadline missing after set_timeout")
	}
	want := fixed.Add(600 * time.Second)
	if !deadline.Equal(want) {
		t.Fatalf("deadline = %v, want %v", deadline, want)
	}
}

// TestSetTimeoutOverCeilingIsRejected is the determinism guarantee shared with
// the exec ceiling (issue #216): a requested live timeout over the server
// ceiling is REJECTED with timeout_too_large, never silently clamped.
func TestSetTimeoutOverCeilingIsRejected(t *testing.T) {
	api := newEnvelopeTestAPI(t)
	api.SetMaxExecTimeoutSeconds(100)

	body, _ := json.Marshal(map[string]any{"sandbox": "sb-1", "timeout_seconds": 1000})
	req := httptest.NewRequest(http.MethodPost, "/v1/set_timeout", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	api.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	got := decodeEnvelope(t, rr.Body.Bytes())
	if got.Code != "timeout_too_large" {
		t.Fatalf("code = %q, want timeout_too_large", got.Code)
	}
	if _, ok := api.Deadline("sb-1"); ok {
		t.Fatal("a rejected set_timeout must not record a deadline")
	}
}

// TestSetTimeoutRejectsNonPositive asserts a zero or negative timeout is a
// client error (invalid_json envelope shape), never a silent no-op.
func TestSetTimeoutRejectsNonPositive(t *testing.T) {
	api := newEnvelopeTestAPI(t)
	body, _ := json.Marshal(map[string]any{"sandbox": "sb-1", "timeout_seconds": 0})
	req := httptest.NewRequest(http.MethodPost, "/v1/set_timeout", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	api.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

// TestActiveStreamsCountsAsWork asserts the work-aware activity signal: a
// sandbox with a live stream (background exec/run_code/PTY) reports a non-zero
// ActiveStreams count, which the idle reaper treats as activity so an
// unattended background job is never reaped mid-run.
func TestActiveStreamsCountsAsWork(t *testing.T) {
	api := newEnvelopeTestAPI(t)
	if got := api.ActiveStreams("sb-1"); got != 0 {
		t.Fatalf("ActiveStreams before any stream = %d, want 0", got)
	}
	release, ok := api.acquireStream("sb-1")
	if !ok {
		t.Fatal("acquireStream rejected with no cap set")
	}
	if got := api.ActiveStreams("sb-1"); got != 1 {
		t.Fatalf("ActiveStreams with one open stream = %d, want 1", got)
	}
	release()
	if got := api.ActiveStreams("sb-1"); got != 0 {
		t.Fatalf("ActiveStreams after release = %d, want 0", got)
	}
}

// TestPauseResumeRoundTripOnMock asserts the pause/resume API surface: pause
// records the sandbox as paused (clock stopped) and resume clears it. This is
// the mock-level behavior; the real memory+fs snapshot correctness across N
// cycles is KVM-gated (see lifecycle_kvm_test.go).
func TestPauseResumeRoundTripOnMock(t *testing.T) {
	api := newEnvelopeTestAPI(t)

	pause, _ := json.Marshal(map[string]any{"sandbox": "sb-1"})
	req := httptest.NewRequest(http.MethodPost, "/v1/pause", bytes.NewReader(pause))
	rr := httptest.NewRecorder()
	api.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("pause status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	if !api.IsPaused("sb-1") {
		t.Fatal("sandbox not marked paused after /v1/pause")
	}

	resume, _ := json.Marshal(map[string]any{"sandbox": "sb-1"})
	req = httptest.NewRequest(http.MethodPost, "/v1/resume", bytes.NewReader(resume))
	rr = httptest.NewRecorder()
	api.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("resume status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	if api.IsPaused("sb-1") {
		t.Fatal("sandbox still paused after /v1/resume")
	}
}

// TestPausedSandboxIsNotIdleReaped asserts the lifecycle invariant: a paused
// sandbox stops its clock, so it is never counted as idle (its deadline does
// not advance into the past while paused). A paused sandbox reports no
// last-activity-driven expiry; the reaper sees it as held, not idle.
func TestPausedSandboxIsNotIdleReaped(t *testing.T) {
	api := newEnvelopeTestAPI(t)
	api.MarkPaused("sb-1", true)
	if !api.IsPaused("sb-1") {
		t.Fatal("MarkPaused did not mark the sandbox paused")
	}
	// Unpausing returns it to the normal idle clock.
	api.MarkPaused("sb-1", false)
	if api.IsPaused("sb-1") {
		t.Fatal("MarkPaused(false) did not clear the paused flag")
	}
}
