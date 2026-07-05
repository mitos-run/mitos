package charttest

import (
	"strings"
	"testing"
)

// The control-plane egress lockdown (issue #704) is an opt-in profile: a
// CiliumNetworkPolicy that flips controller/console/gateway to default-deny
// egress with every legitimate flow enumerated, plus a console-only SMTP
// policy DERIVED from console.onboarding.smtp so the network allow can never
// drift from the config that needs it (the drift that broke hosted signups:
// SMTP env landed, egress did not, every external signup timed out).

// TestControlPlaneNetpolAbsentByDefault asserts the lockdown is opt-in: a
// default install renders neither policy.
func TestControlPlaneNetpolAbsentByDefault(t *testing.T) {
	out := render(t)
	for _, banned := range []string{
		"mitos-control-plane-egress",
		"mitos-console-smtp-egress",
	} {
		if strings.Contains(out, banned) {
			t.Errorf("default render contains %q; the egress lockdown must be opt-in", banned)
		}
	}
}

// TestControlPlaneNetpolBaseline asserts the enabled profile renders the
// shared control-plane policy with the four baseline flows: DNS with L7
// visibility (required for toFQDNs), the apiserver entities, intra-namespace,
// and nothing else (no SMTP rule without smtp.host).
func TestControlPlaneNetpolBaseline(t *testing.T) {
	out := render(t, "networkPolicy.controlPlane.enabled=true")
	pol := section(t, out, "kind: CiliumNetworkPolicy", "mitos-control-plane-egress")
	for _, want := range []string{
		"controller",
		"console",
		"gateway",
		"k8s-app: kube-dns",
		"matchPattern: \"*\"",
		"kube-apiserver",
		"remote-node",
		"k8s:io.kubernetes.pod.namespace: mitos",
	} {
		if !strings.Contains(pol, want) {
			t.Errorf("control-plane egress policy missing %q:\n%s", want, pol)
		}
	}
	if strings.Contains(pol, "toFQDNs") {
		t.Errorf("baseline policy must not carry FQDN egress without smtp.host:\n%s", pol)
	}
	if strings.Contains(out, "mitos-console-smtp-egress") {
		t.Error("SMTP policy rendered without console.onboarding.smtp.host set")
	}
}

// TestControlPlaneNetpolSMTPDerived asserts the console-only SMTP policy is
// derived from console.onboarding.smtp host+port, scoped to the console
// component alone (least privilege: controller and gateway gain no mail-relay
// egress).
func TestControlPlaneNetpolSMTPDerived(t *testing.T) {
	out := render(t,
		"networkPolicy.controlPlane.enabled=true",
		"console.onboarding.smtp.host=smtp.example.com",
		"console.onboarding.smtp.port=2587",
		"console.onboarding.smtp.from=noreply@example.com",
	)
	pol := section(t, out, "kind: CiliumNetworkPolicy", "mitos-console-smtp-egress")
	for _, want := range []string{
		"app.kubernetes.io/component: console",
		"matchName: smtp.example.com",
		"port: \"2587\"",
	} {
		if !strings.Contains(pol, want) {
			t.Errorf("console SMTP egress policy missing %q:\n%s", want, pol)
		}
	}
	for _, banned := range []string{"controller", "gateway"} {
		if strings.Contains(pol, "app.kubernetes.io/component: "+banned) {
			t.Errorf("SMTP egress policy must select only the console, found %s:\n%s", banned, pol)
		}
	}
}

// TestControlPlaneNetpolSMTPDefaultPort asserts the SMTP rule falls back to
// the chart's default submission port when smtp.port is not set.
func TestControlPlaneNetpolSMTPDefaultPort(t *testing.T) {
	out := render(t,
		"networkPolicy.controlPlane.enabled=true",
		"console.onboarding.smtp.host=smtp.example.com",
	)
	pol := section(t, out, "kind: CiliumNetworkPolicy", "mitos-console-smtp-egress")
	if !strings.Contains(pol, "port: \"587\"") {
		t.Errorf("SMTP egress policy must default to port 587:\n%s", pol)
	}
}

// TestControlPlaneNetpolSMTPRequiresProfile asserts the SMTP policy never
// renders on its own: a CiliumNetworkPolicy with an egress section flips the
// selected endpoint to default-deny egress, so shipping the SMTP allow without
// the baseline profile would cut the console off from DNS, Postgres, and the
// apiserver.
func TestControlPlaneNetpolSMTPRequiresProfile(t *testing.T) {
	out := render(t,
		"console.onboarding.smtp.host=smtp.example.com",
	)
	if strings.Contains(out, "mitos-console-smtp-egress") {
		t.Error("SMTP egress policy rendered without the lockdown profile enabled; it must be all or nothing")
	}
}

// TestControlPlaneNetpolExtraEgress asserts deployment-specific rules pass
// through verbatim into the shared policy (the escape hatch for flows the
// chart cannot know, e.g. an in-cluster OIDC issuer namespace).
func TestControlPlaneNetpolExtraEgress(t *testing.T) {
	out := renderJSON(t,
		`networkPolicy={"controlPlane":{"enabled":true,"extraEgress":[{"toEndpoints":[{"matchLabels":{"k8s:io.kubernetes.pod.namespace":"dex"}}]}]}}`,
	)
	pol := section(t, out, "kind: CiliumNetworkPolicy", "mitos-control-plane-egress")
	if !strings.Contains(pol, "k8s:io.kubernetes.pod.namespace: dex") {
		t.Errorf("extraEgress rule not passed through:\n%s", pol)
	}
}

// TestSchemaRejectsNetpolTypo asserts the strict values schema still catches
// typos inside the new block (the silently-ignored-knob case).
func TestSchemaRejectsNetpolTypo(t *testing.T) {
	renderExpectSchemaError(t, "networkPolicy.controlPlane.enabld=true")
}
