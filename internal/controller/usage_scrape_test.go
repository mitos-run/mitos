package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"mitos.run/mitos/internal/tenant"
	"mitos.run/mitos/internal/usage"
)

// TestRegistryNodeListerExposesHTTPEndpoints asserts the NodeLister adapter yields
// each registered node's name and HTTP endpoint (the forkd /v1/metering target),
// and skips a node with no HTTP endpoint. It exposes only the name and HTTP
// endpoint, never the gRPC/CAS endpoints or capacity, so the usage package sees
// the minimal surface and there is no import cycle.
func TestRegistryNodeListerExposesHTTPEndpoints(t *testing.T) {
	reg := NewNodeRegistry()
	reg.Register(&NodeInfo{Name: "n1", Endpoint: "10.0.0.1:9090", HTTPEndpoint: "10.0.0.1:9091", LastHeartbeat: time.Now()})
	reg.Register(&NodeInfo{Name: "n2", Endpoint: "10.0.0.2:9090", HTTPEndpoint: "10.0.0.2:9091", LastHeartbeat: time.Now()})
	// A node with no HTTP endpoint must be skipped (nothing to scrape).
	reg.Register(&NodeInfo{Name: "n3", Endpoint: "10.0.0.3:9090", LastHeartbeat: time.Now()})

	lister := RegistryNodeLister{Registry: reg}
	got := lister.ListNodeEndpoints()

	byName := map[string]string{}
	for _, e := range got {
		byName[e.Name] = e.HTTPEndpoint
	}
	if len(byName) != 2 {
		t.Fatalf("want 2 endpoints (n3 has no HTTP endpoint), got %d: %v", len(byName), byName)
	}
	if byName["n1"] != "10.0.0.1:9091" {
		t.Errorf("n1 endpoint = %q, want 10.0.0.1:9091", byName["n1"])
	}
	if byName["n2"] != "10.0.0.2:9091" {
		t.Errorf("n2 endpoint = %q, want 10.0.0.2:9091", byName["n2"])
	}
	if _, ok := byName["n3"]; ok {
		t.Errorf("n3 (no HTTP endpoint) must be skipped, got %q", byName["n3"])
	}
}

// listCountingClient wraps a client.Client and counts List calls so a test can
// prove the husk pods are listed ONCE per cycle, not once per sandbox.
type listCountingClient struct {
	client.Client
	lists int
}

func (c *listCountingClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	c.lists++
	return c.Client.List(ctx, list, opts...)
}

// huskPod builds a husk pod object with the given name, org label, and namespace.
func huskPod(name, org, ns string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{huskLabel: "true", tenant.OrgLabelKey: org},
		},
	}
}

// TestPodLabelLookupListsOncePerCycle is the MINOR-2 fix proof: Refresh lists the
// husk pods exactly ONCE and every LabelsForSandbox resolves from the cached
// name -> labels snapshot, instead of a cluster-wide List per sandbox (the O(n^2)
// blow-up at fleet scale). Correctness is preserved: the trusted org label resolves
// for each sandbox, and an unknown sandbox is unattributed.
func TestPodLabelLookupListsOncePerCycle(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	base := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			huskPod("sb-acme", "acme", "mitos-org-acme"),
			huskPod("sb-globex", "globex", "mitos-org-globex"),
		).
		Build()
	cc := &listCountingClient{Client: base}
	lookup := &PodLabelLookup{Client: cc}

	// One cycle: refresh once, then resolve every sandbox from the snapshot.
	lookup.Refresh()
	for _, sb := range []string{"sb-acme", "sb-globex", "sb-acme", "sb-globex", "sb-unknown"} {
		ls, ok := lookup.LabelsForSandbox(sb)
		switch sb {
		case "sb-acme":
			if !ok || ls["mitos.run/org"] != "acme" {
				t.Errorf("LabelsForSandbox(%s) = (%v, %t), want acme", sb, ls, ok)
			}
		case "sb-globex":
			if !ok || ls["mitos.run/org"] != "globex" {
				t.Errorf("LabelsForSandbox(%s) = (%v, %t), want globex", sb, ls, ok)
			}
		case "sb-unknown":
			if ok {
				t.Errorf("LabelsForSandbox(%s) should be absent, got %v", sb, ls)
			}
		}
	}

	if cc.lists != 1 {
		t.Fatalf("husk pods listed %d times for one cycle, want exactly 1 (the O(n^2) per-sandbox list is the bug)", cc.lists)
	}
}

