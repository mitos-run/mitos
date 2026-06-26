package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/tenant"
)

// TestBuildOrgDefaultDenyPolicy asserts the per-org NetworkPolicy is
// default-deny in BOTH directions with a single DNS egress allow, applies to
// every pod in the namespace, and carries the org label.
func TestBuildOrgDefaultDenyPolicy(t *testing.T) {
	org := &v1.Org{}
	org.Name = "acme"
	np := buildOrgDefaultDenyPolicy(org, tenant.NamespaceForOrg("acme"))

	if np.Name != orgDenyPolicyName {
		t.Fatalf("name = %q, want %q", np.Name, orgDenyPolicyName)
	}
	if np.Namespace != "mitos-org-acme" {
		t.Fatalf("namespace = %q, want mitos-org-acme", np.Namespace)
	}
	if got := np.Labels[tenant.OrgLabelKey]; got != "acme" {
		t.Fatalf("org label = %q, want acme", got)
	}

	// Empty PodSelector: applies to every pod in the namespace.
	if len(np.Spec.PodSelector.MatchLabels) != 0 || len(np.Spec.PodSelector.MatchExpressions) != 0 {
		t.Fatalf("PodSelector is not empty: %+v", np.Spec.PodSelector)
	}

	// Both Ingress and Egress policy types must be declared (deny applies only to
	// declared directions).
	wantTypes := map[networkingv1.PolicyType]bool{
		networkingv1.PolicyTypeIngress: false,
		networkingv1.PolicyTypeEgress:  false,
	}
	for _, pt := range np.Spec.PolicyTypes {
		if _, ok := wantTypes[pt]; ok {
			wantTypes[pt] = true
		}
	}
	for pt, seen := range wantTypes {
		if !seen {
			t.Fatalf("policy type %s missing; both Ingress and Egress must be declared for default-deny both directions", pt)
		}
	}

	// No ingress rules: deny all ingress.
	if len(np.Spec.Ingress) != 0 {
		t.Fatalf("Ingress rules = %d, want 0 (deny all ingress)", len(np.Spec.Ingress))
	}

	// Exactly one egress rule, allowing only DNS on UDP 53 and TCP 53, with NO
	// peers (a peerless port-only rule scopes the allow to those ports only).
	if len(np.Spec.Egress) != 1 {
		t.Fatalf("Egress rules = %d, want 1 (DNS allow only)", len(np.Spec.Egress))
	}
	dns := np.Spec.Egress[0]
	if len(dns.To) != 0 {
		t.Fatalf("DNS egress rule has peers %+v; want port-only (no peer narrowing beyond port 53)", dns.To)
	}
	gotUDP, gotTCP := false, false
	for _, p := range dns.Ports {
		if p.Port == nil || p.Port.IntValue() != 53 {
			t.Fatalf("egress port = %v, want 53", p.Port)
		}
		switch *p.Protocol {
		case corev1.ProtocolUDP:
			gotUDP = true
		case corev1.ProtocolTCP:
			gotTCP = true
		}
	}
	if !gotUDP || !gotTCP {
		t.Fatalf("DNS allow must cover UDP and TCP 53; got udp=%v tcp=%v", gotUDP, gotTCP)
	}
}

// TestBuildOrgResourceQuotaDefaults asserts the quota uses the controller
// defaults when the Org sets no override.
func TestBuildOrgResourceQuotaDefaults(t *testing.T) {
	org := &v1.Org{}
	org.Name = "acme"
	defCPU := resource.MustParse("16")
	defMem := resource.MustParse("32Gi")

	rq := buildOrgResourceQuota(org, "mitos-org-acme", 25, defCPU, defMem)

	if got := rq.Spec.Hard[corev1.ResourcePods]; got.Value() != 25 {
		t.Fatalf("pods = %v, want 25", got.Value())
	}
	if got := rq.Spec.Hard["count/sandboxes.mitos.run"]; got.Value() != 25 {
		t.Fatalf("count/sandboxes = %v, want 25", got.Value())
	}
	if got := rq.Spec.Hard[corev1.ResourceLimitsCPU]; got.Cmp(defCPU) != 0 {
		t.Fatalf("limits.cpu = %v, want %v", got.String(), defCPU.String())
	}
	if got := rq.Spec.Hard[corev1.ResourceLimitsMemory]; got.Cmp(defMem) != 0 {
		t.Fatalf("limits.memory = %v, want %v", got.String(), defMem.String())
	}
	if got := rq.Labels[tenant.OrgLabelKey]; got != "acme" {
		t.Fatalf("org label = %q, want acme", got)
	}
}

