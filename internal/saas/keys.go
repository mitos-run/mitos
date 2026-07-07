package saas

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// keyPrefix is the tag every raw key carries so a leaked key is greppable and a
// scanner can recognize a Mitos credential. The masked Prefix stored on ApiKey
// is this tag plus a short non-secret slug.
const keyPrefix = "mitos_live_"

// rawKeyBytes is the entropy of the secret segment (256 bits), encoded
// url-safe-base64 without padding into the raw key body.
const rawKeyBytes = 32

// Verification failure reasons. Verify returns one of these so the gateway maps
// them all to a single public unauthorized envelope (a probe cannot tell which),
// while internal logging and tests can still discriminate. None of these errors
// ever carries the raw key value.
var (
	// ErrKeyMalformed: the presented credential is not a Mitos key shape.
	ErrKeyMalformed = errors.New("saas: api key is malformed")
	// ErrKeyUnknown: the key does not resolve to any stored key.
	ErrKeyUnknown = errors.New("saas: api key is not recognized")
	// ErrKeyExpired: the key resolved but is past its expiry.
	ErrKeyExpired = errors.New("saas: api key has expired")
	// ErrKeyRevoked: the key resolved but has been revoked.
	ErrKeyRevoked = errors.New("saas: api key has been revoked")
	// ErrKeyScope: the key resolved and is live but lacks the required scope.
	ErrKeyScope = errors.New("saas: api key lacks the required scope")
	// ErrKeyWrongOrg: the key resolved but is bound to a different org than the
	// one the request targets. This is the cross-org isolation backstop.
	ErrKeyWrongOrg = errors.New("saas: api key is not valid for this organization")
)

// hashSalt is a process-wide pepper mixed into every key hash so a stolen store
// dump alone cannot be brute-forced without it. In production it is loaded from
// a secret (documented follow-up); the default empty salt still gives a sound
// sha256 hash. The salt VALUE is never logged.
//
// KeyService is the issuance and verification service for scoped API keys. It is
// the load-bearing correctness of the SaaS front door: it mints prefix-tagged
// keys, stores ONLY a salted hash, and verifies presented keys in constant time
// against the stored hash, checking expiry, revocation, and scope. The raw key
// is returned exactly once at creation and is never stored, logged, or placed in
// an error.
type KeyService struct {
	store Store
	salt  []byte
	now   func() time.Time
	// idgen generates opaque ids for accounts, orgs, and keys. Injectable so tests
	// are deterministic; the default is a random url-safe id.
	idgen func() string
	// homeRegion is the deployment's placement registry default, threaded
	// through NewAccountService's options reuse (see WithHomeRegion) so a
	// newly minted org is stamped with it. Only NewAccountService reads this
	// field; KeyService itself has no notion of region.
	homeRegion string
}

// KeyServiceOption configures a KeyService.
type KeyServiceOption func(*KeyService)

// WithSalt sets the hash pepper. The value is never logged.
func WithSalt(salt []byte) KeyServiceOption {
	return func(s *KeyService) { s.salt = append([]byte(nil), salt...) }
}

// EnvKeyPepper is the environment variable that holds the process-wide API key
// hash pepper. When set, the SAME value MUST be configured on every component
// that creates or verifies keys (gateway, console, CLI); a mismatch makes keys
// fail to verify. It is opt-in: unset keeps the current unsalted hash, so an
// existing store keeps working until the pepper is deliberately introduced (at
// which point pre-existing keys must be reissued).
const EnvKeyPepper = "MITOS_API_KEY_PEPPER"

// KeyPepperFromEnv reads the API key pepper from EnvKeyPepper. It returns the
// pepper bytes and whether a non-empty value was set. The value is never logged.
func KeyPepperFromEnv() ([]byte, bool) {
	v := os.Getenv(EnvKeyPepper)
	if v == "" {
		return nil, false
	}
	return []byte(v), true
}

// WithClock sets the time source (for deterministic tests).
func WithClock(now func() time.Time) KeyServiceOption {
	return func(s *KeyService) { s.now = now }
}

// WithIDGen sets the id generator (for deterministic tests).
func WithIDGen(gen func() string) KeyServiceOption {
	return func(s *KeyService) { s.idgen = gen }
}

// WithHomeRegion sets the deployment's placement registry default region
// (placement.Registry.DefaultName()), stamped on every org SignUp mints from
// here on. Passed to NewAccountService, not NewKeyService directly; a
// KeyService built for key issuance alone has no use for it.
func WithHomeRegion(region string) KeyServiceOption {
	return func(s *KeyService) { s.homeRegion = region }
}

