package clustersandbox

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"mitos.run/mitos/internal/saas/console"
	"mitos.run/mitos/internal/tenant"
)

// closeRecordingReadCloser wraps an io.ReadCloser and records whether Close
// was called, so a test can assert ctx cancellation actually reached the
// upstream transport instead of leaking it.
type closeRecordingReadCloser struct {
	io.ReadCloser
	closed atomic.Bool
}

func (r *closeRecordingReadCloser) Close() error {
	r.closed.Store(true)
	return r.ReadCloser.Close()
}

// fakePodLogStreamer is the PodLogStreamer test double: it hands back one end
// of an io.Pipe so a test can write bytes as if they were pod-log output,
// with no real API server involved. It records the last ns/pod it was called
// with so tests can assert the husk pod is addressed correctly.
type fakePodLogStreamer struct {
	w       *io.PipeWriter
	rc      *closeRecordingReadCloser
	ns, pod string
}

func newFakePodLogStreamer() *fakePodLogStreamer {
	pr, pw := io.Pipe()
	return &fakePodLogStreamer{w: pw, rc: &closeRecordingReadCloser{ReadCloser: pr}}
}

func (f *fakePodLogStreamer) StreamPodLogs(_ context.Context, ns, pod string, _ corev1.PodLogOptions) (io.ReadCloser, error) {
	f.ns, f.pod = ns, pod
	return f.rc, nil
}

// chanLogSink is a console.LogSink that forwards each written line onto a
// channel, so a test can wait for lines to arrive instead of polling.
type chanLogSink struct {
	lines chan []byte
}

func newChanLogSink() *chanLogSink { return &chanLogSink{lines: make(chan []byte, 16)} }

func (s *chanLogSink) Write(line []byte) error {
	cp := append([]byte(nil), line...)
	s.lines <- cp
	return nil
}

func recvLine(t *testing.T, sink *chanLogSink) string {
	t.Helper()
	select {
	case line := <-sink.lines:
		return string(line)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a log line")
		return ""
	}
}

// TestStreamLogsFollowsPodLogLinesAndStopsOnCancel is the load-bearing follow
// test: lines written to the husk pod's log stream arrive at the sink live,
// the correct namespace/pod are addressed, and canceling ctx (modeling a
// client disconnect) both returns StreamLogs promptly and closes the upstream
// pod-log stream (the follow actually stops, it does not leak).
func TestStreamLogsFollowsPodLogLinesAndStopsOnCancel(t *testing.T) {
	s := sb("alice", "sb-a1", "Ready")
	s.Status.Pod = "sb-a1-husk"
	pods := newFakePodLogStreamer()
	c := newControlWithPods(t, pods, s)

	sink := newChanLogSink()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- c.StreamLogs(ctx, "alice", "sb-a1", sink) }()

	if _, err := pods.w.Write([]byte("line one\nline two\n")); err != nil {
		t.Fatalf("write pod log lines: %v", err)
	}
	if got := recvLine(t, sink); got != "line one\n" {
		t.Fatalf("first line = %q, want %q", got, "line one\n")
	}
	if got := recvLine(t, sink); got != "line two\n" {
		t.Fatalf("second line = %q, want %q", got, "line two\n")
	}
	if pods.ns != tenant.NamespaceForOrg("alice") || pods.pod != "sb-a1-husk" {
		t.Fatalf("StreamPodLogs(ns=%q, pod=%q), want (%q, %q)", pods.ns, pods.pod, tenant.NamespaceForOrg("alice"), "sb-a1-husk")
	}

	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("StreamLogs err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("StreamLogs did not return after ctx cancel; the follow leaked")
	}
	if !pods.rc.closed.Load() {
		t.Fatal("ctx cancel did not close the upstream pod-log stream; the follow was not actually stopped")
	}
}

