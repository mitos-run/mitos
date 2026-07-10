package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Controller-level metrics. These register with controller-runtime's global
// Registry so they appear on the controller's own /metrics endpoint alongside
// the built-in workqueue and reconcile metrics. The node-level fork metrics
// (active sandboxes, fork duration) live in the daemon on the default
// prometheus registry; these are distinct, controller-scoped signals.
//
// No metric carries secret values: labels are pool names and coarse failure
// reasons only.
var (
	// claimPendingTotal counts how many times a claim was requeued because no
	// node had a ready snapshot (the claim stayed Pending). A counter of
	// pending-requeue EVENTS is used rather than a live gauge of currently
	// pending claims: a counter is exact and lock-free to bump at the requeue
	// site, while an honest live gauge would need a periodic recount of all
	// Pending claims (a separate scan with its own staleness window). The
	// counter answers "how often are claims failing to place" directly.
	claimPendingTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "mitos_claim_pending_total",
		Help: "Number of times a claim was requeued for no node with a ready snapshot (claim stayed Pending).",
	})

	// orphanSweepsTotal counts forkd VMs reaped by the GC orphan sweep.
	orphanSweepsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "mitos_orphan_sweeps_total",
		Help: "Number of orphan sandbox VMs terminated by the garbage collector.",
	})

	// volumeOrphanSweepsTotal counts per-sandbox volume backings reclaimed by the
	// GC volume-orphan sweep (a backing whose claim object is gone). It is the
	// volume counterpart to orphanSweepsTotal.
	volumeOrphanSweepsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "mitos_volume_orphan_sweeps_total",
		Help: "Number of orphan volume backings reclaimed by the garbage collector.",
	})

	// claimErrorsTotal counts terminal claim failures, labeled by pool and a
	// coarse reason (fork, secret, volume, token). Reasons are fixed strings,
	// never error text, so no secret or path leaks into a label value.
	claimErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mitos_claim_errors_total",
		Help: "Number of claims that failed terminally, by pool and reason.",
	}, []string{"pool", "reason"})

	// poolReadySnapshots is the per-pool count of ready snapshots, set each pool
	// reconcile. It mirrors SandboxPool.Status.ReadySnapshots as a scrapeable
	// gauge.
	poolReadySnapshots = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mitos_pool_ready_snapshots",
		Help: "Ready snapshots per pool, as of the last pool reconcile.",
	}, []string{"pool"})

	// poolWarmDormant is the per-pool count of DORMANT (unclaimed, warm) husk
	// pods as of the last pool reconcile: the live warm-buffer size.
	poolWarmDormant = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mitos_pool_warm_dormant",
		Help: "Dormant (unclaimed, warm) husk pods per pool, as of the last reconcile.",
	}, []string{"pool"})

	// poolWarmInUse is the per-pool count of claimed/active husk pods (pods
	// carrying mitos.run/claim): the demand the autoscaler sizes against.
	poolWarmInUse = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mitos_pool_warm_in_use",
		Help: "Claimed/active husk pods per pool, as of the last reconcile.",
	}, []string{"pool"})

	// poolDesiredWarm is the per-pool autoscaler target dormant count this
	// reconcile (mirrors SandboxPool.Status.DesiredWarm).
	poolDesiredWarm = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mitos_pool_desired_warm",
		Help: "Autoscaler desired dormant husk pod count per pool, as of the last reconcile.",
	}, []string{"pool"})

	// warmScaleUpTotal and warmScaleDownTotal count autoscaler scale events per
	// pool, so an operator can alert on thrash or a stuck pool.
	warmScaleUpTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mitos_pool_warm_scale_up_total",
		Help: "Number of times the warm-pool autoscaler increased the dormant count, by pool.",
	}, []string{"pool"})
	warmScaleDownTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mitos_pool_warm_scale_down_total",
		Help: "Number of times the warm-pool autoscaler decreased the dormant count, by pool.",
	}, []string{"pool"})

	// refillLatencySeconds measures wall-clock from a husk pod object Create to
	// the pod being counted Ready+dormant (a warm slot), the refill cost the
	// fast-refill follow-up reduces. Buckets span sub-second to the ~10-14 s cold
	// start.
	refillLatencySeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "mitos_pool_refill_latency_seconds",
		Help:    "Seconds from creating a husk pod to it becoming a ready dormant warm slot.",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 4, 8, 12, 20, 30},
	})

	// huskPodCreatedTotal counts husk pods the controller created to fill the warm
	// pool, by pool. With the warm gauges it shows pool churn: a high create rate
	// against a flat dormant gauge means pods are being lost as fast as made.
	huskPodCreatedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mitos_husk_pod_created_total",
		Help: "Number of husk pods the controller created to fill the warm pool, by pool.",
	}, []string{"pool"})

	// huskPodLostTotal counts times an active claim re-pended because its backing
	// husk pod was lost (node drain, eviction, deletion), by pool. A sustained
	// nonzero rate is fleet instability the warm pool is absorbing.
	huskPodLostTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mitos_husk_pod_lost_total",
		Help: "Number of times an active claim re-pended because its backing husk pod was lost, by pool.",
	}, []string{"pool"})

	// nodeLostTotal counts Ready claims marked NodeLost because their node became
	// unhealthy or left the registry (raw-forkd path; the husk path re-pends and
	// is counted by huskPodLostTotal instead). Labeled by node, never a secret.
	nodeLostTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mitos_node_lost_total",
		Help: "Number of Ready claims marked NodeLost after their node went unhealthy (raw-forkd path), by node.",
	}, []string{"node"})

	// nodeLostReforkTotal counts raw-forkd claims that were re-pended for
	// automatic re-fork onto a surviving snapshot holder after their node was lost
	// (issue #372), rather than failed closed. Labeled by the LOST node, never a
	// secret. A claim that fails closed (no surviving holder, or the bounded
	// retries exhausted) is counted by nodeLostTotal instead.
	nodeLostReforkTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mitos_node_lost_refork_total",
		Help: "Number of raw-forkd claims re-pended for automatic re-fork onto a surviving snapshot holder after node loss, by lost node.",
	}, []string{"node"})

	// claimWaitForWarmSeconds measures, per claim, the wall-clock the claim waited
	// for a ready dormant pod from its creation to a successful husk activate. A
	// burst absorbed by warm capacity shows up near the activate cost (~27 ms);
	// a claim that had to wait for a cold-started pod shows up in the seconds.
	claimWaitForWarmSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "mitos_claim_wait_for_warm_seconds",
		Help:    "Seconds a claim waited from creation to activating a warm husk pod.",
		Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 15},
	})

	// snapshotDistributionLagSeconds measures the wall-clock to distribute a
	// template snapshot to one deficit node by PULL from a holder (the
	// content-addressed CAS transfer): from the start of the pull to the digest
	// being registered on the destination node. It is the multi-node snapshot
	// distribution lag of issue #164. The metric is ONLY populated on the
	// multi-node distribution path (a peer token configured AND a holder exists);
	// a single-node cluster, an encrypted template (built per node), or the very
	// first build (no holder yet) never observe it, so an empty series correctly
	// means "no pull-based distribution happened", not "lag is zero". Labeled by
	// template (the content-addressed snapshot id) so an operator sees which
	// template's distribution is slow; the template id is derived from the pool's
	// template ref and carries no secret.
	snapshotDistributionLagSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mitos_snapshot_distribution_lag_seconds",
		Help:    "Seconds to distribute a template snapshot to a deficit node by pull from a holder, by template. Populated only on the multi-node distribution path.",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60, 120, 300},
	}, []string{"template"})

	// forkStageDurationSeconds is the per-stage latency breakdown of a hosted
	// co-location fork, keyed by a fixed stage label. It is how the ~728 ms p50
	// hosted fork is attributed to WHAT dominates: the controller observes one
	// series per boundary it crosses (the fork-snapshot RPC round-trip, each
	// spawn-vm RPC round-trip, each activate RPC round-trip), the husk-reported
	// sub-stages inside those RPCs (fc_boot, vmstate_restore, rootfs_clone,
	// guest_ready, handshake, ...), and the end-to-end total attributed across
	// the level-triggered reconcile passes (stage "total"). The stage label set
	// is a small fixed vocabulary (never error text, an id, or a secret), so the
	// series cardinality is bounded. Buckets span the sub-millisecond floor to a
	// few seconds so a stage anywhere from a CoW reflink to a cold FC boot lands
	// in a real bucket. This is the input to targeting the real bottleneck for
	// the ms-class fork; it changes no fork behavior.
	forkStageDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mitos_fork_stage_duration_seconds",
		Help:    "Per-stage duration of a hosted co-location fork, by stage (controller RPC round-trips, husk sub-stages, and the end-to-end total).",
		Buckets: []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5},
	}, []string{"stage"})

	// Kept separate from forkStageDurationSeconds: that histogram documents a fixed
	// fork-stage vocabulary, and mixing claim labels into it would make any
	// aggregation over "all fork stages" quietly include warm-claim work.
	claimStageDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "mitos_claim_stage_duration_seconds",
		Help:    "Per-stage duration of a warm-claim activate, by stage (Kubernetes round-trips around the microVM restore, the mTLS dial, the activate RPC, and the end-to-end total).",
		Buckets: []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5},
	}, []string{"stage"})
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		claimPendingTotal,
		orphanSweepsTotal,
		volumeOrphanSweepsTotal,
		claimErrorsTotal,
		poolReadySnapshots,
		poolWarmDormant,
		poolWarmInUse,
		poolDesiredWarm,
		warmScaleUpTotal,
		warmScaleDownTotal,
		huskPodCreatedTotal,
		huskPodLostTotal,
		nodeLostTotal,
		nodeLostReforkTotal,
		refillLatencySeconds,
		claimWaitForWarmSeconds,
		snapshotDistributionLagSeconds,
		forkStageDurationSeconds,
		claimStageDurationSeconds,
	)
}

