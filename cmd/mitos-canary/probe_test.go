package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"mitos.run/mitos/internal/agentcli"
)

// fakeAPI is a scriptable sandboxAPI. Each phase can be told to fail, and it
// records the calls it saw so tests can assert cleanup happened.
type fakeAPI struct {
	createErr     error
	createID      string
	execErr       error
	execOut       agentcli.ExecResult
	echoBackNonce bool // when true, Exec returns the nonce it was asked to echo (exit 0)
	terminateErr  error

	created    int
	execed     int
	terminated int
	lastCmd    string
}

func (f *fakeAPI) Create(context.Context, string) (string, error) {
	f.created++
	if f.createErr != nil {
		return "", f.createErr
	}
	id := f.createID
	if id == "" {
		id = "sbx-test"
	}
	return id, nil
}

func (f *fakeAPI) Exec(_ context.Context, _, command string, _ int) (agentcli.ExecResult, error) {
	f.execed++
	f.lastCmd = command
	if f.execErr != nil {
		return agentcli.ExecResult{}, f.execErr
	}
	if f.echoBackNonce {
		// Mimic `echo <nonce>`: stdout is the argument after "echo ".
		return agentcli.ExecResult{ExitCode: 0, Stdout: strings.TrimPrefix(command, "echo ") + "\n"}, nil
	}
	return f.execOut, nil
}

func (f *fakeAPI) Terminate(context.Context, string) error {
	f.terminated++
	return f.terminateErr
}

func newTestMetrics(t *testing.T) *metrics {
	t.Helper()
	return newMetrics(prometheus.NewRegistry())
}

func TestRunProbeSuccess(t *testing.T) {
	f := &fakeAPI{echoBackNonce: true}
	m := newTestMetrics(t)

	res := runProbe(context.Background(), f, m, "python", "canary-abc123", 30)

	if !res.OK {
		t.Fatalf("expected success, got failure at %s: %v", res.Phase, res.Err)
	}
	if res.Phase != phaseVerify {
		t.Errorf("success phase = %s, want %s", res.Phase, phaseVerify)
	}
	if f.created != 1 || f.execed != 1 || f.terminated != 1 {
		t.Errorf("call counts create=%d exec=%d terminate=%d, want 1/1/1", f.created, f.execed, f.terminated)
	}
	if !strings.Contains(f.lastCmd, "canary-abc123") {
		t.Errorf("exec command %q did not carry the nonce", f.lastCmd)
	}
	if got := testutil.ToFloat64(m.up); got != 1 {
		t.Errorf("mitos_canary_up = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.lastSuccess); got == 0 {
		t.Error("last-success timestamp was not set on a green cycle")
	}
}

func TestRunProbeCreateFailsNoTerminate(t *testing.T) {
	f := &fakeAPI{createErr: errors.New("fork_unavailable")}
	m := newTestMetrics(t)

	res := runProbe(context.Background(), f, m, "python", "canary-x", 30)

	if res.OK {
		t.Fatal("expected failure when create fails")
	}
	if res.Phase != phaseCreate {
		t.Errorf("failure phase = %s, want %s", res.Phase, phaseCreate)
	}
	if f.execed != 0 {
		t.Error("exec should not run after a failed create")
	}
	if f.terminated != 0 {
		t.Error("terminate should not run when no sandbox was created")
	}
	if got := testutil.ToFloat64(m.up); got != 0 {
		t.Errorf("mitos_canary_up = %v, want 0", got)
	}
}

func TestRunProbeExecFailsStillTerminates(t *testing.T) {
	f := &fakeAPI{execErr: errors.New("exec stream closed")}
	m := newTestMetrics(t)

	res := runProbe(context.Background(), f, m, "python", "canary-x", 30)

	if res.OK || res.Phase != phaseExec {
		t.Fatalf("expected exec-phase failure, got ok=%v phase=%s", res.OK, res.Phase)
	}
	if f.terminated != 1 {
		t.Error("sandbox must be terminated even when exec fails, to avoid leaks")
	}
}

func TestRunProbeVerifyFailsOnMissingNonce(t *testing.T) {
	// Exec succeeds at the transport level but returns the wrong output: the
	// exec path is broken even though create/exec did not error.
	f := &fakeAPI{execOut: agentcli.ExecResult{ExitCode: 0, Stdout: "unrelated\n"}}
	m := newTestMetrics(t)

	res := runProbe(context.Background(), f, m, "python", "canary-abc123", 30)

	if res.OK || res.Phase != phaseVerify {
		t.Fatalf("expected verify-phase failure, got ok=%v phase=%s", res.OK, res.Phase)
	}
	if f.terminated != 1 {
		t.Error("sandbox must be terminated after a verify failure")
	}
}

func TestRunProbeVerifyFailsOnNonzeroExit(t *testing.T) {
	// The nonce is present but the command reported a non-zero exit.
	f := &fakeAPI{execOut: agentcli.ExecResult{ExitCode: 7, Stdout: "canary-abc123\n"}}
	m := newTestMetrics(t)

	res := runProbe(context.Background(), f, m, "python", "canary-abc123", 30)

	if res.OK || res.Phase != phaseVerify {
		t.Fatalf("expected verify-phase failure on non-zero exit, got ok=%v phase=%s", res.OK, res.Phase)
	}
}

func TestRunProbeTerminateFailureDoesNotFailCycle(t *testing.T) {
	// The user-facing path (create, exec, verify) works; only cleanup fails.
	// The cycle stays green because a fork/exec worked; the leak is visible via
	// the terminate failure counter, not by marking the platform down.
	f := &fakeAPI{echoBackNonce: true, terminateErr: errors.New("terminate 500")}
	m := newTestMetrics(t)

	res := runProbe(context.Background(), f, m, "python", "canary-abc123", 30)

	if !res.OK {
		t.Fatalf("terminate-only failure should not fail the cycle, got failure at %s: %v", res.Phase, res.Err)
	}
	got := testutil.ToFloat64(m.probeTotal.WithLabelValues(string(phaseTerminate), "failure"))
	if got != 1 {
		t.Errorf("terminate failure counter = %v, want 1", got)
	}
}

func TestHealthLiveness(t *testing.T) {
	h := &health{stalenessWindow: 50 * time.Millisecond}

	// Before the first tick, liveness is true (still starting up).
	if !h.live() {
		t.Error("expected live before first tick")
	}
	if h.isReady() {
		t.Error("expected not-ready before first cycle")
	}

	h.tick()
	h.ready()
	if !h.live() {
		t.Error("expected live right after a tick")
	}
	if !h.isReady() {
		t.Error("expected ready after first cycle")
	}

	// Force the tick to be stale.
	h.mu.Lock()
	h.lastTick = time.Now().Add(-time.Second)
	h.mu.Unlock()
	if h.live() {
		t.Error("expected not-live when the loop has not ticked within the staleness window")
	}
}
