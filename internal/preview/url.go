package preview

import (
	"errors"
	"net/url"
	"time"
)

// MintURL builds the full signed preview URL a caller dials to reach a sandbox
// port: https://<sandbox-id>.preview.<domain>/?token=<signed>. The token binds
// the sandbox id, the port, and the expiry; the host vhost selects the route.
// This is the single mint point the controller / sandbox-server expose so the
// SDK get_host(port) returns one well-formed signed URL. The returned URL
// carries a bearer credential and must be treated as a secret.
func MintURL(s *Signer, domain, sandboxID string, port int, expiresAt time.Time) (string, error) {
	if domain == "" {
		return "", errors.New("preview: domain must not be empty")
	}
	tok, err := s.Mint(sandboxID, port, expiresAt)
	if err != nil {
		return "", err
	}
	u := url.URL{
		Scheme:   "https",
		Host:     sandboxID + "." + previewLabel + "." + domain,
		Path:     "/",
		RawQuery: url.Values{"token": {tok}}.Encode(),
	}
	return u.String(), nil
}
