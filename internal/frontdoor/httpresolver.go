package frontdoor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// compile-time interface check.
var _ SessionResolver = (*HTTPSessionResolver)(nil)

// HTTPSessionResolver resolves a session token by calling the console's
// POST /internal/session/resolve endpoint. The bearer token and session token
// values are never logged.
type HTTPSessionResolver struct {
	resolveURL string
	token      string
	client     *http.Client
}

// NewHTTPSessionResolver constructs an HTTPSessionResolver. If client is nil, a
// default client with a 5-second timeout is used.
func NewHTTPSessionResolver(resolveURL, token string, client *http.Client) *HTTPSessionResolver {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return &HTTPSessionResolver{
		resolveURL: resolveURL,
		token:      token,
		client:     client,
	}
}

// Resolve calls the console session-resolve endpoint and maps the response to
// an Identity. It returns ErrNoSession on a 401 response, and a wrapped error
// for any other non-200 status or transport failure.
// The session token and bearer token are never logged.
func (r *HTTPSessionResolver) Resolve(ctx context.Context, sessionToken string) (Identity, error) {
	payload, err := json.Marshal(struct {
		Session string `json:"session"`
	}{Session: sessionToken})
	if err != nil {
		return Identity{}, fmt.Errorf("frontdoor: marshal session request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.resolveURL, bytes.NewReader(payload))
	if err != nil {
		return Identity{}, fmt.Errorf("frontdoor: build session resolve request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.token)

	resp, err := r.client.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("frontdoor: session resolve request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode == http.StatusUnauthorized {
		return Identity{}, ErrNoSession
	}
	if resp.StatusCode != http.StatusOK {
		return Identity{}, fmt.Errorf("frontdoor: session resolve: unexpected status %d", resp.StatusCode)
	}

	var body struct {
		AccountID string `json:"accountId"`
		OrgID     string `json:"orgId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Identity{}, fmt.Errorf("frontdoor: session resolve: decode response: %w", err)
	}

	return Identity{AccountID: body.AccountID, OrgID: body.OrgID}, nil
}
