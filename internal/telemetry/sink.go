package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

// NoopSink discards every batch. It is the default sink so a misconfigured
// "enabled" with no endpoint sends nothing rather than erroring; the FromEnv
// constructor refuses to enable without a real sink, so NoopSink is only ever
// reached by an explicit caller choice.
type NoopSink struct{}

// Send discards the batch.
func (NoopSink) Send(context.Context, []sentEvent) error { return nil }

// StdoutSink writes each event as one JSON line (JSON Lines) to a writer
// (os.Stdout by default). It is the seam for self-host operators who want to run
// their OWN pipeline: point telemetry at stdout and ship the lines with their
// existing log/forwarding stack, with no network egress from Mitos. The lines
// carry only sanitized events (hashed org id, deny-listed properties), so they
// are safe to forward.
type StdoutSink struct {
	mu sync.Mutex
	w  io.Writer
}

// NewStdoutSink returns a StdoutSink writing to w. A nil w defaults to os.Stdout.
func NewStdoutSink(w io.Writer) *StdoutSink {
	if w == nil {
		w = os.Stdout
	}
	return &StdoutSink{w: w}
}

// Send writes each event as a JSON line.
func (s *StdoutSink) Send(_ context.Context, events []sentEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	enc := json.NewEncoder(s.w)
	for i := range events {
		if err := enc.Encode(events[i]); err != nil {
			return fmt.Errorf("telemetry stdout encode: %w", err)
		}
	}
	return nil
}

// HTTPSink POSTs a batch as a single JSON document to a configured endpoint. It
// is the network sink. It is chosen over an OTLP exporter deliberately: the OTel
// exporters in this repo carry SPANS (request-path tracing), not arbitrary
// product-analytics events, so reusing them would be a semantic mismatch and
// would pull in an OTLP-logs exporter not currently in go.mod. A plain JSON POST
// adds ZERO new dependencies (net/http + encoding/json) and is the most portable
// target for a self-host operator pointing telemetry at their own collector
// (PostHog, Segment-compatible, or a custom endpoint).
//
// An optional bearer token authenticates the POST. The token is a SECRET: it is
// set only from a secretKeyRef-sourced env var and is never logged.
type HTTPSink struct {
	endpoint string
	token    string
	client   *http.Client
}

// NewHTTPSink builds an HTTPSink for endpoint. An empty token sends no
// Authorization header. A nil client defaults to one with a short timeout so a
// slow collector never stalls the flush loop.
func NewHTTPSink(endpoint, token string, client *http.Client) *HTTPSink {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return &HTTPSink{endpoint: endpoint, token: token, client: client}
}

// batchPayload is the on-wire shape of an HTTP batch: a JSON object with an
// events array, so a receiver can add envelope metadata later without breaking
// the contract.
type batchPayload struct {
	Events []sentEvent `json:"events"`
}

// Send POSTs the batch as JSON. A non-2xx response is an error so the flush loop
// can observe (and swallow) the failure; the body is never echoed into the error
// to avoid leaking a collector-side message. The bearer token is attached as a
// header and never logged.
func (h *HTTPSink) Send(ctx context.Context, events []sentEvent) error {
	body, err := json.Marshal(batchPayload{Events: events})
	if err != nil {
		return fmt.Errorf("telemetry http marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telemetry http request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if h.token != "" {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("telemetry http send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain so the connection can be reused; ignore the content.
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("telemetry http send: unexpected status %d", resp.StatusCode)
	}
	return nil
}
