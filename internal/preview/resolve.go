package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// ErrResolveDisabled is returned by Resolver.Resolve when no URL is configured.
var ErrResolveDisabled = errors.New("identity resolution is disabled")

// Resolver resolves an email address to an account ID and org IDs by calling
// the SaaS identity resolve endpoint. The Token is a bearer credential and is
// never logged or placed in error messages.
type Resolver struct {
	URL    string
	Token  string
	Client *http.Client
}

// NewResolver returns a Resolver that uses http.DefaultClient. url is the base
// URL of the SaaS service (no trailing slash); token is the internal bearer
// credential, never logged.
func NewResolver(url, token string) *Resolver {
	return &Resolver{
		URL:    url,
		Token:  token,
		Client: http.DefaultClient,
	}
}

// Resolve resolves email to an accountID and orgIDs by posting to the identity
// resolve endpoint. If URL is empty, ErrResolveDisabled is returned immediately
// and no HTTP request is made. The Token is never included in any error message.
func (r *Resolver) Resolve(ctx context.Context, email string) (accountID string, orgIDs []string, err error) {
	if r.URL == "" {
		return "", nil, ErrResolveDisabled
	}

	body, err := json.Marshal(struct {
		Email string `json:"email"`
	}{Email: email})
	if err != nil {
		return "", nil, fmt.Errorf("resolve: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.URL+"/internal/identity/resolve", bytes.NewReader(body))
	if err != nil {
		return "", nil, fmt.Errorf("resolve: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Bearer token is set but never echoed in logs or errors.
	req.Header.Set("Authorization", "Bearer "+r.Token)

	resp, err := r.Client.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("resolve: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Do not include the token in any error message.
		return "", nil, fmt.Errorf("resolve: unexpected status %d", resp.StatusCode)
	}

	var out struct {
		AccountID string   `json:"accountId"`
		OrgIDs    []string `json:"orgIds"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", nil, fmt.Errorf("resolve: decode response: %w", err)
	}
	return out.AccountID, out.OrgIDs, nil
}
