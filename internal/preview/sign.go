// Package preview implements per-sandbox preview URLs: a signed, expiring URL
// (Daytona style) that names a sandbox and a port, plus a reverse proxy that
// resolves <label>.<domain> to the sandbox backend, verifies the
// signed token and the per-sandbox bearer gate, and proxies to the backend
// (issue #126).
//
// Signed-URL scheme. A preview token is a detached HMAC over a small JSON
// payload that binds the sandbox id, the target port, and an absolute
// expiry. The wire form is base64url(payload) + "." + base64url(tag), where
// tag = HMAC-SHA256(serverSecret, domainTag || base64url(payload)). This is the
// same HMAC-SHA256 + constant-time-compare core proven in internal/captoken and
// internal/workspace (SigV4); preview tokens are NOT captokens because they need
// no macaroon attenuation chain, only a single expiring binding, so a focused
// signer keeps the scheme small and auditable. The never-accept-after-expiry and
// never-accept-tampered properties are unit tested in sign_test.go.
//
// Security: a token VALUE is a bearer credential. The serialized token and the
// server secret are never logged, never placed in an error message, condition,
// event, or host path. This package logs nothing in the signing path; callers
// must treat Mint output as a secret.
package preview

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// minSecretLen is the minimum server-secret length, mirroring captoken's
// 16-byte floor so a preview signer cannot be stood up with a trivially
// guessable key.
const minSecretLen = 16

// domainTag domain-separates the preview MAC from any other HMAC-SHA256 use of
// the same secret elsewhere in the system.
const domainTag = "mitos-preview-v1\x00"

// Signer mints and verifies signed, expiring preview URLs over a server secret.
type Signer struct {
	secret []byte // bearer secret, never logged
}

// Claims is the verified content of a preview token: the sandbox it names, the
// backend port it grants, and the absolute expiry it was minted with.
type Claims struct {
	SandboxID string
	Port      int
	ExpiresAt time.Time
}

// payload is the on-wire JSON shape. Field names are short to keep tokens
// compact; ExpiresAt is a Unix second so the token is timezone-free.
type payload struct {
	SandboxID string `json:"s"`
	Port      int    `json:"p"`
	ExpiresAt int64  `json:"e"`
}

// NewSigner returns a Signer over secret. It rejects a secret shorter than
// minSecretLen so a preview deployment cannot run with a weak key.
func NewSigner(secret []byte) (*Signer, error) {
	if len(secret) < minSecretLen {
		return nil, fmt.Errorf("preview signing secret must be at least %d bytes; configure a longer MITOS_PREVIEW_SECRET", minSecretLen)
	}
	cp := make([]byte, len(secret))
	copy(cp, secret)
	return &Signer{secret: cp}, nil
}

// Mint returns a signed token binding sandboxID and port until expiresAt. The
// returned string is a bearer credential and must be treated as a secret.
func (s *Signer) Mint(sandboxID string, port int, expiresAt time.Time) (string, error) {
	if sandboxID == "" {
		return "", errors.New("preview: sandbox id must not be empty")
	}
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("preview: port %d out of range 1-65535", port)
	}
	pl := payload{SandboxID: sandboxID, Port: port, ExpiresAt: expiresAt.Unix()}
	raw, err := json.Marshal(pl)
	if err != nil {
		return "", fmt.Errorf("preview: marshal payload: %w", err)
	}
	encPayload := base64.RawURLEncoding.EncodeToString(raw)
	tag := s.tag(encPayload)
	encTag := base64.RawURLEncoding.EncodeToString(tag)
	return encPayload + "." + encTag, nil
}

// Verify checks token against the current wall clock and returns its claims.
func (s *Signer) Verify(token string) (Claims, error) {
	return s.VerifyAt(token, time.Now())
}

// VerifyAt checks token as of now: it validates the HMAC tag in constant time
// and rejects an expired token. now is injectable so the expiry boundary is
// deterministically testable.
func (s *Signer) VerifyAt(token string, now time.Time) (Claims, error) {
	encPayload, encTag, ok := splitToken(token)
	if !ok {
		return Claims{}, errors.New("preview: malformed token")
	}
	gotTag, err := base64.RawURLEncoding.DecodeString(encTag)
	if err != nil {
		return Claims{}, errors.New("preview: malformed token signature")
	}
	wantTag := s.tag(encPayload)
	if subtle.ConstantTimeCompare(gotTag, wantTag) != 1 {
		return Claims{}, errors.New("preview: token signature does not verify")
	}
	raw, err := base64.RawURLEncoding.DecodeString(encPayload)
	if err != nil {
		return Claims{}, errors.New("preview: malformed token payload")
	}
	var pl payload
	if err := json.Unmarshal(raw, &pl); err != nil {
		return Claims{}, errors.New("preview: malformed token payload")
	}
	exp := time.Unix(pl.ExpiresAt, 0)
	if now.After(exp) {
		return Claims{}, errors.New("preview: token expired")
	}
	return Claims{SandboxID: pl.SandboxID, Port: pl.Port, ExpiresAt: exp}, nil
}

// tag computes the detached HMAC over the domain tag and the encoded payload.
func (s *Signer) tag(encPayload string) []byte {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(domainTag))
	mac.Write([]byte(encPayload))
	return mac.Sum(nil)
}

// splitToken splits a "payload.signature" token without allocating on the happy
// path; it returns ok=false unless there is exactly one separator.
func splitToken(token string) (payloadPart, sigPart string, ok bool) {
	for i := 0; i < len(token); i++ {
		if token[i] == '.' {
			payloadPart = token[:i]
			sigPart = token[i+1:]
			// Reject a second separator or empty halves.
			if payloadPart == "" || sigPart == "" {
				return "", "", false
			}
			for j := i + 1; j < len(token); j++ {
				if token[j] == '.' {
					return "", "", false
				}
			}
			return payloadPart, sigPart, true
		}
	}
	return "", "", false
}
