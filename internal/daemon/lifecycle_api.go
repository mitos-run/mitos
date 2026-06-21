package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// This file holds the first-class lifecycle controls on the sandbox HTTP API
// (issue #218): live set_timeout (adjust a RUNNING sandbox's TTL), and
// pause/resume (snapshot full state and stop the idle clock, then restore).
//
// The forkd/engine seam: set_timeout records a live deadline the lifetime
// reaper reads through ListSandboxes (the running-sandbox TTL); pause marks the
// sandbox held so the work-aware idle reaper never reaps it mid-pause. The REAL
// memory+filesystem snapshot under pause and its correctness across N repeated
// cycles is the KVM acceptance bar driven by the fork engine; this API surface
// is the durable, unit-testable control plane that drives it. The standalone
// sandbox-server and forkd both serve these routes through SandboxAPI.Handler.

// setTimeoutRequest is the live-TTL adjustment for a RUNNING sandbox. This is
// the server endpoint the #206 E2B shim's setTimeout maps onto.
type setTimeoutRequest struct {
	Sandbox        string `json:"sandbox"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

// handleSetTimeout adjusts a live sandbox's TTL. The new deadline is now +
// timeout_seconds against the API clock. A requested timeout over the server
// ceiling is REJECTED with the typed timeout_too_large code, never silently
// clamped (issue #216), so the deadline a caller sets is the deadline it gets.
// A zero or negative timeout is a client error. The recorded deadline is read
// by the lifetime reaper through ListSandboxes; it does not itself reap.
func (api *SandboxAPI) handleSetTimeout(w http.ResponseWriter, r *http.Request) {
	var req setTimeoutRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "invalid json", 400)
		return
	}
	if req.TimeoutSeconds <= 0 {
		writeErr(w, "timeout_seconds must be a positive number of seconds", 400)
		return
	}
	// Determinism (issue #216): reject an over-ceiling live timeout rather than
	// silently reducing it, exactly as the exec path does.
	if e := api.checkTimeout(req.Sandbox, req.TimeoutSeconds); e != nil {
		writeAPIErr(w, *e)
		return
	}
	deadline := api.SetTimeout(req.Sandbox, time.Duration(req.TimeoutSeconds)*time.Second)

	api.auditor.Record(AuditEvent{
		SandboxID: req.Sandbox,
		Op:        "set_timeout",
		Detail:    fmt.Sprintf("timeout_s=%d", req.TimeoutSeconds),
		OK:        true,
	})

	writeJSON(w, map[string]any{
		"status":          "ok",
		"deadline_unix":   deadline.Unix(),
		"timeout_seconds": req.TimeoutSeconds,
	})
}

// SetTimeout records a live TTL deadline of now + d for sandboxID and returns
// the absolute deadline. It is the running-sandbox TTL the lifetime reaper
// reads via ListSandboxes; calling it again replaces the deadline (extend or
// shorten). The clock is the API's now (overridable in tests).
func (api *SandboxAPI) SetTimeout(sandboxID string, d time.Duration) time.Time {
	deadline := api.now().Add(d)
	api.mu.Lock()
	api.deadlines[sandboxID] = deadline
	api.mu.Unlock()
	return deadline
}

// Deadline returns the live TTL deadline recorded by set_timeout for sandboxID.
// The bool is false when no live timeout has been set (the sandbox runs under
// its creation-time idle/maxLifetime only).
func (api *SandboxAPI) Deadline(sandboxID string) (time.Time, bool) {
	api.mu.RLock()
	t, ok := api.deadlines[sandboxID]
	api.mu.RUnlock()
	return t, ok
}

// ActiveStreams reports the number of currently OPEN streams (streaming exec,
// run_code, PTY) for sandboxID. It is the work-aware idle signal (issue #218):
// a non-zero count means a background job is running, so the idle reaper must
// treat the sandbox as active and never reap it mid-run even with no inbound
// API interaction.
func (api *SandboxAPI) ActiveStreams(sandboxID string) int {
	api.mu.RLock()
	n := api.openStreams[sandboxID]
	api.mu.RUnlock()
	return n
}

// EnginePauser is the engine-side pause/resume the SandboxAPI drives on a real
// forkd: pause snapshots full state (memory + filesystem) and pauses the VM,
// resume restores it. The standalone sandbox-server and unit tests leave it
// unset, so the API records the held state only (no VM behind it).
type EnginePauser interface {
	Pause(sandboxID string) error
	Resume(sandboxID string) error
}

// SetEnginePauser installs the engine pause/resume hook (issue #218). forkd sets
// it so the pause/resume HTTP endpoints drive the real Firecracker
// snapshot/restore; the standalone server leaves it nil and the endpoints record
// the held state only. Must be called before the API serves requests; the field
// is not synchronized.
func (api *SandboxAPI) SetEnginePauser(p EnginePauser) {
	api.enginePauser = p
}

// pauseRequest names the sandbox to pause or resume.
type pauseRequest struct {
	Sandbox string `json:"sandbox"`
}

// handlePause marks the sandbox paused: the idle clock stops and (on the real
// engine) full state (memory + filesystem) is snapshotted so a later resume
// restores it. The mock path records the paused state only; the real
// memory+fs snapshot and its correctness across N repeated cycles is the
// KVM-gated acceptance bar (lifecycle_kvm_test.go), driven by the fork engine's
// snapshot/restore. A paused sandbox is never idle-reaped.
func (api *SandboxAPI) handlePause(w http.ResponseWriter, r *http.Request) {
	var req pauseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "invalid json", 400)
		return
	}
	// On a real forkd, snapshot full state (memory + filesystem) and pause the
	// VM BEFORE marking the clock stopped, so a failed snapshot does not leave a
	// sandbox reported paused but still running. The standalone server and unit
	// tests leave the pauser unset and only record the held state.
	if api.enginePauser != nil {
		if err := api.enginePauser.Pause(req.Sandbox); err != nil {
			writeErr(w, fmt.Sprintf("pause sandbox: %v", err), 500)
			return
		}
	}
	api.MarkPaused(req.Sandbox, true)
	api.auditor.Record(AuditEvent{
		SandboxID: req.Sandbox,
		Op:        "pause",
		OK:        true,
	})
	writeJSON(w, map[string]string{"status": "paused"})
}

// handleResume clears the paused state: the idle clock restarts and (on the
// real engine) the snapshotted full state is restored. Resuming a sandbox that
// was not paused is a no-op that still returns ok, so a client need not track
// the exact paused state.
func (api *SandboxAPI) handleResume(w http.ResponseWriter, r *http.Request) {
	var req pauseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "invalid json", 400)
		return
	}
	if api.enginePauser != nil {
		if err := api.enginePauser.Resume(req.Sandbox); err != nil {
			writeErr(w, fmt.Sprintf("resume sandbox: %v", err), 500)
			return
		}
	}
	api.MarkPaused(req.Sandbox, false)
	api.touch(req.Sandbox) // resume counts as activity: restart the idle clock cleanly
	api.auditor.Record(AuditEvent{
		SandboxID: req.Sandbox,
		Op:        "resume",
		OK:        true,
	})
	writeJSON(w, map[string]string{"status": "running"})
}

// MarkPaused sets or clears the paused flag for sandboxID. A paused sandbox has
// a stopped idle clock and is never idle-reaped; clearing it returns the
// sandbox to the normal idle/TTL clock. It is the seam the engine pause/resume
// drives and the unit-testable signal the reaper reads.
func (api *SandboxAPI) MarkPaused(sandboxID string, paused bool) {
	api.mu.Lock()
	if paused {
		api.paused[sandboxID] = true
	} else {
		delete(api.paused, sandboxID)
	}
	api.mu.Unlock()
}

// IsPaused reports whether sandboxID is currently paused (clock stopped).
func (api *SandboxAPI) IsPaused(sandboxID string) bool {
	api.mu.RLock()
	p := api.paused[sandboxID]
	api.mu.RUnlock()
	return p
}

// HoldStreamForTest reserves one open-stream slot for sandboxID and returns a
// release func, simulating a live background job (streaming exec, run_code, or
// PTY) so the work-aware idle signal (ActiveStreams > 0) is exercisable without
// driving a real vsock stream. It is the injection seam controller envtests use
// to assert a sandbox with a background job is not idle-reaped (issue #218).
func (api *SandboxAPI) HoldStreamForTest(sandboxID string) (release func()) {
	r, _ := api.acquireStream(sandboxID)
	return r
}
