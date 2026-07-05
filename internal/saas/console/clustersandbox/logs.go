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
	"sync"

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

// maxLogBufferBytes bounds the total BYTES StreamLogs holds queued between
// the upstream follow read and the sink write. A prior line-COUNT cap (512
// lines) let a slow sink accumulate up to 512 x maxLogLineBytes (~512 MiB)
// per stream, since each line can independently be as large as
// maxLogLineBytes; several concurrent slow viewers could exhaust the
// multi-tenant console's memory. A slow sink (a laggy client, or one that
// stopped reading) must never make this accumulate unbounded memory: once
// the queued bytes exceed this cap, the OLDEST queued lines are dropped, by
// bytes, to make room for the newest, so a lagging client sees a gap in the
// tail rather than the console process growing without bound. A single line
// is always <= maxLogLineBytes (1 MiB, enforced upstream by
// discardOversizedLine), so it always fits within this cap on its own.
const maxLogBufferBytes = 4 * 1024 * 1024

// maxLogLineBytes bounds a single line's buffered length: a pod writing one
// absurdly long line with no newline must not grow memory without bound
// either. It sizes the bufio.Reader streamPodLines reads through, so a line
// that does not fit is detected as bufio.ErrBufferFull rather than silently
// growing (readBoundedLine below turns that into a graceful truncation
// instead of a hard error).
const maxLogLineBytes = 1 << 20 // 1 MiB

