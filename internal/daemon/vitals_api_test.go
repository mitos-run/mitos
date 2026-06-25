package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mitos.run/mitos/internal/fork"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

func vitalsAPI(t *testing.T, sandboxID string, fake *fakeGuestSandbox) *SandboxAPI {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "vitals")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	sockPath := filepath.Join(dir, "vsock.sock")
	startFakeGuestGRPCUDS(t, sockPath, fake)

	api := NewSandboxAPI(dir)
	api.RegisterToken(sandboxID, "tok")
	if err := api.RegisterSandbox(sandboxID, sockPath); err != nil {
		t.Fatal(err)
	}
	api.RegisterStreamPath(sandboxID, sockPath)
	return api
}

// TestHandleVitals_Labeled drives the host-side guest telemetry consumer: a
// /v1/vitals request resolves a sandbox, asks its guest agent over gRPC, and
// returns the snapshot LABELED with the claim/pool/workspace the host knows.
func TestHandleVitals_Labeled(t *testing.T) {
	// Configure the fake to emit specific vitals and a process table.
	// Mapping: StealFraction=0.2 -> CpuStealPercent=20.0; BalloonReclaimedKB=512000 -> MemBalloonBytes=524288000.
	// MemTotalKB=2048000 -> MemTotalBytes=2097152000; MemUsedKB=1024000 -> MemUsedBytes=1048576000.
	fake := &fakeGuestSandbox{
		vitalsResponse: &sandboxv1.GuestVitals{
			CpuStealPercent: 20.0,
			MemTotalBytes:   2048000 * 1024,
			MemUsedBytes:    1024000 * 1024,
			MemBalloonBytes: 512000 * 1024,
		},
		processesResponse: &sandboxv1.ProcessList{
			Processes: []*sandboxv1.ProcessInfo{
				{Pid: 1, Command: "agent", State: "S", RssBytes: 4096 * 1024},
				{Pid: 99, Command: "python", State: "R", RssBytes: 65536 * 1024},
			},
		},
	}
	api := vitalsAPI(t, "sb-1", fake)
	api.SetVitalsLabels("sb-1", VitalsLabels{Claim: "claim-a", Pool: "pool-x", Workspace: "ws-1", Namespace: "team-ns"})

	req := httptest.NewRequest(http.MethodPost, "/v1/vitals", strings.NewReader(`{"sandbox":"sb-1"}`))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got LabeledVitals
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Claim != "claim-a" || got.Pool != "pool-x" || got.Workspace != "ws-1" || got.Namespace != "team-ns" {
		t.Errorf("labels not applied: %+v", got)
	}
	if got.Vitals.StealFraction != 0.2 || got.Vitals.BalloonReclaimedKB != 512000 {
		t.Errorf("vitals not carried: steal=%v balloon=%v", got.Vitals.StealFraction, got.Vitals.BalloonReclaimedKB)
	}
	if len(got.Vitals.Processes) != 2 || got.Vitals.Processes[1].Comm != "python" {
		t.Errorf("process table not carried: %+v", got.Vitals.Processes)
	}
}

// TestForkRecordsVitalsLabels drives the Fork path's label plumbing (issue
// #164): forkd records the claim/pool/workspace/namespace the controller passed
// in the Fork request so the sandbox's /v1/vitals snapshot is labeled. The mock
// engine has no guest, so this asserts the recording, not the live sample.
func TestForkRecordsVitalsLabels(t *testing.T) {
	engine := fork.NewMockEngine() // KVMAvailable=false
	engine.ForkDelay = 0
	if err := engine.CreateTemplate("py", "py", nil, nil); err != nil {
		t.Fatal(err)
	}
	api := NewSandboxAPI(t.TempDir())
	srv := NewServer(engine, api)

	labels := VitalsLabels{Claim: "claim-a", Pool: "pool-x", Workspace: "ws-1", Namespace: "team-ns"}
	if _, err := srv.Fork(context.Background(), "py", "sb-mock", nil, nil, nil, nil, "tok", labels); err != nil {
		t.Fatalf("fork: %v", err)
	}

	got := api.vitalsLabelsFor("sb-mock")
	if got != labels {
		t.Errorf("recorded labels = %+v, want %+v", got, labels)
	}
}

// TestHandleNodeVitals_Batch drives the node-level vitals batch endpoint the
// control-plane sampler scrapes (issue #164 Phase 1.a): it returns one
// LabeledVitals per guest-reachable sandbox on this forkd, and a sandbox whose
// guest is unreachable is skipped+counted, never failing the report.
func TestHandleNodeVitals_Batch(t *testing.T) {
	dir := t.TempDir()
	api := NewSandboxAPI(dir)

	// sb-a: reachable guest with vitals + a 2-process table.
	fakeA := &fakeGuestSandbox{
		vitalsResponse: &sandboxv1.GuestVitals{
			CpuStealPercent: 20.0,
			MemTotalBytes:   2048000 * 1024,
			MemUsedBytes:    1024000 * 1024,
			MemBalloonBytes: 512000 * 1024,
		},
		processesResponse: &sandboxv1.ProcessList{
			Processes: []*sandboxv1.ProcessInfo{
				{Pid: 1, Command: "agent", State: "S"},
				{Pid: 99, Command: "python", State: "R"},
			},
		},
	}
	sockA := filepath.Join(dir, "a.sock")
	startFakeGuestGRPCUDS(t, sockA, fakeA)
	if err := api.RegisterSandbox("sb-a", sockA); err != nil {
		t.Fatal(err)
	}
	api.RegisterStreamPath("sb-a", sockA)
	api.SetVitalsLabels("sb-a", VitalsLabels{Claim: "claim-a", Pool: "pool-x", Namespace: "team-ns"})

	// sb-down: registered but the guest socket never accepts, so its snapshot
	// errors and it must be skipped+counted, not fail the whole report.
	api.RegisterStreamPath("sb-down", filepath.Join(dir, "missing.sock"))
	api.SetVitalsLabels("sb-down", VitalsLabels{Claim: "claim-d", Pool: "pool-x"})

	req := httptest.NewRequest(http.MethodGet, "/v1/vitals/node", nil)
	rec := httptest.NewRecorder()
	api.handleNodeVitals(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got NodeVitals
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Sandboxes) != 1 {
		t.Fatalf("want 1 reachable sandbox, got %d (%+v)", len(got.Sandboxes), got.Sandboxes)
	}
	if got.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1 (sb-down)", got.Skipped)
	}
	e := got.Sandboxes[0]
	if e.Pool != "pool-x" || e.Claim != "claim-a" {
		t.Errorf("labels not carried: %+v", e.VitalsLabels)
	}
	if e.Vitals.StealFraction != 0.2 || e.Vitals.BalloonReclaimedKB != 512000 {
		t.Errorf("vitals not carried: %+v", e.Vitals)
	}
	if len(e.Vitals.Processes) != 2 {
		t.Errorf("process count = %d, want 2", len(e.Vitals.Processes))
	}
}

func TestHandleVitals_RequiresBearer(t *testing.T) {
	api := NewSandboxAPI(t.TempDir())
	api.RegisterToken("sb-1", "tok")
	req := httptest.NewRequest(http.MethodPost, "/v1/vitals", strings.NewReader(`{"sandbox":"sb-1"}`))
	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}
