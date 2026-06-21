package controller

import (
	"crypto/tls"
	"fmt"
	"strconv"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"mitos.run/mitos/internal/observability"
)

// NodeRegistry tracks forkd instances across the cluster.
// Forkd pods register themselves via gRPC heartbeats.
// The controller uses this to select nodes for fork operations.
type NodeRegistry struct {
	mu    sync.RWMutex
	nodes map[string]*NodeInfo

	// TLS, when set, is the controller's mTLS client config used to dial
	// every node that does not carry its own NodeInfo.TLS. Set once at
	// startup before any dial; nil means insecure (tests, mock mode).
	TLS *tls.Config

	// overcommitFactor scales each node's reported memory budget when computing
	// schedulable headroom: available = total*factor - used. 1.0 (the default)
	// is no overcommit; a value above 1 leans on CoW sharing across forks of the
	// same template to pack more sandboxes per node. Guarded by mu.
	overcommitFactor float64
}

type NodeInfo struct {
	Name     string
	Endpoint string
	// HTTPEndpoint is the forkd HTTP sandbox API (exec/files), e.g. "10.0.3.7:9091".
	// This is what claim status endpoints point at.
	HTTPEndpoint string
	// CASEndpoint is the forkd DEDICATED token-gated TLS CAS listener
	// (e.g. "10.0.3.7:9092"), the source a peer pulls templates from. It is a
	// SEPARATE port from HTTPEndpoint: CAS distribution is served over TLS on its
	// own listener so the sandbox HTTP API scheme is unchanged. Populated by
	// discovery from the same pod IP as HTTPEndpoint with the CAS port.
	CASEndpoint     string
	ActiveSandboxes int32
	MaxSandboxes    int32
	MemoryTotal     int64
	MemoryUsed      int64

	// GPU capacity (issue #221). A GPU node advertises the GPU node label and a
	// GPU type; the controller mirrors that here so GPU pools are scheduled ONLY
	// onto GPU-capable nodes, mirroring the KVM node selection. GPUTotal is the
	// node's GPU device count; GPUUsed is how many are already assigned to
	// sandboxes (a GPU is exclusively assigned, never CoW-shared). GPUTotal 0
	// means the node is CPU-only and never admits a GPU request. The actual
	// device-attach/VFIO path is hardware-gated (docs/platforms/gpu.md); these
	// fields are the scheduling seam only.
	GPUTotal int32
	GPUUsed  int32
	// GPUType is the SKU the node advertises (for example "nvidia-a100"), matched
	// against a typed GPU request. Empty on a CPU-only node.
	GPUType string
	TemplateIDs     []string
	SnapshotIDs     []string
	// TemplateDigests maps each held template id to its content-addressed
	// snapshot manifest digest, as reported by the node's GetCapacity. Safe
	// to log; used by the pool reconciler to record the digest in CRD status.
	TemplateDigests map[string]string
	// TemplateEstimates maps each template id to the node's per-template
	// capacity estimate (shared-once and average per-fork unique bytes). The
	// scheduler uses it to project the marginal memory cost of placing a fork.
	TemplateEstimates map[string]TemplateCapacity
	LastHeartbeat     time.Time
	// probeFailures is the number of CONSECUTIVE forkd liveness probes
	// (GetCapacity) that have failed for this node. Discovery resets it to 0 on a
	// successful probe and increments it on a failure (carried across the
	// every-15s re-register). A node at or above probeFailureThreshold is treated
	// as unhealthy and dropped from scheduling even while its pod is still
	// Running: a hung forkd or a dead host whose pod has not yet been observed
	// gone must not keep receiving forks. A single failure does NOT flap a node
	// out (the threshold absorbs a transient blip).
	probeFailures int
	// TLS, when set, overrides the registry-level TLS config for dials to
	// this node; lets tests run mixed TLS/insecure fleets in one registry.
	TLS  *tls.Config
	conn *grpc.ClientConn
}

