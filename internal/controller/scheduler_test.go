package controller

import (
	"errors"
	"testing"
	"time"
)

const gib = int64(1024 * 1024 * 1024)

// warmNode builds a healthy node holding templateID with the given memory
// budget/usage and a per-template estimate (shared-once, avg unique, forks).
func warmNode(name string, total, used int64, templateID string, sharedOnce, avgUnique int64, forks int32) *NodeInfo {
	return &NodeInfo{
		Name:          name,
		Endpoint:      name + ":9090",
		MemoryTotal:   total,
		MemoryUsed:    used,
		TemplateIDs:   []string{templateID},
		LastHeartbeat: time.Now(),
		TemplateEstimates: map[string]TemplateCapacity{
			templateID: {
				TemplateID:         templateID,
				SharedOnceBytes:    sharedOnce,
				AvgForkUniqueBytes: avgUnique,
				ForkCount:          forks,
			},
		},
	}
}

// coldNode builds a healthy node with a budget but no knowledge of templateID.
func coldNode(name string, total, used int64) *NodeInfo {
	return &NodeInfo{
		Name:          name,
		Endpoint:      name + ":9090",
		MemoryTotal:   total,
		MemoryUsed:    used,
		LastHeartbeat: time.Now(),
	}
}

func TestSelectNodeAdmitsWhenForkFits(t *testing.T) {
	r := NewNodeRegistry()
	r.Register(warmNode("n1", 4*gib, 1*gib, "py", 256*1024*1024, 8*1024*1024, 3))

	node, err := r.SelectNode("py", "")
	if err != nil {
		t.Fatalf("SelectNode: %v", err)
	}
	if node.Name != "n1" {
		t.Fatalf("got %q want n1", node.Name)
	}
}

func TestSelectNodeNoCapacityWhenAllFull(t *testing.T) {
	r := NewNodeRegistry()
	// Both nodes are full: used == total, a cold or warm placement cannot fit.
	r.Register(warmNode("n1", 2*gib, 2*gib, "py", 256*1024*1024, 8*1024*1024, 1))
	r.Register(coldNode("n2", 2*gib, 2*gib))

	_, err := r.SelectNode("py", "")
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("expected ErrNoCapacity, got %v", err)
	}
}

func TestSelectNodePacksWarmHolderOverColdEmpty(t *testing.T) {
	r := NewNodeRegistry()
	// Cold node has the MOST free memory but does not hold the template. The
	// warm holder still fits, so packing prefers it (maximize CoW sharing).
	r.Register(coldNode("cold", 64*gib, 0))
	r.Register(warmNode("warm", 8*gib, 2*gib, "py", 256*1024*1024, 8*1024*1024, 5))

	node, err := r.SelectNode("py", "")
	if err != nil {
		t.Fatalf("SelectNode: %v", err)
	}
	if node.Name != "warm" {
		t.Fatalf("got %q want warm (packing prefers warm holder)", node.Name)
	}
}

func TestSelectNodePacksDenserWarmThenSpills(t *testing.T) {
	r := NewNodeRegistry()
	// Two warm holders. dense runs more forks and still fits -> pack it. Then
	// fill dense so it no longer admits and confirm we spill to sparse.
	dense := warmNode("dense", 8*gib, 4*gib, "py", 256*1024*1024, 8*1024*1024, 20)
	sparse := warmNode("sparse", 8*gib, 1*gib, "py", 256*1024*1024, 8*1024*1024, 2)
	r.Register(dense)
	r.Register(sparse)

	node, err := r.SelectNode("py", "")
	if err != nil {
		t.Fatalf("SelectNode: %v", err)
	}
	if node.Name != "dense" {
		t.Fatalf("got %q want dense (pack the denser holder)", node.Name)
	}

	// Now fill dense to capacity: it no longer admits, spill to sparse.
	full := warmNode("dense", 8*gib, 8*gib, "py", 256*1024*1024, 8*1024*1024, 20)
	r.Register(full)
	node, err = r.SelectNode("py", "")
	if err != nil {
		t.Fatalf("SelectNode after fill: %v", err)
	}
	if node.Name != "sparse" {
		t.Fatalf("got %q want sparse (spill when dense is full)", node.Name)
	}
}

