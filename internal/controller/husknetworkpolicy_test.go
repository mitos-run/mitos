package controller

import (
	"testing"

	v1alpha1 "github.com/paperclipinc/mitos/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBuildHuskNetworkPolicyDefaultDeny(t *testing.T) {
	pool := &v1alpha1.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
	np := buildHuskNetworkPolicy(pool, nil)
	if np.Spec.PodSelector.MatchLabels[huskLabel] != "true" {
		t.Errorf("selector = %v, want mitos.run/husk=true", np.Spec.PodSelector.MatchLabels)
	}
	// PolicyTypes must include Egress for the default-deny egress posture.
	var hasEgress bool
	for _, pt := range np.Spec.PolicyTypes {
		if pt == "Egress" {
			hasEgress = true
		}
	}
	if !hasEgress {
		t.Error("policy must declare Egress policy type for default-deny egress")
	}
	// With no allows, exactly one egress rule: DNS to kube-dns (UDP/TCP 53).
	if len(np.Spec.Egress) != 1 {
		t.Fatalf("egress rules = %d, want 1 (DNS only)", len(np.Spec.Egress))
	}
	// Owner GC label so the NetworkPolicy is traceable to its pool.
	if np.Labels[huskPoolLabel] != "p" {
		t.Errorf("pool label = %v, want p", np.Labels)
	}
}

func TestBuildHuskNetworkPolicyAddsAllowDestinations(t *testing.T) {
	pool := &v1alpha1.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
	np := buildHuskNetworkPolicy(pool, []string{"10.0.0.5:5432"})
	// DNS rule + one allow rule.
	if len(np.Spec.Egress) != 2 {
		t.Fatalf("egress rules = %d, want 2 (DNS + one allow)", len(np.Spec.Egress))
	}
	allowRule := np.Spec.Egress[1]
	if len(allowRule.To) != 1 || allowRule.To[0].IPBlock == nil || allowRule.To[0].IPBlock.CIDR != "10.0.0.5/32" {
		t.Errorf("allow peer = %+v, want IPBlock 10.0.0.5/32", allowRule.To)
	}
}

// TestBuildHuskNetworkPolicySkipsNameAllow asserts a name:port entry produces NO
// extra egress rule (NetworkPolicy has no name-based egress; the in-pod DNS
// proxy enforces names). Only the DNS rule remains.
func TestBuildHuskNetworkPolicySkipsNameAllow(t *testing.T) {
	pool := &v1alpha1.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
	np := buildHuskNetworkPolicy(pool, []string{"api.example.com:443"})
	if len(np.Spec.Egress) != 1 {
		t.Fatalf("egress rules = %d, want 1 (DNS only; name allow not expressible)", len(np.Spec.Egress))
	}
}
