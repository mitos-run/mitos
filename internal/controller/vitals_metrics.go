package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// vitalsMetrics is the Prometheus view of the guests' live health (issue #164
// Phase 1.a): per (org, pool) gauges for CPU steal, balloon-reclaimed memory,
// used memory, and the in-guest process count, sampled from forkd's node-level
// GET /v1/vitals/node and attributed to an org via the SAME trusted
// mitos.run/org husk-pod label the usage scraper uses.
//
// AGGREGATION (documented in the Help text): a guest vital is a PER-SANDBOX
// value, but the metric is keyed on (org, pool), so the sampler must aggregate
// the sandboxes in each (org, pool) bucket. The choice is the "is my org's
// fleet starved" signal:
//
//   - cpu_steal_percent: MAX across the bucket's sandboxes. The worst-starved
//     sandbox is the one that hurts; a SUM or AVG would hide a single pinned
//     sandbox behind quiet neighbors.
//   - mem_balloon_bytes, mem_used_bytes: SUM across the bucket. Fleet memory
//     pressure is additive; the operator wants the org/pool footprint.
//   - process_count: SUM across the bucket. The total in-guest process count
//     for the org/pool fleet.
//
// CARDINALITY + SECRET HYGIENE: the label set is EXACTLY {org, pool}, both
// bounded, trusted control-plane values (an org id and a SandboxPool name, never
// a secret, never a sandbox id, never a pid). process_count is the LENGTH of the
// guest process list, a number; no per-process command line, argv, pid, env, or
// any free-form string ever enters a label or a value. There is deliberately no
// per-sandbox or per-pid label, so the series count is bounded by (org x pool),
// not by the sandbox count.
type vitalsMetrics struct {
	cpuStealPercent *prometheus.GaugeVec
	memBalloonBytes *prometheus.GaugeVec
	memUsedBytes    *prometheus.GaugeVec
	processCount    *prometheus.GaugeVec
}

func newVitalsMetrics() *vitalsMetrics {
	labels := []string{"org", "pool"}
	return &vitalsMetrics{
		cpuStealPercent: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mitos_guest_cpu_steal_percent",
			Help: "Max CPU steal percent (0-100) across the guests in this org and pool, sampled from forkd GET /v1/vitals/node. Max is the worst-starved sandbox in the bucket; the 'is my org's fleet starved' signal. Labels are org and pool only, never a sandbox id or secret.",
		}, labels),
		memBalloonBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mitos_guest_mem_balloon_bytes",
			Help: "Sum of host-balloon-reclaimed guest memory bytes across the guests in this org and pool. Sum is the additive org/pool footprint. Labels are org and pool only, never a sandbox id or secret.",
		}, labels),
		memUsedBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mitos_guest_mem_used_bytes",
			Help: "Sum of guest-used memory bytes across the guests in this org and pool. Labels are org and pool only, never a sandbox id or secret.",
		}, labels),
		processCount: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mitos_guest_process_count",
			Help: "Sum of the in-guest process count (the LENGTH of each guest's process table, a number) across the guests in this org and pool. No process command, argv, pid, or env is ever exported. Labels are org and pool only.",
		}, labels),
	}
}

// mustRegister registers the guest vitals gauges on reg. It panics on a duplicate
// registration, the standard fail-fast for a misconfigured wiring.
func (m *vitalsMetrics) mustRegister(reg prometheus.Registerer) {
	reg.MustRegister(m.cpuStealPercent, m.memBalloonBytes, m.memUsedBytes, m.processCount)
}

// observe replaces the published series with the supplied per-(org, pool)
// aggregates. It Resets first because, unlike the store-fed usage cumulative,
// these are instantaneous gauges: a sandbox that went away this cycle should drop
// its contribution, and an org/pool with no guests this cycle should have no
// stale series. Reset+Set is correct here precisely because the value is a live
// level, not a monotonic total.
func (m *vitalsMetrics) observe(aggs map[vitalsKey]vitalsAgg) {
	m.cpuStealPercent.Reset()
	m.memBalloonBytes.Reset()
	m.memUsedBytes.Reset()
	m.processCount.Reset()
	for k, a := range aggs {
		m.cpuStealPercent.WithLabelValues(k.org, k.pool).Set(a.maxStealPercent)
		m.memBalloonBytes.WithLabelValues(k.org, k.pool).Set(float64(a.sumBalloonBytes))
		m.memUsedBytes.WithLabelValues(k.org, k.pool).Set(float64(a.sumUsedBytes))
		m.processCount.WithLabelValues(k.org, k.pool).Set(float64(a.sumProcessCount))
	}
}

// guestVitalsMetrics is the package-level guest vitals view, registered ONCE on
// the controller-runtime metrics registry so the series appear on the
// controller's /metrics endpoint. It is populated only when the --vitals-sampler
// flag turns the sampler on; otherwise it registers with no series.
var guestVitalsMetrics = newVitalsMetrics()

func init() {
	guestVitalsMetrics.mustRegister(ctrlmetrics.Registry)
}
