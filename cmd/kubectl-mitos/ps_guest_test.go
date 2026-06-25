package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	connect "connectrpc.com/connect"
	sandboxv1 "mitos.run/mitos/proto/sandbox/v1"
)

// TestFetchGuestVitals_FirstSampleAndProcesses verifies the ps --processes path
// builds the snapshot from TWO Connect calls: it takes the FIRST GuestVitals
// sample (numeric fields) AND the Processes table, mapping both into the local
// struct. The bearer token and sandbox id must ride on the headers of BOTH calls,
// and the rendered table must show the real process rows over Connect.
func TestFetchGuestVitals_FirstSampleAndProcesses(t *testing.T) {
	svc := &fakeSandboxSvc{
		vitalsSamples: []*sandboxv1.GuestVitals{
			{
				CpuStealPercent: 10, // percent; the local struct holds a [0,1] fraction
				MemTotalBytes:   2048000 * 1024,
				MemUsedBytes:    1024000 * 1024,
				MemBalloonBytes: 512000 * 1024,
				ProcessCount:    2,
			},
			// A second sample must be ignored: only the first is read.
			{CpuStealPercent: 99},
		},
		procList: &sandboxv1.ProcessList{
			Processes: []*sandboxv1.ProcessInfo{
				{Pid: 1, Command: "agent", State: "S", RssBytes: 4096 * 1024},
				{Pid: 99, Command: "python", State: "R", RssBytes: 65536 * 1024},
			},
		},
	}
	endpoint := newFakeSandboxServer(t, svc)

	v, err := fetchGuestVitals(context.Background(), nil, endpoint, "tok-123", "sb-1")
	if err != nil {
		t.Fatalf("fetchGuestVitals: %v", err)
	}
	// Both the Vitals and the Processes call must carry the same auth headers.
	if svc.gotAuth != "Bearer tok-123" || svc.gotSandboxID != "sb-1" {
		t.Errorf("vitals headers = (%q,%q), want (Bearer tok-123, sb-1)", svc.gotAuth, svc.gotSandboxID)
	}
	if svc.procAuth != "Bearer tok-123" || svc.procSandboxID != "sb-1" {
		t.Errorf("processes headers = (%q,%q), want (Bearer tok-123, sb-1)", svc.procAuth, svc.procSandboxID)
	}
	if v.Vitals.StealFraction != 0.1 {
		t.Errorf("steal fraction = %v, want 0.1", v.Vitals.StealFraction)
	}
	if v.Vitals.MemTotalKB != 2048000 || v.Vitals.MemUsedKB != 1024000 {
		t.Errorf("mem = %d/%d KB, want 1024000/2048000", v.Vitals.MemUsedKB, v.Vitals.MemTotalKB)
	}
	if v.Vitals.BalloonReclaimedKB != 512000 {
		t.Errorf("balloon = %d KB, want 512000", v.Vitals.BalloonReclaimedKB)
	}
	// The process table must be populated from the Processes RPC.
	if len(v.Vitals.Processes) != 2 {
		t.Fatalf("processes = %+v, want 2 rows", v.Vitals.Processes)
	}
	if v.Vitals.Processes[0].Comm != "agent" || v.Vitals.Processes[0].PID != 1 {
		t.Errorf("row 0 = %+v, want agent pid 1", v.Vitals.Processes[0])
	}
	if v.Vitals.Processes[1].Comm != "python" || v.Vitals.Processes[1].State != "R" || v.Vitals.Processes[1].RSSKB != 65536 {
		t.Errorf("row 1 = %+v, want python R 65536 KB", v.Vitals.Processes[1])
	}
	// The user-visible table must render the real rows over Connect.
	out := renderGuestProcesses(v)
	for _, want := range []string{"PID", "COMMAND", "STATE", "agent", "python", "99"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered table missing %q:\n%s", want, out)
		}
	}
}

func TestFetchGuestVitals_Unreachable(t *testing.T) {
	// A non-routable endpoint must error so ps falls back to the object listing
	// rather than rendering a fabricated table.
	_, err := fetchGuestVitals(context.Background(), http.DefaultClient, "127.0.0.1:0", "tok", "sb-1")
	if err == nil {
		t.Error("expected error for unreachable endpoint")
	}
}

// TestFetchGuestVitals_ProcessesFailureDegrades verifies that when the Vitals
// call succeeds but the Processes call fails, the whole snapshot errors so the
// caller falls back to the object listing instead of rendering vitals with an
// empty table.
func TestFetchGuestVitals_ProcessesFailureDegrades(t *testing.T) {
	svc := &fakeSandboxSvc{
		vitalsSamples: []*sandboxv1.GuestVitals{{CpuStealPercent: 1}},
		procErr:       connect.NewError(connect.CodeUnavailable, errInline("guest table unavailable")),
	}
	endpoint := newFakeSandboxServer(t, svc)

	_, err := fetchGuestVitals(context.Background(), nil, endpoint, "tok", "sb-1")
	if err == nil {
		t.Fatal("a Processes failure must error so ps degrades to the object listing")
	}
}

// TestRenderGuestProcesses formats the real process table; this is the text a
// user sees from `kubectl mitos ps <name> --processes`.
func TestRenderGuestProcesses(t *testing.T) {
	v := labeledVitals{
		Claim:     "claim-a",
		Namespace: "team-ns",
		Vitals: guestVitals{
			Processes: []guestProcess{
				{PID: 1, Comm: "agent", State: "S", CPUJiffies: 42, RSSKB: 4096},
				{PID: 99, Comm: "python", State: "R", CPUJiffies: 7, RSSKB: 65536},
			},
		},
	}
	out := renderGuestProcesses(v)
	for _, want := range []string{"NAMESPACE", "team-ns", "PID", "COMMAND", "STATE", "RSS", "agent", "python", "99"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}