// probeFailureThreshold is the number of CONSECUTIVE failed liveness probes
// after which a node is treated as unhealthy. Set above 1 so a single transient
// probe blip does not flap a node out of the schedulable set; at the every-15s
// discovery interval, three failures means roughly 45s of an unreachable forkd
// before the node is dropped, well inside the 2-minute heartbeat TTL.
const probeFailureThreshold = 3

// gpuNodeLabel is the node label a GPU-capable node carries (issue #221). A GPU
// pool's sandboxes are pinned to nodes carrying this label, mirroring the KVM
// node label (huskpod.go huskKVMNodeLabel). The node-side GPU setup (VFIO
// binding, device plugin advertising the GPU resource) is documented and
// hardware-gated in docs/platforms/gpu.md; the controller only consumes the
// label/resource a properly prepared node advertises. The companion GPU SKU
// label a typed request matches against is "mitos.run/gpu-type" (see
// docs/platforms/gpu.md and GPUFromNodeLabels).
const gpuNodeLabel = "mitos.run/gpu"

// gpuTypeNodeLabel is the node label advertising the GPU SKU (for example
// "nvidia-a100") that a typed GPU request matches against.
const gpuTypeNodeLabel = "mitos.run/gpu-type"

// GPUFromNodeLabels reads a node's GPU capability from its Kubernetes labels
// (issue #221), the bridge from a GPU-prepared node to the NodeInfo the
// scheduler consumes. A node is GPU-capable when it carries gpuNodeLabel="true";
// the device count is parsed from that label's integer value when present (a
// bare "true" advertises a single device), and the SKU comes from
// gpuTypeNodeLabel. Returns total=0 (CPU-only) when the GPU label is absent or
// not truthy. The node-side VFIO binding and device-plugin setup that make a
// node advertise these labels are hardware-gated (docs/platforms/gpu.md); this
// is the label-to-scheduler mapping only and needs no hardware to exercise.
func GPUFromNodeLabels(labels map[string]string) (total int32, gpuType string) {
	v, ok := labels[gpuNodeLabel]
	if !ok {
		return 0, ""
	}
	switch v {
	case "", "false", "0":
		return 0, ""
	case "true":
		total = 1
	default:
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			total = int32(n)
		} else {
			total = 1
		}
	}
	return total, labels[gpuTypeNodeLabel]
}

// TemplateCapacity is the controller-side mirror of the forkd proto
// TemplateCapacity: the per-template memory estimate the scheduler bin-packs
// with. SharedOnceBytes is the CoW shared set a cold start of this template
// pays once; AvgForkUniqueBytes is the mean per-fork unique footprint every
// fork (warm or cold) adds.
type TemplateCapacity struct {
	TemplateID         string
	SnapshotDigest     string
	SharedOnceBytes    int64
	AvgForkUniqueBytes int64
	ForkCount          int32
}

func NewNodeRegistry() *NodeRegistry {
	return &NodeRegistry{
		nodes:            make(map[string]*NodeInfo),
		overcommitFactor: 1.0,
	}
}

// SetOvercommitFactor sets the memory overcommit factor used when projecting
// schedulable headroom. Values at or below zero are ignored (the factor stays
// as it was). A factor above 1 leans on CoW sharing to pack more forks per
// node; document the tradeoff (a node can be driven into reclaim/OOM if the
// sharing assumption does not hold) before raising it in production.
func (r *NodeRegistry) SetOvercommitFactor(f float64) {
	if f <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.overcommitFactor = f
}

// SetNodeMemory overwrites a registered node's reported memory budget
// (MemoryTotal) and current usage (MemoryUsed) under the write lock. It exists
// for tests and for capacity bookkeeping that does not arrive on a full
// heartbeat; production heartbeats set these fields via Register. A node not in
// the registry is a no-op.
func (r *NodeRegistry) SetNodeMemory(name string, total, used int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if node, ok := r.nodes[name]; ok {
		node.MemoryTotal = total
		node.MemoryUsed = used
	}
}

