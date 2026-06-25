package console

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// SinkConfig is one audit-sink destination for an org. It describes where audit
// events are delivered; it carries NO credential (credentials are injected
// out-of-band at the infrastructure layer). Endpoint is the delivery URL.
// Type must be one of: webhook, s3, splunk, datadog.
type SinkConfig struct {
	ID        string    `json:"id"`
	OrgID     string    `json:"org_id"`
	Type      string    `json:"type"`
	Endpoint  string    `json:"endpoint"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

// SinkRegistry is the org-scoped seam for managing audit-sink destinations.
// All methods are org-scoped: List returns only the named org's sinks; Add
// appends a new sink for the org; Delete removes a sink and returns ErrNotFound
// if the sink does not exist in that org (cross-org delete is indistinguishable
// from not found).
type SinkRegistry interface {
	List(ctx context.Context, orgID string) []SinkConfig
	Add(ctx context.Context, orgID, sinkType, endpoint string) (SinkConfig, error)
	Delete(ctx context.Context, orgID, id string) error
}

// MemSinkRegistry is the in-memory tested default for SinkRegistry. It stores
// per-org sink slices and never returns or deletes one org's sinks from another.
// Safe for concurrent use.
type MemSinkRegistry struct {
	mu    sync.RWMutex
	byOrg map[string][]SinkConfig
	n     int
	Now   func() time.Time
}

// NewMemSinkRegistry returns an empty in-memory sink registry.
func NewMemSinkRegistry() *MemSinkRegistry {
	return &MemSinkRegistry{
		byOrg: map[string][]SinkConfig{},
		Now:   time.Now,
	}
}

// List returns a copy of the org's sink configs. It never returns another
// org's sinks.
func (m *MemSinkRegistry) List(_ context.Context, orgID string) []SinkConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	src := m.byOrg[orgID]
	out := make([]SinkConfig, len(src))
	copy(out, src)
	return out
}

// Add creates a new enabled sink for the org, assigns a deterministic ID via
// a counter, and returns the stored config.
func (m *MemSinkRegistry) Add(_ context.Context, orgID, sinkType, endpoint string) (SinkConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n++
	cfg := SinkConfig{
		ID:        fmt.Sprintf("sink_%s_%d", orgID, m.n),
		OrgID:     orgID,
		Type:      sinkType,
		Endpoint:  endpoint,
		Enabled:   true,
		CreatedAt: m.Now(),
	}
	m.byOrg[orgID] = append(m.byOrg[orgID], cfg)
	return cfg, nil
}

// Delete removes the sink with the given id from the org. Cross-org delete
// (or missing id) returns ErrNotFound; the two cases are indistinguishable.
func (m *MemSinkRegistry) Delete(_ context.Context, orgID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	sinks := m.byOrg[orgID]
	for i, sc := range sinks {
		if sc.ID == id {
			m.byOrg[orgID] = append(sinks[:i], sinks[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}

// AuditSink is the delivery interface for a single audit event to a configured
// destination. Implementations must honor ctx cancellation.
type AuditSink interface {
	Deliver(ctx context.Context, cfg SinkConfig, ev AuditEvent) error
}

// sinkFunc is an adapter that lets a plain function satisfy AuditSink. It is
// used in tests to inject fake delivery behavior without a real HTTP server.
type sinkFunc func(ctx context.Context, cfg SinkConfig, ev AuditEvent) error

func (f sinkFunc) Deliver(ctx context.Context, cfg SinkConfig, ev AuditEvent) error {
	return f(ctx, cfg, ev)
}

// webhookSink delivers audit events as JSON POST requests to the sink's Endpoint.
// Delivery uses a short-timeout client so a slow receiver never blocks the
// Record call path. Credentials are never embedded in this type; the Endpoint
// URL may carry an opaque token in the path or query, but this type does not
// log the URL.
type webhookSink struct {
	client *http.Client
}

// newWebhookSink returns a webhookSink with a five-second delivery timeout.
func newWebhookSink() *webhookSink {
	return &webhookSink{
		client: &http.Client{Timeout: 5 * time.Second},
	}
}

// Deliver POSTs the event as JSON to cfg.Endpoint. A non-2xx response is
// treated as a delivery error.
func (ws *webhookSink) Deliver(ctx context.Context, cfg SinkConfig, ev AuditEvent) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("webhook sink marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webhook sink build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := ws.client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook sink post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook sink: endpoint returned %d", resp.StatusCode)
	}
	return nil
}

// DispatchingRecorder wraps an AuditRecorder and a SinkRegistry. Record calls
// the inner recorder first, then best-effort delivers the event to every
// ENABLED sink for the org. Delivery failures are logged and never returned to
// the caller; an audit or sink failure must never fail the user action.
//
// WaitForDispatch is a test-only helper that blocks until all in-flight
// dispatch goroutines have finished. It must not be called concurrently with
// Record in tests that care about final delivery counts.
type DispatchingRecorder struct {
	inner AuditRecorder
	sinks SinkRegistry
	sink  AuditSink
	log   *slog.Logger
	wg    sync.WaitGroup
}

// NewDispatchingRecorder builds a DispatchingRecorder. log may be nil; when nil
// dispatch failures are silently discarded (only expected in tests that do not
// care about log output).
func NewDispatchingRecorder(inner AuditRecorder, sinks SinkRegistry, sink AuditSink) *DispatchingRecorder {
	return &DispatchingRecorder{
		inner: inner,
		sinks: sinks,
		sink:  sink,
	}
}

// withLog returns the same recorder with the logger set. Called by New when
// wiring the console so dispatch failures reach the console's log sink.
func (r *DispatchingRecorder) withLog(l *slog.Logger) *DispatchingRecorder {
	r.log = l
	return r
}

// Record appends the event to the inner recorder, then best-effort dispatches
// to the org's enabled sinks in a goroutine tracked by wg. Any delivery
// failure is logged and swallowed.
//
// KNOWN FOLLOW-UP: the per-event goroutine-per-sink dispatch below is
// unbounded: a burst of events or a large sink list can spawn an unbounded
// number of goroutines. A bounded worker pool and full SSRF egress protection
// (blocking private/loopback ranges at the transport level) are tracked for
// production hardening before this code reaches a multi-tenant environment.
func (r *DispatchingRecorder) Record(ctx context.Context, ev AuditEvent) error {
	if err := r.inner.Record(ctx, ev); err != nil {
		return err
	}
	cfgs := r.sinks.List(ctx, ev.OrgID)
	for _, cfg := range cfgs {
		if !cfg.Enabled {
			continue
		}
		r.wg.Add(1)
		go func(c SinkConfig) {
			defer r.wg.Done()
			if err := r.sink.Deliver(context.Background(), c, ev); err != nil {
				if r.log != nil {
					r.log.Warn("audit sink delivery failed",
						"org", ev.OrgID,
						"sink_id", c.ID,
						"sink_type", c.Type,
						"err", err.Error(),
					)
				}
			}
		}(cfg)
	}
	return nil
}

// List delegates to the inner recorder.
func (r *DispatchingRecorder) List(ctx context.Context, orgID string) ([]AuditEvent, error) {
	return r.inner.List(ctx, orgID)
}

// WaitForDispatch blocks until all in-flight best-effort dispatch goroutines
// have finished. It is intended for use in tests to make dispatch deterministic.
func (r *DispatchingRecorder) WaitForDispatch() {
	r.wg.Wait()
}
