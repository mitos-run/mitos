package controller

import (
	"testing"

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
