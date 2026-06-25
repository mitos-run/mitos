package main

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/metering"
)

func topSandbox(name, node, endpoint, sandboxID string) v1.Sandbox {
	return v1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: v1.SandboxStatus{
			Node:      node,
			Endpoint:  endpoint,
			SandboxID: sandboxID,
		},
	}
}

func TestBuildTopRowsMatchesBySandboxID(t *testing.T) {
	sandboxes := []v1.Sandbox{
		topSandbox("alpha", "node-1", "10.0.0.5:9091", "sbx-alpha"),
		topSandbox("beta", "node-1", "10.0.0.5:9091", "sbx-beta"),
	}
	fetch := func(_ context.Context, endpoint string) (metering.Report, bool) {
		if endpoint != "10.0.0.5:9091" {
			t.Fatalf("unexpected endpoint %q", endpoint)
		}
		return metering.Report{
			Sandboxes: []metering.SandboxMetering{
				{ID: "sbx-alpha", MemoryUnique: 100, MemoryShared: 200},
			},
		}, true
	}
	rows := buildTopRows(context.Background(), sandboxes, fetch)
	byName := map[string]topRowResult{}
	for _, r := range rows {
		byName[r.Name] = topRowResult{found: r.Found, unique: r.Datum.MemoryUnique}
	}
	if !byName["alpha"].found || byName["alpha"].unique != 100 {
		t.Errorf("alpha should have its datum, got %+v", byName["alpha"])
	}
	// beta has no row in the report: it must be Found=false (a dash), never a 0.
	if byName["beta"].found {
		t.Errorf("beta has no metering row, must be Found=false, got %+v", byName["beta"])
	}
}

func TestBuildTopRowsFetchesEachEndpointOnce(t *testing.T) {
	sandboxes := []v1.Sandbox{
		topSandbox("a", "node-1", "ep1", "sa"),
		topSandbox("b", "node-1", "ep1", "sb"),
		topSandbox("c", "node-2", "ep2", "sc"),
	}
	calls := map[string]int{}
	fetch := func(_ context.Context, endpoint string) (metering.Report, bool) {
		calls[endpoint]++
		return metering.Report{}, true
	}
	_ = buildTopRows(context.Background(), sandboxes, fetch)
	if calls["ep1"] != 1 {
		t.Errorf("ep1 should be fetched once (memoized), got %d", calls["ep1"])
	}
	if calls["ep2"] != 1 {
		t.Errorf("ep2 should be fetched once, got %d", calls["ep2"])
	}
}

func TestBuildTopRowsUnreachableFetchIsDash(t *testing.T) {
	sandboxes := []v1.Sandbox{topSandbox("alpha", "node-1", "ep", "sbx")}
	fetch := func(_ context.Context, _ string) (metering.Report, bool) {
		return metering.Report{}, false // forkd unreachable
	}
	rows := buildTopRows(context.Background(), sandboxes, fetch)
	if rows[0].Found {
		t.Errorf("an unreachable forkd must yield Found=false, got %+v", rows[0])
	}
}

func TestBuildTopRowsNoEndpointIsDash(t *testing.T) {
	sandboxes := []v1.Sandbox{topSandbox("pending", "", "", "")}
	calls := 0
	fetch := func(_ context.Context, _ string) (metering.Report, bool) {
		calls++
		return metering.Report{}, true
	}
	rows := buildTopRows(context.Background(), sandboxes, fetch)
	if rows[0].Found {
		t.Errorf("a sandbox with no endpoint must be Found=false, got %+v", rows[0])
	}
	if calls != 0 {
		t.Errorf("no endpoint should not trigger a fetch, got %d calls", calls)
	}
}

type topRowResult struct {
	found  bool
	unique int64
}
