package main

import (
	"log/slog"

	"mitos.run/mitos/internal/saas/console"
	"mitos.run/mitos/internal/saas/console/clusternodes"
)

// buildNodeSource returns the k8s node-inventory seam for GET
// /console/admin/nodes: the real cluster-backed lister when a kube client is
// available, or nil (NOT an in-memory fallback) outside a cluster. Unlike
// buildSandboxControl, nil is the CORRECT, permanent answer here: a nil
// console.NodeSource is the documented signal the handler uses to report an
// honest {"available": false} rather than fabricating an empty node list for
// a deployment that has no cluster to inventory (e.g. dev, or a
// sandbox-server-only install).
func buildNodeSource(logger *slog.Logger) console.NodeSource {
	c, err := kubeClient()
	if err != nil {
		logger.Info("node inventory unavailable (not in cluster?); GET /console/admin/nodes will report available:false", "err", err.Error())
		return nil
	}
	return clusternodes.New(c)
}
