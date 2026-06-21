package metering

import "testing"

const (
	mib = int64(1024 * 1024)
)

// TestAggregateTenForksOneTemplate is the headline CoW case: ten forks of one
// template, each reporting 256 MiB shared and 1 MiB unique, must count the
// shared set ONCE (10 MiB unique + 256 MiB shared), not ten times.
func TestAggregateTenForksOneTemplate(t *testing.T) {
	samples := make([]Sample, 0, 10)
	for i := 0; i < 10; i++ {
		samples = append(samples, Sample{
			ID:           string(rune('a' + i)),
			Template:     "A",
			MemoryShared: 256 * mib,
			MemoryUnique: 1 * mib,
		})
	}

	r := Aggregate(samples)

	if want := 10 * mib; r.TotalUnique != want {
		t.Errorf("TotalUnique = %d, want %d", r.TotalUnique, want)
	}
	if want := 10*mib + 256*mib; r.UsedCoWAware != want {
		t.Errorf("UsedCoWAware = %d, want %d", r.UsedCoWAware, want)
	}
	if want := 10*mib + 2560*mib; r.UsedNaive != want {
		t.Errorf("UsedNaive = %d, want %d", r.UsedNaive, want)
	}
	if want := 2304 * mib; r.CoWSavings != want {
		t.Errorf("CoWSavings = %d, want %d", r.CoWSavings, want)
	}
	if want := 256 * mib; r.SharedOnceTotal() != want {
		t.Errorf("SharedOnceTotal = %d, want %d", r.SharedOnceTotal(), want)
	}
	if len(r.Templates) != 1 {
		t.Fatalf("Templates len = %d, want 1", len(r.Templates))
	}
	if r.Templates[0].ForkCount != 10 {
		t.Errorf("Templates[A].ForkCount = %d, want 10", r.Templates[0].ForkCount)
	}
	if want := 256 * mib; r.Templates[0].SharedOnce != want {
		t.Errorf("Templates[A].SharedOnce = %d, want %d", r.Templates[0].SharedOnce, want)
	}
}

// TestAggregateTwoTemplates verifies two distinct templates each contribute one
// shared region (no cross-template dedup).
func TestAggregateTwoTemplates(t *testing.T) {
	samples := []Sample{
		{ID: "a1", Template: "A", MemoryShared: 256 * mib, MemoryUnique: 1 * mib},
		{ID: "a2", Template: "A", MemoryShared: 256 * mib, MemoryUnique: 1 * mib},
		{ID: "b1", Template: "B", MemoryShared: 128 * mib, MemoryUnique: 2 * mib},
	}

	r := Aggregate(samples)

	if want := 4 * mib; r.TotalUnique != want {
		t.Errorf("TotalUnique = %d, want %d", r.TotalUnique, want)
	}
	// Two shared regions counted once each: 256 + 128.
	if want := 4*mib + 256*mib + 128*mib; r.UsedCoWAware != want {
		t.Errorf("UsedCoWAware = %d, want %d", r.UsedCoWAware, want)
	}
	if want := (256 + 128) * mib; r.SharedOnceTotal() != want {
		t.Errorf("SharedOnceTotal = %d, want %d", r.SharedOnceTotal(), want)
	}
	if len(r.Templates) != 2 {
		t.Fatalf("Templates len = %d, want 2", len(r.Templates))
	}
	// Deterministic ordering: A before B.
	if r.Templates[0].Template != "A" || r.Templates[1].Template != "B" {
		t.Errorf("template order = %q,%q, want A,B", r.Templates[0].Template, r.Templates[1].Template)
	}
}

// TestAggregateEmpty: no samples yields the zero report.
func TestAggregateEmpty(t *testing.T) {
	r := Aggregate(nil)
	if r.UsedCoWAware != 0 || r.UsedNaive != 0 || r.CoWSavings != 0 || r.TotalUnique != 0 {
		t.Errorf("empty report not all zero: %+v", r)
	}
	if len(r.Templates) != 0 || len(r.Sandboxes) != 0 {
		t.Errorf("empty report has rows: %+v", r)
	}
}

// TestAggregateSingleFork: one fork means CoW-aware equals naive (no sharing to
// deduplicate).
func TestAggregateSingleFork(t *testing.T) {
	r := Aggregate([]Sample{{ID: "x", Template: "A", MemoryShared: 256 * mib, MemoryUnique: 1 * mib}})
	if r.UsedCoWAware != r.UsedNaive {
		t.Errorf("single fork: UsedCoWAware %d != UsedNaive %d", r.UsedCoWAware, r.UsedNaive)
	}
	if r.CoWSavings != 0 {
		t.Errorf("single fork CoWSavings = %d, want 0", r.CoWSavings)
	}
}

// TestAggregateEmptyTemplateOwnGroup: sandboxes with no Template never share
// with each other, even if their shared bytes are identical.
func TestAggregateEmptyTemplateOwnGroup(t *testing.T) {
	samples := []Sample{
		{ID: "x", Template: "", MemoryShared: 100 * mib, MemoryUnique: 1 * mib},
		{ID: "y", Template: "", MemoryShared: 100 * mib, MemoryUnique: 1 * mib},
	}
	r := Aggregate(samples)
	// No dedup: both shared regions counted.
	if want := 2*mib + 200*mib; r.UsedCoWAware != want {
		t.Errorf("UsedCoWAware = %d, want %d (no sharing for empty template)", r.UsedCoWAware, want)
	}
	if len(r.Templates) != 2 {
		t.Errorf("Templates len = %d, want 2 (each its own group)", len(r.Templates))
	}
}

