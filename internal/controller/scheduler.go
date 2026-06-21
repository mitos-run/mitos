package controller

import "errors"

// ErrNoCapacity is returned by SelectNode when the registry has healthy nodes
// but none of them can admit the projected fork under the current overcommit
// policy. Callers use errors.Is to distinguish a transient capacity shortage
// (scale out or raise the overcommit factor) from a hard scheduling error. It
// is deliberately distinct from the empty-registry and no-healthy-nodes errors.
var ErrNoCapacity = errors.New("no node has capacity")

// Default cold-start estimates used when no node reports a per-template
// estimate for the requested template (the template has never been forked
// anywhere). These keep a cold placement's marginal cost non-zero so the
// scheduler does not treat an unknown template as free.
const (
	defaultColdSharedBytes int64 = 256 * 1024 * 1024 // 256 MiB shared set
	defaultForkUniqueBytes int64 = 8 * 1024 * 1024   // 8 MiB per-fork unique
)

// available returns the schedulable headroom on a node under the overcommit
// factor: total*factor - used. A node reporting MemoryTotal 0 has an UNKNOWN
// budget (the forkd meminfo read failed, e.g. darwin/dev, or mock without a
// total); such nodes are treated as effectively unlimited so dev and mock
// paths keep scheduling. The bool reports whether the budget is known.
func (r *NodeRegistry) available(node *NodeInfo) (int64, bool) {
	if node.MemoryTotal <= 0 {
		return 0, false
	}
	factor := r.overcommitFactor
	if factor <= 0 {
		factor = 1.0
	}
	return int64(float64(node.MemoryTotal)*factor) - node.MemoryUsed, true
}

// isWarmFor reports whether the node already runs forks of templateID: it holds
// the snapshot and its per-template estimate records at least one fork. A warm
// node pays only the per-fork unique cost for an additional fork (the shared
// set is already resident); a cold node pays the shared set too.
func (n *NodeInfo) isWarmFor(templateID string) bool {
	if templateID == "" {
		return false
	}
	if est, ok := n.TemplateEstimates[templateID]; ok && est.ForkCount > 0 {
		return true
	}
	return false
}

// forkCountFor returns how many forks of templateID the node runs, per its
// per-template estimate (0 when unknown). Used to rank warm holders by density.
func (n *NodeInfo) forkCountFor(templateID string) int32 {
	if est, ok := n.TemplateEstimates[templateID]; ok {
		return est.ForkCount
	}
	return 0
}

// templateEstimateAcross returns a per-template estimate for templateID,
// preferring the candidate node's own estimate and falling back to ANY healthy
// node that has one. The bool reports whether any node knew the template.
// Caller must hold at least the read lock.
func (r *NodeRegistry) templateEstimateAcross(node *NodeInfo, templateID string) (TemplateCapacity, bool) {
	if templateID == "" {
		return TemplateCapacity{}, false
	}
	if est, ok := node.TemplateEstimates[templateID]; ok {
		return est, true
	}
	for _, n := range r.nodes {
		if est, ok := n.TemplateEstimates[templateID]; ok {
			return est, true
		}
	}
	return TemplateCapacity{}, false
}

// projectedCost is the marginal memory a fork of templateID would add to node.
// Warm node: only the average per-fork unique footprint (shared set already
// resident). Cold node: the shared set paid once plus the per-fork unique.
// Estimates come from this node first, then any node, then a configured
// default. Caller must hold at least the read lock.
func (r *NodeRegistry) projectedCost(node *NodeInfo, templateID string) int64 {
	est, known := r.templateEstimateAcross(node, templateID)

	unique := defaultForkUniqueBytes
	if known && est.AvgForkUniqueBytes > 0 {
		unique = est.AvgForkUniqueBytes
	}

	if node.isWarmFor(templateID) {
		return unique
	}

	shared := defaultColdSharedBytes
	if known && est.SharedOnceBytes > 0 {
		shared = est.SharedOnceBytes
	}
	return shared + unique
}

// atSandboxCeiling reports whether the node has reached its forkd-side sandbox
// COUNT cap (MaxSandboxes, PR #110): MaxSandboxes > 0 AND ActiveSandboxes is at
// or above it. MaxSandboxes 0 means the cap is unset/unlimited (e.g. the mock
// engine) and never blocks scheduling. Enforcing this at schedule time keeps the
// node-side ResourceExhausted reject a rare race, not the primary path to the
// cap.
func (n *NodeInfo) atSandboxCeiling() bool {
	return n.MaxSandboxes > 0 && n.ActiveSandboxes >= n.MaxSandboxes
}

// admitsRequest is the size- and GPU-aware admission check (issue #221). A node
// admits the request when it is below its sandbox-count ceiling, satisfies the
// request's GPU demand (GPU-capable, the right type, with enough free devices),
// and has memory headroom for the LARGER of the per-template CoW projected cost
// and the request's EXPLICIT memory size. The explicit-size term is what lets a
// large sandbox (above the 8 GiB E2B class) be admitted only where the node
// actually has the RAM; a small CoW estimate must not let an oversized request
// slip onto a node that cannot hold it. Nodes with an unknown budget
// (MemoryTotal 0, dev/mock) admit on memory but are still subject to the count
// ceiling and the GPU gate. Caller must hold at least the read lock.
func (r *NodeRegistry) admitsRequest(node *NodeInfo, req ForkRequest) bool {
	if node.atSandboxCeiling() {
		return false
	}
	if !node.admitsGPU(req) {
		return false
	}
	if !node.admitsTier(req) {
		return false
	}
	avail, known := r.available(node)
	if !known {
		return true
	}
	cost := r.projectedCost(node, req.TemplateID)
	if req.MemoryBytes > cost {
		cost = req.MemoryBytes
	}
	return cost <= avail
}

// admitsGPU reports whether the node satisfies the request's GPU demand. A
// non-GPU request (GPUCount 0) is satisfied by ANY node. A GPU request requires
// the node to be GPU-capable (GPUTotal > 0), to advertise the requested type
// (when the request names one), and to have at least GPUCount free devices
// (GPUTotal minus GPUUsed). A GPU is assigned EXCLUSIVELY to a sandbox and is
// never CoW-shared across forks, so device availability is a hard count, not a
// projected estimate. This is the scheduling gate only; the device-attach/VFIO
// path is hardware-gated (docs/platforms/gpu.md).
func (n *NodeInfo) admitsGPU(req ForkRequest) bool {
	if req.GPUCount <= 0 {
		return true
	}
	if n.GPUTotal <= 0 {
		return false
	}
	if req.GPUType != "" && n.GPUType != req.GPUType {
		return false
	}
	return n.GPUTotal-n.GPUUsed >= req.GPUCount
}

// admitsTier reports whether the node's declared isolation tier MEETS the
// request's minimum assurance floor (issue #40). A request with no floor
// (MinIsolationTier empty) is satisfied by ANY node, so the control is strictly
// opt-in. A request with a floor is satisfied only by a node whose tier is at
// least as strong: a hardware-kvm floor never admits a PVM (lower-assurance)
// node, and an UNDECLARED node (empty tier) never admits a real floor
// (fail-closed). This is the scheduling gate that keeps a security-sensitive
// tenant off a weaker-isolation node; the node-side mechanism that earns a tier
// is operational (docs/platforms/pvm-evaluation.md, docs/threat-model.md).
func (n *NodeInfo) admitsTier(req ForkRequest) bool {
	return n.IsolationTier.meets(req.MinIsolationTier)
}
