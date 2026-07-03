package controller

import (
	"context"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"mitos.run/mitos/internal/tenant"
	"mitos.run/mitos/internal/usage"
)

// This file is the controller-side wiring for the live usage metering scraper
// (issue #164): the small adapters that let internal/usage scrape the forkd
// fleet WITHOUT importing internal/controller (which would be an import cycle:
// the controller wires the usage collector). internal/usage defines the narrow
// usage.NodeLister and usage.LabelLookup interfaces; these concrete types satisfy
// them over the controller's NodeRegistry and cached client.

// RegistryNodeLister adapts the controller's NodeRegistry to usage.NodeLister so
// the live SampleSource can iterate forkd nodes and scrape each one's
// GET /v1/metering. It exposes ONLY the node name and HTTP endpoint, never the
// gRPC/CAS endpoints, TLS config, or capacity, so the usage package depends on
// the minimal surface.
type RegistryNodeLister struct {
	Registry *NodeRegistry
}

// ListNodeEndpoints returns the current forkd nodes' names and HTTP endpoints. A
// node without an HTTP endpoint is skipped here (the scraper would skip it
// anyway). The node name is a hostname, never a secret.
func (l RegistryNodeLister) ListNodeEndpoints() []usage.NodeEndpoint {
	nodes := l.Registry.ListNodes()
	out := make([]usage.NodeEndpoint, 0, len(nodes))
	for _, n := range nodes {
		if n.HTTPEndpoint == "" {
			continue
		}
		out = append(out, usage.NodeEndpoint{Name: n.Name, HTTPEndpoint: n.HTTPEndpoint})
	}
	return out
}

// PodLabelLookup adapts the controller's cached client to usage.LabelLookup so the
// live OrgResolver (usage.LabelOrgResolver) can read the TRUSTED mitos.run/org
// label off a sandbox's backing husk pod. The forkd sandbox id equals the husk
// pod name (the controller sets the husk pod's --vm-id to its POD_NAME), and a
// husk pod lives in its tenant's per-org namespace (mitos-org-<id>), so the
// org label and the namespace already agree; the lookup finds the pod by name
// across namespaces and returns its labels.
//
// SECURITY: the only label that flows to the resolver is the controller's OWN
// stamped label set, derived from the trusted per-org namespace, never from
// client input. The resolver reads only mitos.run/org from it.
//
// PER-CYCLE SNAPSHOT: it lists husk pods exactly ONCE per scrape cycle and resolves
// every sandbox from the cached name -> labels map, instead of a cluster-wide List
// per sandbox (an O(n^2) blow-up at fleet scale). Refresh (called by the live
// source at the start of each Collect) rebuilds the snapshot; LabelsForSandbox is
// then a map read.
type PodLabelLookup struct {
	Client client.Client

	mu     sync.RWMutex
	byName map[string]map[string]string
	primed bool
}

// Refresh lists the husk pods (carrying mitos.run/husk) ONCE and caches the
// name -> labels map for the cycle about to run. On a List error it installs an
// EMPTY snapshot, so every sandbox resolves as unattributed for the cycle rather
// than from a stale map: a transient miss must never bill a sandbox to the wrong
// org. It implements usage.RefreshableLookup.
func (l *PodLabelLookup) Refresh() {
	byName := map[string]map[string]string{}
	var pods corev1.PodList
	if err := l.Client.List(context.Background(), &pods, client.MatchingLabels{huskLabel: "true"}); err == nil {
		for i := range pods.Items {
			byName[pods.Items[i].Name] = pods.Items[i].Labels
		}
	}
	l.mu.Lock()
	l.byName = byName
	l.primed = true
	l.mu.Unlock()
}

// LabelsForSandbox returns the labels on the husk pod backing sandboxID (pod name
// == sandbox id) from the per-cycle snapshot, and whether the pod was found. If no
// snapshot has been taken yet (Refresh not called), it primes one so a direct
// call still works. A sandbox whose pod the snapshot does not hold (just-created,
// already-gone, or a failed list) returns (nil, false), which the resolver treats
// as unattributed for the cycle.
func (l *PodLabelLookup) LabelsForSandbox(sandboxID string) (map[string]string, bool) {
	l.mu.RLock()
	primed := l.primed
	l.mu.RUnlock()
	if !primed {
		l.Refresh()
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	labels, ok := l.byName[sandboxID]
	return labels, ok
}

// HuskPodScrapeLister adapts the controller's cached client to usage.HuskPodLister
// so the live usage.HuskSource (issue #613) can scrape each CLAIMED husk pod's
// in-pod GET /v1/metering. In production every sandbox VM runs inside its own husk
// pod, which forkd's engine never tracks, so the forkd node source reports nothing
// for it; this lister points the husk source at the pods themselves.
//
// It lists mitos.run/husk pods that are Running, carry a claim label
// (mitos.run/claim, so they are actually claimed, not warm), have a PodIP, and
// carry a NON-EMPTY trusted mitos.run/org label. It returns each pod's vm-id (the
// pod name, which the pod reports as its single metering sample id), its org (the
// trusted label), and its podIP:huskSandboxPort endpoint.
//
// SECURITY: the org is the controller's OWN stamped label, derived from the
// trusted per-org namespace (see buildHuskPod), NEVER client input; the pod is
// untrusted for org. A pod with no org label (self-host single-tenant) is omitted,
// so it stays out of the billable samples rather than being forced into an org.
type HuskPodScrapeLister struct {
	Client client.Client
}

// ListHuskPods lists the claimed, org-labeled, Running husk pods cluster-wide and
// returns their vm-id, org, and podIP:port endpoint. A List error is returned to
// the caller so the collection cycle fails loudly and retries; swallowing it
// would make an API or RBAC fault indistinguishable from an empty fleet and
// silently zero the bill. Pod names and IPs are not secrets.
func (l *HuskPodScrapeLister) ListHuskPods(ctx context.Context) ([]usage.HuskPod, error) {
	var pods corev1.PodList
	if err := l.Client.List(ctx, &pods,
		client.MatchingLabels{huskLabel: "true"},
		client.HasLabels{huskClaimLabel},
	); err != nil {
		return nil, fmt.Errorf("list husk pods: %w", err)
	}
	out := make([]usage.HuskPod, 0, len(pods.Items))
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase != corev1.PodRunning || p.Status.PodIP == "" {
			continue
		}
		org := p.Labels[tenant.OrgLabelKey]
		if org == "" {
			continue
		}
		out = append(out, usage.HuskPod{
			VMID:     p.Name,
			OrgID:    org,
			Endpoint: fmt.Sprintf("%s:%d", p.Status.PodIP, huskSandboxPort),
		})
	}
	return out, nil
}
