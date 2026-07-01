package controller

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "mitos.run/mitos/api/v1"
	forkdpb "mitos.run/mitos/proto/forkd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// nodeLostReforkCountAnnotation records how many times a raw-forkd claim has been
// automatically re-forked after node loss. It is the durable, per-claim bound on
// the auto-replacement retries (issue #372): the GC reads it to decide re-fork vs
// fail-closed and bumps it on each re-pend, so a claim whose replacement node
// keeps dying cannot retry forever. It is metadata, not status, so it survives the
// status re-pend write below.
const nodeLostReforkCountAnnotation = "mitos.run/nodelost-refork-count"

// maxRawForkdNodeLostReforks bounds how many times a single raw-forkd claim is
// automatically re-forked onto a surviving snapshot-holder after node loss before
// the GC fails it closed with a terminal NodeLost. The bound is over the claim's
// whole life (the count is never reset), so a claim repeatedly landing on dying
// nodes fails closed instead of churning forever. Husk mode is unaffected; it
// self-heals via the warm pool re-pend.
const maxRawForkdNodeLostReforks = 3

// GarbageCollector is a manager Runnable that periodically reconciles forkd
// actuals against CRD-desired state. In one pass it sweeps orphan VMs: a forkd
// sandbox on a healthy node with no backing Ready claim or fork child, older
// than OrphanGrace, is terminated. This is also controller-restart
// reconciliation: after a restart the desired set is rebuilt from CRD state and
// any VM not accounted for is reaped.
type GarbageCollector struct {
	Client   client.Client
	Registry *NodeRegistry

	// Interval is the period between GC passes. Default 30s.
	Interval time.Duration
	// OrphanGrace is the minimum uptime a forkd sandbox must have before the
	// orphan sweep will terminate it. This protects a freshly-forked VM whose
	// claim status has not been written yet. Default 60s.
	OrphanGrace time.Duration
	// DefaultTTLSeconds is the TTL applied to a finished claim that does not set
	// spec.ttlSecondsAfterFinished. Default 600s.
	DefaultTTLSeconds int32
	// EnableHuskPods mirrors the controller run mode. In husk mode a Ready claim
	// is backed by a husk pod, and node-loss recovery is owned by
	// checkHuskPodLost + the husk pod watch, which RE-PEND the claim onto a
	// replacement dormant slot (the warm pool self-heals). The GC must NOT
	// terminally-fail such a claim: a GC pass winning the race against the
	// re-pend would defeat the husk self-heal. When set, markNodeLost is a no-op.
	// In raw-forkd mode (false) a lost node means the ephemeral VM is gone with
	// no recovery, so the claim is correctly failed (and the GC TTL reaps it).
	EnableHuskPods bool
}