// Register adds or updates a node in the registry.
func (r *NodeRegistry) Register(info *NodeInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.nodes == nil {
		r.nodes = make(map[string]*NodeInfo)
	}
	if old, ok := r.nodes[info.Name]; ok && old.conn != nil {
		if old.Endpoint == info.Endpoint && info.conn == nil {
			info.conn = old.conn // carry the dialed connection forward
		} else {
			old.conn.Close()
		}
	}
	info.LastHeartbeat = time.Now()
	r.nodes[info.Name] = info
}

// Unregister removes a node from the registry.
func (r *NodeRegistry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if node, ok := r.nodes[name]; ok {
		if node.conn != nil {
			node.conn.Close()
		}
		delete(r.nodes, name)
	}
}

// SelectNode picks the node for a fork of snapshotID (the template id), packing
// to maximize CoW sharing rather than spreading load. It admits only nodes
// whose projected marginal cost fits their schedulable headroom (capacity-aware
// bin-packing), then scores admitted nodes to PACK warm snapshot-holders dense.
//
// Errors are distinct: an empty registry and no healthy nodes are scheduling
// preconditions; ErrNoCapacity means healthy nodes exist but none admit the
// fork under the overcommit policy (a transient shortage, scale out or raise
// the factor).
func (r *NodeRegistry) SelectNode(snapshotID string, preferredNode string) (*NodeInfo, error) {
	return r.SelectNodeForFork(ForkRequest{TemplateID: snapshotID, PreferredNode: preferredNode})
}

// ForkRequest is the full input to node selection (issue #221): the template to
// fork plus the requested SIZE and GPU demand, so the scheduler can admit large
// sandboxes (above the 8 GiB E2B class) only where the node has the RAM and pin
// GPU sandboxes to GPU-capable nodes. MemoryBytes 0 means "size from the
// per-template CoW estimate" (the legacy behavior SelectNode preserves);
// GPUCount 0 means a CPU-only sandbox.
type ForkRequest struct {
	// TemplateID is the snapshot/template the sandbox forks from.
	TemplateID string
	// PreferredNode is honored only when healthy AND it admits the request.
	PreferredNode string
	// MemoryBytes is the sandbox's explicit memory size in bytes. When greater
	// than the per-template projected CoW cost it gates admission, so an oversized
	// sandbox lands only on a node that can hold it.
	MemoryBytes int64
	// GPUCount is the number of GPUs the sandbox requires (0 for CPU-only). A
	// positive count restricts placement to GPU-capable nodes with free devices.
	GPUCount int32
	// GPUType narrows a GPU request to nodes advertising this SKU (empty = any).
	GPUType string
}

// SelectNodeForFork is the size- and GPU-aware node selection (issue #221). It
// admits only nodes that satisfy the request's memory SIZE and GPU demand, then
// packs admitted nodes to maximize CoW sharing exactly as SelectNode does. GPU
// requests are pinned to GPU-capable nodes (mirroring the KVM node selection);
// large sizes are admitted only where the node has the RAM. Errors stay distinct:
// empty registry and no healthy nodes are scheduling preconditions; ErrNoCapacity
// means healthy nodes exist but none admit (memory exhausted, no GPU node, no
// matching GPU type, or GPU devices exhausted).
func (r *NodeRegistry) SelectNodeForFork(req ForkRequest) (*NodeInfo, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if len(r.nodes) == 0 {
		return nil, fmt.Errorf("no forkd nodes registered")
	}

	// Preferred node is honored only when healthy AND it admits the request.
	// Otherwise fall through to the general scoring so a full preferred node
	// does not pin the claim.
	if req.PreferredNode != "" {
		if node, ok := r.nodes[req.PreferredNode]; ok && node.isHealthy() && r.admitsRequest(node, req) {
			return node, nil
		}
	}

	healthy := false
	var admitted []*NodeInfo
	for _, node := range r.nodes {
		if !node.isHealthy() {
			continue
		}
		healthy = true
		if r.admitsRequest(node, req) {
			admitted = append(admitted, node)
		}
	}

	if !healthy {
		return nil, fmt.Errorf("no healthy forkd nodes available")
	}
	if len(admitted) == 0 {
		if req.GPUCount > 0 {
			return nil, fmt.Errorf("%w: no GPU-capable node admits the request (count %d, type %q); add GPU nodes labeled %s or free GPU devices", ErrNoCapacity, req.GPUCount, req.GPUType, gpuNodeLabel)
		}
		return nil, fmt.Errorf("%w: cluster memory exhausted under the overcommit policy (factor %.2f); scale out forkd nodes or raise the overcommit factor", ErrNoCapacity, r.overcommitFactor)
	}

	return r.packBest(admitted, req.TemplateID), nil
}

