// Package clusternodes is the real console.NodeSource: a read-only inventory
// of the cluster's Kubernetes nodes for the instance-operator plane's GET
// /console/admin/nodes. It carries no per-org data (nodes are a
// deployment-wide resource, never namespaced), so unlike every other
// cluster-* adapter in this directory there is no org scoping to enforce
// here; the endpoint's own authorization (isInstanceAdmin) is the only gate.
//
// KVM and Dedicated read the SAME conventions the Helm chart wires
// (deploy/charts/mitos/values.yaml): mitos.run/kvm is a node LABEL (the
// nodeSelector forkd's DaemonSet uses to land on nodes with /dev/kvm),
// mitos.run/dedicated is a taint key (the toleration bare-metal KVM workers
// carry). Dedicated additionally accepts an equivalent LABEL for deployments
// that also stamp one, but the taint is the convention this repo actually
// ships.
package clusternodes

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"mitos.run/mitos/internal/saas/console"
)

const (
	kvmNodeLabel   = "mitos.run/kvm"
	dedicatedTaint = "mitos.run/dedicated"
)

// Source implements console.NodeSource against the Kubernetes API.
type Source struct {
	c client.Client
}

// New builds the cluster-backed node source.
func New(c client.Client) *Source {
	return &Source{c: c}
}

// Nodes lists every Node in the cluster.
func (s *Source) Nodes(ctx context.Context) ([]console.NodeView, error) {
	var list corev1.NodeList
	if err := s.c.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	out := make([]console.NodeView, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, viewOf(&list.Items[i]))
	}
	return out, nil
}

// viewOf maps a corev1.Node to the console view.
func viewOf(n *corev1.Node) console.NodeView {
	ready := false
	for _, cond := range n.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			ready = cond.Status == corev1.ConditionTrue
			break
		}
	}
	dedicated := n.Labels[dedicatedTaint] == "true"
	for _, taint := range n.Spec.Taints {
		if taint.Key == dedicatedTaint {
			dedicated = true
			break
		}
	}
	cpu := n.Status.Allocatable[corev1.ResourceCPU]
	mem := n.Status.Allocatable[corev1.ResourceMemory]
	return console.NodeView{
		Name:           n.Name,
		Ready:          ready,
		KVM:            n.Labels[kvmNodeLabel] == "true",
		Dedicated:      dedicated,
		AllocatableCPU: cpu.String(),
		AllocatableMem: mem.String(),
	}
}
