package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"mitos.run/mitos/internal/fork"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// The per-sandbox, bearer-authenticated guest vitals snapshot (the labeled
// process table for `kubectl mitos ps`/top) is served by the Connect
// sandbox.v1.Sandbox.Vitals RPC; the legacy /v1/vitals JSON route was removed in
// #358. The Connect Vitals path is covered by internal/sandboxrpc/vitals_test.go.
// vitalsSnapshot (the shared host-side gRPC consumer this file once exercised
// through /v1/vitals) is still exercised here via the node batch endpoint
// (TestHandleNodeVitals_Batch), which reads the same snapshot and strips it to
// numeric fields.

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
// control-plane sampler scrapes (issue #164 Phase 1.a): it returns one numeric
// NodeVitalsEntry per guest-reachable sandbox on this forkd, and a sandbox whose
// guest is unreachable is skipped+counted, never failing the report.
//
// SECRET HYGIENE (the point of this test): this endpoint is UNAUTHENTICATED, so it
// must carry ONLY the numeric vitals plus a numeric process_count, NEVER the
// per-process table. The assertions below confirm process_count == 2 for the
// 2-process fixture and that the serialized JSON does not contain any per-process
// command string ("agent"/"python") or per-process key (comm/pid/state).
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

	// Regression guard: the raw JSON wire MUST NOT contain any per-process command
	// name or per-process field key. The unauthenticated node endpoint leaks only
	// numeric vitals plus a count; re-embedding the table would re-leak comm/pid/state.
	raw := rec.Body.String()
	for _, forbidden := range []string{"agent", "python", "comm", `"pid"`, `"state"`, `"rss_kb"`, "processes"} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("node vitals JSON leaks per-process token %q; body = %s", forbidden, raw)
		}
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
	// Only the numeric process_count crosses the wire, never the table.
	if e.Vitals.ProcessCount != 2 {
		t.Errorf("process_count = %d, want 2", e.Vitals.ProcessCount)
	}
}