// packBest scores admitted nodes to maximize density. Warm snapshot-holders
// (they hold the snapshot and already run forks of it) win over cold nodes so
// CoW sharing is reused; among warm holders the densest (most existing forks)
// is packed first; among cold-only candidates the one with the most free memory
// is chosen to spread cold starts. Ties break deterministically by node name.
func (r *NodeRegistry) packBest(admitted []*NodeInfo, snapshotID string) *NodeInfo {
	best := admitted[0]
	for _, node := range admitted[1:] {
		if r.denser(node, best, snapshotID) {
			best = node
		}
	}
	return best
}

// packTier ranks a node for the given snapshot: 2 = warm (holds the snapshot and
// runs forks of it; reuses the resident CoW set), 1 = holder (holds the
// snapshot but no recorded forks yet; still cheaper than a cold start), 0 =
// cold (no snapshot). Higher packs first.
func (n *NodeInfo) packTier(snapshotID string) int {
	if snapshotID == "" || !n.hasSnapshot(snapshotID) {
		return 0
	}
	if n.forkCountFor(snapshotID) > 0 {
		return 2
	}
	return 1
}

// denser reports whether candidate c should beat the current best b under the
// packing policy: a higher pack tier wins (warm over holder over cold); within
// the warm/holder tiers the node running MORE forks of the snapshot packs
// first; within the cold tier the node with the MOST free memory wins (spread
// cold starts), with unknown-budget nodes ranked last. Ties break
// deterministically by the lexicographically smaller node name.
func (r *NodeRegistry) denser(c, b *NodeInfo, snapshotID string) bool {
	cTier, bTier := c.packTier(snapshotID), b.packTier(snapshotID)
	if cTier != bTier {
		return cTier > bTier
	}

	if cTier > 0 { // both hold the snapshot: pack the denser holder
		cForks, bForks := c.forkCountFor(snapshotID), b.forkCountFor(snapshotID)
		if cForks != bForks {
			return cForks > bForks
		}
		return c.Name < b.Name
	}

	// both cold: spread to the node with the most free memory. Unknown budgets
	// (MemoryTotal 0) are comparable only by name and rank below any known
	// budget so a real node with headroom is preferred over a dev node.
	cAvail, cKnown := r.available(c)
	bAvail, bKnown := r.available(b)
	if cKnown != bKnown {
		return cKnown
	}
	if cKnown && cAvail != bAvail {
		return cAvail > bAvail
	}
	return c.Name < b.Name
}

// NodesWithTemplate returns healthy nodes that hold the given template snapshot.
func (r *NodeRegistry) NodesWithTemplate(templateID string) []*NodeInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*NodeInfo
	for _, n := range r.nodes {
		if n.isHealthy() && n.hasSnapshot(templateID) {
			out = append(out, n)
		}
	}
	return out
}

// SnapshotHolder is a healthy node that holds a template snapshot, paired with
// the content-addressed digest THAT node recorded for it. Each node builds its
// template snapshot independently, so these digests differ per node; a husk pod
// pinned to a node must verify against this node's digest, never another's.
type SnapshotHolder struct {
	Name   string
	Digest string
}

// SnapshotHolders returns every healthy node holding templateID with its own
// recorded digest (empty string if the node has not reported one yet). The
// name/digest pairs are copied out under the lock so callers never read
// NodeInfo concurrently.
func (r *NodeRegistry) SnapshotHolders(templateID string) []SnapshotHolder {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []SnapshotHolder
	for _, n := range r.nodes {
		if n.isHealthy() && n.hasSnapshot(templateID) {
			out = append(out, SnapshotHolder{Name: n.Name, Digest: n.TemplateDigests[templateID]})
		}
	}
	return out
}