// NewKeyService builds a key service over store.
func NewKeyService(store Store, opts ...KeyServiceOption) *KeyService {
	s := &KeyService{
		store: store,
		now:   time.Now,
		idgen: randomID,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// CreateKeyRequest is the input to CreateKey.
type CreateKeyRequest struct {
	OrgID  string
	Name   string
	Scopes []string
	// TTL is the lifetime; zero means the key never expires.
	TTL time.Duration
}

// CreatedKey is the result of CreateKey. RawKey is the secret, shown EXACTLY
// ONCE; the caller must surface it to the user and then discard it. It is never
// stored. Record is the stored metadata (hash, prefix, scopes), safe to persist
// and list.
type CreatedKey struct {
	RawKey string
	Record ApiKey
}

// CreateKey mints a new scoped key for an org. The returned RawKey is the only
// time the secret exists outside the user's possession; the store keeps only its
// salted hash and the masked prefix. CreateKey never logs or returns the raw key
// in any error.
func (s *KeyService) CreateKey(ctx context.Context, req CreateKeyRequest) (CreatedKey, error) {
	if req.OrgID == "" {
		return CreatedKey{}, fmt.Errorf("create key: org id is required")
	}
	// Confirm the org exists so a key cannot be minted for a non-existent org.
	if _, err := s.store.GetOrg(ctx, req.OrgID); err != nil {
		return CreatedKey{}, fmt.Errorf("create key: resolve org: %w", err)
	}

	secret, err := randomSecret()
	if err != nil {
		return CreatedKey{}, fmt.Errorf("create key: generate secret: %w", err)
	}
	raw := keyPrefix + secret
	masked := maskKey(raw)

	now := s.now()
	rec := ApiKey{
		ID:        s.idgen(),
		OrgID:     req.OrgID,
		Name:      req.Name,
		Prefix:    masked,
		Hash:      s.hash(raw),
		Scopes:    append([]string(nil), req.Scopes...),
		CreatedAt: now,
	}
	if req.TTL > 0 {
		rec.ExpiresAt = now.Add(req.TTL)
	}
	if err := s.store.PutApiKey(ctx, rec); err != nil {
		return CreatedKey{}, fmt.Errorf("create key: store: %w", err)
	}
	return CreatedKey{RawKey: raw, Record: rec}, nil
}

// VerifyResult is the outcome of a successful Verify: the resolved key record
// (with the OrgID the request is now authorized against) and nothing secret.
type VerifyResult struct {
	Key ApiKey
}

// Verify resolves and validates a presented raw key. It checks, in order: the
// key shape; that the salted hash resolves to a stored key; that the key is not
// expired; that it is not revoked; and that it carries requiredScope (empty
// requiredScope means scope is not checked). On any failure it returns a typed
// error and a zero result, and NEVER reveals whether the failure was "unknown
// key" versus "wrong key": the hash lookup is the only resolution path, so a
// forged key simply fails to resolve.
//
// The hash comparison is constant-time: the store lookup is by exact hash, and
// the recomputed hash is compared to the stored hash with subtle.ConstantTimeCompare
// so a timing side channel cannot probe the hash byte by byte.
func (s *KeyService) Verify(ctx context.Context, raw, requiredScope string) (VerifyResult, error) {
	if !strings.HasPrefix(raw, keyPrefix) || len(raw) <= len(keyPrefix) {
		return VerifyResult{}, ErrKeyMalformed
	}
	h := s.hash(raw)
	rec, err := s.store.GetApiKeyByHash(ctx, h)
	if err != nil {
		return VerifyResult{}, ErrKeyUnknown
	}
	// Constant-time confirm the stored hash equals the recomputed hash. The store
	// lookup is by map key, so this guards against a store that returns a near
	// match and keeps the compare timing independent of how many bytes agree.
	if subtle.ConstantTimeCompare([]byte(rec.Hash), []byte(h)) != 1 {
		return VerifyResult{}, ErrKeyUnknown
	}
	now := s.now()
	if rec.IsExpired(now) {
		return VerifyResult{}, ErrKeyExpired
	}
	if rec.IsRevoked() {
		return VerifyResult{}, ErrKeyRevoked
	}
	if requiredScope != "" && !rec.HasScope(requiredScope) {
		return VerifyResult{}, ErrKeyScope
	}
	return VerifyResult{Key: rec}, nil
}

// hash returns the hex sha256 of the salt followed by the raw key. The raw key
// is never returned or logged; only this digest is stored.
func (s *KeyService) hash(raw string) string {
	h := sha256.New()
	h.Write(s.salt)
	h.Write([]byte(raw))
	return hex.EncodeToString(h.Sum(nil))
}

// maskKey returns the safe-to-display prefix of a raw key: the tag plus the
// first 6 characters of the secret body, the rest redacted. This is what is
// stored as ApiKey.Prefix and shown in listings; it reveals no usable secret.
func maskKey(raw string) string {
	body := strings.TrimPrefix(raw, keyPrefix)
	shown := body
	if len(shown) > 6 {
		shown = shown[:6]
	}
	return keyPrefix + shown + "..."
}

// randomSecret returns a url-safe-base64 secret body with rawKeyBytes of entropy.
func randomSecret() (string, error) {
	b := make([]byte, rawKeyBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// randomID returns a short opaque id used for accounts, orgs, and keys. The
// alphabet is lowercase base32 ([a-z2-7]) because org ids reach Kubernetes:
// they are stamped on Sandbox objects as the tenant label value (must begin
// and end alphanumeric) and, in org-tenancy mode, embedded in the org
// namespace name (RFC1123: lowercase only). base64url ids gave ~6% of orgs a
// leading or trailing '-'/'_' that no sandbox create could ever pass (#593).
func randomID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		// rand.Read failing is catastrophic; fall back to a time-derived id rather
		// than panic so the process stays up. This never carries a secret.
		return fmt.Sprintf("id-%d", time.Now().UnixNano())
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}
