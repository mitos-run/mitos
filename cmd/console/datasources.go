package main

import (
	"log/slog"
	"os"

	"mitos.run/mitos/internal/saas/console"
	"mitos.run/mitos/internal/saas/console/clusterforktree"
	"mitos.run/mitos/internal/saas/console/clusterinstruments"
	"mitos.run/mitos/internal/usage"
)

// buildUsageStore returns the usage store the console reads through. When the
// controller's internal usage API is configured (MITOS_USAGE_API_URL +
// MITOS_USAGE_API_TOKEN), the console reads the SAME per-org usage the
// controller's collector recorded over that machine-to-machine HTTP seam (there
// is no shared database). Absent that config it falls back to an empty in-memory
// store (with a warning) so the console still starts for local dev and tests.
// The org is always scoped from the gateway-verified request, never the client.
func buildUsageStore(logger *slog.Logger) usage.UsageStore {
	base := os.Getenv("MITOS_USAGE_API_URL")
	token := os.Getenv("MITOS_USAGE_API_TOKEN")
	if base != "" && token != "" {
		logger.Info("usage store: controller internal usage API (org-scoped HTTP)", "url", base)
		return usage.NewHTTPStore(base, token, nil)
	}
	if base != "" && token == "" {
		logger.Warn("MITOS_USAGE_API_URL is set but MITOS_USAGE_API_TOKEN is empty; using in-memory usage store")
	} else {
		logger.Warn("MITOS_USAGE_API_URL unset; using in-memory usage store (dev/local only, no real usage)")
	}
	return usage.NewMemUsageStore()
}

// buildInstruments returns the proof-snapshot source: the cluster-backed source
// (org-scoped over the controller's v1 Sandbox records) when a kube client is
// available, falling back to the in-memory source (with a warning) in dev /
// outside a cluster so the console still starts.
func buildInstruments(logger *slog.Logger) console.InstrumentsSource {
	c, err := kubeClient()
	if err != nil {
		logger.Warn("cluster instruments unavailable (not in cluster?); using in-memory instruments", "err", err.Error())
		return console.NewMemInstruments()
	}
	logger.Info("instruments: cluster (org-scoped v1 Sandbox measurements)")
	return clusterinstruments.New(c)
}

// buildForkTree returns the fork-tree source: the cluster-backed source
// (org-scoped over the controller's v1 Sandbox records) when a kube client is
// available, falling back to the in-memory source (with a warning) in dev /
// outside a cluster so the console still starts.
func buildForkTree(logger *slog.Logger) console.ForkTreeSource {
	c, err := kubeClient()
	if err != nil {
		logger.Warn("cluster fork tree unavailable (not in cluster?); using in-memory fork tree", "err", err.Error())
		return console.NewMemForkTree()
	}
	logger.Info("fork tree: cluster (org-scoped v1 Sandbox query)")
	return clusterforktree.New(c)
}