// truncatedLineMarker replaces a single log line that exceeds maxLogLineBytes
// with no newline in sight. It lets a follow degrade gracefully (skip the
// oversized line, keep streaming) instead of hard-erroring the whole
// connection the way bufio.Scanner's ErrTooLong used to.
const truncatedLineMarker = "[line truncated]\n"

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
// byte-bounded, drop-oldest buffer (lineBuffer, capped at maxLogBufferBytes)
// so a slow sink cannot make this hold unbounded memory. It returns when ctx
// is done (r is closed, which stops the upstream follow), r reaches a clean
// EOF (nil error), or sink.Write errors (the client disconnected). A single
// line over maxLogLineBytes does not end the follow: it is replaced by
// truncatedLineMarker and streaming continues with the next line.
func streamPodLines(ctx context.Context, r io.ReadCloser, sink console.LogSink) error {
	defer r.Close()

	buf := newLineBuffer()
	go func() {
		br := bufio.NewReaderSize(r, maxLogLineBytes)
		for {
			line, err := readBoundedLine(br)
			if len(line) > 0 {
				buf.push(line)
			}
			if err != nil {
				if err == io.EOF {
					buf.close(nil) // a clean end, matching bufio.Scanner's nil Err() on EOF
				} else {
					buf.close(err)
				}
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-buf.notify:
			lines, closed, err := buf.drain()
			for _, line := range lines {
				if writeErr := sink.Write(line); writeErr != nil {
					return writeErr
				}
			}
			if closed {
				return err // nil on a clean EOF
			}
		}
	}
}

// readBoundedLine reads one newline-terminated line from br. A line that fits
// within maxLogLineBytes is returned verbatim, with its line ending collapsed
// to a single trailing '\n' (mirroring bufio.ScanLines' \r?\n rule). A line
// that does NOT fit is never buffered past the cap: readBoundedLine discards
// it via discardOversizedLine and returns truncatedLineMarker in its place, so
// one absurdly long line degrades the stream instead of erroring it out. The
// final line of input with no trailing newline (a clean EOF mid-line) is
// still returned, with '\n' appended and any trailing '\r' stripped via the
// SAME stripCRLF rule as the delimiter path (so a CRLF-terminated stream that
// happens to end mid-line, e.g. the last write before the pod exits, does not
// leave a stray '\r' in the emitted line), alongside the io.EOF (or other
// read) error that ended it, matching bufio.Scanner's last-line behavior.
func readBoundedLine(br *bufio.Reader) ([]byte, error) {
	chunk, err := br.ReadSlice('\n')
	if err == nil {
		return stripCRLF(chunk), nil
	}
	if err == bufio.ErrBufferFull {
		return discardOversizedLine(br)
	}
	if len(chunk) > 0 {
		line := append(append([]byte(nil), chunk...), '\n')
		return stripCRLF(line), err
	}
	return nil, err
}

// stripCRLF returns a fresh copy of chunk (which ends with '\n', as returned
// by br.ReadSlice('\n')) with any single '\r' immediately preceding the '\n'
// removed, matching bufio.ScanLines' \r?\n line-ending rule.
func stripCRLF(chunk []byte) []byte {
	line := append([]byte(nil), chunk...)
	if n := len(line); n >= 2 && line[n-2] == '\r' {
		line[n-2] = '\n'
		return line[:n-1]
	}
	return line
}

// discardOversizedLine is called once br's buffer has filled without finding
// a newline: the current line already exceeds maxLogLineBytes. It discards
// everything already buffered (never returns it to the caller) and keeps
// reading, still bounded by br's own fixed buffer size so memory never grows
// past maxLogLineBytes, until it actually reaches the line's real newline (or
// the stream ends), then reports a single truncatedLineMarker line so the
// caller's follow keeps going instead of hard-erroring on bufio.ErrTooLong.
func discardOversizedLine(br *bufio.Reader) ([]byte, error) {
	for {
		_, err := br.ReadSlice('\n')
		if err == nil || err == io.EOF {
			return []byte(truncatedLineMarker), nil
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return nil, err
	}
}

// lineBuffer is a byte-bounded, drop-oldest queue of pending log lines
// shared between streamPodLines's single producer goroutine (the upstream
// scan loop) and its consumer (the ctx/notify select loop). It replaces a
// plain buffered channel because a channel can only bound line COUNT, not
// total bytes; lineBuffer instead tracks running byte size and drops from
// the front until push's new line fits under maxLogBufferBytes.
//
// notify is a capacity-1 "something changed" signal, not a data channel: the
// consumer wakes on it and then calls drain to actually read the queued
// lines (and the closed/err state) under the lock. This is the standard
// level-triggered wakeup idiom, so a signal coalesced by the capacity-1
// buffer while the consumer is busy is never a lost wakeup: the consumer
// always re-checks state after waking, and every state change (push, close)
// posts to notify after releasing the lock.
type lineBuffer struct {
	mu     sync.Mutex
	lines  [][]byte
	bytes  int
	notify chan struct{}
	closed bool
	err    error
}

func newLineBuffer() *lineBuffer {
	return &lineBuffer{notify: make(chan struct{}, 1)}
}

// push appends line to the queue, then drops the OLDEST queued lines (by
// bytes, never the just-appended newest one) until the total is back within
// maxLogBufferBytes. A single line is always <= maxLogLineBytes (well under
// maxLogBufferBytes, enforced upstream by discardOversizedLine), so the
// newest line alone always fits and is never itself dropped.
func (b *lineBuffer) push(line []byte) {
	b.mu.Lock()
	b.lines = append(b.lines, line)
	b.bytes += len(line)
	for b.bytes > maxLogBufferBytes && len(b.lines) > 1 {
		b.bytes -= len(b.lines[0])
		b.lines[0] = nil
		b.lines = b.lines[1:]
	}
	b.mu.Unlock()
	b.wake()
}

// close records the scan loop's terminal outcome (nil for a clean EOF, the
// read error otherwise) for drain to report once every already-queued line
// has been drained.
func (b *lineBuffer) close(err error) {
	b.mu.Lock()
	b.closed = true
	b.err = err
	b.mu.Unlock()
	b.wake()
}

// drain atomically takes every currently queued line (resetting the queue to
// empty) along with the closed/err state, so the consumer can forward the
// lines to sink and then decide whether to keep looping.
func (b *lineBuffer) drain() (lines [][]byte, closed bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	lines, b.lines = b.lines, nil
	b.bytes = 0
	return lines, b.closed, b.err
}

// wake posts a non-blocking signal to notify; a already-pending signal (the
// consumer has not yet woken to consume it) makes this a no-op, since the
// consumer will observe the latest state on its next drain regardless.
func (b *lineBuffer) wake() {
	select {
	case b.notify <- struct{}{}:
	default:
	}
}
