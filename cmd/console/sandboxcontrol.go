package main

import (
	"log/slog"

	"mitos.run/mitos/internal/saas/console"
	"mitos.run/mitos/internal/saas/console/clustersandbox"
)

// buildSandboxControl returns the live-sandbox control: the real cluster-backed
// query (org-scoped over the controller's v1alpha2 Sandbox records) when a kube
// client is available, falling back to the in-memory control (with a warning) in
// dev / outside a cluster so the console still starts.
func buildSandboxControl(logger *slog.Logger) console.SandboxControl {
	c, err := kubeClient()
	if err != nil {
		logger.Warn("cluster sandbox control unavailable (not in cluster?); using in-memory control", "err", err.Error())
		return console.NewMemSandboxControl()
	}
	logger.Info("sandbox control: cluster (org-scoped v1alpha2 Sandbox query)")
	return clustersandbox.New(c)
}
