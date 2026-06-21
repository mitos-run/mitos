// Package metering aggregates per-sandbox resource samples into a node report
// that accounts for copy-on-write (CoW) sharing across forks of the same
// template. Firecracker forks restore the same template snapshot with
// MAP_PRIVATE, so every fork of a template maps the SAME shared page set; the
// naive "sum every fork's shared bytes" double-counts that set once per fork.
// This package counts each template's shared footprint ONCE.
//
// metering must not import internal/fork: fork imports metering. The Sample
// struct is the narrow input the engine fills from its sandboxes so there is no
// import cycle.
package metering

import "sort"

// Sample is one sandbox's metered footprint. Template is the grouping key (the
// template/snapshot the fork was restored from); all forks of the same Template
// map ~the same shared page set, so their MemoryShared is counted once. An
// empty Template means the sandbox is its own group (no sharing assumed).
//
// DiskUnique is backing storage this sandbox alone owns (a Fresh volume, or a
// fork's divergence from a reflinked seed). DiskShared is the template seed
// storage a Snapshot/reflink volume shares with its template siblings; like
// MemoryShared it is counted once per (template, volume) by Aggregate.
type Sample struct {
	ID           string
	Template     string
	MemoryUnique int64
	MemoryShared int64
	DiskUnique   int64
	DiskShared   int64
	// EgressBytes is this sandbox's total egress bytes read from its per-sandbox
	// nftables egress counter (issue #219), the metering seam #211 attributes
	// network usage from. It is per-sandbox and never deduplicated across forks
	// (unlike the CoW memory totals), so Aggregate echoes it into the row as-is.
	// Zero when networking is disabled or the counter is unreadable.
	EgressBytes int64
}

// SandboxMetering is the per-sandbox row in a Report. It echoes the sample so
// operators/billing see each sandbox's contribution. Its field layout MUST stay
// identical to Sample: Aggregate converts Sample to SandboxMetering directly.
type SandboxMetering struct {
	ID           string
	Template     string
	MemoryUnique int64
	MemoryShared int64
	DiskUnique   int64
	DiskShared   int64
	// EgressBytes echoes the sample's per-sandbox egress byte total (issue #219).
	EgressBytes int64
}

// TemplateMetering is the per-template row in a Report. SharedOnce is the
// representative shared memory counted a single time for the whole template
// group (the MAX of the group's forks' MemoryShared, the conservative
// representative since all forks of a template should map ~the same shared
// set). DiskSharedOnce is the same idea for the template's reflinked volume
// seeds.
type TemplateMetering struct {
	Template       string
	ForkCount      int
	SharedOnce     int64
	DiskSharedOnce int64
}

// Report is the CoW-aware node metering rollup.
//
// TotalUnique is the sum of every sample's MemoryUnique (never shared, so never
// deduplicated). UsedCoWAware is TotalUnique plus each template's SharedOnce
// counted a single time: the honest resident footprint. UsedNaive is
// TotalUnique plus every sample's MemoryShared (the double-counted total) for
// comparison; both totals share the same unique base so CoWSavings (UsedNaive
// minus UsedCoWAware) isolates exactly the shared memory the CoW model reveals
// is NOT actually consumed per-fork.
//
// The Disk* totals mirror the memory totals for backing storage.
type Report struct {
	Sandboxes []SandboxMetering
	Templates []TemplateMetering

	UsedCoWAware int64
	UsedNaive    int64
	CoWSavings   int64
	TotalUnique  int64

	DiskUsedCoWAware int64
	DiskUsedNaive    int64
	DiskCoWSavings   int64
	DiskTotalUnique  int64
}

// Aggregate folds per-sandbox samples into a CoW-aware Report. Forks are
// grouped by Template; each group's shared footprint (memory and disk) is
// counted once using the MAX of the group's members as the representative.
// Sandboxes with an empty Template are each their own single-member group, so
// nothing is shared across them. Output ordering is deterministic: Templates by
// name, Sandboxes by ID. Empty input yields the zero Report.
func Aggregate(samples []Sample) Report {
	var report Report
	if len(samples) == 0 {
		return report
	}

	// Per-template accumulation. A distinct group key per empty-Template
	// sandbox keeps those sandboxes from ever sharing with each other.
	type group struct {
		template  string
		forkCount int
		sharedMax int64
		diskMax   int64
	}
	groups := make(map[string]*group)
	order := make([]string, 0)

	sandboxes := make([]SandboxMetering, 0, len(samples))
	for _, s := range samples {
		report.TotalUnique += s.MemoryUnique
		report.UsedNaive += s.MemoryShared
		report.DiskTotalUnique += s.DiskUnique
		report.DiskUsedNaive += s.DiskShared

		key := s.Template
		if key == "" {
			// Own group: key on the sandbox ID so it never coalesces with
			// another empty-Template sandbox.
			key = "\x00sandbox\x00" + s.ID
		}
		g, ok := groups[key]
		if !ok {
			g = &group{template: s.Template}
			groups[key] = g
			order = append(order, key)
		}
		g.forkCount++
		if s.MemoryShared > g.sharedMax {
			g.sharedMax = s.MemoryShared
		}
		if s.DiskShared > g.diskMax {
			g.diskMax = s.DiskShared
		}

		sandboxes = append(sandboxes, SandboxMetering(s))
	}

	templates := make([]TemplateMetering, 0, len(order))
	var sharedOnceTotal, diskSharedOnceTotal int64
	for _, key := range order {
		g := groups[key]
		sharedOnceTotal += g.sharedMax
		diskSharedOnceTotal += g.diskMax
		templates = append(templates, TemplateMetering{
			Template:       g.template,
			ForkCount:      g.forkCount,
			SharedOnce:     g.sharedMax,
			DiskSharedOnce: g.diskMax,
		})
	}

	// UsedNaive/DiskUsedNaive accumulated only the per-sample shared bytes
	// above; add the unique total so both totals share the same unique base
	// and CoWSavings isolates exactly the deduplicated shared bytes.
	report.UsedNaive += report.TotalUnique
	report.DiskUsedNaive += report.DiskTotalUnique

	report.UsedCoWAware = report.TotalUnique + sharedOnceTotal
	report.CoWSavings = report.UsedNaive - report.UsedCoWAware
	report.DiskUsedCoWAware = report.DiskTotalUnique + diskSharedOnceTotal
	report.DiskCoWSavings = report.DiskUsedNaive - report.DiskUsedCoWAware

	sort.Slice(templates, func(i, j int) bool { return templates[i].Template < templates[j].Template })
	sort.Slice(sandboxes, func(i, j int) bool { return sandboxes[i].ID < sandboxes[j].ID })
	report.Templates = templates
	report.Sandboxes = sandboxes

	return report
}

// SharedOnceTotal is the CoW-aware shared memory footprint of the node: the sum
// over templates of each template's SharedOnce (each template's shared set
// counted a single time). It equals UsedCoWAware minus TotalUnique. The engine
// reports this as Capacity.MemoryShared.
func (r Report) SharedOnceTotal() int64 {
	return r.UsedCoWAware - r.TotalUnique
}
