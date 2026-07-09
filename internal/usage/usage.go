// Package usage turns per-node CoW-aware operational metering (internal/metering,
// the forkd GET /v1/metering endpoint) into per-organization, time-integrated,
// auditable usage records, and serves an org-scoped public usage API on top of
// them (issue #211). It does NOT re-implement metering: the node metering.Report
// is the source of truth for instantaneous footprint and CoW deduplication. This
// package AGGREGATES those reports across nodes and across time into billable
// usage records, idempotently, so the records survive node loss, controller
// restart, and late or duplicate samples without double-billing.
//
// The full design (billable units, the rate-vs-counter split, the hold-then-gap
// missed-scrape decision, the idempotency property, the CoW reconciliation, and
// the collection/store/org-resolver seams) is documented in
// docs/saas/usage-pipeline.md.
package usage

import (
	"sort"
	"time"

	"mitos.run/mitos/internal/metering"
)

const bytesPerGiB = float64(1 << 30)

// Config tunes the integrator and collector. DefaultConfig is the billing-grade
// default; tests override individual fields.
type Config struct {
	// Window is the fixed, wall-clock-aligned bucket width. Records are keyed on
	// the window start, so a stable window is what makes the (sandbox, window) key
	// reproducible across collectors and restarts.
	Window time.Duration
	// MaxHold bounds how long a sample's rate level is held forward across a gap
	// in scrapes before the integrator records a gap (zero) for the remainder. See
	// the hold-then-gap decision in docs/saas/usage-pipeline.md.
	MaxHold time.Duration
}

// DefaultConfig returns the billing-grade defaults: a 60s window and a 120s
// (2 x window) maximum hold across a missed scrape.
func DefaultConfig() Config {
	return Config{Window: 60 * time.Second, MaxHold: 120 * time.Second}
}

// Sample is one scrape of one sandbox at an instant, tagged with the owning org,
// the sandbox id, the node, and the timestamp. It carries the instantaneous rate
// LEVELS (vCPUs, CoW-aware memory split into unique + amortized shared, disk) and
// the cumulative COUNTERS (egress bytes, GPU-seconds). The integrator integrates
// the levels over time and deltas the counters; see the package doc.
//
// MemUniqueBytes and MemSharedAmortizedBytes are kept separate for audit: the
// billable memory level is their sum, while a naive per-VM biller would have
// charged unique plus the FULL (un-amortized) shared set. Keeping them apart is
// what makes the CoW saving auditable per sandbox.
type Sample struct {
	OrgID     string
	SandboxID string
	// Region is the placement value (issue #712 phase 0) this sandbox's tree
	// root was created in, best-effort: empty for a single-value deployment
	// or a sandbox predating this field. It never gates billability, unlike
	// OrgID: an unresolved region simply carries forward as empty.
	Region    string
	Node      string
	Timestamp time.Time

	// Rate levels (integrated over time).
	VCPUs                   int32
	MemUniqueBytes          int64
	MemSharedAmortizedBytes int64
	DiskBytes               int64

	// Cumulative counters (read as a delta across the window).
	EgressBytes int64
	GPUSeconds  int64
}

// memLevel is the billable instantaneous memory level: unique plus the amortized
// share of the template's shared-once set.
func (s Sample) memLevel() int64 { return s.MemUniqueBytes + s.MemSharedAmortizedBytes }

// UsageRecord is the billable usage of one sandbox in one window, scoped to its
// owning org. It is idempotent on (OrgID, SandboxID, Window): the store upserts
// by that key, and because Integrate is pure over the window's samples, replaying
// overlapping samples recomputes the same record value rather than adding to it.
type UsageRecord struct {
	OrgID     string
	SandboxID string
	// Region is the placement value (issue #712 phase 0) this sandbox's tree
	// root was created in, best-effort: empty for a single-value deployment,
	// a sandbox predating this field, or a record whose samples never carried
	// one. It is NOT part of the idempotency key and carries no billing math
	// change; it is a dimension for reporting/attribution only.
	Region string
	// Window is the window start (wall-clock aligned). It is the time component of
	// the idempotency key.
	Window time.Time

	// Billable units, integrated over the window.
	VCPUSeconds     float64
	MemGiBSeconds   float64
	StorageGiBHours float64
	EgressBytes     int64
	GPUSeconds      int64
}