func TestSelectNodeOvercommitAdmitsMore(t *testing.T) {
	r := NewNodeRegistry()
	// Cold placement cost = 256 MiB shared + 8 MiB unique = 264 MiB. Node has
	// 200 MiB free at factor 1.0 (does not admit) but 200+1024 MiB at 2.0.
	const free = 200 * 1024 * 1024
	total := int64(2 * gib)
	used := total - free
	r.Register(coldNode("n1", total, used))
	// Seed a cross-node estimate for "py" via a separate warm node that is
	// permanently full (used far exceeds even 2x its budget) so it can never be
	// chosen, letting n1 compute a cold cost for py from the shared estimate.
	full := warmNode("full", gib, 4*gib, "py", 256*1024*1024, 8*1024*1024, 1)
	r.Register(full)

	if _, err := r.SelectNode("py", ""); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("at factor 1.0 expected ErrNoCapacity, got %v", err)
	}

	r.SetOvercommitFactor(2.0)
	node, err := r.SelectNode("py", "")
	if err != nil {
		t.Fatalf("at factor 2.0 SelectNode: %v", err)
	}
	if node.Name != "n1" {
		t.Fatalf("got %q want n1 (overcommit admits)", node.Name)
	}
}

func TestSelectNodeBypassesPreferredThatDoesNotFit(t *testing.T) {
	r := NewNodeRegistry()
	// Preferred node is full; another node fits. Preference is bypassed.
	r.Register(warmNode("pref", 2*gib, 2*gib, "py", 256*1024*1024, 8*1024*1024, 1))
	r.Register(warmNode("other", 8*gib, 1*gib, "py", 256*1024*1024, 8*1024*1024, 1))

	node, err := r.SelectNode("py", "pref")
	if err != nil {
		t.Fatalf("SelectNode: %v", err)
	}
	if node.Name != "other" {
		t.Fatalf("got %q want other (preferred full, bypassed)", node.Name)
	}
}

func TestSelectNodeHonorsPreferredThatFits(t *testing.T) {
	r := NewNodeRegistry()
	r.Register(warmNode("pref", 8*gib, 1*gib, "py", 256*1024*1024, 8*1024*1024, 1))
	r.Register(warmNode("denser", 8*gib, 1*gib, "py", 256*1024*1024, 8*1024*1024, 50))

	node, err := r.SelectNode("py", "pref")
	if err != nil {
		t.Fatalf("SelectNode: %v", err)
	}
	if node.Name != "pref" {
		t.Fatalf("got %q want pref (preferred fits, honored)", node.Name)
	}
}

func TestSelectNodeDeterministicTieBreak(t *testing.T) {
	r := NewNodeRegistry()
	// Two cold nodes with identical free memory: pick by name (alpha < beta).
	r.Register(coldNode("beta", 8*gib, 1*gib))
	r.Register(coldNode("alpha", 8*gib, 1*gib))

	for i := 0; i < 5; i++ {
		node, err := r.SelectNode("py", "")
		if err != nil {
			t.Fatalf("SelectNode: %v", err)
		}
		if node.Name != "alpha" {
			t.Fatalf("iteration %d: got %q want alpha (deterministic tie-break)", i, node.Name)
		}
	}
}

func TestSelectNodeNoNodesDistinctFromNoCapacity(t *testing.T) {
	r := NewNodeRegistry()
	_, err := r.SelectNode("py", "")
	if err == nil {
		t.Fatal("expected an error for empty registry")
	}
	if errors.Is(err, ErrNoCapacity) {
		t.Fatal("empty registry must NOT be ErrNoCapacity")
	}
}

// TestSelectNodeRejectsNodeAtSandboxCeiling asserts a node that has reached its
// MaxSandboxes count ceiling is NOT selected, even when it has ample memory.
// The count ceiling is the forkd-side cap (PR #110): enforcing it at schedule
// time makes the node-side ResourceExhausted a rare race, not the primary path.
func TestSelectNodeRejectsNodeAtSandboxCeiling(t *testing.T) {
	r := NewNodeRegistry()
	// Plenty of memory, but already at the sandbox-count ceiling.
	full := warmNode("full", 64*gib, 1*gib, "py", 256*1024*1024, 8*1024*1024, 5)
	full.MaxSandboxes = 5
	full.ActiveSandboxes = 5
	r.Register(full)

	if _, err := r.SelectNode("py", ""); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("expected ErrNoCapacity (node at sandbox ceiling), got %v", err)
	}
}