// TestAggregateDiskSharedOnce: two Snapshot-volume forks of one template share
// the seed; the seed disk is counted once, divergence stays unique.
func TestAggregateDiskSharedOnce(t *testing.T) {
	samples := []Sample{
		{ID: "f1", Template: "A", DiskShared: 1024 * mib, DiskUnique: 10 * mib},
		{ID: "f2", Template: "A", DiskShared: 1024 * mib, DiskUnique: 20 * mib},
	}
	r := Aggregate(samples)

	if want := 30 * mib; r.DiskTotalUnique != want {
		t.Errorf("DiskTotalUnique = %d, want %d", r.DiskTotalUnique, want)
	}
	// Seed counted once: 1024 + (10 + 20).
	if want := 30*mib + 1024*mib; r.DiskUsedCoWAware != want {
		t.Errorf("DiskUsedCoWAware = %d, want %d", r.DiskUsedCoWAware, want)
	}
	if want := 30*mib + 2048*mib; r.DiskUsedNaive != want {
		t.Errorf("DiskUsedNaive = %d, want %d", r.DiskUsedNaive, want)
	}
	if want := 1024 * mib; r.DiskCoWSavings != want {
		t.Errorf("DiskCoWSavings = %d, want %d", r.DiskCoWSavings, want)
	}
	if len(r.Templates) != 1 || r.Templates[0].DiskSharedOnce != 1024*mib {
		t.Errorf("template disk shared-once wrong: %+v", r.Templates)
	}
}

// TestAggregateSandboxOrdering: Sandboxes are sorted by ID deterministically.
func TestAggregateSandboxOrdering(t *testing.T) {
	r := Aggregate([]Sample{
		{ID: "c", Template: "A"},
		{ID: "a", Template: "A"},
		{ID: "b", Template: "A"},
	})
	got := []string{r.Sandboxes[0].ID, r.Sandboxes[1].ID, r.Sandboxes[2].ID}
	want := []string{"a", "b", "c"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sandbox order = %v, want %v", got, want)
		}
	}
}

// TestAggregatePassesEgressBytes asserts each sample's per-sandbox egress byte
// count (the #211 metering seam fed from the nftables egress counter) is carried
// through into its SandboxMetering row unchanged. Egress bytes are per-sandbox
// and never deduplicated across forks (unlike CoW memory), so the row simply
// echoes the sample.
func TestAggregatePassesEgressBytes(t *testing.T) {
	samples := []Sample{
		{ID: "sb-a", Template: "tmpl", MemoryUnique: 10, EgressBytes: 4096},
		{ID: "sb-b", Template: "tmpl", MemoryUnique: 10, EgressBytes: 0},
	}
	report := Aggregate(samples)
	byID := map[string]int64{}
	for _, s := range report.Sandboxes {
		byID[s.ID] = s.EgressBytes
	}
	if byID["sb-a"] != 4096 {
		t.Errorf("sb-a egress bytes = %d, want 4096", byID["sb-a"])
	}
	if byID["sb-b"] != 0 {
		t.Errorf("sb-b egress bytes = %d, want 0", byID["sb-b"])
	}
}

// TestAggregatePassesGPUSeconds asserts each sample's per-sandbox GPU-seconds
// (issue #221, the billable GPU unit feeding the usage pipeline #211 and Stripe
// #212) is echoed into its row unchanged AND summed into the report total. A GPU
// is assigned EXCLUSIVELY to one sandbox and is never CoW-shared across forks
// (unlike template memory), so GPU-seconds is summed straight, like EgressBytes,
// never deduplicated.
func TestAggregatePassesGPUSeconds(t *testing.T) {
	samples := []Sample{
		{ID: "sb-a", Template: "tmpl", MemoryUnique: 10, GPUCount: 1, GPUSeconds: 120},
		{ID: "sb-b", Template: "tmpl", MemoryUnique: 10, GPUCount: 2, GPUSeconds: 60},
		{ID: "sb-c", Template: "tmpl", MemoryUnique: 10}, // no GPU
	}
	report := Aggregate(samples)

	byID := map[string]int64{}
	gpuByID := map[string]int32{}
	for _, s := range report.Sandboxes {
		byID[s.ID] = s.GPUSeconds
		gpuByID[s.ID] = s.GPUCount
	}
	if byID["sb-a"] != 120 {
		t.Errorf("sb-a GPU-seconds = %d, want 120", byID["sb-a"])
	}
	if byID["sb-b"] != 60 {
		t.Errorf("sb-b GPU-seconds = %d, want 60", byID["sb-b"])
	}
	if byID["sb-c"] != 0 {
		t.Errorf("sb-c GPU-seconds = %d, want 0", byID["sb-c"])
	}
	if gpuByID["sb-b"] != 2 {
		t.Errorf("sb-b GPU count = %d, want 2", gpuByID["sb-b"])
	}
	// Summed straight across forks: 120 + 60 + 0 = 180. Two forks of the same
	// template do NOT deduplicate a GPU (each holds its own device).
	if want := int64(180); report.TotalGPUSeconds != want {
		t.Errorf("TotalGPUSeconds = %d, want %d", report.TotalGPUSeconds, want)
	}
}
