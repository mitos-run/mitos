package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	exposePosterDefaultMaxAttempts = 3
	exposePosterDefaultBackoff     = 200 * time.Millisecond
	exposePosterDefaultTimeout     = 5 * time.Second
)

// ExposePoster POSTs the full route set to the proxy admin endpoint with a
// bearer credential. It retries on 5xx and transport errors up to MaxAttempts
// with a fixed Backoff between attempts; 4xx responses are terminal (no retry).
// The Token is set only on the request header and is never logged or included
// in error messages.
type ExposePoster struct {
	// URL is the proxy admin endpoint base, e.g. "http://proxy:9092".
	// An empty URL makes Sync a no-op.
	URL string
	// Token is the admin bearer credential. Never logged.
	Token       string
	Client      *http.Client
	MaxAttempts int
	Backoff     time.Duration
}

// NewExposePoster returns an ExposePoster configured with sane defaults. An
// empty url returns a poster whose Sync is a no-op; callers need not nil-check.
func NewExposePoster(url, token string) *ExposePoster {
	return &ExposePoster{
		// Trim a trailing slash so URL + "/internal/routes" never yields a
		// double slash.
		URL:         strings.TrimRight(url, "/"),
		Token:       token,
		Client:      &http.Client{Timeout: exposePosterDefaultTimeout},
		MaxAttempts: exposePosterDefaultMaxAttempts,
		Backoff:     exposePosterDefaultBackoff,
	}
}

// Sync POSTs {"routes":routes} to the proxy admin endpoint. It returns nil on
// 2xx, retries on 5xx or transport error up to MaxAttempts (with Backoff
// between attempts), and returns a terminal error on 4xx without retrying.
// A no-op when URL is empty.
func (p *ExposePoster) Sync(ctx context.Context, routes []ExposeRoute) error {
	if p.URL == "" {
		return nil
	}

	payload := struct {
		Routes []ExposeRoute `json:"routes"`
	}{Routes: routes}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal expose routes: %w", err)
	}

	attempts := p.MaxAttempts
	if attempts <= 0 {
		attempts = exposePosterDefaultMaxAttempts
	}
	backoff := p.Backoff
	if backoff <= 0 {
		backoff = exposePosterDefaultBackoff
	}
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: exposePosterDefaultTimeout}
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("expose route sync cancelled: %w", ctx.Err())
			case <-time.After(backoff):
			}
		}

		retryable, err := p.post(ctx, client, body)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable {
			return err
		}
	}
	return fmt.Errorf("expose route sync failed after %d attempts: %w", attempts, lastErr)
}

// post sends one POST to the admin endpoint. It returns whether the failure is
// retryable (transport error or 5xx) and the error. A 2xx returns (false, nil).
// A 4xx returns (false, err). The bearer token is set only on the header.
func (p *ExposePoster) post(ctx context.Context, client *http.Client, body []byte) (retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL+"/internal/routes", bytes.NewReader(body))
	if err != nil {
		// Malformed URL is a permanent configuration error.
		return false, fmt.Errorf("build expose route request for %s: %w", p.URL, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.Token)

	resp, err := client.Do(req)
	if err != nil {
		// Transport error: transient, retry.
		return true, fmt.Errorf("post expose routes to %s: %w", p.URL, err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return false, nil
	case resp.StatusCode >= 500:
		return true, fmt.Errorf("expose route sync: %s returned status %d", p.URL, resp.StatusCode)
	default:
		return false, fmt.Errorf("expose route sync: %s returned status %d", p.URL, resp.StatusCode)
	}
}