// Integrate folds an unordered set of samples (possibly spanning many sandboxes,
// nodes, windows, with duplicates and gaps) into per-(sandbox, window) usage
// records. It is PURE and deterministic: the same samples always yield the same
// records, which is the property the idempotent store relies on. Records are
// returned sorted by (SandboxID, Window) for a stable result.
func Integrate(samples []Sample, cfg Config) []UsageRecord {
	if cfg.Window <= 0 {
		cfg.Window = DefaultConfig().Window
	}
	if cfg.MaxHold <= 0 {
		cfg.MaxHold = DefaultConfig().MaxHold
	}

	// Group samples by sandbox. Within each sandbox de-duplicate by timestamp (a
	// duplicate scrape at the same instant must not be integrated twice) and sort
	// by time so the integration walks forward.
	bySandbox := map[string][]Sample{}
	for _, s := range samples {
		bySandbox[s.SandboxID] = append(bySandbox[s.SandboxID], s)
	}

	recs := map[recKey]*UsageRecord{}

	for sandbox, group := range bySandbox {
		group = dedupeByTimestamp(group)
		sort.Slice(group, func(i, j int) bool { return group[i].Timestamp.Before(group[j].Timestamp) })

		// Rate units: integrate level * elapsed between consecutive samples, clipped
		// to window bounds, with the earlier level held up to MaxHold then gapped.
		for i := 0; i+1 < len(group); i++ {
			a := group[i]
			b := group[i+1]
			span := b.Timestamp.Sub(a.Timestamp)
			if span <= 0 {
				continue
			}
			held := span
			if held > cfg.MaxHold {
				held = cfg.MaxHold
			}
			// The interval over which a's level applies is [a.Timestamp, a.Timestamp+held].
			integrateInterval(recs, sandbox, a, a.Timestamp, a.Timestamp.Add(held), cfg.Window)
		}

		// Counter units: the in-window progress of each cumulative counter is the
		// SUM of the reset-aware positive steps between consecutive samples. A
		// non-decreasing step is the plain difference; a DECREASE means the counter
		// reset (a sandbox restart zeroes its egress/GPU counter), so the post-reset
		// value is fresh progress counted from zero, never a negative bill. Each
		// step counterStep(a, b) is attributed to the EARLIER sample's window, so a
		// step whose two samples straddle a window boundary (a the last sample of one
		// window, b the first of the next) is billed to a's window instead of being
		// dropped (issue #755). Attributing to the earlier window is what keeps this
		// stable under the collector's rolling, pruned sample buffer: b is newer than
		// a, so whenever a is still buffered b is too, and a window's counter total
		// recomputes to the same value on every cycle until the window is pruned (the
		// idempotency property). For a counter that never resets the per-window steps
		// telescope to last-minus-first over the whole lifetime; a single sample with
		// no successor contributes nothing, so a lone boundary sample never emits a
		// spurious zero-usage record. group is already sorted by timestamp.
		for i := 0; i+1 < len(group); i++ {
			a := group[i]
			b := group[i+1]
			egress := counterStep(a.EgressBytes, b.EgressBytes)
			gpu := counterStep(a.GPUSeconds, b.GPUSeconds)
			if egress == 0 && gpu == 0 {
				continue
			}
			w := a.Timestamp.Truncate(cfg.Window)
			k := recKey{sandbox: sandbox, window: w}
			r := recs[k]
			if r == nil {
				r = &UsageRecord{OrgID: a.OrgID, Region: a.Region, SandboxID: sandbox, Window: w}
				recs[k] = r
			}
			r.EgressBytes += egress
			r.GPUSeconds += gpu
		}
	}

	out := make([]UsageRecord, 0, len(recs))
	for _, r := range recs {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SandboxID != out[j].SandboxID {
			return out[i].SandboxID < out[j].SandboxID
		}
		return out[i].Window.Before(out[j].Window)
	})
	return out
}

// recKey is the per-(sandbox, window) record key used while integrating.
type recKey struct {
	sandbox string
	window  time.Time
}

// integrateInterval adds the rate-unit integral of sample a's levels over
// [start, end] into the per-window records, splitting at window boundaries so
// each window's record only accrues its own slice of the interval.
func integrateInterval(recs map[recKey]*UsageRecord, sandbox string, a Sample, start, end time.Time, window time.Duration) {
	for start.Before(end) {
		w := start.Truncate(window)
		windowEnd := w.Add(window)
		sliceEnd := end
		if windowEnd.Before(sliceEnd) {
			sliceEnd = windowEnd
		}
		secs := sliceEnd.Sub(start).Seconds()

		k := recKey{sandbox: sandbox, window: w}
		r := recs[k]
		if r == nil {
			r = &UsageRecord{OrgID: a.OrgID, Region: a.Region, SandboxID: sandbox, Window: w}
			recs[k] = r
		}
		r.VCPUSeconds += float64(a.VCPUs) * secs
		r.MemGiBSeconds += float64(a.memLevel()) / bytesPerGiB * secs
		r.StorageGiBHours += float64(a.DiskBytes) / bytesPerGiB * (secs / 3600.0)

		start = sliceEnd
	}
}

// dedupeByTimestamp drops samples with a duplicate timestamp for the same
// sandbox, keeping the first seen. A duplicate scrape at the same instant carries
// no new information and must not be integrated twice; this is the front-line
// guard for the duplicate-sample idempotency property.
func dedupeByTimestamp(group []Sample) []Sample {
	seen := map[time.Time]bool{}
	out := group[:0:0]
	for _, s := range group {
		if seen[s.Timestamp] {
			continue
		}
		seen[s.Timestamp] = true
		out = append(out, s)
	}
	return out
}