// TemplateDigestOnNode returns the content-addressed digest THAT node recorded
// for templateID. Nodes build snapshots independently so digests differ per
// node; a husk pod activation must verify against the digest of the node the pod
// runs on, never a cluster-wide pick (issue #177). Returns false if the node is
// unknown or has not reported a digest.
func (r *NodeRegistry) TemplateDigestOnNode(nodeName, templateID string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if n, ok := r.nodes[nodeName]; ok {
		if d, ok := n.TemplateDigests[templateID]; ok && d != "" {
			return d, true
		}
	}
	return "", false
}

// TemplateSource picks a healthy node that holds the template AND reports a
// content-addressed digest for it, and returns the holder, its CAS-serving base
// URL, and the digest. It is the source the pool reconciler distributes from:
// a deficit node pulls the manifest (by digest) from this holder's CAS surface.
// ok is false when no holder reports a digest (e.g. only a mock-engine holder
// with an empty digest, which cannot be a pull source). The holder is chosen
// deterministically by node name so repeated reconciles pick the same source.
func (r *NodeRegistry) TemplateSource(templateID string) (holder *NodeInfo, casURL, digest string, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var best *NodeInfo
	var bestDigest string
	for _, n := range r.nodes {
		if !n.isHealthy() || !n.hasSnapshot(templateID) {
			continue
		}
		d := n.TemplateDigests[templateID]
		if d == "" || n.CASEndpoint == "" {
			continue
		}
		if best == nil || n.Name < best.Name {
			best = n
			bestDigest = d
		}
	}
	if best == nil {
		return nil, "", "", false
	}
	return best, best.casBaseURL(), bestDigest, true
}

// casBaseURL derives the node's CAS-serving base URL from its DEDICATED CAS
// endpoint. The CAS surface is served under /cas on its OWN listener (a separate
// port from the sandbox HTTP API), over TLS only (template distribution requires
// mTLS), so the scheme is https. Returns "" when the node reports no CAS
// endpoint.
func (n *NodeInfo) casBaseURL() string {
	if n.CASEndpoint == "" {
		return ""
	}
	return "https://" + n.CASEndpoint + "/cas"
}

// AddTemplate records that a node now holds the given template snapshot.
// Takes the write lock so NodeInfo.TemplateIDs is never mutated concurrently
// with readers like NodesWithTemplate.
func (r *NodeRegistry) AddTemplate(nodeName, templateID string) {
	r.AddTemplateWithDigest(nodeName, templateID, "")
}

// AddTemplateWithDigest records a node's template snapshot and its
// content-addressed digest (empty digest is allowed, e.g. mock engine). The
// digest is what the pool reconciler surfaces in the CRD status.
func (r *NodeRegistry) AddTemplateWithDigest(nodeName, templateID, digest string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	node, ok := r.nodes[nodeName]
	if !ok {
		return
	}
	if digest != "" {
		if node.TemplateDigests == nil {
			node.TemplateDigests = make(map[string]string)
		}
		node.TemplateDigests[templateID] = digest
	}
	for _, t := range node.TemplateIDs {
		if t == templateID {
			return
		}
	}
	node.TemplateIDs = append(node.TemplateIDs, templateID)
}

// TemplateDigest returns the content-addressed digest reported by any healthy
// node holding the template, and whether one was found. Nodes report identical
// content for the same template (content addressing), so the first match wins.
func (r *NodeRegistry) TemplateDigest(templateID string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, n := range r.nodes {
		if !n.isHealthy() || !n.hasSnapshot(templateID) {
			continue
		}
		if d, ok := n.TemplateDigests[templateID]; ok && d != "" {
			return d, true
		}
	}
	return "", false
}

// GetNode returns the registered node by name.
func (r *NodeRegistry) GetNode(name string) (*NodeInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	node, ok := r.nodes[name]
	return node, ok
}

