// Package clusterinstruments is the real console.InstrumentsSource: it measures
// an org's proof snapshot from the controller's v1 Sandbox records scoped to one
// org. Under hard isolation each org's sandboxes live in its own namespace
// (tenant.NamespaceForOrg), so org scoping is the namespace boundary plus the
// org label as defense in depth: an org only ever sees its own namespace's
// sandboxes, mirroring the cross-tenant boundary in internal/saas/controlplane.
//
// MEASURED, NEVER FABRICATED (the docs/saas/pricing.md no-unverified-numbers
// rule): every value comes from the org's own Sandbox records.
//   - ForksServed is the count of the org's sandboxes that were forked from
//     another sandbox (spec.source.fromSandbox), the org's fork fan-out.
//   - ActivateP50/P99Millis are percentiles of the org's measured
//     status.startupLatencyMs across its Ready sandboxes (the same activate path
//     bench/husk-activate-latency.sh times).
//
// CoWSavingsBytes and MarginalBytesPerFork are per-node CoW metering that is NOT
// carried on the Sandbox CR; they are left zero here and are the documented
// metering follow-up (the same #33 metering the usage collector scrapes), so
// this source never reports a CoW number it has not measured.
package clusterinstruments

import (
	"context"
	"fmt"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "mitos.run/mitos/api/v1"
	"mitos.run/mitos/internal/saas/console"
	"mitos.run/mitos/internal/tenant"
)

// Source implements console.InstrumentsSource against the Kubernetes API.
type Source struct {
	c client.Client
}

// New builds the cluster-backed instruments source.
func New(c client.Client) *Source {
	return &Source{c: c}
}

// Snapshot measures the org's proof metrics from its own sandboxes. It returns a
// zero-but-org-scoped snapshot for an org with no sandboxes, and NEVER another
// org's metrics: the List is scoped to the org's namespace AND matches the org
// label.
func (s *Source) Snapshot(ctx context.Context, orgID string) (console.Instruments, error) {
	var list v1.SandboxList
	if err := s.c.List(ctx, &list,
		client.InNamespace(tenant.NamespaceForOrg(orgID)),
		client.MatchingLabels(tenant.OrgLabels(orgID)),
	); err != nil {
		return console.Instruments{}, fmt.Errorf("list sandboxes: %w", err)
	}

	out := console.Instruments{OrgID: orgID}
	latencies := make([]float64, 0, len(list.Items))
	for i := range list.Items {
		sb := &list.Items[i]
		if sb.Spec.Source.FromSandbox != nil {
			out.ForksServed++
		}
		if sb.Status.StartupLatencyMs > 0 {
			latencies = append(latencies, float64(sb.Status.StartupLatencyMs))
		}
	}
	out.ActivateP50Millis = percentile(latencies, 50)
	out.ActivateP99Millis = percentile(latencies, 99)
	return out, nil
}

// percentile returns the p-th percentile (nearest-rank) of vs, or 0 for an empty
// set. It sorts a copy so the caller's slice is untouched. p is clamped to
// [0, 100].
func percentile(vs []float64, p int) float64 {
	if len(vs) == 0 {
		return 0
	}
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	s := make([]float64, len(vs))
	copy(s, vs)
	sort.Float64s(s)
	// Nearest-rank: rank = ceil(p/100 * N), 1-based, clamped to [1, N].
	rank := (p*len(s) + 99) / 100
	if rank < 1 {
		rank = 1
	}
	if rank > len(s) {
		rank = len(s)
	}
	return s[rank-1]
}
