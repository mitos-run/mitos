package controller

import (
	"context"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