// NodeHealthy reports whether the named node is registered and still
// heartbeating. It returns false when the node is absent.
func (r *NodeRegistry) NodeHealthy(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	node, ok := r.nodes[name]
	if !ok {
		return false
	}
	return node.isHealthy()
}

// GetConnection returns a gRPC connection to a node's forkd, dialing once.
// Transport credentials are chosen per node: NodeInfo.TLS wins, then the
// registry-level TLS config, then insecure (tests and mock mode).
func (r *NodeRegistry) GetConnection(nodeName string) (*grpc.ClientConn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	node, ok := r.nodes[nodeName]
	if !ok {
		return nil, fmt.Errorf("node %s not found", nodeName)
	}
	if node.conn != nil {
		return node.conn, nil
	}

	creds := insecure.NewCredentials()
	switch {
	case node.TLS != nil:
		creds = credentials.NewTLS(node.TLS)
	case r.TLS != nil:
		creds = credentials.NewTLS(r.TLS)
	}
	conn, err := grpc.NewClient(
		node.Endpoint,
		grpc.WithTransportCredentials(creds),
		// Propagate trace context to forkd: the client handler injects the
		// active span's W3C trace headers so the forkd.Fork span joins the
		// controller's trace. No-op when tracing is disabled.
		grpc.WithStatsHandler(observability.GRPCClientStatsHandler()),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to forkd on %s: %w", nodeName, err)
	}
	node.conn = conn
	return conn, nil
}

// NodeMTLS reports whether dials to the named node use mTLS, mirroring the
// transport-credential choice in GetConnection: a per-node NodeInfo.TLS or the
// registry-level TLS config means the channel is mTLS; both nil means insecure.
// An unregistered node is reported insecure (false). This is the fail-closed
// gate for delivering the at-rest encryption key: the key may only be sent over
// a channel this reports true for.
func (r *NodeRegistry) NodeMTLS(nodeName string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	node, ok := r.nodes[nodeName]
	if !ok {
		return false
	}
	return node.TLS != nil || r.TLS != nil
}

// ListNodes returns all registered nodes.
func (r *NodeRegistry) ListNodes() []*NodeInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	nodes := make([]*NodeInfo, 0, len(r.nodes))
	for _, n := range r.nodes {
		nodes = append(nodes, n)
	}
	return nodes
}

// PruneStale removes nodes that haven't sent a heartbeat recently.
func (r *NodeRegistry) PruneStale(maxAge time.Duration) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	pruned := 0
	for name, node := range r.nodes {
		if time.Since(node.LastHeartbeat) >= maxAge {
			if node.conn != nil {
				node.conn.Close()
			}
			delete(r.nodes, name)
			pruned++
		}
	}
	return pruned
}

// isHealthy reports whether the node is schedulable. Health requires BOTH a
// recent heartbeat (the 2-minute last-seen TTL) AND a live forkd: a node whose
// liveness probe (GetCapacity) has failed probeFailureThreshold times in a row
// is unhealthy even with a fresh heartbeat, since its pod can still be Running
// while forkd is hung or its host is dead. This ties health to a LIVENESS
// signal, not just last-seen, so a hung forkd cannot stay schedulable.
func (n *NodeInfo) isHealthy() bool {
	if n.probeFailures >= probeFailureThreshold {
		return false
	}
	return time.Since(n.LastHeartbeat) < 2*time.Minute
}

// priorProbeFailures returns the consecutive-probe-failure count currently
// recorded for the named node, or 0 when the node is not registered. Discovery
// uses it to carry the count across the every-15s re-register (which replaces
// the whole NodeInfo struct).
func (r *NodeRegistry) priorProbeFailures(name string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if node, ok := r.nodes[name]; ok {
		return node.probeFailures
	}
	return 0
}

func (n *NodeInfo) hasSnapshot(id string) bool {
	for _, s := range n.SnapshotIDs {
		if s == id {
			return true
		}
	}
	for _, t := range n.TemplateIDs {
		if t == id {
			return true
		}
	}
	return false
}
