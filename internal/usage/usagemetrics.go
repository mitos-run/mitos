package usage

import "github.com/prometheus/client_golang/prometheus"

// Metrics is the dual-use Prometheus view of the SAME integrated UsageRecords the
// collector upserts (issue #164): one per-org series so the billing data is
// immediately visible on the operator dashboard, without standing up a separate
// metrics pipeline. The collector calls Observe with each cycle's records via its
// OnRecords hook, so the metric and the store are always the same numbers.
//
// CARDINALITY + SECRET HYGIENE: the only label is org (an org id, never a secret,
// never a sandbox id), so the series count is bounded by the org count, not by the
// sandbox count. No argv, env, file bytes, node identity, or sandbox id ever
// enters a label or value. Disk/storage are intentionally NOT exported here: the
// one keystone series is vCPU-seconds (the primary billable rate unit), with
// memory and egress as opt-in companions.
type Metrics struct {
	vcpuSeconds *prometheus.GaugeVec
	memGiBSecs  *prometheus.GaugeVec
	egressBytes *prometheus.GaugeVec
}

// NewMetrics builds the per-org usage metric vectors. They are unregistered; the
// wiring (the controller) registers them onto its metrics registry with
// MustRegister so they appear on the controller's /metrics endpoint.
func NewMetrics() *Metrics {
	return &Metrics{
		vcpuSeconds: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mitos_usage_vcpu_seconds_total",
			Help: "Integrated vCPU-seconds of billable sandbox usage, by org, as of the last collector cycle.",
		}, []string{"org"}),
		memGiBSecs: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mitos_usage_mem_gib_seconds_total",
			Help: "Integrated memory GiB-seconds of billable sandbox usage (CoW-aware), by org, as of the last collector cycle.",
		}, []string{"org"}),
		egressBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mitos_usage_egress_bytes_total",
			Help: "Integrated egress bytes of billable sandbox usage, by org, as of the last collector cycle.",
		}, []string{"org"}),
	}
}

// MustRegister registers the per-org usage metric vectors on reg (the controller's
// metrics registry). It panics on a duplicate registration, the standard
// fail-fast for a misconfigured wiring.
func (m *Metrics) MustRegister(reg prometheus.Registerer) {
	reg.MustRegister(m.vcpuSeconds, m.memGiBSecs, m.egressBytes)
}

// Observe sets the per-org gauges from one collector cycle's integrated records.
// It is the Collector.OnRecords hook. It RESETS the vectors first, then sets each
// org to the SUM of its records' billable units across the cycle, so a re-scrape
// of the same window lands on the same value (idempotent, never doubled) and an
// org with no records in this cycle drops to absent rather than holding a stale
// value. An unattributed record (empty OrgID, the self-host path) is skipped: it
// is in the physical-footprint totals but carries no org to bill.
func (m *Metrics) Observe(recs []UsageRecord) {
	m.vcpuSeconds.Reset()
	m.memGiBSecs.Reset()
	m.egressBytes.Reset()

	type agg struct {
		vcpu   float64
		mem    float64
		egress float64
	}
	byOrg := map[string]*agg{}
	for _, r := range recs {
		if r.OrgID == "" {
			continue
		}
		a := byOrg[r.OrgID]
		if a == nil {
			a = &agg{}
			byOrg[r.OrgID] = a
		}
		a.vcpu += r.VCPUSeconds
		a.mem += r.MemGiBSeconds
		a.egress += float64(r.EgressBytes)
	}
	for org, a := range byOrg {
		m.vcpuSeconds.WithLabelValues(org).Set(a.vcpu)
		m.memGiBSecs.WithLabelValues(org).Set(a.mem)
		m.egressBytes.WithLabelValues(org).Set(a.egress)
	}
}
