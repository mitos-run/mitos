package controller

import (
	"context"
	"fmt"
	"net"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	"github.com/paperclipinc/mitos/internal/netconf"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// huskNetworkPolicyName is the per-pool NetworkPolicy name. It selects this
// pool's husk pods (mitos.run/husk=true) for default-deny egress.
func huskNetworkPolicyName(pool string) string { return pool + "-husk-egress" }

// buildHuskNetworkPolicy builds the best-effort Kubernetes NetworkPolicy for a
// pool's husk pods: default-deny egress (a non-empty Egress policy type with a
// limited rule set denies everything else), plus an allow for cluster DNS
// (UDP/TCP 53) and one egress rule per enforceable IP:port in the template
// allowlist.
//
// HONEST CNI CAVEAT: a NetworkPolicy only enforces if the cluster CNI
// implements it (Calico, Cilium, etc.). On a CNI without NetworkPolicy support
// this object is inert. It is defense in depth ONLY; the in-pod nftables filter
// the husk-stub programs is the guarantee that holds regardless of CNI. This is
// documented in docs/threat-model.md.
//
// Name entries in the allowlist are NOT expressible as NetworkPolicy egress
// peers (NetworkPolicy has no name-based egress), so only IP:port allows are
// added here; name-based egress is enforced solely by the in-pod DNS proxy.
func buildHuskNetworkPolicy(pool *v1alpha1.SandboxPool, allow []string) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	dnsPort := intstr.FromInt(53)

	egress := []networkingv1.NetworkPolicyEgressRule{
		{
			// Cluster DNS so the guest can resolve through the node resolver; the
			// in-pod DNS proxy is the controlled resolver, but the pod's own DNS
			// (and the proxy's upstream) needs port 53 egress.
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &udp, Port: &dnsPort},
				{Protocol: &tcp, Port: &dnsPort},
			},
		},
	}

	// One egress rule per enforceable IP:port allow. Malformed/name entries are
	// skipped (SplitAllowList returns names separately); a parse error yields no
	// allow rules (the default-deny still applies, fail-closed).
	if enforceable, _, err := netconf.SplitAllowList(allow); err == nil {
		for _, hp := range enforceable {
			port := intstr.FromInt(hp.Port)
			egress = append(egress, networkingv1.NetworkPolicyEgressRule{
				To: []networkingv1.NetworkPolicyPeer{{
					IPBlock: &networkingv1.IPBlock{CIDR: hp.IP.String() + hostMask(hp.IP)},
				}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
			})
		}
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      huskNetworkPolicyName(pool.Name),
			Namespace: pool.Namespace,
			Labels:    map[string]string{huskPoolLabel: pool.Name},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{huskLabel: "true"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      egress,
		},
	}
}

// hostMask returns the single-host CIDR suffix for an IP (/32 for v4, /128 for
// v6). The allowlist is IPv4-only today (SplitAllowList enforces it), so this is
// /32 in practice, but the v6 branch keeps it correct if that changes.
func hostMask(ip net.IP) string {
	if ip.To4() != nil {
		return "/32"
	}
	return "/128"
}

// ensureHuskNetworkPolicy creates or updates the pool's husk NetworkPolicy,
// owner-referenced to the pool for GC. Best effort: a failure is returned so the
// reconcile retries, but the CALLER does NOT block husk pod creation on it,
// because the in-pod nftables filter (not this object) is the egress guarantee.
func (r *SandboxPoolReconciler) ensureHuskNetworkPolicy(ctx context.Context, pool *v1alpha1.SandboxPool, allow []string) error {
	desired := buildHuskNetworkPolicy(pool, allow)

	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      desired.Name,
			Namespace: desired.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		np.Labels = desired.Labels
		np.Spec = desired.Spec
		// Owner-ref to the pool so the NetworkPolicy is garbage-collected with the
		// pool. SetControllerReference is idempotent for the same owner.
		if existing := metav1.GetControllerOf(np); existing == nil {
			if serr := controllerutil.SetControllerReference(pool, np, r.Scheme()); serr != nil {
				return fmt.Errorf("set owner on husk network policy %s: %w", np.Name, serr)
			}
		}
		return nil
	})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("ensure husk network policy for pool %s: %w", pool.Name, err)
	}
	return nil
}
