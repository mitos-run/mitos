package main

import (
	"log/slog"

	"mitos.run/mitos/internal/saas/console"
	"mitos.run/mitos/internal/saas/console/clustersandbox"
)

// buildSandboxControl returns the live-sandbox control: the real cluster-backed
// query (org-scoped over the controller's v1 Sandbox records) when a kube
// client is available, falling back to the in-memory control (with a warning) in
// dev / outside a cluster so the console still starts.
func buildSandboxControl(logger *slog.Logger) console.SandboxControl {
	c, err := kubeClient()
	if err != nil {
		logger.Warn("cluster sandbox control unavailable (not in cluster?); using in-memory control", "err", err.Error())
		return console.NewMemSandboxControl()
	}
	logger.Info("sandbox control: cluster (org-scoped v1 Sandbox query; create/fork/exec wired, see clustersandbox)")
	return clustersandbox.New(c)
}

// buildLogStreamer returns the log-streaming seam. In a real cluster there is
// still no live log transport (unlike create/fork/exec, which clustersandbox
// wires onto real CRD/HTTP operations, there is no forkd/guest RPC that
// exposes a sandbox's stdout/stderr yet). Rather than let the in-memory
// default silently serve an always-empty, always-successful stream that
// LOOKS like a working "no logs yet" sandbox, the cluster path wires an
// explicit UnsupportedRawLogStreamer so GET .../logs/stream reports 501 and
// the SPA can show an honest "not available yet" state. Outside a cluster
// (dev, or -dev smoke testing), returning nil lets console.New fill its
// in-memory default, which is fine since nothing else is real there either.
func buildLogStreamer(logger *slog.Logger, control console.SandboxControl) console.LogStreamer {
	if _, err := kubeClient(); err != nil {
		return nil
	}
	logger.Warn("log streaming: no real transport is wired yet; GET .../logs/stream returns 501 (documented follow-up)")
	return console.NewAuthorizingLogStreamer(control, console.NewUnsupportedRawLogStreamer())
}