func (g *GarbageCollector) Start(ctx context.Context) error {
	g.applyDefaults()
	ticker := time.NewTicker(g.Interval)
	defer ticker.Stop()
	for {
		g.runOnce(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// RunOnce executes a single GC pass. It exists so tests can drive one pass
// deterministically instead of waiting on the ticker.
func (g *GarbageCollector) RunOnce(ctx context.Context) {
	g.applyDefaults()
	g.runOnce(ctx)
}

// applyDefaults fills zero-valued tunables so a GarbageCollector driven via
// RunOnce (without Start) still uses the documented defaults.
func (g *GarbageCollector) applyDefaults() {
	if g.Interval == 0 {
		g.Interval = 30 * time.Second
	}
	if g.OrphanGrace == 0 {
		g.OrphanGrace = 60 * time.Second
	}
	if g.DefaultTTLSeconds == 0 {
		g.DefaultTTLSeconds = 600
	}
}

func (g *GarbageCollector) runOnce(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("gc")

	var sandboxes v1.SandboxList
	if err := g.Client.List(ctx, &sandboxes); err != nil {
		logger.Error(err, "list sandboxes")
		return
	}

	desired := g.desiredAlive(sandboxes.Items)
	liveIDs := g.liveIDs(sandboxes.Items)

	// Order matters only loosely: markNodeLost only touches sandboxes whose node
	// is unhealthy/absent, and sweepOrphans only visits healthy nodes, so the two
	// never act on the same node. A sandbox just marked NodeLost stamps
	// FinishedAt=now, so it is too fresh for any later TTL pass to delete.
	g.markNodeLost(ctx, logger, sandboxes.Items)
	g.sweepOrphans(ctx, logger, desired, liveIDs, sandboxes.Items)
	g.sweepOrphanVolumes(ctx, logger, desired, liveIDs)
	g.ttlFinished(ctx, logger, sandboxes.Items)
}

// desiredAlive builds the set of VMs the control plane expects alive, keyed by
// node then sandbox id: Ready poolRef sandboxes (status.Node + status.SandboxID)
// and Ready fork children (status.Children entries with a Node, SandboxID, and
// Ready phase).
func (g *GarbageCollector) desiredAlive(sandboxes []v1.Sandbox) map[string]map[string]bool {
	desired := make(map[string]map[string]bool)
	add := func(node, id string) {
		if node == "" || id == "" {
			return
		}
		if desired[node] == nil {
			desired[node] = make(map[string]bool)
		}
		desired[node][id] = true
	}
	for i := range sandboxes {
		c := &sandboxes[i]
		if c.Status.Phase == v1.SandboxReady {
			add(c.Status.Node, c.Status.SandboxID)
		}
		for _, fi := range c.Status.Children {
			if fi.Phase == v1.SandboxReady {
				add(fi.Node, fi.SandboxID)
			}
		}
	}
	return desired
}

// liveIDs builds a node-independent set of sandbox ids the control plane still
// has a live CRD object for, so the orphan sweep never kills a VM whose backing
// object exists even when that object never wrote status.Node/status.SandboxID
// (e.g. a sandbox wedged in Restoring or Pending past the grace).
//
// The controller uses sandbox.Name AS the sandbox id (the engine calls
// forkOnNode(ctx, node, snapshotID, sb.Name, ...) and forkd echoes it back, so
// status.SandboxID == sb.Name once Ready). So every non-terminal sandbox
// contributes sb.Name regardless of its status, and every non-terminal fork
// child contributes its explicit SandboxID from status.Children. A VM is only a
// sweep candidate once its sandbox object is gone (or its node is lost).
func (g *GarbageCollector) liveIDs(sandboxes []v1.Sandbox) map[string]bool {
	live := make(map[string]bool)
	for i := range sandboxes {
		c := &sandboxes[i]
		if c.Status.Phase == v1.SandboxTerminated || c.Status.Phase == v1.SandboxFailed {
			continue
		}
		live[c.Name] = true
		for _, fi := range c.Status.Children {
			if fi.Phase == v1.SandboxTerminated || fi.Phase == v1.SandboxFailed {
				continue
			}
			if fi.SandboxID != "" {
				live[fi.SandboxID] = true
			}
		}
	}
	return live
}

// sweepOrphans terminates forkd sandboxes on healthy nodes that are not in the
// desired-alive set, not in the node-independent liveIDs set, and whose uptime
// exceeds OrphanGrace. Only healthy nodes are swept: a VM on an unreachable node
// is owned by the NodeLost path. The liveIDs guard closes the stuck-Restoring
// window: a VM keeps living as long as its claim object exists, while a
// genuinely-abandoned VM (claim object gone) is still reaped.
func (g *GarbageCollector) sweepOrphans(ctx context.Context, logger logr.Logger, desired map[string]map[string]bool, liveIDs map[string]bool, sandboxes []v1.Sandbox) {
	// Index terminal claims by sandbox id (the claim name) so the sweep can
	// surface a typed condition when it reaps a VM a still-present claim once
	// backed. Only terminal claims appear here: a non-terminal claim by name is
	// in liveIDs and never swept. The claim having reached a terminal phase while
	// its VM lingered is the re-adopted-orphan case the condition names.
	terminalClaims := make(map[string]*v1.Sandbox, len(sandboxes))
	for i := range sandboxes {
		c := &sandboxes[i]
		if c.Status.Phase == v1.SandboxTerminated || c.Status.Phase == v1.SandboxFailed {
			terminalClaims[c.Name] = c
		}
	}
	for _, node := range g.Registry.ListNodes() {
		if !g.Registry.NodeHealthy(node.Name) {
			continue
		}
		live := g.listSandboxes(ctx, node.Name)
		for _, sb := range live {
			if desired[node.Name][sb.SandboxId] {
				continue
			}
			if liveIDs[sb.SandboxId] {
				// A CRD object still backs this VM by name: leave it alone, even
				// if its status was never written.
				continue
			}
			if sb.UptimeSeconds < int64(g.OrphanGrace.Seconds()) {
				// Freshly forked, status not yet written: leave it alone.
				continue
			}
			if err := terminateOnNode(ctx, g.Registry, node.Name, sb.SandboxId); err != nil {
				logger.Error(err, "terminate orphan sandbox", "node", node.Name, "sandbox", sb.SandboxId)
				continue
			}
			recordOrphanSweep()
			logger.Info("terminated orphan sandbox", "node", node.Name, "sandbox", sb.SandboxId)
			// If a terminal claim still names this VM, the GC (not a graceful
			// terminate) reaped a VM that lingered past the claim's terminal
			// transition: stamp a typed condition so an operator/SDK can tell the
			// two apart.
			if c, ok := terminalClaims[sb.SandboxId]; ok {
				g.stampOrphanReaped(ctx, logger, c)
			}
		}
	}
}

// stampOrphanReaped records the typed OrphanReaped condition on a terminal claim
// whose lingering VM the orphan sweep just reaped. The condition is idempotent
// (setCondition no-ops an identical re-assert) so repeated passes on a claim
// that survives its TTL do not churn the object. The message is operator-legible
// and carries no secret value.
func (g *GarbageCollector) stampOrphanReaped(ctx context.Context, logger logr.Logger, claim *v1.Sandbox) {
	changed := setCondition(&claim.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		LastTransitionTime: metav1.Now(),
		Reason:             "OrphanReaped",
		Message:            "the garbage collector reaped a backing VM that lingered past this claim's terminal transition (a terminate that crashed or was missed); no action is needed, the VM is gone",
	})
	if !changed {
		return
	}
	if err := g.Client.Status().Update(ctx, claim); err != nil {
		logger.Error(err, "stamp OrphanReaped condition", "claim", claim.Name)
	}
}

// sweepOrphanVolumes reclaims per-sandbox volume backing dirs on healthy nodes
// that are not in the desired-alive set, not in the node-independent liveIDs
// set, and whose age exceeds OrphanGrace. It is the volume counterpart to
// sweepOrphans and reuses the exact same desired and liveIDs sets, since a
// volume backing is keyed by the same sandbox id (the claim name) the VM is.
//
// The orphan case is a backing dir left behind when a terminate crashed or was
// missed: the VM is gone but its backing files survived. The grace and liveID
// nets are the same safety valves as the VM sweep: a backing for a non-terminal
// claim by name is left alone (even if its status never landed), and a backing
// freshly prepared (younger than OrphanGrace) is left alone so a just-forked
// sandbox whose claim status has not landed yet is never reclaimed. Only healthy
// nodes are visited, mirroring sweepOrphans.
func (g *GarbageCollector) sweepOrphanVolumes(ctx context.Context, logger logr.Logger, desired map[string]map[string]bool, liveIDs map[string]bool) {
	for _, node := range g.Registry.ListNodes() {
		if !g.Registry.NodeHealthy(node.Name) {
			continue
		}
		for _, vol := range g.listVolumes(ctx, node.Name) {
			if desired[node.Name][vol.SandboxId] {
				continue
			}
			if liveIDs[vol.SandboxId] {
				// A CRD object still backs this volume by name: leave it alone.
				continue
			}
			if vol.AgeSeconds < int64(g.OrphanGrace.Seconds()) {
				// Freshly prepared, claim status not yet written: leave it alone.
				continue
			}
			if err := reclaimVolumeOnNode(ctx, g.Registry, node.Name, vol.SandboxId); err != nil {
				logger.Error(err, "reclaim orphan volume", "node", node.Name, "sandbox", vol.SandboxId)
				continue
			}
			recordVolumeOrphanSweep()
			logger.Info("reclaimed orphan volume", "node", node.Name, "sandbox", vol.SandboxId)
		}
	}
}

// markNodeLost handles raw-forkd Ready claims whose node is no longer a healthy
// registered node. In raw-forkd mode (issue #372) a lost node does not always
// end the claim: when a surviving node still holds the pool's template snapshot
// and the per-claim re-fork bound is not yet spent, the claim is RE-PENDED so the
// claim reconciler re-issues the fork onto that surviving holder (the same
// SelectNode + forkOnNode placement path, no KVM needed to decide). The claim
// only reaches the terminal Failed phase with a NodeLost condition when it fails
// closed: no surviving snapshot holder, or the bounded retries are exhausted.
//
// We reuse the existing SandboxFailed phase with a NodeLost reason for the
// terminal case rather than adding a dedicated phase const: the phase set stays
// small and a NodeLost claim is, for every consumer, just a failed claim with a
// specific reason. The node is gone, so there is nothing to terminate; we only
// stamp state, bounded by the GC interval.
//
// In husk mode this whole path is a no-op: a Ready husk-backed claim recovers
// from node loss by re-pending onto a replacement dormant slot (checkHuskPodLost
// + the husk pod watch own that path). Acting here would race that re-pend and
// defeat the husk self-heal.
func (g *GarbageCollector) markNodeLost(ctx context.Context, logger logr.Logger, sandboxes []v1.Sandbox) {
	if g.EnableHuskPods {
		return
	}
	for i := range sandboxes {
		c := &sandboxes[i]
		if c.Status.Phase != v1.SandboxReady {
			continue
		}
		if c.Status.Node == "" || g.Registry.NodeHealthy(c.Status.Node) {
			continue
		}
		g.handleRawNodeLost(ctx, logger, c)
	}
}

// handleRawNodeLost decides between auto re-fork and fail-closed for a single
// raw-forkd Ready claim on a lost node. It re-pends the claim (back to Pending,
// stale placement cleared, a typed NodeLostReforking condition, the per-claim
// re-fork count bumped) when a surviving snapshot holder exists AND the bound is
// not yet spent; the claim reconciler then re-issues the fork onto a surviving
// holder via its normal placement path. Otherwise it fails the claim closed with
// a terminal NodeLost condition whose message names the actual cause (no surviving
// holder, or the retries exhausted).
func (g *GarbageCollector) handleRawNodeLost(ctx context.Context, logger logr.Logger, c *v1.Sandbox) {
	holderSurvives := g.snapshotHolderSurvives(ctx, c)
	reforks := nodeLostReforkCount(c)
	lostNode := c.Status.Node

	if holderSurvives && reforks < maxRawForkdNodeLostReforks {
		// Bump the durable per-claim re-fork count first (metadata, so it survives
		// the status re-pend write). On a write error, leave the claim Ready and
		// retry next pass rather than re-pending without recording the attempt.
		if c.Annotations == nil {
			c.Annotations = map[string]string{}
		}
		c.Annotations[nodeLostReforkCountAnnotation] = strconv.Itoa(reforks + 1)
		if err := g.Client.Update(ctx, c); err != nil {
			logger.Error(err, "stamp re-fork count for NodeLost claim", "claim", c.Name, "node", lostNode)
			return
		}
		// Re-pend: clear the stale placement so no consumer reads a dead endpoint,
		// and let the claim reconciler re-fork onto a surviving holder.
		c.Status.Phase = v1.SandboxPending
		c.Status.Node = ""
		c.Status.Endpoint = ""
		c.Status.SandboxID = ""
		c.Status.StartedAt = nil
		setCondition(&c.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "NodeLostReforking",
			Message:            fmt.Sprintf("node %s running this sandbox was lost; a surviving node holds the pool template snapshot, so the sandbox is being re-forked onto it (attempt %d of %d)", lostNode, reforks+1, maxRawForkdNodeLostReforks),
		})
		if err := g.Client.Status().Update(ctx, c); err != nil {
			logger.Error(err, "re-pend NodeLost claim for auto re-fork", "claim", c.Name, "node", lostNode)
			return
		}
		recordNodeLostRefork(lostNode)
		logger.Info("raw-forkd claim re-pending for auto re-fork after node loss", "claim", c.Name, "node", lostNode, "attempt", reforks+1)
		return
	}

	// Fail closed: no surviving snapshot holder, or the bounded retries are spent.
	now := metav1.Now()
	c.Status.Phase = v1.SandboxFailed
	c.Status.FinishedAt = &now
	message := fmt.Sprintf("node %s running this sandbox is no longer healthy or registered, and no surviving node holds the pool template snapshot to re-fork onto; recreate the claim once a snapshot holder is available", lostNode)
	if holderSurvives {
		message = fmt.Sprintf("node %s running this sandbox was lost; automatic re-fork onto a surviving snapshot holder was attempted %d times (the bound) without a durable placement, so the claim is failed; recreate the claim", lostNode, reforks)
	}
	setCondition(&c.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		LastTransitionTime: now,
		Reason:             "NodeLost",
		Message:            message,
	})
	if err := g.Client.Status().Update(ctx, c); err != nil {
		logger.Error(err, "mark claim NodeLost", "claim", c.Name, "node", lostNode)
		return
	}
	recordNodeLost(lostNode)
	logger.Info("claim transitioned to NodeLost", "claim", c.Name, "node", lostNode)
}

