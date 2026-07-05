package main

import (
	"log/slog"

	"k8s.io/client-go/kubernetes"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"mitos.run/mitos/internal/saas/console"
	"mitos.run/mitos/internal/saas/console/clustersandbox"
)

// buildSandboxControl returns the live-sandbox control: the real cluster-backed
// query (org-scoped over the controller's v1 Sandbox records) when a kube
// client is available, falling back to the in-memory control (with a warning) in
// dev / outside a cluster so the console still starts. It also wires the
// husk-pod log follow transport (see clustersandbox/logs.go): when a client-go
// clientset cannot be built (should not happen if kubeClient itself
// succeeded, since both read the same ambient kube config, but is not
// asserted), sandbox control still comes up and StreamLogs reports
// console.ErrUnsupported for every sandbox instead of failing console
// startup over a non-essential capability.
func buildSandboxControl(logger *slog.Logger) console.SandboxControl {
	c, err := kubeClient()
	if err != nil {
		logger.Warn("cluster sandbox control unavailable (not in cluster?); using in-memory control", "err", err.Error())
		return console.NewMemSandboxControl()
	}
	pods, err := buildPodLogStreamer()
	if err != nil {
		logger.Warn("husk-pod log follow unavailable; GET .../logs/stream will return 501", "err", err.Error())
	} else {
		logger.Info("log streaming: cluster (husk-pod follow via PodLogOptions.Follow, see clustersandbox/logs.go)")
	}
	logger.Info("sandbox control: cluster (org-scoped v1 Sandbox query; create/fork/exec/logs wired, see clustersandbox)")
	return clustersandbox.New(c, pods)
}

// buildPodLogStreamer builds the production clustersandbox.PodLogStreamer: a
// client-go typed clientset over the same ambient kube config kubeClient
// reads (in-cluster service account, or KUBECONFIG in dev). A typed clientset
// is required here (not the controller-runtime client kubeClient returns)
// because the pod-logs subresource stream is a client-go-only capability.
func buildPodLogStreamer() (clustersandbox.PodLogStreamer, error) {
	cfg, err := ctrlconfig.GetConfig()
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return clustersandbox.NewClientsetPodLogStreamer(cs), nil
}

// buildLogStreamer returns the log-streaming seam. clustersandbox.Control
// satisfies console.LogStreamer directly (StreamLogs, org-scoped identically
// to Get/Terminate/Exec), so a real cluster control IS its own log streamer;
// StreamLogs itself reports console.ErrUnsupported for any sandbox with no
// husk pod backing it or when the pods transport could not be built (see
// buildSandboxControl above), so GET .../logs/stream still honestly 501s in
// those cases rather than faking a stream. Outside a cluster (dev, or -dev
// smoke testing) control is the in-memory console.NewMemSandboxControl,
// which does not implement LogStreamer; returning nil there lets console.New
// fill its in-memory log-streamer default, which is fine since nothing else
// is real there either.
func buildLogStreamer(_ *slog.Logger, control console.SandboxControl) console.LogStreamer {
	if ls, ok := control.(console.LogStreamer); ok {
		return ls
	}
	return nil
}
