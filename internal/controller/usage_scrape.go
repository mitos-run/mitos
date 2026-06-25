package controller

import (
	"context"

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
type PodLabelLookup struct {
	Client client.Client
}

// LabelsForSandbox returns the labels on the husk pod backing sandboxID (pod name
// == sandbox id), and whether the pod was found. It lists husk pods cluster-wide
// (the husk pods carry mitos.run/husk) and matches by name, so it finds the pod
// in whichever per-org namespace the tenant's workloads live in. A sandbox whose
// pod the cache has not observed (just-created or already-gone), or a List error,
// returns (nil, false), which the resolver treats as unattributed: a transient
// miss must never bill the sandbox to the wrong org, only leave it unattributed
// for that cycle.
func (l PodLabelLookup) LabelsForSandbox(sandboxID string) (map[string]string, bool) {
	var pods corev1.PodList
	if err := l.Client.List(context.Background(), &pods, client.MatchingLabels{huskLabel: "true"}); err != nil {
		return nil, false
	}
	for i := range pods.Items {
		if pods.Items[i].Name == sandboxID {
			return pods.Items[i].Labels, true
		}
	}
	return nil, false
}