// snapshotHolderSurvives reports whether a healthy node still holds the template
// snapshot the claim's pool forks from, so the claim can be re-forked onto it.
// It is the placement-only decision (no KVM): it resolves the pool's template id
// (poolTemplateID) and asks the NodeRegistry whether any healthy node holds it.
// It returns false (fail closed) for a claim with no pool source (a fromSandbox
// or fromRevision sandbox is never auto-re-forked) or when the pool cannot be
// read, preserving the terminal NodeLost behavior for those.
func (g *GarbageCollector) snapshotHolderSurvives(ctx context.Context, c *v1.Sandbox) bool {
	if c.Spec.Source.PoolRef == nil || c.Spec.Source.PoolRef.Name == "" {
		return false
	}
	var pool v1.SandboxPool
	if err := g.Client.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: c.Spec.Source.PoolRef.Name}, &pool); err != nil {
		return false
	}
	return len(g.Registry.NodesWithTemplate(poolTemplateID(&pool))) > 0
}

// nodeLostReforkCount reads the durable per-claim auto re-fork count from the
// claim's annotation, defaulting to 0 when absent or unparseable.
func nodeLostReforkCount(c *v1.Sandbox) int {
	if v := c.Annotations[nodeLostReforkCountAnnotation]; v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// ttlFinished deletes claims in a terminal phase (Terminated or Failed) whose
// FinishedAt is older than the effective TTL: the claim's
// spec.ttlSecondsAfterFinished if set, else DefaultTTLSeconds. Deletion
// triggers the terminate finalizer, which is bounded and tolerant. A claim
// with no FinishedAt is skipped, and a claim already being deleted is left to
// its finalizer. A claim freshly stamped terminal earlier in this same pass has
// FinishedAt=now, so it is too young to delete here; SandboxForks have no
// FinishedAt today, so TTL of forks is a follow-up.
func (g *GarbageCollector) ttlFinished(ctx context.Context, logger logr.Logger, sandboxes []v1.Sandbox) {
	now := time.Now()
	for i := range sandboxes {
		c := &sandboxes[i]
		if !c.DeletionTimestamp.IsZero() {
			continue
		}
		if c.Status.Phase != v1.SandboxTerminated && c.Status.Phase != v1.SandboxFailed {
			continue
		}
		if c.Status.FinishedAt == nil {
			continue
		}
		ttl := g.DefaultTTLSeconds
		if sandboxTTLSecondsAfterFinished(c) != nil {
			ttl = *sandboxTTLSecondsAfterFinished(c)
		}
		if now.Sub(c.Status.FinishedAt.Time) < time.Duration(ttl)*time.Second {
			continue
		}
		if err := g.Client.Delete(ctx, c); err != nil {
			logger.Error(err, "ttl delete finished claim", "claim", c.Name)
			continue
		}
		logger.Info("ttl deleted finished claim", "claim", c.Name, "phase", c.Status.Phase)
	}
}

// listSandboxes calls forkd ListSandboxes on the node with a bounded timeout,
// returning nil on any error (the node will be revisited next pass).
func (g *GarbageCollector) listSandboxes(ctx context.Context, nodeName string) []*forkdpb.SandboxInfo {
	conn, err := g.Registry.GetConnection(nodeName)
	if err != nil {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := forkdpb.NewForkDaemonClient(conn).ListSandboxes(cctx, &forkdpb.ListSandboxesRequest{})
	if err != nil {
		return nil
	}
	return resp.Sandboxes
}

// listVolumes calls forkd ListVolumes on the node with a bounded timeout,
// returning nil on any error (the node will be revisited next pass). It mirrors
// listSandboxes for the volume-orphan sweep.
func (g *GarbageCollector) listVolumes(ctx context.Context, nodeName string) []*forkdpb.VolumeInfo {
	conn, err := g.Registry.GetConnection(nodeName)
	if err != nil {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := forkdpb.NewForkDaemonClient(conn).ListVolumes(cctx, &forkdpb.ListVolumesRequest{})
	if err != nil {
		return nil
	}
	return resp.Volumes
}