// Reconciliation is the audit-visible naive-vs-CoW memory split for a node
// report, carried alongside the per-sandbox samples so an operator can see the
// exact bytes the CoW model removed from the bill. It reconciles to the
// metering.Report source: CoWAware equals UsedCoWAware, Naive equals UsedNaive,
// and CoWSavings is the difference (the docs/metering.md CoWSavings line).
type Reconciliation struct {
	Node       string
	CoWAware   int64
	Naive      int64
	CoWSavings int64
}

// SamplesFromReport converts a node metering.Report into per-sandbox usage
// Samples, tagging each with its owning org (via orgOf) and its allocated vCPU
// count (via vcpusOf, a documented seam: vCPU count is not in the metering
// report, it comes from the sandbox spec). The CoW-aware memory level is
// preserved WITHOUT double-counting: each template's shared-once set is amortized
// evenly across the forks that map it, so summing every returned sample's memory
// level reconstructs exactly report.UsedCoWAware, never report.UsedNaive. The
// returned Reconciliation keeps the naive-vs-CoW split visible for audit.
//
// A sandbox whose org cannot be resolved (orgOf returns false) is dropped from
// the billable samples but still counted in the reconciliation totals so the
// node's physical footprint stays auditable.
//
// regionOf resolves a sandbox id to its best-effort region (issue #712 phase
// 0); an unresolved id or a source that never tracks region returns "". A nil
// regionOf is treated as always returning "".
func SamplesFromReport(
	node string,
	at time.Time,
	report metering.Report,
	orgOf func(sandboxID string) (orgID string, ok bool),
	vcpusOf func(sandboxID string) int32,
	regionOf func(sandboxID string) (region string),
) ([]Sample, Reconciliation) {
	// Each template's shared-once representative is split evenly across the forks
	// in that template group. Build per-template fork counts and the shared-once
	// figure from the report's Templates rows (the authoritative CoW source).
	type tinfo struct {
		forks      int
		sharedOnce int64
	}
	tmpl := map[string]tinfo{}
	for _, t := range report.Templates {
		tmpl[t.Template] = tinfo{forks: t.ForkCount, sharedOnce: t.SharedOnce}
	}

	// Track, per template, how much amortized shared memory has already been
	// assigned and the index of the first fork, so the integer-division remainder
	// can be pushed onto one fork. This keeps the summed amortized shared bytes per
	// template equal to its SharedOnce exactly, so the node total reconstructs
	// UsedCoWAware (the audit invariant the test asserts).
	assigned := map[string]int64{}
	firstIdx := map[string]int{}

	samples := make([]Sample, 0, len(report.Sandboxes))
	for _, sb := range report.Sandboxes {
		org, ok := orgOf(sb.ID)
		if !ok {
			continue
		}
		var region string
		if regionOf != nil {
			region = regionOf(sb.ID)
		}
		var amortized int64
		if info, present := tmpl[sb.Template]; present && info.forks > 0 {
			amortized = info.sharedOnce / int64(info.forks)
		}
		idx := len(samples)
		if sb.Template != "" {
			assigned[sb.Template] += amortized
			if _, seen := firstIdx[sb.Template]; !seen {
				firstIdx[sb.Template] = idx
			}
		}
		samples = append(samples, Sample{
			OrgID:                   org,
			Region:                  region,
			SandboxID:               sb.ID,
			Node:                    node,
			Timestamp:               at,
			VCPUs:                   vcpusOf(sb.ID),
			MemUniqueBytes:          sb.MemoryUnique,
			MemSharedAmortizedBytes: amortized,
			DiskBytes:               sb.DiskUnique + sb.DiskShared,
			EgressBytes:             sb.EgressBytes,
			GPUSeconds:              sb.GPUSeconds,
		})
	}

	// Push each template's integer-division remainder onto its first included fork
	// so the summed amortized shared bytes equal SharedOnce exactly. A template
	// whose forks were all dropped (org unresolved) has no first index and is
	// skipped; its shared bytes simply do not appear in the billable samples.
	for t, info := range tmpl {
		if info.forks == 0 {
			continue
		}
		idx, ok := firstIdx[t]
		if !ok {
			continue
		}
		rem := info.sharedOnce - assigned[t]
		samples[idx].MemSharedAmortizedBytes += rem
	}

	recon := Reconciliation{
		Node:       node,
		CoWAware:   report.UsedCoWAware,
		Naive:      report.UsedNaive,
		CoWSavings: report.UsedNaive - report.UsedCoWAware,
	}
	return samples, recon
}

// counterStep returns the in-window progress of a cumulative counter between two
// consecutive readings. A non-decreasing step is the plain difference; a decrease
// means the counter reset (for example a sandbox restart zeroing it), so the new
// lower value is fresh progress counted from zero rather than a negative bill.
func counterStep(prev, curr int64) int64 {
	if curr >= prev {
		return curr - prev
	}
	return curr
}