// observeSnapshotDistributionLag records the seconds taken to distribute a
// template snapshot to one node by pull from a holder, for templateID. It is
// called ONLY on the multi-node pull path, so the series stays empty until at
// least one pull-based distribution happens (the value is meaningful only on
// multi-node).
func observeSnapshotDistributionLag(templateID string, seconds float64) {
	snapshotDistributionLagSeconds.WithLabelValues(templateID).Observe(seconds)
}

// recordClaimPending bumps the pending-requeue counter.
func recordClaimPending() {
	claimPendingTotal.Inc()
}

// recordOrphanSweep bumps the orphan-sweep counter once per reaped VM.
func recordOrphanSweep() {
	orphanSweepsTotal.Inc()
}

// recordVolumeOrphanSweep bumps the volume-orphan-sweep counter once per
// reclaimed volume backing.
func recordVolumeOrphanSweep() {
	volumeOrphanSweepsTotal.Inc()
}

// recordClaimError bumps the per-pool, per-reason claim-error counter. reason
// must be a fixed label (e.g. "fork", "secret", "volume", "token"), never error
// text.
func recordClaimError(pool, reason string) {
	claimErrorsTotal.WithLabelValues(pool, reason).Inc()
}

// setPoolReadySnapshots records the ready-snapshot count for a pool.
func setPoolReadySnapshots(pool string, ready int32) {
	poolReadySnapshots.WithLabelValues(pool).Set(float64(ready))
}

