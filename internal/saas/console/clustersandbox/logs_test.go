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

// TestLineBufferPushDropsOldestWhenOverByteCap is the deterministic unit
// test for lineBuffer's drop-oldest behavior at small scale: once the total
// queued bytes would exceed maxLogBufferBytes, push drops from the front
// (oldest first) until the newest line fits, never dropping the newest line
// itself.
func TestLineBufferPushDropsOldestWhenOverByteCap(t *testing.T) {
	buf := newLineBuffer()
	// Three lines whose first two together already exceed a tiny custom
	// budget would be awkward to express against the real (4 MiB)
	// maxLogBufferBytes, so this test instead drives push directly with
	// lines sized relative to the real cap: two lines just over half the
	// cap each, so the second push's drop-oldest loop must evict the first.
	half := maxLogBufferBytes/2 + 1
	a := bytes.Repeat([]byte("a"), half)
	b := bytes.Repeat([]byte("b"), half)
	buf.push(a)
	buf.push(b) // "a" no longer fits alongside "b"; it must be dropped

	lines, _, _ := buf.drain()
	if len(lines) != 1 || string(lines[0]) != string(b) {
		t.Fatalf("drained %d line(s), want exactly the newest (b) line; oldest (a) was not dropped", len(lines))
	}
}

// TestLineBufferPushBoundsTotalBytesAndKeepsNewest is the TDD test for the
// MAJOR fix in issue #726: pushing many near-1-MiB lines (each individually
// under maxLogLineBytes, exactly as a chatty sandbox's real log lines would
// be truncated to by discardOversizedLine) must NEVER let the buffer's total
// queued bytes exceed maxLogBufferBytes, regardless of how many lines a slow
// consumer lets accumulate. The prior line-COUNT cap (512 lines) allowed up
// to ~512 MiB here; the byte-bounded queue must hold at most a handful of
// near-1-MiB lines. The newest line must always survive (drop-oldest, never
// drop-newest).
func TestLineBufferPushBoundsTotalBytesAndKeepsNewest(t *testing.T) {
	buf := newLineBuffer()
	const lineSize = maxLogLineBytes - 10 // near 1 MiB, comfortably under maxLogBufferBytes on its own
	const pushCount = 50                  // 50 * ~1 MiB > 48 MiB, far over the 4 MiB cap

	var last []byte
	for i := 0; i < pushCount; i++ {
		line := bytes.Repeat([]byte{byte('a' + i%26)}, lineSize)
		last = line
		buf.push(line)
		if buf.bytes > maxLogBufferBytes {
			t.Fatalf("after push %d: buffered bytes = %d, want <= maxLogBufferBytes (%d)", i, buf.bytes, maxLogBufferBytes)
		}
	}

	lines, _, _ := buf.drain()
	if len(lines) == 0 {
		t.Fatal("every line was dropped; the newest line must always be kept")
	}
	if newest := lines[len(lines)-1]; string(newest) != string(last) {
		t.Fatal("the newest pushed line was not the newest line retained; drop-oldest was violated")
	}
	// Sanity: with lineSize near 1 MiB and a 4 MiB cap, at most a handful of
	// lines can coexist, proving this is nowhere near the old ~512-line cap.
	if len(lines) > 10 {
		t.Fatalf("retained %d lines of ~1 MiB each, want a small handful under the 4 MiB cap", len(lines))
	}
}
