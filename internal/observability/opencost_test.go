package observability

import (
	"math"
	"testing"
)

// TestReconcileNamespaceCost_WithinTolerance is the issue's Layer 2 test:
// per-namespace OpenCost spend must reconcile with the sum of the namespace's
// claim resource-seconds priced at the cluster rates, within a tolerance. Here
// two claims accrue cpu-seconds and memory-GB-seconds; priced at the rates they
// should equal the OpenCost-reported namespace spend, so the reconcile passes.
func TestReconcileNamespaceCost_WithinTolerance(t *testing.T) {
	rates := Rates{CPUCoreHour: 0.04, MemGBHour: 0.005}
	claims := []ClaimUsage{
		// 2 cores for 1800s, 4 GB for 1800s.
		{Claim: "a", CPUCoreSeconds: 2 * 1800, MemGBSeconds: 4 * 1800},
		// 1 core for 3600s, 2 GB for 3600s.
		{Claim: "b", CPUCoreSeconds: 1 * 3600, MemGBSeconds: 2 * 3600},
	}
	// Expected priced spend: convert core-seconds to core-hours.
	wantCPU := (2*1800.0 + 1*3600.0) / 3600.0 * 0.04
	wantMem := (4*1800.0 + 2*3600.0) / 3600.0 * 0.005
	reported := wantCPU + wantMem

	r := ReconcileNamespaceCost("tenant-a", reported, claims, rates, 0.01)
	if !r.WithinTolerance {
		t.Errorf("expected reconcile within tolerance, got drift %.4f (expected %.4f, reported %.4f)",
			r.RelativeDrift, r.ExpectedCost, r.ReportedCost)
	}
	if math.Abs(r.ExpectedCost-reported) > 1e-9 {
		t.Errorf("expected cost = %.6f, want %.6f", r.ExpectedCost, reported)
	}
}

// TestReconcileNamespaceCost_DriftFails: an OpenCost report 20% above the priced
// resource-seconds is outside a 5% tolerance, so the reconcile flags drift (a
// pricing or attribution discrepancy an operator must investigate).
func TestReconcileNamespaceCost_DriftFails(t *testing.T) {
	rates := Rates{CPUCoreHour: 0.04, MemGBHour: 0.005}
	claims := []ClaimUsage{{Claim: "a", CPUCoreSeconds: 3600, MemGBSeconds: 3600}}
	// 1 core-hour priced at 0.04 + 1 GB-hour priced at 0.005.
	expected := 0.04 + 0.005
	reported := expected * 1.20 // 20% high

	r := ReconcileNamespaceCost("tenant-a", reported, claims, rates, 0.05)
	if r.WithinTolerance {
		t.Error("expected drift to exceed 5% tolerance")
	}
	if r.RelativeDrift < 0.19 || r.RelativeDrift > 0.21 {
		t.Errorf("relative drift = %.4f, want ~0.20", r.RelativeDrift)
	}
}

// A zero expected cost (no usage) with a zero report reconciles; a zero expected
// cost with a nonzero report is full drift, not a divide-by-zero.
func TestReconcileNamespaceCost_ZeroUsage(t *testing.T) {
	rates := Rates{CPUCoreHour: 0.04, MemGBHour: 0.005}
	if r := ReconcileNamespaceCost("ns", 0, nil, rates, 0.05); !r.WithinTolerance {
		t.Error("zero report and zero usage should reconcile")
	}
	if r := ReconcileNamespaceCost("ns", 5.0, nil, rates, 0.05); r.WithinTolerance {
		t.Error("nonzero report with zero usage should be flagged, not divide-by-zero")
	}
}

func TestPriceResourceSeconds(t *testing.T) {
	rates := Rates{CPUCoreHour: 0.04, MemGBHour: 0.005}
	// 1 core-hour + 1 GB-hour.
	got := priceResourceSeconds(ClaimUsage{CPUCoreSeconds: 3600, MemGBSeconds: 3600}, rates)
	want := 0.04 + 0.005
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("priced = %.6f, want %.6f", got, want)
	}
}
