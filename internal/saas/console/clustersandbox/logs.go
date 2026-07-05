// This file wires the live log-follow transport for a cluster deployment: the
// husk stub pod's Kubernetes pod-log stream, the same source
// cmd/kubectl-mitos/logs.go already reads (one-shot) for the operator "logs"
// verb. api/v1 Sandbox.Status.Pod documents the husk pod name; a sandbox on
// the raw-forkd path (no husk pod) has no log source yet, so StreamLogs
// reports that honestly (console.ErrUnsupported) rather than a fabricated
// empty stream.
package clustersandbox

import (
	"bufio"
	"context"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	"mitos.run/mitos/internal/saas/console"
)

// PodLogStreamer opens a (optionally following) raw byte stream of one pod's
// logs. The production implementation adapts a client-go typed clientset
// (NewClientsetPodLogStreamer); tests fake it directly with no real API
// server so StreamLogs's cancellation, EOF, and buffering behavior can be
// exercised deterministically.
type PodLogStreamer interface {
	// StreamPodLogs opens pod's log stream in namespace ns per opts (Follow
	// is set true for live tailing) and returns its raw bytes. The caller
	// MUST Close the returned stream exactly once, including when ctx is
	// canceled mid-stream: closing it is what stops the upstream follow.
	StreamPodLogs(ctx context.Context, ns, pod string, opts corev1.PodLogOptions) (io.ReadCloser, error)
}

// clientsetPodLogStreamer adapts a client-go typed clientset to
// PodLogStreamer. A typed clientset (not the controller-runtime client
// already used elsewhere in this package) is required here because the pod
// logs subresource stream is a client-go-only capability.
type clientsetPodLogStreamer struct {
	cs kubernetes.Interface
}

// NewClientsetPodLogStreamer returns the production PodLogStreamer, built
// once at startup from the same kube config as the controller-runtime client
// (see cmd/console/sandboxcontrol.go).
func NewClientsetPodLogStreamer(cs kubernetes.Interface) PodLogStreamer {
	return clientsetPodLogStreamer{cs: cs}
}

// StreamPodLogs opens the pod-logs subresource stream via the typed
// clientset, tied to ctx exactly like every other Kubernetes API call in this
// package: canceling ctx cancels the underlying HTTP request, which is what
// makes upstream-follow cancellation work.
func (p clientsetPodLogStreamer) StreamPodLogs(ctx context.Context, ns, pod string, opts corev1.PodLogOptions) (io.ReadCloser, error) {
	return p.cs.CoreV1().Pods(ns).GetLogs(pod, &opts).Stream(ctx)
}

// logLineBufferCap bounds how many pod-log lines StreamLogs holds between the
// upstream follow read and the sink write. A slow sink (a laggy client, or
// one that stopped reading) must never make this accumulate unbounded memory:
// once the buffer is full, the OLDEST queued line is dropped to make room for
// the newest, so a lagging client sees a gap in the tail rather than the
// console process growing without bound.
const logLineBufferCap = 512

// maxLogLineBytes bounds a single scanned line (bufio.Scanner's own token
// cap): a pod writing one absurdly long line with no newline must not grow
// memory without bound either.
const maxLogLineBytes = 1 << 20 // 1 MiB

// StreamLogs streams orgID's sandboxID husk pod's stdout/stderr, following
// new output as the pod produces it, until ctx is done or the pod's log
// stream ends in a clean EOF (the container exited or the sandbox was
// deleted). A cross-org or missing sandboxID is console.ErrNotFound, checked
// via the SAME s.get Get/Terminate/Exec use, BEFORE any pod is touched. A
// sandbox with no husk pod backing it (the raw-forkd path; see api/v1
// SandboxStatus.Pod's doc) or a deployment with no pods transport wired has
// no log source yet and returns console.ErrUnsupported honestly.
func (s *Control) StreamLogs(ctx context.Context, orgID, sandboxID string, sink console.LogSink) error {
	sb, err := s.get(ctx, orgID, sandboxID)
	if err != nil {
		return err
	}
	if s.pods == nil || sb.Status.Pod == "" {
		return console.ErrUnsupported
	}
	stream, err := s.pods.StreamPodLogs(ctx, sb.Namespace, sb.Status.Pod, corev1.PodLogOptions{Follow: true})
	if err != nil {
		return fmt.Errorf("stream logs for pod %s: %w", sb.Status.Pod, err)
	}
	return streamPodLines(ctx, stream, sink)
}

// streamPodLines pumps newline-delimited lines from r into sink through a
// bounded, drop-oldest buffer (logLineBufferCap) so a slow sink cannot make
// this hold unbounded memory. It returns when ctx is done (r is closed, which
// stops the upstream follow), r reaches a clean EOF (nil error), or
// sink.Write errors (the client disconnected).
func streamPodLines(ctx context.Context, r io.ReadCloser, sink console.LogSink) error {
	defer r.Close()

	lines := make(chan []byte, logLineBufferCap)
	scanDone := make(chan error, 1)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), maxLogLineBytes)
		for scanner.Scan() {
			line := append(append([]byte(nil), scanner.Bytes()...), '\n')
			pushDropOldest(lines, line)
		}
		scanDone <- scanner.Err()
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case line, ok := <-lines:
			if !ok {
				return <-scanDone // nil on a clean EOF
			}
			if err := sink.Write(line); err != nil {
				return err
			}
		}
	}
}

// pushDropOldest sends line on ch, dropping the oldest queued line first if
// ch is full. It never blocks: there is exactly one producer goroutine
// (streamPodLines's scan loop above), so the drop-then-retry loop always
// terminates in at most one drop.
func pushDropOldest(ch chan []byte, line []byte) {
	for {
		select {
		case ch <- line:
			return
		default:
			select {
			case <-ch:
			default:
			}
		}
	}
}