// TestSelectNodeSpillsOffSandboxCeilingNode asserts the scheduler spills to a
// node with count headroom when the densest warm holder is at its ceiling.
func TestSelectNodeSpillsOffSandboxCeilingNode(t *testing.T) {
	r := NewNodeRegistry()
	dense := warmNode("dense", 64*gib, 1*gib, "py", 256*1024*1024, 8*1024*1024, 10)
	dense.MaxSandboxes = 10
	dense.ActiveSandboxes = 10 // at ceiling
	sparse := warmNode("sparse", 64*gib, 1*gib, "py", 256*1024*1024, 8*1024*1024, 2)
	sparse.MaxSandboxes = 10
	sparse.ActiveSandboxes = 2 // has headroom
	r.Register(dense)
	r.Register(sparse)

	node, err := r.SelectNode("py", "")
	if err != nil {
		t.Fatalf("SelectNode: %v", err)
	}
	if node.Name != "sparse" {
		t.Fatalf("got %q want sparse (dense is at the sandbox ceiling)", node.Name)
	}
}

// TestSelectNodeZeroMaxSandboxesMeansNoCountCeiling asserts MaxSandboxes 0 (the
// cap is unset/unlimited, e.g. mock engine) never blocks scheduling on count.
func TestSelectNodeZeroMaxSandboxesMeansNoCountCeiling(t *testing.T) {
	r := NewNodeRegistry()
	n := warmNode("n1", 64*gib, 1*gib, "py", 256*1024*1024, 8*1024*1024, 100)
	n.MaxSandboxes = 0
	n.ActiveSandboxes = 100
	r.Register(n)

	node, err := r.SelectNode("py", "")
	if err != nil {
		t.Fatalf("SelectNode: %v", err)
	}
	if node.Name != "n1" {
		t.Fatalf("got %q want n1 (MaxSandboxes 0 means no count ceiling)", node.Name)
	}
}

// --- Larger-size admission (issue #221) ---

// TestSelectNodeForForkAdmitsLargeSizeThatFits asserts a large explicit memory
// request (above the 8 GiB E2B class) is admitted against a node with the
// capacity, even though the per-template CoW estimate alone is small. The
// requested size, not just the projected fork cost, gates admission.
func TestSelectNodeForForkAdmitsLargeSizeThatFits(t *testing.T) {
	r := NewNodeRegistry()
	// 64 GiB node, mostly free; ask for a 32 GiB sandbox (well above 8 GiB).
	r.Register(warmNode("big", 64*gib, 2*gib, "py", 256*1024*1024, 8*1024*1024, 1))

	node, err := r.SelectNodeForFork(ForkRequest{TemplateID: "py", MemoryBytes: 32 * gib})
	if err != nil {
		t.Fatalf("SelectNodeForFork: %v", err)
	}
	if node.Name != "big" {
		t.Fatalf("got %q want big", node.Name)
	}
}

// TestSelectNodeForForkRejectsLargeSizeOverCapacity asserts a large request that
// exceeds every node's headroom is rejected with ErrNoCapacity, even though the
// small CoW estimate would otherwise admit.
func TestSelectNodeForForkRejectsLargeSizeOverCapacity(t *testing.T) {
	r := NewNodeRegistry()
	// 8 GiB node with 6 GiB free; a 32 GiB request cannot fit.
	r.Register(warmNode("small", 8*gib, 2*gib, "py", 256*1024*1024, 8*1024*1024, 1))

	_, err := r.SelectNodeForFork(ForkRequest{TemplateID: "py", MemoryBytes: 32 * gib})
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("expected ErrNoCapacity for oversize request, got %v", err)
	}
}

