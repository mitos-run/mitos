package charttest

import (
	"strings"
	"testing"
)

// readinessReadyz is the rendered readiness probe fragment pointing at the
// binary's /readyz endpoint (readiness split from liveness).
const readinessReadyz = "readinessProbe:\n            httpGet:\n              path: /readyz"

// livenessHealthz is the rendered liveness probe fragment: liveness stays the
// static /healthz so a not-ready pod is never killed for a dead dependency.
const livenessHealthz = "livenessProbe:\n            httpGet:\n              path: /healthz"

// TestGatewayProbesSplitReadinessFromLiveness asserts the gateway Deployment
// probes readiness on /readyz (drain-aware) and liveness on /healthz, so a
// draining or not-ready replica leaves the Service without being restarted.
func TestGatewayProbesSplitReadinessFromLiveness(t *testing.T) {
	out := render(t)
	deploy := section(t, out, "kind: Deployment", "mitos-gateway")
	mustContain(t, deploy, readinessReadyz)
	mustContain(t, deploy, livenessHealthz)
}

// TestConsoleProbesSplitReadinessFromLiveness asserts the console Deployment
// probes readiness on /readyz (which pings the configured Postgres) and
// liveness on /healthz, so a console with a dead database stops receiving
// traffic instead of reporting Ready.
func TestConsoleProbesSplitReadinessFromLiveness(t *testing.T) {
	out := render(t)
	deploy := section(t, out, "kind: Deployment", "mitos-console")
	mustContain(t, deploy, readinessReadyz)
	mustContain(t, deploy, livenessHealthz)
}

// TestPodDisruptionBudgetsAbsentByDefault asserts the default render carries no
// PodDisruptionBudget: the PDBs are an explicit opt-in because with a single
// replica a minAvailable-1 PDB would block every node drain.
func TestPodDisruptionBudgetsAbsentByDefault(t *testing.T) {
	out := render(t)
	if strings.Contains(out, "kind: PodDisruptionBudget") {
		t.Fatal("default render contains a PodDisruptionBudget; PDBs must be an explicit opt-in")
	}
}

// TestPodDisruptionBudgetsRenderWhenEnabled asserts each workload's opt-in
// renders a policy/v1 PDB selecting that workload's pods with minAvailable 1,
// so a voluntary disruption (node drain, rollout of the node pool) always
// leaves a serving replica.
func TestPodDisruptionBudgetsRenderWhenEnabled(t *testing.T) {
	out := render(t,
		"controller.podDisruptionBudget.enabled=true",
		"gateway.podDisruptionBudget.enabled=true",
		"console.podDisruptionBudget.enabled=true",
	)
	for _, name := range []string{"mitos-controller", "mitos-gateway", "mitos-console"} {
		pdb := section(t, out, "kind: PodDisruptionBudget", name)
		mustContain(t, pdb, "apiVersion: policy/v1")
		mustContain(t, pdb, "minAvailable: 1")
		component := strings.TrimPrefix(name, "mitos-")
		mustContain(t, pdb, "app.kubernetes.io/component: "+component)
	}
}

// TestPodDisruptionBudgetFollowsWorkloadGate asserts a PDB is never rendered
// for a disabled workload, even when its podDisruptionBudget value is on: an
// orphan PDB selecting zero pods would wedge unrelated drains in the namespace.
func TestPodDisruptionBudgetFollowsWorkloadGate(t *testing.T) {
	out := render(t,
		"gateway.enabled=false",
		"gateway.podDisruptionBudget.enabled=true",
		"console.enabled=false",
		"console.podDisruptionBudget.enabled=true",
	)
	if strings.Contains(out, "kind: PodDisruptionBudget") {
		t.Fatal("PDB rendered for a disabled workload")
	}
}
