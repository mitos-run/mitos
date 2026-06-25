package preview

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Identity carries the authenticated user claims that a grant or session encodes.
// Tasks 3 and 4 reuse this type.
type Identity struct {
	Sub           string   `json:"sub"`
	Email         string   `json:"email"`
	EmailVerified bool     `json:"email_verified"`
	OrgIDs        []string `json:"org_ids,omitempty"`
}

// grantDomainTag domain-separates the grant MAC from any other HMAC-SHA256 use
// of the same secret in the system.
const grantDomainTag = "mitos-expose-grant-v1\x00"

// grantPayload is the on-wire JSON shape for a grant token.
type grantPayload struct {
	Label         string   `json:"label"`
	Sub           string   `json:"sub"`
	Email         string   `json:"email"`
	EmailVerified bool     `json:"email_verified"`
	OrgIDs        []string `json:"org_ids,omitempty"`
	Exp           int64    `json:"exp"`
	Nonce         string   `json:"nonce"` // base64url of 16 random bytes
}

// nonceEntry records the expiry of a nonce so stale entries can be lazily GCed.
type nonceEntry struct {
	exp time.Time
}

// GrantSigner mints and verifies short-lived, single-use HMAC grant tokens.
type GrantSigner struct {
	secret []byte   // bearer secret, never logged
	used   sync.Map // nonce (string) -> nonceEntry
}

// NewGrantSigner returns a GrantSigner over secret. It rejects a secret shorter
// than minSecretLen to prevent deployment with a weak key.
//
// Callers should pass a secret DISTINCT from the one given to NewSessionCodec.
// The domain tags make sharing one secret cryptographically safe, but distinct
// secrets limit the blast radius if either one leaks.
func NewGrantSigner(secret []byte) (*GrantSigner, error) {
	if len(secret) < minSecretLen {
		return nil, fmt.Errorf("grant signing secret must be at least %d bytes; configure a longer secret", minSecretLen)
	}
	cp := make([]byte, len(secret))
	copy(cp, secret)
	return &GrantSigner{secret: cp}, nil
}

// Mint returns a signed, single-use grant token binding label and id until
// expiresAt. The returned string is a bearer credential.
func (g *GrantSigner) Mint(label string, id Identity, expiresAt time.Time) (string, error) {
	if label == "" {
		return "", errors.New("grant: label must not be empty")
	}
	nonceBuf := make([]byte, 16)
	if _, err := rand.Read(nonceBuf); err != nil {
		return "", fmt.Errorf("grant: generate nonce: %w", err)
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceBuf)
	pl := grantPayload{
		Label:         label,
		Sub:           id.Sub,
		Email:         id.Email,
		EmailVerified: id.EmailVerified,
		OrgIDs:        id.OrgIDs,
		Exp:           expiresAt.Unix(),
		Nonce:         nonce,
	}
	raw, err := json.Marshal(pl)
	if err != nil {
		return "", fmt.Errorf("grant: marshal payload: %w", err)
	}
	encPayload := base64.RawURLEncoding.EncodeToString(raw)
	tag := g.grantTag(encPayload)
	encTag := base64.RawURLEncoding.EncodeToString(tag)
	return encPayload + "." + encTag, nil
}

// Verify checks token against label and now, enforces single-use via the nonce
// cache, and returns the Identity on success.
func (g *GrantSigner) Verify(token, label string, now time.Time) (Identity, error) {
	encPayload, encTag, ok := splitToken(token)
	if !ok {
		return Identity{}, errors.New("grant: malformed token")
	}
	gotTag, err := base64.RawURLEncoding.DecodeString(encTag)
	if err != nil {
		return Identity{}, errors.New("grant: malformed token signature")
	}
	wantTag := g.grantTag(encPayload)
	if subtle.ConstantTimeCompare(gotTag, wantTag) != 1 {
		return Identity{}, errors.New("grant: token signature does not verify")
	}
	raw, err := base64.RawURLEncoding.DecodeString(encPayload)
	if err != nil {
		return Identity{}, errors.New("grant: malformed token payload")
	}
	var pl grantPayload
	if err := json.Unmarshal(raw, &pl); err != nil {
		return Identity{}, errors.New("grant: malformed token payload")
	}
	exp := time.Unix(pl.Exp, 0)
	if now.After(exp) {
		return Identity{}, errors.New("grant: token expired")
	}
	if pl.Label != label {
		return Identity{}, errors.New("grant: label mismatch")
	}
	// Single-use check: record nonce or reject if already seen.
	if _, loaded := g.used.LoadOrStore(pl.Nonce, nonceEntry{exp: exp}); loaded {
		return Identity{}, errors.New("grant: token already used")
	}
	// Lazy GC: evict expired nonces in a best-effort pass.
	g.used.Range(func(k, v any) bool {
		if entry, ok := v.(nonceEntry); ok && now.After(entry.exp) {
			g.used.Delete(k)
		}
		return true
	})
	return Identity{
		Sub:           pl.Sub,
		Email:         pl.Email,
		EmailVerified: pl.EmailVerified,
		OrgIDs:        pl.OrgIDs,
	}, nil
}

// grantTag computes the detached HMAC over the grant domain tag and encoded
// payload.
func (g *GrantSigner) grantTag(encPayload string) []byte {
	mac := hmac.New(sha256.New, g.secret)
	mac.Write([]byte(grantDomainTag))
	mac.Write([]byte(encPayload))
	return mac.Sum(nil)
}