// TestSelectNodeForForkLargeSizeSpillsToBigNode asserts a large request spills
// off a node that cannot fit it onto one that can.
func TestSelectNodeForForkLargeSizeSpillsToBigNode(t *testing.T) {
	r := NewNodeRegistry()
	small := warmNode("small", 16*gib, 1*gib, "py", 256*1024*1024, 8*1024*1024, 5)
	big := coldNode("big", 128*gib, 1*gib)
	r.Register(small)
	r.Register(big)

	// 32 GiB request does not fit the warm small node (15 GiB free); spill to big.
	node, err := r.SelectNodeForFork(ForkRequest{TemplateID: "py", MemoryBytes: 32 * gib})
	if err != nil {
		t.Fatalf("SelectNodeForFork: %v", err)
	}
	if node.Name != "big" {
		t.Fatalf("got %q want big (large size spills off small warm node)", node.Name)
	}
}

// --- GPU-aware node selection (issue #221) ---

// gpuNode builds a healthy GPU-capable node holding templateID.
func gpuNode(name string, total, used int64, templateID, gpuType string, gpuTotal, gpuUsed int32) *NodeInfo {
	n := warmNode(name, total, used, templateID, 256*1024*1024, 8*1024*1024, 1)
	n.GPUType = gpuType
	n.GPUTotal = gpuTotal
	n.GPUUsed = gpuUsed
	return n
}

// TestSelectNodeForForkGPUFiltersToGPUNodes asserts a GPU request is scheduled
// ONLY onto a GPU-capable node, never a CPU-only node, mirroring how KVM node
// selection pins to KVM nodes. No hardware is involved: GPU capability is a node
// label/resource the registry mirrors.
func TestSelectNodeForForkGPUFiltersToGPUNodes(t *testing.T) {
	r := NewNodeRegistry()
	r.Register(coldNode("cpu-only", 128*gib, 1*gib)) // most free memory, but no GPU
	r.Register(gpuNode("gpu", 64*gib, 2*gib, "py", "nvidia-a100", 4, 1))

	node, err := r.SelectNodeForFork(ForkRequest{TemplateID: "py", GPUCount: 1})
	if err != nil {
		t.Fatalf("SelectNodeForFork: %v", err)
	}
	if node.Name != "gpu" {
		t.Fatalf("got %q want gpu (GPU request must pin to a GPU node)", node.Name)
	}
}

// TestSelectNodeForForkGPUNoGPUNodesIsNoCapacity asserts a GPU request when no
// GPU node exists is ErrNoCapacity, not a silent CPU-node placement.
func TestSelectNodeForForkGPUNoGPUNodesIsNoCapacity(t *testing.T) {
	r := NewNodeRegistry()
	r.Register(coldNode("cpu1", 128*gib, 1*gib))
	r.Register(coldNode("cpu2", 128*gib, 1*gib))

	_, err := r.SelectNodeForFork(ForkRequest{TemplateID: "py", GPUCount: 1})
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("expected ErrNoCapacity (no GPU node), got %v", err)
	}
}

// TestSelectNodeForForkGPUTypeMatch asserts a typed GPU request matches only a
// node advertising that GPU type.
func TestSelectNodeForForkGPUTypeMatch(t *testing.T) {
	r := NewNodeRegistry()
	r.Register(gpuNode("a100", 64*gib, 1*gib, "py", "nvidia-a100", 4, 0))
	r.Register(gpuNode("h100", 64*gib, 1*gib, "py", "nvidia-h100", 4, 0))

	node, err := r.SelectNodeForFork(ForkRequest{TemplateID: "py", GPUCount: 1, GPUType: "nvidia-h100"})
	if err != nil {
		t.Fatalf("SelectNodeForFork: %v", err)
	}
	if node.Name != "h100" {
		t.Fatalf("got %q want h100 (typed GPU request)", node.Name)
	}

	if _, err := r.SelectNodeForFork(ForkRequest{TemplateID: "py", GPUCount: 1, GPUType: "nvidia-v100"}); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("expected ErrNoCapacity for unavailable GPU type, got %v", err)
	}
}

