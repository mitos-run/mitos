package controller

import (
	"testing"
	"time"

	"github.com/go-logr/logr"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// counterByLabel gathers the controller-runtime registry and returns the value
// of the named counter series whose labels include every given pair (0 if none).
func counterByLabel(t *testing.T, name string, want map[string]string) float64 {
	t.Helper()
	fams, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, fam := range fams {
		if fam.GetName() != name {
			continue
		}
		for _, m := range fam.GetMetric() {
			ok := true
			for k, v := range want {
				found := false
				for _, lp := range m.GetLabel() {
					if lp.GetName() == k && lp.GetValue() == v {
						found = true
						break
					}
				}
				if !found {
					ok = false
					break
				}
			}
			if ok {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// TestFleetMetricHelpers asserts the new fleet-observability counters move when
// their record* helpers run, and that forgetPoolMetrics drops the per-pool
// series. The helpers are unexported, so this is an internal-package test.
func TestFleetMetricHelpers(t *testing.T) {
	const pool = "ns/fleet-metric-pool"
	const node = "fleet-metric-node"

	cases := []struct {
		name   string
		metric string
		labels map[string]string
		record func()
	}{
		{"husk pod created", "mitos_husk_pod_created_total", map[string]string{"pool": pool}, func() { recordHuskPodCreated(pool) }},
		{"husk pod lost", "mitos_husk_pod_lost_total", map[string]string{"pool": pool}, func() { recordHuskPodLost(pool) }},
		{"node lost", "mitos_node_lost_total", map[string]string{"node": node}, func() { recordNodeLost(node) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := counterByLabel(t, tc.metric, tc.labels)
			tc.record()
			if got := counterByLabel(t, tc.metric, tc.labels); got != before+1 {
				t.Errorf("%s = %v, want %v", tc.metric, got, before+1)
			}
		})
	}

	t.Run("forget drops per-pool series", func(t *testing.T) {
		recordHuskPodCreated(pool)
		forgetPoolMetrics(pool)
		if got := counterByLabel(t, "mitos_husk_pod_created_total", map[string]string{"pool": pool}); got != 0 {
			t.Errorf("husk_pod_created_total after forget = %v, want 0", got)
		}
	})
}

// histogramCountByLabel returns the observation count of the named histogram
// series whose labels include every given pair (0 if none).
func histogramCountByLabel(t *testing.T, name string, want map[string]string) uint64 {
	t.Helper()
	fams, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, fam := range fams {
		if fam.GetName() != name {
			continue
		}
		for _, m := range fam.GetMetric() {
			ok := true
			for k, v := range want {
				found := false
				for _, lp := range m.GetLabel() {
					if lp.GetName() == k && lp.GetValue() == v {
						found = true
						break
					}
				}
				if !found {
					ok = false
					break
				}
			}
			if ok {
				return m.GetHistogram().GetSampleCount()
			}
		}
	}
	return 0
}

// TestSnapshotDistributionLagMetric asserts the multi-node snapshot-distribution
// lag histogram records an observation when observeSnapshotDistributionLag runs,
// and that an untouched template has an empty series (the value is meaningful
// only on the multi-node path; an empty series means no pull-based distribution
// happened, not zero lag).
func TestSnapshotDistributionLagMetric(t *testing.T) {
	const tmpl = "dist-lag-template"
	if got := histogramCountByLabel(t, "mitos_snapshot_distribution_lag_seconds", map[string]string{"template": tmpl}); got != 0 {
		t.Fatalf("untouched series count = %d, want 0", got)
	}
	observeSnapshotDistributionLag(tmpl, 1.5)
	if got := histogramCountByLabel(t, "mitos_snapshot_distribution_lag_seconds", map[string]string{"template": tmpl}); got != 1 {
		t.Errorf("after one observe, count = %d, want 1", got)
	}
}

// TestForkStageTimingMetric asserts the per-stage fork breakdown reaches the
// mitos_fork_stage_duration_seconds histogram: logForkStage must observe BOTH the
// controller-side RPC round-trip stage AND every husk-reported sub-stage as its
// own labeled series, and observeForkStage("total", ...) must record the
// end-to-end total. This is the check that a single hosted fork emits an
// attributable breakdown. The metric is unexported, so this is an
// internal-package test.
func TestForkStageTimingMetric(t *testing.T) {
	const rpcStage = "spawn_vm_rpc"
	// A representative husk sub-stage split of a co-located fork child: fresh
	// Firecracker boot dominates, the CoW rootfs clone and vmstate restore are
	// cheap, and the guest-ready wait is the second cost.
	huskStages := map[string]float64{
		"fc_boot":         42.0,
		"rootfs_clone":    3.0,
		"vmstate_restore": 5.0,
		"guest_ready":     18.0,
		"handshake":       2.0,
	}

	before := map[string]uint64{rpcStage: histogramCountByLabel(t, "mitos_fork_stage_duration_seconds", map[string]string{"stage": rpcStage})}
	for stage := range huskStages {
		before[stage] = histogramCountByLabel(t, "mitos_fork_stage_duration_seconds", map[string]string{"stage": stage})
	}

	logForkStage(logr.Discard(), "fork-timing-test", rpcStage, 71*time.Millisecond, 65.0, huskStages)

	if got := histogramCountByLabel(t, "mitos_fork_stage_duration_seconds", map[string]string{"stage": rpcStage}); got != before[rpcStage]+1 {
		t.Errorf("round-trip stage %q count = %d, want %d", rpcStage, got, before[rpcStage]+1)
	}
	for stage := range huskStages {
		if got := histogramCountByLabel(t, "mitos_fork_stage_duration_seconds", map[string]string{"stage": stage}); got != before[stage]+1 {
			t.Errorf("husk sub-stage %q count = %d, want %d", stage, got, before[stage]+1)
		}
	}

	beforeTotal := histogramCountByLabel(t, "mitos_fork_stage_duration_seconds", map[string]string{"stage": "total"})
	observeForkStage("total", 0.728)
	if got := histogramCountByLabel(t, "mitos_fork_stage_duration_seconds", map[string]string{"stage": "total"}); got != beforeTotal+1 {
		t.Errorf("total stage count = %d, want %d", got, beforeTotal+1)
	}
}

// TestClaimStageTimingMetricIsSeparateFromForkStages asserts that warm-claim stage
// timings land on their own histogram. mitos_fork_stage_duration_seconds documents a
// fixed fork-stage vocabulary; folding claim_* labels into it would make any
// whole-metric aggregation over fork stages silently include claim work.
func TestClaimStageTimingMetricIsSeparateFromForkStages(t *testing.T) {
	const stage = "activate_rpc"

	forkBefore := histogramCountByLabel(t, "mitos_fork_stage_duration_seconds", map[string]string{"stage": "claim_" + stage})
	claimBefore := histogramCountByLabel(t, "mitos_claim_stage_duration_seconds", map[string]string{"stage": stage})

	observeClaimStage(stage, 0.061)

	if got := histogramCountByLabel(t, "mitos_claim_stage_duration_seconds", map[string]string{"stage": stage}); got != claimBefore+1 {
		t.Errorf("claim stage count = %d, want %d", got, claimBefore+1)
	}
	if got := histogramCountByLabel(t, "mitos_fork_stage_duration_seconds", map[string]string{"stage": "claim_" + stage}); got != forkBefore {
		t.Errorf("claim timing leaked into the fork-stage histogram: count = %d, want %d", got, forkBefore)
	}
}
