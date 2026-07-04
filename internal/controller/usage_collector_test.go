package controller

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"mitos.run/mitos/internal/usage"
)

// TestUsageCycleLogsSummaryAtDefaultVerbosity is the issue #682 (was #665)
// observability fix proof: a SUCCESSFUL collection cycle must emit a one-line
// summary (samples, records, orgs, duration, skip counters) at DEFAULT
// verbosity. Before the fix a healthy cycle logged nothing at V(0), so a
// zero-collecting pipeline looked identical to a healthy one. The summary
// carries counts only: no sandbox ids, org ids, node identity, or secrets.
func TestUsageCycleLogsSummaryAtDefaultVerbosity(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	// Verbosity 0 is the default production level: V(1) lines are dropped, so
	// the summary must be a plain Info to be seen.
	logger := funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, args)
	}, funcr.Options{Verbosity: 0})

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fakeclient.NewClientBuilder().WithScheme(scheme).Build()

	nodeSource := usage.NewNodeRegistrySource(
		RegistryNodeLister{Registry: NewNodeRegistry()},
		usage.StaticOrgs{}, nil, nil, "", nil,
	)
	huskSource := usage.NewHuskSource(&HuskPodScrapeLister{Client: cl}, nil, nil, "", nil)
	collector := usage.NewCollector(
		usage.NewMultiSource(nodeSource, huskSource),
		usage.NewMemUsageStore(),
		usage.DefaultConfig(),
	)

	u := &UsageCollectorRunnable{}
	u.cycle(context.Background(), logger, collector, nodeSource, huskSource)

	mu.Lock()
	defer mu.Unlock()
	for _, l := range lines {
		if strings.Contains(l, "usage collection cycle") &&
			strings.Contains(l, "samples") &&
			strings.Contains(l, "records") &&
			strings.Contains(l, "orgs") &&
			strings.Contains(l, "duration") {
			return
		}
	}
	t.Fatalf("no healthy-cycle summary at default verbosity; got lines: %v", lines)
}