// scrapablePod builds a Running, claimed, org-labeled husk pod with a PodIP: the
// exact shape HuskPodScrapeLister must return for the usage.HuskSource to scrape.
// claim is the mitos.run/claim label value: the claiming Sandbox's NAME, which is
// the API-visible sandbox id (the hosted gateway names the Sandbox sb-<hex> and
// returns that name as the customer-facing id).
func scrapablePod(name, claim, org, podIP string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "mitos-org-" + org,
			Labels: map[string]string{
				huskLabel:       "true",
				huskClaimLabel:  claim,
				"mitos.run/org": org,
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: podIP},
	}
}

// TestHuskPodScrapeListerSelectsClaimedOrgLabeledPods asserts the lister returns
// each Running, claimed, org-labeled husk pod's vm-id (pod name), API-visible
// sandbox id (the mitos.run/claim label value, the claiming Sandbox's name; issue
// #663), trusted org, and podIP:huskSandboxPort endpoint, and OMITS pods that are
// unclaimed, unattributed (no org label), not Running, or have no PodIP: only a
// billable claimed pod is scraped, and org comes from the trusted label, never
// client input.
func TestHuskPodScrapeListerSelectsClaimedOrgLabeledPods(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	// Unclaimed warm pod (no claim label): must be omitted.
	warm := huskPod("python-husk-warm", "acme", "mitos-org-acme")
	// Claimed but no org label (self-host single-tenant): must be omitted.
	noOrg := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "python-husk-selfhost",
			Namespace: "default",
			Labels:    map[string]string{huskLabel: "true", huskClaimLabel: "sb-selfhost"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.9"},
	}
	// Claimed + org labeled but no PodIP yet: must be omitted.
	noIP := scrapablePod("python-husk-noip", "sb-noip", "acme", "")

	cl := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			// The hosted shape from issue #663: the pod NAME is the husk pod name
			// (the vm-id), while the claim label carries the sb-... id the
			// customer saw. Both must surface, in the right fields.
			scrapablePod("python-husk-2blsp", "sb-82813f5c", "acme", "10.0.0.1"),
			scrapablePod("python-husk-9w8k5", "sb-45eac994", "globex", "10.0.0.2"),
			warm, noOrg, noIP,
		).
		Build()

	lister := &HuskPodScrapeLister{Client: cl}
	got, err := lister.ListHuskPods(context.Background())
	if err != nil {
		t.Fatalf("ListHuskPods: %v", err)
	}

	byVM := map[string]usage.HuskPod{}
	for _, p := range got {
		byVM[p.VMID] = p
	}
	if len(byVM) != 2 {
		t.Fatalf("want 2 scrapable pods, got %d: %+v", len(byVM), got)
	}
	if p := byVM["python-husk-2blsp"]; p.APIID != "sb-82813f5c" || p.OrgID != "acme" || p.Endpoint != "10.0.0.1:9091" {
		t.Errorf("python-husk-2blsp = %+v, want api id sb-82813f5c org acme endpoint 10.0.0.1:9091", p)
	}
	if p := byVM["python-husk-9w8k5"]; p.APIID != "sb-45eac994" || p.OrgID != "globex" || p.Endpoint != "10.0.0.2:9091" {
		t.Errorf("python-husk-9w8k5 = %+v, want api id sb-45eac994 org globex endpoint 10.0.0.2:9091", p)
	}
	for _, absent := range []string{"python-husk-warm", "python-husk-selfhost", "python-husk-noip"} {
		if _, ok := byVM[absent]; ok {
			t.Errorf("%s must be omitted (unclaimed / no org / no PodIP)", absent)
		}
	}
}
