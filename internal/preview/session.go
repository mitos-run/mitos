package preview

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// SessionCookieName is the __Host- prefixed cookie name, which the browser
// enforces to be Secure, Path=/, and without a Domain attribute.
const SessionCookieName = "__Host-mitos_expose"

// sessionDomainTag domain-separates the session MAC from any other HMAC-SHA256
// use of the same secret in the system.
const sessionDomainTag = "mitos-expose-session-v1\x00"

// sessionPayload is the on-wire JSON shape for a session cookie value.
type sessionPayload struct {
	Sub           string   `json:"sub"`
	Email         string   `json:"email"`
	EmailVerified bool     `json:"email_verified"`
	OrgIDs        []string `json:"org_ids,omitempty"`
	Exp           int64    `json:"exp"`
}

// SessionCodec encodes and decodes HMAC-signed session cookie values.
type SessionCodec struct {
	secret []byte // bearer secret, never logged
}

// NewSessionCodec returns a SessionCodec over secret. It rejects a secret
// shorter than minSecretLen to prevent deployment with a weak key.
//
// Callers should pass a secret DISTINCT from the one given to NewGrantSigner.
// The domain tags make sharing one secret cryptographically safe, but distinct
// secrets limit the blast radius if either one leaks.
func NewSessionCodec(secret []byte) (*SessionCodec, error) {
	if len(secret) < minSecretLen {
		return nil, fmt.Errorf("session signing secret must be at least %d bytes; configure a longer secret", minSecretLen)
	}
	cp := make([]byte, len(secret))
	copy(cp, secret)
	return &SessionCodec{secret: cp}, nil
}

// Encode returns a signed cookie value carrying id until expiresAt. The
// returned string is a bearer credential.
func (c *SessionCodec) Encode(id Identity, expiresAt time.Time) (string, error) {
	pl := sessionPayload{
		Sub:           id.Sub,
		Email:         id.Email,
		EmailVerified: id.EmailVerified,
		OrgIDs:        id.OrgIDs,
		Exp:           expiresAt.Unix(),
	}
	raw, err := json.Marshal(pl)
	if err != nil {
		return "", fmt.Errorf("session: marshal payload: %w", err)
	}
	encPayload := base64.RawURLEncoding.EncodeToString(raw)
	tag := c.sessionTag(encPayload)
	encTag := base64.RawURLEncoding.EncodeToString(tag)
	return encPayload + "." + encTag, nil
}

// Decode validates a cookie value as of now and returns the Identity on
// success. Sessions are reusable until expiry; there is no single-use check.
func (c *SessionCodec) Decode(value string, now time.Time) (Identity, error) {
	encPayload, encTag, ok := splitToken(value)
	if !ok {
		return Identity{}, errors.New("session: malformed value")
	}
	gotTag, err := base64.RawURLEncoding.DecodeString(encTag)
	if err != nil {
		return Identity{}, errors.New("session: malformed value signature")
	}
	wantTag := c.sessionTag(encPayload)
	if subtle.ConstantTimeCompare(gotTag, wantTag) != 1 {
		return Identity{}, errors.New("session: value signature does not verify")
	}
	raw, err := base64.RawURLEncoding.DecodeString(encPayload)
	if err != nil {
		return Identity{}, errors.New("session: malformed value payload")
	}
	var pl sessionPayload
	if err := json.Unmarshal(raw, &pl); err != nil {
		return Identity{}, errors.New("session: malformed value payload")
	}
	exp := time.Unix(pl.Exp, 0)
	if now.After(exp) {
		return Identity{}, errors.New("session: value expired")
	}
	return Identity{
		Sub:           pl.Sub,
		Email:         pl.Email,
		EmailVerified: pl.EmailVerified,
		OrgIDs:        pl.OrgIDs,
	}, nil
}

// sessionTag computes the detached HMAC over the session domain tag and encoded
// payload.
func (c *SessionCodec) sessionTag(encPayload string) []byte {
	mac := hmac.New(sha256.New, c.secret)
	mac.Write([]byte(sessionDomainTag))
	mac.Write([]byte(encPayload))
	return mac.Sum(nil)
}

// NewSessionCookie returns an http.Cookie ready to be set on the response. It
// enforces the __Host- prefix requirements: Secure, HttpOnly, SameSite=Lax,
// Path="/", no Domain attribute.
func NewSessionCookie(value string, ttl time.Duration) *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    value,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Path:     "/",
		MaxAge:   int(ttl.Seconds()),
		// Domain intentionally unset: __Host- prefix requires host-only binding.
	}
}