// setWarmPoolGauges records the warm-pool size, in-use, and desired counts for a
// pool in one call (pool is the namespace/name key, never a secret).
func setWarmPoolGauges(pool string, dormant, inUse, desired int32) {
	poolWarmDormant.WithLabelValues(pool).Set(float64(dormant))
	poolWarmInUse.WithLabelValues(pool).Set(float64(inUse))
	poolDesiredWarm.WithLabelValues(pool).Set(float64(desired))
}

// recordWarmScaleUp / recordWarmScaleDown bump the per-pool scale-event counters.
func recordWarmScaleUp(pool string)   { warmScaleUpTotal.WithLabelValues(pool).Inc() }
func recordWarmScaleDown(pool string) { warmScaleDownTotal.WithLabelValues(pool).Inc() }

// recordHuskPodCreated bumps the per-pool husk-pod-created counter (once per pod
// the controller creates to fill the warm pool).
func recordHuskPodCreated(pool string) { huskPodCreatedTotal.WithLabelValues(pool).Inc() }

// recordHuskPodLost bumps the per-pool counter for a claim re-pended because its
// backing husk pod was lost.
func recordHuskPodLost(pool string) { huskPodLostTotal.WithLabelValues(pool).Inc() }

// recordNodeLost bumps the per-node counter for a Ready claim marked NodeLost.
// node is a hostname (never a secret).
func recordNodeLost(node string) { nodeLostTotal.WithLabelValues(node).Inc() }

// recordNodeLostRefork bumps the per-node counter for a raw-forkd claim re-pended
// for automatic re-fork after node loss (issue #372). node is the LOST hostname
// (never a secret).
func recordNodeLostRefork(node string) { nodeLostReforkTotal.WithLabelValues(node).Inc() }

// forgetPoolMetrics drops every per-pool warm-pool label series for the given
// pool key. It is called only when a SandboxPool is genuinely deleted (the
// reconcile saw NotFound), so the controller does not accumulate one stale
// label series per distinct pool name over its lifetime. The gauges and
// scale-event counters carry a single "pool" label, so DeleteLabelValues clears
// them; the call is a no-op for a pool that was never recorded. Per-pool claim
// error series (pool, reason) are intentionally NOT cleared here: they are a
// terminal failure record an operator may still want to read after the pool is
// gone, and reason is not enumerable from this site.
func forgetPoolMetrics(pool string) {
	poolReadySnapshots.DeleteLabelValues(pool)
	poolWarmDormant.DeleteLabelValues(pool)
	poolWarmInUse.DeleteLabelValues(pool)
	poolDesiredWarm.DeleteLabelValues(pool)
	warmScaleUpTotal.DeleteLabelValues(pool)
	warmScaleDownTotal.DeleteLabelValues(pool)
	huskPodCreatedTotal.DeleteLabelValues(pool)
	huskPodLostTotal.DeleteLabelValues(pool)
}

// observeRefillLatency records seconds from husk pod create to ready dormant.
func observeRefillLatency(seconds float64) { refillLatencySeconds.Observe(seconds) }

// observeClaimWaitForWarm records seconds a claim waited from creation to
// activating a warm husk pod.
func observeClaimWaitForWarm(seconds float64) { claimWaitForWarmSeconds.Observe(seconds) }

// observeForkStage records the duration of one hosted-fork stage. stage must be
// a fixed label from the per-stage fork breakdown vocabulary (e.g.
// "fork_snapshot_rpc", "spawn_vm_rpc", "vmstate_restore", "guest_ready",
// "total"), never error text, an id, or a secret.
func observeForkStage(stage string, seconds float64) {
	forkStageDurationSeconds.WithLabelValues(stage).Observe(seconds)
}

// observeClaimStage records one stage of a warm-claim activate.
func observeClaimStage(stage string, seconds float64) {
	claimStageDurationSeconds.WithLabelValues(stage).Observe(seconds)
}