// TestStreamLogsCleanEOFOnPodStreamEnd asserts that when the pod's log stream
// ends on its own (the sandbox terminated, the kubelet closed it), StreamLogs
// returns nil (a clean end), not an error.
func TestStreamLogsCleanEOFOnPodStreamEnd(t *testing.T) {
	s := sb("alice", "sb-a1", "Ready")
	s.Status.Pod = "sb-a1-husk"
	pods := newFakePodLogStreamer()
	c := newControlWithPods(t, pods, s)
	sink := newChanLogSink()

	done := make(chan error, 1)
	go func() { done <- c.StreamLogs(context.Background(), "alice", "sb-a1", sink) }()

	if _, err := pods.w.Write([]byte("last line\n")); err != nil {
		t.Fatalf("write pod log line: %v", err)
	}
	if got := recvLine(t, sink); got != "last line\n" {
		t.Fatalf("line = %q, want %q", got, "last line\n")
	}
	if err := pods.w.Close(); err != nil {
		t.Fatalf("close pod log writer: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("StreamLogs err = %v, want nil (clean EOF)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("StreamLogs did not return on upstream EOF")
	}
}

// TestStreamLogsCrossOrgIsNotFoundAndNeverTouchesPodLogs asserts a cross-org
// sandbox id is refused as ErrNotFound BEFORE the pod-log transport is ever
// reached, the same authorize-before-transport guarantee Exec proves.
func TestStreamLogsCrossOrgIsNotFoundAndNeverTouchesPodLogs(t *testing.T) {
	s := sb("bob", "sb-b1", "Ready")
	s.Status.Pod = "sb-b1-husk"
	pods := newFakePodLogStreamer()
	c := newControlWithPods(t, pods, s)

	err := c.StreamLogs(context.Background(), "alice", "sb-b1", newChanLogSink())
	if err != console.ErrNotFound {
		t.Fatalf("cross-org StreamLogs err = %v, want console.ErrNotFound", err)
	}
	if pods.pod != "" {
		t.Fatal("StreamLogs reached the pod-log transport for a cross-org id; authorization bypassed")
	}
}

// TestStreamLogsNoHuskPodIsUnsupported asserts a sandbox with no husk pod
// backing it (the raw-forkd path; see api/v1 SandboxStatus.Pod's doc) reports
// ErrUnsupported honestly rather than a permanently-empty stream.
func TestStreamLogsNoHuskPodIsUnsupported(t *testing.T) {
	s := sb("alice", "sb-a1", "Ready") // Status.Pod left empty
	pods := newFakePodLogStreamer()
	c := newControlWithPods(t, pods, s)

	err := c.StreamLogs(context.Background(), "alice", "sb-a1", newChanLogSink())
	if err != console.ErrUnsupported {
		t.Fatalf("err = %v, want console.ErrUnsupported", err)
	}
	if pods.pod != "" {
		t.Fatal("StreamLogs called the pod-log transport despite no husk pod")
	}
}

// TestStreamLogsNoPodsTransportIsUnsupported asserts a Control built with no
// PodLogStreamer (dev / outside a cluster) reports ErrUnsupported rather than
// panicking on a nil transport.
func TestStreamLogsNoPodsTransportIsUnsupported(t *testing.T) {
	s := sb("alice", "sb-a1", "Ready")
	s.Status.Pod = "sb-a1-husk"
	c := newControl(t, s) // nil pods transport
	err := c.StreamLogs(context.Background(), "alice", "sb-a1", newChanLogSink())
	if err != console.ErrUnsupported {
		t.Fatalf("err = %v, want console.ErrUnsupported", err)
	}
}

// TestStreamPodLinesOversizedLineEmitsTruncatedMarkerAndContinues asserts that
// a single log line exceeding maxLogLineBytes degrades gracefully: before
// this fix bufio.Scanner's ErrTooLong hard-erred the whole follow on such a
// line. Now the oversized line is replaced by truncatedLineMarker and the
// stream keeps following subsequent, normal-sized lines through to a clean
// EOF.
func TestStreamPodLinesOversizedLineEmitsTruncatedMarkerAndContinues(t *testing.T) {
	oversized := bytes.Repeat([]byte("x"), maxLogLineBytes+10)
	pr, pw := io.Pipe()
	sink := newChanLogSink()

	done := make(chan error, 1)
	go func() { done <- streamPodLines(context.Background(), pr, sink) }()

	go func() {
		_, _ = pw.Write([]byte("before\n"))
		_, _ = pw.Write(oversized)
		_, _ = pw.Write([]byte("\n"))
		_, _ = pw.Write([]byte("after\n"))
		_ = pw.Close()
	}()

	if got := recvLine(t, sink); got != "before\n" {
		t.Fatalf("first line = %q, want %q", got, "before\n")
	}
	if got := recvLine(t, sink); got != truncatedLineMarker {
		t.Fatalf("oversized line = %q, want the truncated marker %q", got, truncatedLineMarker)
	}
	if got := recvLine(t, sink); got != "after\n" {
		t.Fatalf("line after the oversized one = %q, want %q; the follow did not continue", got, "after\n")
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("streamPodLines err = %v, want nil (clean EOF)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("streamPodLines did not return on clean EOF after the oversized line")
	}
}

// TestReadBoundedLineEOFMidLineStripsTrailingCR is the regression test for the
// EOF-mid-line branch of readBoundedLine: when the underlying reader hits EOF
// (or any other read error) before finding a newline, the last, partial line
// is still returned with '\n' appended. Before this fix, a trailing '\r' left
// over from a CRLF-terminated stream that happened to end mid-line (no final
// newline) was never stripped, unlike the delimiter path (stripCRLF), which
// runs on every complete line. A pod's final write before it exits is exactly
// this shape, so the caller must not see a stray '\r' in the emitted line.
func TestReadBoundedLineEOFMidLineStripsTrailingCR(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("last-line\r"))
	line, err := readBoundedLine(br)
	if err != io.EOF {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	if got, want := string(line), "last-line\n"; got != want {
		t.Fatalf("line = %q, want %q (trailing \\r stripped)", got, want)
	}
}

// TestPushDropOldestDropsOldestWhenFull is the deterministic unit test for
// the bounded, drop-oldest buffer streamPodLines relies on to bound memory
// against a slow sink: once full, pushing one more line drops the OLDEST
// queued line, not the newest.
func TestPushDropOldestDropsOldestWhenFull(t *testing.T) {
	ch := make(chan []byte, 2)
	pushDropOldest(ch, []byte("a"))
	pushDropOldest(ch, []byte("b"))
	pushDropOldest(ch, []byte("c")) // "a" is the oldest; it must be dropped

	first, second := string(<-ch), string(<-ch)
	if first != "b" || second != "c" {
		t.Fatalf("drained = [%q %q], want [\"b\" \"c\"] (drop-oldest)", first, second)
	}
}
