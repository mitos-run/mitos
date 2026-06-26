package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// InternalOrgHeader carries the org id on the controller's INTERNAL usage API
// (the machine-to-machine seam the console reads). The endpoint is bearer-gated
// by a shared secret and is NOT browser-facing, so the org is supplied by the
// trusted caller (the console, which itself derived the org from the
// gateway-verified session) in this header, exactly as the identity-resolve
// endpoint trusts its caller. It is never read from a public, session-bearing
// path; the public usage API (UsageHandler) reads the org from the context the
// gateway attached, never from a header.
const InternalOrgHeader = "X-Mitos-Org"

// ErrReadOnly is returned by a read-only UsageStore (the HTTP-backed store the
// console reads through) on a write. The console never writes usage; only the
// controller's collector does, into its own store.
var ErrReadOnly = errors.New("usage: store is read-only")

// HTTPStore is a read-only UsageStore that reads an org's usage records from the
// controller's INTERNAL usage API over HTTP. It is the seam that lets the
// console (a separate process) read the SAME usage the controller's collector
// recorded, without a shared database: the controller mounts the internal usage
// API over its collector store, and the console reads it through this client.
//
// SECURITY: the request carries the internal bearer token (a shared secret,
// never logged) and the org id in InternalOrgHeader. The org is supplied by the
// console, which derived it from the gateway-verified session, so a tenant can
// never read another org's usage: the console only ever sets its own caller's
// org. The endpoint is M2M and not reachable by a browser session.
type HTTPStore struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewHTTPStore builds the read-only HTTP-backed usage store over the controller's
// internal usage API base URL (for example http://mitos-controller:8092). The
// token is the internal bearer shared secret. client may be nil (a default
// client with a bounded timeout is used).
func NewHTTPStore(baseURL, token string, client *http.Client) *HTTPStore {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &HTTPStore{baseURL: baseURL, token: token, client: client}
}

// UpsertRecord is a no-op error: the console never writes usage, only reads it.
func (s *HTTPStore) UpsertRecord(_ context.Context, _ UsageRecord) error {
	return ErrReadOnly
}

// ListRecords reads the org's records in [from, to) from the controller's
// internal usage API. The org is sent in InternalOrgHeader (the M2M trust
// boundary), the optional window bounds as RFC3339 query parameters. It returns
// only the named org's records: the controller's handler scopes the store read
// to that org.
func (s *HTTPStore) ListRecords(ctx context.Context, orgID string, from, to time.Time) ([]UsageRecord, error) {
	u, err := url.Parse(s.baseURL + "/internal/usage")
	if err != nil {
		return nil, fmt.Errorf("parse internal usage url: %w", err)
	}
	q := u.Query()
	if !from.IsZero() {
		q.Set("from", from.UTC().Format(time.RFC3339))
	}
	if !to.IsZero() {
		q.Set("to", to.UTC().Format(time.RFC3339))
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build internal usage request: %w", err)
	}
	req.Header.Set(InternalOrgHeader, orgID)
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("read internal usage: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("internal usage api returned status %d", resp.StatusCode)
	}
	var out UsageResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode internal usage: %w", err)
	}
	if out.Records == nil {
		return []UsageRecord{}, nil
	}
	return out.Records, nil
}
