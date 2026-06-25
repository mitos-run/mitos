package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// TestExecTimeoutExit124SurfacesExecTimeoutEnvelope asserts the blocking
// /v1/exec path maps the conventional 124 timeout exit code to the typed
// exec_timeout envelope (504), so a caller branches on the execution-deadline
// code rather than comparing exit codes (issue #216).
func TestExecTimeoutExit124SurfacesExecTimeoutEnvelope(t *testing.T) {
	dir := shortVsockDir(t)
	sock := filepath.Join(dir, "sbT", "vsock.sock")
	// The guest reports exit 124 for a command killed at its deadline.
	// Set execStdout to a non-empty string so the fakeGuestSandbox Exec handler
	// does not override execExit with the default 7.
	fake := &fakeGuestSandbox{
		execStdout: "timeout",
		execExit:   int32(execTimeoutExitCode),
	}
	startFakeGuestGRPCUDS(t, sock, fake)
	api := NewSandboxAPI(dir)
	api.AllowTokenless()
	if err := api.RegisterSandbox("sbT", sock); err != nil {
		t.Fatal(err)
	}
	api.RegisterStreamPath("sbT", sock)

	body, _ := json.Marshal(map[string]any{"sandbox": "sbT", "command": "sleep 999", "timeout": 5})
	req := httptest.NewRequest(http.MethodPost, "/v1/exec", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	api.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", rr.Code)
	}
	got := decodeEnvelope(t, rr.Body.Bytes())
	if got.Code != "exec_timeout" {
		t.Fatalf("code = %q, want exec_timeout", got.Code)
	}
	var env struct {
		Error struct {
			Context map[string]any `json:"context"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Error.Context["timeout_s"] != float64(5) {
		t.Errorf("context timeout_s = %v, want 5", env.Error.Context["timeout_s"])
	}
}

// TestExecTimeoutOverCeilingIsRejectedNotClamped is the determinism guarantee
// (issue #216): a requested exec timeout over the server ceiling is REJECTED
// with the typed timeout_too_large code, never silently reduced. The rejection
// carries the requested value and the ceiling in context so the caller can pick
// a value at or under it.
func TestExecTimeoutOverCeilingIsRejectedNotClamped(t *testing.T) {
	api := newEnvelopeTestAPI(t)
	api.SetMaxExecTimeoutSeconds(100)

	body, _ := json.Marshal(map[string]any{
		"sandbox": "sb-1", "command": "true", "timeout": 1000,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/exec", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	api.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	got := decodeEnvelope(t, rr.Body.Bytes())
	if got.Code != "timeout_too_large" {
		t.Fatalf("code = %q, want timeout_too_large", got.Code)
	}
	// The full envelope must carry the ceiling and the requested value so a
	// caller can correct the request without guessing.
	var env struct {
		Error struct {
			Context map[string]any `json:"context"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Error.Context["max_timeout_s"] != float64(100) {
		t.Errorf("context max_timeout_s = %v, want 100", env.Error.Context["max_timeout_s"])
	}
	if env.Error.Context["requested_s"] != float64(1000) {
		t.Errorf("context requested_s = %v, want 1000", env.Error.Context["requested_s"])
	}
}

// TestExecTimeoutAtCeilingIsHonored asserts a timeout exactly at the ceiling is
// NOT rejected (it is honored): the ceiling check passes through to the normal
// exec path, which here 404s because no sandbox is registered. The point is that
// the timeout_too_large gate did not fire at the boundary.
func TestExecTimeoutAtCeilingIsHonored(t *testing.T) {
	api := newEnvelopeTestAPI(t)
	api.SetMaxExecTimeoutSeconds(100)

	body, _ := json.Marshal(map[string]any{
		"sandbox": "sb-1", "command": "true", "timeout": 100,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/exec", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	api.Handler().ServeHTTP(rr, req)

	if rr.Code == http.StatusBadRequest {
		got := decodeEnvelope(t, rr.Body.Bytes())
		if got.Code == "timeout_too_large" {
			t.Fatalf("timeout at the ceiling must be honored, not rejected")
		}
	}
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (timeout honored, sandbox absent)", rr.Code)
	}
}

// TestRunCodeTimeoutOverCeilingIsRejected asserts run_code applies the same
// ceiling rejection as exec.
func TestRunCodeTimeoutOverCeilingIsRejected(t *testing.T) {
	api := newEnvelopeTestAPI(t)
	api.SetMaxExecTimeoutSeconds(100)

	body, _ := json.Marshal(map[string]any{
		"sandbox": "sb-1", "code": "print(1)", "timeout": 1000,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/run_code/stream", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	api.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	got := decodeEnvelope(t, rr.Body.Bytes())
	if got.Code != "timeout_too_large" {
		t.Fatalf("code = %q, want timeout_too_large", got.Code)
	}
}

// TestExecStreamTimeoutOverCeilingIsRejected asserts the streaming exec path
// rejects an over-ceiling timeout BEFORE writing the 200 stream header, so the
// rejection is a clean enveloped 400, not a terminal error frame.
func TestExecStreamTimeoutOverCeilingIsRejected(t *testing.T) {
	api := newEnvelopeTestAPI(t)
	api.SetMaxExecTimeoutSeconds(100)

	body, _ := json.Marshal(map[string]any{
		"sandbox": "sb-1", "command": "true", "timeout": 1000,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/exec/stream", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	api.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	got := decodeEnvelope(t, rr.Body.Bytes())
	if got.Code != "timeout_too_large" {
		t.Fatalf("code = %q, want timeout_too_large", got.Code)
	}
}

// TestDefaultCeilingIs24Hours asserts the documented default ceiling so the
// exec_background default (86400s) is honored out of the box.
func TestDefaultCeilingIs24Hours(t *testing.T) {
	api := newEnvelopeTestAPI(t)
	if got := api.maxExecTimeout; got != defaultMaxExecTimeoutSeconds {
		t.Fatalf("default ceiling = %d, want %d", got, defaultMaxExecTimeoutSeconds)
	}
	if defaultMaxExecTimeoutSeconds != 86400 {
		t.Fatalf("documented default ceiling is 86400s (24h), got %d", defaultMaxExecTimeoutSeconds)
	}
}
