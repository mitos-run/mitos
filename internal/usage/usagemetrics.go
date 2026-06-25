package usage

import "github.com/prometheus/client_golang/prometheus"

// Metrics is the Prometheus view of the SAME cumulative per-org usage the billing
// store rolls up (issue #164): one per-org series so the billing data is
// immediately visible on the operator dashboard, without standing up a separate
// metrics pipeline.
//
// SYSTEM OF RECORD: the metric is driven from the store's per-org CUMULATIVE
// Totals (UsageStore TotalsProvider.TotalsByOrg), the same number the bill and the
// usage API report, NOT from a pruned sample buffer. So each series is monotonic
// within a process: a Gauge Set to the cumulative store total never goes backwards
// and never drops a known org just because that org was quiet this cycle. The
// durable store (follow-up) is the billing system of record; this in-memory
// cumulative is best-effort across controller restarts, which is why a restart is
// the only event that can reset a series.
//
// CARDINALITY + SECRET HYGIENE: the only label is org (an org id, never a secret,
// never a sandbox id), so the series count is bounded by the org count, not by the
// sandbox count. No argv, env, file bytes, node identity, or sandbox id ever
// enters a label or value. Disk/storage are intentionally NOT exported here: the
// keystone series is vCPU-seconds (the primary billable rate unit), with memory
// GiB-seconds, egress bytes, and GPU-seconds as the companion billable
// dimensions (issue #164 Phase 1.b).
type Metrics struct {
	vcpuSeconds *prometheus.GaugeVec
	memGiBSecs  *prometheus.GaugeVec
	egressBytes *prometheus.GaugeVec
	gpuSeconds  *prometheus.GaugeVec
}

// NewMetrics builds the per-org usage metric vectors. They are unregistered; the
// wiring (the controller) registers them onto its metrics registry with
// MustRegister so they appear on the controller's /metrics endpoint.
//
// NAMING: the _total suffix is honest because each series is driven from the
// store's per-org cumulative Totals, which is monotonic within a process and
// survives record eviction (the bounded record map does NOT bound this
// cumulative). It is reset only by a controller restart, where the durable store
// is the billing system of record.
func NewMetrics() *Metrics {
	return &Metrics{
		vcpuSeconds: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mitos_usage_vcpu_seconds_total",
			Help: "Cumulative vCPU-seconds of billable sandbox usage by org, summed over every settled usage record (the same number the bill rolls up). Monotonic within a controller process; reset only by a restart, where the durable store is the system of record.",
		}, []string{"org"}),
		memGiBSecs: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mitos_usage_mem_gib_seconds_total",
			Help: "Cumulative memory GiB-seconds of billable sandbox usage by org (CoW-aware), summed over every settled usage record. Monotonic within a controller process; reset only by a restart.",
		}, []string{"org"}),
		egressBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mitos_usage_egress_bytes_total",
			Help: "Cumulative egress bytes of billable sandbox usage by org, summed over every settled usage record. Monotonic within a controller process; reset only by a restart.",
		}, []string{"org"}),
		// GPU-seconds is the fourth billable metering dimension already carried in the
		// store's per-org cumulative Totals (issue #164 Phase 1.b). It is published from
		// the SAME OnTotals path as the others, so it is monotonic and identical to the
		// billed figure. The label set is org only; pool is NOT in Totals today (the
		// store keys records on (org, sandbox, window)), so a pool label is a documented
		// follow-up that requires the metering report to carry the husk pod's pool.
		gpuSeconds: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mitos_usage_gpu_seconds_total",
			Help: "Cumulative GPU-seconds of billable sandbox usage by org, summed over every settled usage record. Monotonic within a controller process; reset only by a restart, where the durable store is the system of record.",
		}, []string{"org"}),
	}
}

// MustRegister registers the per-org usage metric vectors on reg (the controller's
// metrics registry). It panics on a duplicate registration, the standard
// fail-fast for a misconfigured wiring.
func (m *Metrics) MustRegister(reg prometheus.Registerer) {
	reg.MustRegister(m.vcpuSeconds, m.memGiBSecs, m.egressBytes, m.gpuSeconds)
}

// Observe sets the per-org gauges from the store's CUMULATIVE per-org Totals. It
// is the cycle hook the collector wiring calls with store.TotalsByOrg() after each
// upsert. Because the input is the monotonic cumulative (sum of ALL settled
// records, surviving eviction), Set lands each series on the same number the bill
// holds, never goes backwards, and never drops a known org that was quiet this
// cycle. An org with empty id never reaches the store totals (the self-host path
// carries no org to bill), so no empty-org series is emitted.
//
// It does NOT Reset the vectors: a Reset followed by setting only the orgs present
// this cycle is exactly the bug that dropped quiet orgs. The store's cumulative
// map is the complete set of known orgs, so every known org is Set every cycle.
func (m *Metrics) Observe(totals map[string]Totals) {
	for org, t := range totals {
		if org == "" {
			continue
		}
		m.vcpuSeconds.WithLabelValues(org).Set(t.VCPUSeconds)
		m.memGiBSecs.WithLabelValues(org).Set(t.MemGiBSeconds)
		m.egressBytes.WithLabelValues(org).Set(float64(t.EgressBytes))
		m.gpuSeconds.WithLabelValues(org).Set(float64(t.GPUSeconds))
	}
}
