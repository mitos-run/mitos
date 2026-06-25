package preview

import (
	"context"
	"net/http"
	"strings"
)

// ForwardAuth performs a subrequest to authURL to check whether r is allowed.
// It sets X-Forwarded-Method, X-Forwarded-Uri, and X-Forwarded-Host from r,
// and forwards r's Cookie header. The request body is never sent.
//
// On a 2xx response, allow is true and id carries the parsed Identity from the
// X-Auth-Request-* response headers. copyHeaders contains every X-Auth-Request-*
// response header so the proxy can forward them upstream.
//
// On non-2xx, allow is false and status carries the auth service's status code.
// err is non-nil only on transport/network failures.
//
// Callers MUST call StripForwardAuthHeaders on the inbound request headers before
// calling ForwardAuth and before proxying upstream, so a client cannot spoof
// identity by injecting X-Auth-Request-* headers.
func ForwardAuth(ctx context.Context, client *http.Client, authURL string, r *http.Request) (allow bool, id *Identity, copyHeaders http.Header, status int, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, authURL, nil)
	if err != nil {
		return false, nil, nil, 0, err
	}
	req.Header.Set("X-Forwarded-Method", r.Method)
	req.Header.Set("X-Forwarded-Uri", r.URL.RequestURI())
	req.Header.Set("X-Forwarded-Host", r.Host)
	if cookie := r.Header.Get("Cookie"); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}

	resp, err := client.Do(req)
	if err != nil {
		return false, nil, nil, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return false, nil, nil, resp.StatusCode, nil
	}

	// Build Identity from X-Auth-Request-* response headers.
	id = &Identity{}
	id.Email = resp.Header.Get("X-Auth-Request-Email")
	id.Sub = resp.Header.Get("X-Auth-Request-User")
	if verifiedRaw := resp.Header.Get("X-Auth-Request-Verified-Email"); verifiedRaw == "true" {
		id.EmailVerified = true
	}
	if groupsRaw := resp.Header.Get("X-Auth-Request-Groups"); groupsRaw != "" {
		parts := strings.Split(groupsRaw, ",")
		groups := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				groups = append(groups, p)
			}
		}
		id.OrgIDs = groups
	}

	// Collect every X-Auth-Request-* response header to pass upstream.
	copyHeaders = make(http.Header)
	for k, vs := range resp.Header {
		if strings.HasPrefix(http.CanonicalHeaderKey(k), "X-Auth-Request-") {
			copyHeaders[http.CanonicalHeaderKey(k)] = append([]string(nil), vs...)
		}
	}

	return true, id, copyHeaders, resp.StatusCode, nil
}

// StripForwardAuthHeaders deletes all headers from h whose canonical form
// starts with "X-Auth-Request-". This MUST be called on the inbound request
// before ForwardAuth and before proxying upstream so a client cannot spoof
// identity by sending X-Auth-Request-* headers themselves.
func StripForwardAuthHeaders(h http.Header) {
	for k := range h {
		if strings.HasPrefix(http.CanonicalHeaderKey(k), "X-Auth-Request-") {
			delete(h, k)
		}
	}
}
