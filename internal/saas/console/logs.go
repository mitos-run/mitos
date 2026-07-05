package console

import (
	"context"
	"net/http"
	"sync"

	"mitos.run/mitos/internal/apierr"
	"mitos.run/mitos/internal/saas"
)

// RawLogStreamer is the org-UNAWARE transport seam: it streams a sandbox's logs
// by id with no notion of ownership. The real implementation is the existing
// forkd→guest vsock exec/log transport (forkd :9091); this is the seam it plugs
// into. It MUST NOT be exposed directly to the BFF — it is always wrapped by an
// AuthorizingLogStreamer so ownership is checked before any byte is streamed.
type RawLogStreamer interface {
	StreamRaw(ctx context.Context, sandboxID string, sink LogSink) error
}

// AuthorizingLogStreamer is the BFF's log seam: it AUTHORIZES (the sandbox must
// belong to the caller's org) and only then proxies the raw transport. This is
// the place org-scoping is enforced for log streaming; a cross-org sandbox id is
// reported as ErrNotFound and the transport is never reached, so authorization
// cannot be bypassed via the streaming path.
type AuthorizingLogStreamer struct {
	control SandboxControl
	raw     RawLogStreamer
}

// NewAuthorizingLogStreamer composes an org-scoped SandboxControl (for the
// ownership check) with a raw transport (for the bytes).
func NewAuthorizingLogStreamer(control SandboxControl, raw RawLogStreamer) *AuthorizingLogStreamer {
	return &AuthorizingLogStreamer{control: control, raw: raw}
}

// StreamLogs verifies the sandbox belongs to org, then streams its logs. A
// sandbox that does not exist or belongs to another org is ErrNotFound and the
// raw transport is not touched.
func (a *AuthorizingLogStreamer) StreamLogs(ctx context.Context, orgID, sandboxID string, sink LogSink) error {
	if _, err := a.control.Get(ctx, orgID, sandboxID); err != nil {
		return err // ErrNotFound for a missing OR cross-org sandbox
	}
	return a.raw.StreamRaw(ctx, sandboxID, sink)
}

// UnsupportedRawLogStreamer is the RawLogStreamer wired in a real cluster
// deployment today: there is currently no forkd/guest RPC that exposes a
// sandbox's stdout/stderr (unlike exec/fork/create, which map onto existing
// CRD or HTTP operations, live log streaming has no real transport yet, a
// documented control-plane gap). StreamRaw always reports ErrUnsupported,
// which the console maps to HTTP 501, so GET .../logs/stream shows an honest
// "not available yet" state instead of a permanently-empty stream that looks
// like a successful, quiet sandbox.
type UnsupportedRawLogStreamer struct{}

// NewUnsupportedRawLogStreamer returns the always-ErrUnsupported raw log
// streamer.
func NewUnsupportedRawLogStreamer() UnsupportedRawLogStreamer { return UnsupportedRawLogStreamer{} }

// StreamRaw always returns ErrUnsupported.
func (UnsupportedRawLogStreamer) StreamRaw(context.Context, string, LogSink) error {
	return ErrUnsupported
}

// MemRawLogStreamer is the in-memory RawLogStreamer tested default: a fixed
// sandboxID -> lines map. Safe for concurrent use.
type MemRawLogStreamer struct {
	mu    sync.RWMutex
	lines map[string][][]byte
}

// NewMemRawLogStreamer returns an empty in-memory raw log streamer.
func NewMemRawLogStreamer() *MemRawLogStreamer {
	return &MemRawLogStreamer{lines: map[string][][]byte{}}
}

// Add appends lines for a sandbox (test/wiring helper).
func (m *MemRawLogStreamer) Add(sandboxID string, lines ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ln := range lines {
		m.lines[sandboxID] = append(m.lines[sandboxID], []byte(ln))
	}
}

// StreamRaw writes the sandbox's buffered lines to the sink in order.
func (m *MemRawLogStreamer) StreamRaw(_ context.Context, sandboxID string, sink LogSink) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, ln := range m.lines[sandboxID] {
		if err := sink.Write(ln); err != nil {
			return err
		}
	}
	return nil
}

// httpLogSink adapts an http.ResponseWriter into a LogSink, flushing each line so
// the client sees a live tail. It records whether any byte was written so the
// handler can still send an error status if authorization failed before the
// first write.
type httpLogSink struct {
	w     http.ResponseWriter
	wrote bool
}

func (s *httpLogSink) Write(line []byte) error {
	s.wrote = true
	if _, err := s.w.Write(line); err != nil {
		return err
	}
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// handleSandboxLogs streams one sandbox's logs for the caller's org. Gated the
// SAME way as inspect (handleInspectSandbox in console.go): the sandbox must
// belong to the org (404 via c.failSandbox on a missing/cross-org id) and the
// caller must hold PermReadOnly on its project, with a denial ALSO mapped to
// 404 (not 403) so a caller without project access cannot tell an
// out-of-reach sandbox apart from one that does not exist. Only once that
// check passes does the AuthorizingLogStreamer's own org check run and the
// raw transport get reached; once streaming starts the status is already 200
// and we simply stop on error.
func (c *Console) handleSandboxLogs(w http.ResponseWriter, r *http.Request) {
	accountID, orgID, e, ok := c.caller(r)
	if !ok {
		apierr.Encode(w, e)
		return
	}
	id := r.PathValue("id")
	sb, err := c.deps.Sandboxes.Get(r.Context(), orgID, id)
	if err != nil {
		c.failSandbox(w, err)
		return
	}
	// The project tag gates access; a lookup error must fail closed, not fall
	// back to the unassigned/org-wide path.
	pid, err := c.deps.ResourceProjects.Project(r.Context(), orgID, "sandbox", sb.ID)
	if err != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the sandbox project assignment could not be read"))
		return
	}
	canSee, accessErr := c.canAccessSandbox(r.Context(), accountID, orgID, pid, saas.PermReadOnly)
	if accessErr != nil {
		apierr.Encode(w, apierr.Get(apierr.CodeInternal).WithCause("the sandbox access check could not be completed"))
		return
	}
	if !canSee {
		apierr.Encode(w, apierr.Get(apierr.CodeNotFound).
			WithCause("the sandbox does not exist or is not accessible"))
		return
	}
	sink := &httpLogSink{w: w}
	if err := c.deps.Logs.StreamLogs(r.Context(), orgID, id, sink); err != nil && !sink.wrote {
		c.failSandbox(w, err)
		return
	}
}