// TestSelectNodeForForkGPUExhaustedDevices asserts a GPU node with no free
// devices (GPUUsed at GPUTotal) does not admit a GPU request.
func TestSelectNodeForForkGPUExhaustedDevices(t *testing.T) {
	r := NewNodeRegistry()
	// 4 GPUs, all in use: cannot take another GPU sandbox.
	r.Register(gpuNode("gpu", 64*gib, 1*gib, "py", "nvidia-a100", 4, 4))

	_, err := r.SelectNodeForFork(ForkRequest{TemplateID: "py", GPUCount: 1})
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("expected ErrNoCapacity (GPU devices exhausted), got %v", err)
	}
}

// TestSelectNodeForForkGPUMultiDeviceCount asserts a request for more GPUs than a
// node has free is rejected, but one that fits is admitted.
func TestSelectNodeForForkGPUMultiDeviceCount(t *testing.T) {
	r := NewNodeRegistry()
	r.Register(gpuNode("gpu", 64*gib, 1*gib, "py", "nvidia-a100", 4, 2)) // 2 free

	if _, err := r.SelectNodeForFork(ForkRequest{TemplateID: "py", GPUCount: 4}); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("expected ErrNoCapacity (asked 4, 2 free), got %v", err)
	}
	node, err := r.SelectNodeForFork(ForkRequest{TemplateID: "py", GPUCount: 2})
	if err != nil {
		t.Fatalf("SelectNodeForFork for 2 GPUs: %v", err)
	}
	if node.Name != "gpu" {
		t.Fatalf("got %q want gpu", node.Name)
	}
}

// TestSelectNodeForForkNonGPURequestPrefersCPUNode asserts a CPU-only request is
// NOT pinned to GPU nodes; scarce GPU nodes are preserved for GPU work. A
// non-GPU request schedules normally and is not forced off a GPU node, but the
// packing must not waste GPU capacity unnecessarily; here we only assert a
// CPU-only request still schedules when only a GPU node holds the warm template.
func TestSelectNodeForForkNonGPURequestSchedulesAnywhere(t *testing.T) {
	r := NewNodeRegistry()
	r.Register(gpuNode("gpu", 64*gib, 1*gib, "py", "nvidia-a100", 4, 0))

	node, err := r.SelectNodeForFork(ForkRequest{TemplateID: "py"})
	if err != nil {
		t.Fatalf("SelectNodeForFork: %v", err)
	}
	if node.Name != "gpu" {
		t.Fatalf("got %q want gpu (CPU-only request may use a warm GPU node)", node.Name)
	}
}

// TestGPUFromNodeLabels asserts the node-label-to-scheduler mapping (issue #221)
// reads GPU capability without any hardware: a "true" label means one device, an
// integer label means that many, an absent or falsey label means CPU-only, and
// the type label is carried through.
func TestGPUFromNodeLabels(t *testing.T) {
	cases := []struct {
		name      string
		labels    map[string]string
		wantTotal int32
		wantType  string
	}{
		{"absent", map[string]string{}, 0, ""},
		{"false", map[string]string{"mitos.run/gpu": "false"}, 0, ""},
		{"zero", map[string]string{"mitos.run/gpu": "0"}, 0, ""},
		{"true single", map[string]string{"mitos.run/gpu": "true"}, 1, ""},
		{"count 4 with type", map[string]string{"mitos.run/gpu": "4", "mitos.run/gpu-type": "nvidia-a100"}, 4, "nvidia-a100"},
		{"garbage falls back to one", map[string]string{"mitos.run/gpu": "yes"}, 1, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			total, gpuType := GPUFromNodeLabels(tc.labels)
			if total != tc.wantTotal {
				t.Errorf("total = %d, want %d", total, tc.wantTotal)
			}
			if gpuType != tc.wantType {
				t.Errorf("type = %q, want %q", gpuType, tc.wantType)
			}
		})
	}
}

func TestSelectNodeUnknownBudgetTreatedUnlimited(t *testing.T) {
	r := NewNodeRegistry()
	// A node reporting MemoryTotal 0 (meminfo unavailable, e.g. darwin/dev)
	// must still be selectable so dev and mock paths keep working.
	r.Register(&NodeInfo{Name: "dev", Endpoint: "dev:9090", LastHeartbeat: time.Now()})
	node, err := r.SelectNode("", "")
	if err != nil {
		t.Fatalf("SelectNode: %v", err)
	}
	if node.Name != "dev" {
		t.Fatalf("got %q want dev", node.Name)
	}
}