// TestBuildOrgResourceQuotaOverride asserts the Org's spec.quota override wins
// per field, and an unset field falls back to the controller default.
func TestBuildOrgResourceQuotaOverride(t *testing.T) {
	defCPU := resource.MustParse("16")
	defMem := resource.MustParse("32Gi")

	org := &v1.Org{}
	org.Name = "bigco"
	org.Spec.Quota = &v1.OrgQuota{
		MaxSandboxes: 200,
		CPU:          resource.MustParse("128"),
		// MaxPods and Memory unset: MaxPods aligns to MaxSandboxes, Memory falls
		// back to the default.
	}

	rq := buildOrgResourceQuota(org, "mitos-org-bigco", 25, defCPU, defMem)

	if got := rq.Spec.Hard["count/sandboxes.mitos.run"]; got.Value() != 200 {
		t.Fatalf("count/sandboxes = %v, want 200 (override)", got.Value())
	}
	if got := rq.Spec.Hard[corev1.ResourcePods]; got.Value() != 200 {
		t.Fatalf("pods = %v, want 200 (aligned to MaxSandboxes override)", got.Value())
	}
	wantCPU := resource.MustParse("128")
	if got := rq.Spec.Hard[corev1.ResourceLimitsCPU]; got.Cmp(wantCPU) != 0 {
		t.Fatalf("limits.cpu = %v, want 128 (override)", got.String())
	}
	if got := rq.Spec.Hard[corev1.ResourceLimitsMemory]; got.Cmp(defMem) != 0 {
		t.Fatalf("limits.memory = %v, want %v (default fallback)", got.String(), defMem.String())
	}
}

// TestBuildOrgResourceQuotaExplicitMaxPods asserts an explicit MaxPods override
// is honored independently of MaxSandboxes.
func TestBuildOrgResourceQuotaExplicitMaxPods(t *testing.T) {
	org := &v1.Org{}
	org.Name = "co"
	org.Spec.Quota = &v1.OrgQuota{MaxSandboxes: 10, MaxPods: 40}

	rq := buildOrgResourceQuota(org, "mitos-org-co", 25, resource.MustParse("16"), resource.MustParse("32Gi"))

	if got := rq.Spec.Hard["count/sandboxes.mitos.run"]; got.Value() != 10 {
		t.Fatalf("count/sandboxes = %v, want 10", got.Value())
	}
	if got := rq.Spec.Hard[corev1.ResourcePods]; got.Value() != 40 {
		t.Fatalf("pods = %v, want 40 (explicit MaxPods)", got.Value())
	}
}

// TestOrgNamespaceLabels asserts the namespace carries the org label plus the
// three PSA privileged labels.
func TestOrgNamespaceLabels(t *testing.T) {
	l := orgNamespaceLabels("acme")
	if l[tenant.OrgLabelKey] != "acme" {
		t.Fatalf("org label = %q, want acme", l[tenant.OrgLabelKey])
	}
	for _, k := range []string{
		"pod-security.kubernetes.io/enforce",
		"pod-security.kubernetes.io/audit",
		"pod-security.kubernetes.io/warn",
	} {
		if l[k] != "privileged" {
			t.Fatalf("%s = %q, want privileged", k, l[k])
		}
	}
}
