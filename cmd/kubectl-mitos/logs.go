package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	v1 "mitos.run/mitos/api/v1"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
)

// huskSandboxLabel and huskLabel mirror the controller's husk pod labels: the
// pod backing a sandbox carries mitos.run/sandbox=<sandbox> and
// mitos.run/husk=true.
const (
	huskSandboxLabel = "mitos.run/sandbox"
	huskLabel        = "mitos.run/husk"
)

// podConsole is the husk stub pod console for a sandbox: the pod name and its
// log stream. Found is false when no husk pod backs the sandbox (a mock/no-husk
// control plane), so logs reports that honestly instead of erroring.
type podConsole struct {
	PodName string
	Logs    string
	Found   bool
}

// runLogs prints the husk stub pod console backing a sandbox, then a one-line
// note about the guest console. On a control plane with no husk pods
// (mock/no-VMM) the stub console is reported absent rather than erroring: the
// guest console needs a running sandbox (the #18 boundary).
func runLogs(namespace, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	console, err := huskPodConsole(ctx, namespace, name)
	if err != nil {
		return err
	}
	fmt.Print(renderLogs(name, console))
	return nil
}

// renderLogs formats the husk stub console plus the guest-console note. When
// the stub pod is present its name and log body are shown; when absent (no husk
// pod backs the sandbox) that is stated plainly. The guest console always
// carries the #18 boundary note: it is only reachable through a running sandbox
// over the guest serial/vsock console, which this read-only operator path does
// not stream on a mock or no-VMM control plane.
func renderLogs(sandbox string, console podConsole) string {
	var b strings.Builder
	if console.Found {
		fmt.Fprintf(&b, "=== husk stub console (pod %s) ===\n", console.PodName)
		body := console.Logs
		if strings.TrimSpace(body) == "" {
			b.WriteString("(no stub console output yet)\n")
		} else {
			b.WriteString(body)
			if !strings.HasSuffix(body, "\n") {
				b.WriteByte('\n')
			}
		}
	} else {
		fmt.Fprintf(&b, "=== husk stub console ===\nno husk pod backs claim %q (mock or no-VMM control plane)\n", sandbox)
	}
	b.WriteString("=== guest console ===\n")
	b.WriteString("guest console needs a running sandbox: it streams over the guest serial/vsock console of a live VMM (issue #18), not reachable from this read-only operator path on a mock or no-VMM control plane.\n")
	return b.String()
}

// huskPodConsole is the production huskConsoleFetcher: it builds a typed
// clientset from the standard kubeconfig, finds the husk pod backing the
// sandbox by its mitos.run/sandbox + mitos.run/husk labels, and streams that
// pod's logs. A sandbox with no husk pod yields Found=false (not an error) so
// logs reads the same on a mock control plane.
func huskPodConsole(ctx context.Context, namespace, sandbox string) (podConsole, error) {
	cfg, err := ctrlconfig.GetConfig()
	if err != nil {
		return podConsole{}, fmt.Errorf("load kubeconfig: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return podConsole{}, fmt.Errorf("build clientset: %w", err)
	}

	// Confirm the sandbox exists so a typo is a clear error, not a silent
	// "no husk pod".
	var sandboxObj v1.Sandbox
	if err := func() error {
		c, cerr := newClient()
		if cerr != nil {
			return cerr
		}
		return c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: sandbox}, &sandboxObj)
	}(); err != nil {
		if apierrors.IsNotFound(err) {
			return podConsole{}, fmt.Errorf("sandbox %q not found in namespace %q", sandbox, namespace)
		}
		return podConsole{}, fmt.Errorf("get sandbox: %w", err)
	}

	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,%s=true", huskSandboxLabel, sandbox, huskLabel),
	})
	if err != nil {
		return podConsole{}, fmt.Errorf("list husk pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return podConsole{Found: false}, nil
	}
	pod := pods.Items[0]

	stream, err := clientset.CoreV1().Pods(namespace).
		GetLogs(pod.Name, &corev1.PodLogOptions{}).Stream(ctx)
	if err != nil {
		return podConsole{}, fmt.Errorf("stream logs for pod %s: %w", pod.Name, err)
	}
	defer stream.Close()
	data, err := io.ReadAll(stream)
	if err != nil {
		return podConsole{}, fmt.Errorf("read logs for pod %s: %w", pod.Name, err)
	}
	return podConsole{PodName: pod.Name, Logs: string(data), Found: true}, nil
}
