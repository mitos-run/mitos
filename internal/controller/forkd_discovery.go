package controller

import (
	"context"
	"fmt"
	"time"

	forkdpb "github.com/paperclipinc/sandbox/proto/forkd"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const forkdComponentLabel = "app.kubernetes.io/component"

// ForkdDiscovery keeps the NodeRegistry in sync with running forkd pods.
// It lists labeled pods periodically, registers them, refreshes capacity via
// GetCapacity, and prunes nodes that stop heartbeating.
type ForkdDiscovery struct {
	Client    client.Client
	Registry  *NodeRegistry
	Namespace string        // namespace forkd runs in, e.g. "agent-run"
	Interval  time.Duration // default 15s
	GRPCPort  int           // default 9090
	HTTPPort  int           // default 9091
}

func (d *ForkdDiscovery) Start(ctx context.Context) error {
	if d.Interval == 0 {
		d.Interval = 15 * time.Second
	}
	if d.GRPCPort == 0 {
		d.GRPCPort = 9090
	}
	if d.HTTPPort == 0 {
		d.HTTPPort = 9091
	}
	ticker := time.NewTicker(d.Interval)
	defer ticker.Stop()
	for {
		d.sync(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (d *ForkdDiscovery) sync(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("forkd-discovery")

	var pods corev1.PodList
	if err := d.Client.List(ctx, &pods,
		client.InNamespace(d.Namespace),
		client.MatchingLabels{forkdComponentLabel: "forkd"},
	); err != nil {
		logger.Error(err, "list forkd pods")
		return
	}

	d.syncPods(ctx, pods.Items)

	d.Registry.PruneStale(2 * time.Minute)
}

// syncPods registers every running forkd pod and refreshes its capacity.
func (d *ForkdDiscovery) syncPods(ctx context.Context, pods []corev1.Pod) {
	for _, pod := range pods {
		info, ok := NodeInfoFromPod(pod, d.GRPCPort, d.HTTPPort)
		if !ok {
			continue
		}
		// Register first so GetConnection can dial; refresh fills capacity,
		// then re-register stores the refreshed fields (Register is an upsert).
		d.Registry.Register(info)
		d.refreshCapacity(ctx, info)
		d.Registry.Register(info)
	}
}

// refreshCapacity fills template/capacity fields via forkd's GetCapacity.
// Registration still happens when the call fails — SelectNode's health window
// and the next sync handle flapping pods.
func (d *ForkdDiscovery) refreshCapacity(ctx context.Context, info *NodeInfo) {
	conn, err := d.Registry.GetConnection(info.Name)
	if err != nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := forkdpb.NewForkDaemonClient(conn).GetCapacity(cctx, &forkdpb.GetCapacityRequest{})
	if err != nil {
		return
	}
	info.ActiveSandboxes = resp.ActiveSandboxes
	info.MaxSandboxes = resp.MaxSandboxes
	info.MemoryTotal = resp.MemoryTotalBytes
	info.MemoryUsed = resp.MemoryUsedBytes
	info.TemplateIDs = resp.TemplateIds
	info.SnapshotIDs = resp.SnapshotIds
}

// NodeInfoFromPod maps a forkd pod to a NodeInfo. Returns false when the pod
// is not running, has no IP, or has no node assignment yet.
func NodeInfoFromPod(pod corev1.Pod, grpcPort, httpPort int) (*NodeInfo, bool) {
	if pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" || pod.Spec.NodeName == "" {
		return nil, false
	}
	return &NodeInfo{
		Name:         pod.Spec.NodeName,
		Endpoint:     fmt.Sprintf("%s:%d", pod.Status.PodIP, grpcPort),
		HTTPEndpoint: fmt.Sprintf("%s:%d", pod.Status.PodIP, httpPort),
	}, true
}
