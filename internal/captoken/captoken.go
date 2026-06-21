// Package captoken implements macaroon-style attenuated capability tokens for
// per-sandbox runtime authorization (issue #25, docs/api/v2-spec.md section 3,
// design in docs/api/capability-budgets.md).
//
// A token carries a capability Budget (the five creator-set maxima) and a set of
// Scopes, sealed with an HMAC-SHA256 caveat chain. The load-bearing correctness
// property is that attenuation can NEVER widen: a child token derived from a
// parent is element-wise no greater on any budget field and holds a subset of
// the parent's scopes. The token embeds the claims of EVERY link in its chain,
// and Verify walks the chain checking both the HMAC integrity of each link and
// the never-widen invariant between adjacent links, so even an actor who holds
// the signing key cannot forge a wider child: widening one link's claims is
// rejected structurally, independent of whether its HMAC re-seals cleanly.
//
// Security: a token VALUE is a bearer credential. The serialized form and the
// HMAC tags are never logged, never placed in an error message, condition,
// event, or host path. This package logs nothing; callers must treat Serialize
// output as a secret. The HMAC primitive (crypto/hmac + crypto/sha256) is the
// standard library; no new crypto is invented here.
package captoken

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// Scope is a coarse runtime capability a token may carry. Scopes are a subset
// lattice: a child token may drop scopes but never add one its parent lacks.
type Scope string

// The runtime scopes that map to the §4 imperative surface. Fork, Checkpoint,
// and Extend are the budget-gated self-service operations of §3.
const (
	ScopeExec       Scope = "exec"
	ScopeFiles      Scope = "files"
	ScopeFork       Scope = "fork"
	ScopeCheckpoint Scope = "checkpoint"
	ScopeExtend     Scope = "extend"
	ScopeNetwork    Scope = "network"
)

// Budget is the creator-set capability budget carried by a sandbox token. Each
// field is a non-negative ceiling; the five fields mirror the §3 budget block
// (maxForks, maxCheckpoints, maxCpuSeconds, maxLifetimeExtension, maxEgressBytes).
// Counts and CPU/lifetime seconds and egress bytes are all expressed as int64 so
// the same arithmetic (min, subtract-with-floor) applies uniformly. The Go shape
// mirrors the api/v1alpha1 Budget design type; this package owns the
// token-bearing copy so internal/captoken stays free of a Kubernetes dependency.
type Budget struct {
	MaxForks             int64 `json:"maxForks"`
	MaxCheckpoints       int64 `json:"maxCheckpoints"`
	MaxCpuSeconds        int64 `json:"maxCpuSeconds"`
	MaxLifetimeExtension int64 `json:"maxLifetimeExtension"`
	MaxEgressBytes       int64 `json:"maxEgressBytes"`
}

// BudgetSpend is the amount of a budget already consumed, used to compute the
// remaining budget a parent may delegate to a child (budget minus spend).
type BudgetSpend struct {
	Forks             int64 `json:"forks"`
	Checkpoints       int64 `json:"checkpoints"`
	CpuSeconds        int64 `json:"cpuSeconds"`
	LifetimeExtension int64 `json:"lifetimeExtension"`
	EgressBytes       int64 `json:"egressBytes"`
}

// Remaining returns the budget left after subtracting spend, floored at zero on
// every field. This is the upper bound on what a parent may delegate.
func (b Budget) Remaining(spend BudgetSpend) Budget {
	return Budget{
		MaxForks:             subFloor(b.MaxForks, spend.Forks),
		MaxCheckpoints:       subFloor(b.MaxCheckpoints, spend.Checkpoints),
		MaxCpuSeconds:        subFloor(b.MaxCpuSeconds, spend.CpuSeconds),
		MaxLifetimeExtension: subFloor(b.MaxLifetimeExtension, spend.LifetimeExtension),
		MaxEgressBytes:       subFloor(b.MaxEgressBytes, spend.EgressBytes),
	}
}

// Intersect returns the element-wise minimum of two budgets. The result is no
// greater than either operand on any field, which is the never-widen guarantee
// at the budget level.
func (b Budget) Intersect(other Budget) Budget {
	return Budget{
		MaxForks:             min64(b.MaxForks, other.MaxForks),
		MaxCheckpoints:       min64(b.MaxCheckpoints, other.MaxCheckpoints),
		MaxCpuSeconds:        min64(b.MaxCpuSeconds, other.MaxCpuSeconds),
		MaxLifetimeExtension: min64(b.MaxLifetimeExtension, other.MaxLifetimeExtension),
		MaxEgressBytes:       min64(b.MaxEgressBytes, other.MaxEgressBytes),
	}
}

// fitsWithin reports whether b is element-wise no greater than parent on every
// budget field. This is the budget half of the never-widen check Verify walks.
func (b Budget) fitsWithin(parent Budget) bool {
	return b.MaxForks <= parent.MaxForks &&
		b.MaxCheckpoints <= parent.MaxCheckpoints &&
		b.MaxCpuSeconds <= parent.MaxCpuSeconds &&
		b.MaxLifetimeExtension <= parent.MaxLifetimeExtension &&
		b.MaxEgressBytes <= parent.MaxEgressBytes
}

// claims is the canonical, signed view of one link in a token's chain. The whole
// chain of claims is embedded in the token so Verify can walk it and enforce the
// never-widen invariant between adjacent links structurally.
type claims struct {
	SandboxID string  `json:"sandboxID"`
	Budget    Budget  `json:"budget"`
	Scopes    []Scope `json:"scopes"`
}

func (c claims) hasScope(sc Scope) bool {
	for _, s := range c.Scopes {
		if s == sc {
			return true
		}
	}
	return false
}

// Token is a capability token: an immutable chain of attenuation links plus the
// HMAC caveat tags that authenticate it. The last link is the effective
// capability. The zero value is not a valid token; use Mint or Attenuate.
type Token struct {
	// links is the attenuation history, root first. The effective budget and
	// scopes are links[len-1].
	links []claims
	// tags is the HMAC caveat chain. tags[0] authenticates links[0] under the
	// signer root key; tags[i] = HMAC(tags[i-1], links[i]) binds each link to its
	// parent so a link cannot be re-parented or have its claims silently mutated
	// without breaking the chain. Unexported so a struct print never leaks it.
	tags [][]byte
}

// SandboxID returns the sandbox this token authorizes (stable across the chain).
func (t Token) SandboxID() string {
	if len(t.links) == 0 {
		return ""
	}
	return t.links[len(t.links)-1].SandboxID
}

// Budget returns the token's effective capability budget (the last link).
func (t Token) Budget() Budget {
	if len(t.links) == 0 {
		return Budget{}
	}
	return t.links[len(t.links)-1].Budget
}

// Scopes returns the token's effective scope set (the last link), sorted.
func (t Token) Scopes() []Scope {
	if len(t.links) == 0 {
		return nil
	}
	return t.links[len(t.links)-1].Scopes
}

// HasScope reports whether the token's effective scope set carries sc.
func (t Token) HasScope(sc Scope) bool {
	if len(t.links) == 0 {
		return false
	}
	return t.links[len(t.links)-1].hasScope(sc)
}

// Depth is the number of attenuation links (1 for a root token).
func (t Token) Depth() int { return len(t.links) }

// Signer holds the HMAC key used to seal and verify tokens. One Signer (one key)
// authenticates the whole forkd token surface; rotation is a key swap.
type Signer struct {
	key []byte
}

// minKeyLen is the minimum HMAC key length we accept: 16 bytes (128 bits) is the
// floor for a keyed-MAC secret.
const minKeyLen = 16

// NewSigner returns a Signer over key. The key is a secret; it is copied so the
// caller's slice is not retained, and it is never logged.
func NewSigner(key []byte) (*Signer, error) {
	if len(key) < minKeyLen {
		return nil, fmt.Errorf("captoken: signing key must be at least %d bytes", minKeyLen)
	}
	cp := make([]byte, len(key))
	copy(cp, key)
	return &Signer{key: cp}, nil
}

// canonicalize sorts and de-duplicates a scope slice so encoding is stable.
func canonicalize(scopes []Scope) []Scope {
	seen := map[Scope]struct{}{}
	out := make([]Scope, 0, len(scopes))
	for _, s := range scopes {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func encodeClaims(c claims) ([]byte, error) {
	c.Scopes = canonicalize(c.Scopes)
	b, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("captoken: encode claims: %w", err)
	}
	return b, nil
}

// rootTag derives the tag authenticating the root link under the signer key. The
// literal label domain-separates the root tag from any link tag.
func (s *Signer) rootTag(c claims) ([]byte, error) {
	enc, err := encodeClaims(c)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte("captoken-root-v1\x00"))
	mac.Write(enc)
	return mac.Sum(nil), nil
}

// linkTag chains a child link onto its parent tag: the parent tag is the MAC
// key, so a child cannot be re-parented onto a different (wider) parent without
// producing a different tag.
func (s *Signer) linkTag(parentTag []byte, c claims) ([]byte, error) {
	enc, err := encodeClaims(c)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, parentTag)
	mac.Write([]byte("captoken-link-v1\x00"))
	mac.Write(enc)
	return mac.Sum(nil), nil
}

// Mint creates a fresh root token for a sandbox with the given budget and
// scopes. The root token has no parent: it defines the widest capability the
// sandbox will ever hold.
func Mint(s *Signer, sandboxID string, budget Budget, scopes []Scope) (Token, error) {
	if s == nil {
		return Token{}, errors.New("captoken: nil signer")
	}
	if sandboxID == "" {
		return Token{}, errors.New("captoken: empty sandbox id")
	}
	root := claims{SandboxID: sandboxID, Budget: budget, Scopes: canonicalize(scopes)}
	tag, err := s.rootTag(root)
	if err != nil {
		return Token{}, err
	}
	return Token{links: []claims{root}, tags: [][]byte{tag}}, nil
}

// Attenuate derives a child token from parent. The child's budget is the
// element-wise minimum of the parent's budget and requested (so it never widens
// any field), and the child's scopes are the intersection of the parent's scopes
// and requested scopes (so it never gains a scope). The parent's chain is
// extended by one link bound to the parent tag. A self-initiated fork uses the
// parent's REMAINING budget (parent.Budget already net of spend) as the parent
// here, so the child can never exceed budget-minus-spend.
func Attenuate(s *Signer, parent Token, requested Budget, requestedScopes []Scope) (Token, error) {
	if s == nil {
		return Token{}, errors.New("captoken: nil signer")
	}
	if len(parent.tags) == 0 || len(parent.links) == 0 {
		return Token{}, errors.New("captoken: parent token is unsealed")
	}
	parentClaims := parent.links[len(parent.links)-1]
	child := claims{
		SandboxID: parentClaims.SandboxID,
		Budget:    parentClaims.Budget.Intersect(requested),
		Scopes:    intersectScopes(parentClaims.Scopes, requestedScopes),
	}
	tag, err := s.linkTag(parent.tags[len(parent.tags)-1], child)
	if err != nil {
		return Token{}, err
	}
	return Token{
		links: append(append([]claims{}, parent.links...), child),
		tags:  append(append([][]byte{}, parent.tags...), tag),
	}, nil
}

// intersectScopes returns the scopes present in both sets, canonicalized. The
// result is a subset of parent, which is the never-widen guarantee at the scope
// level.
func intersectScopes(parent, requested []Scope) []Scope {
	want := map[Scope]struct{}{}
	for _, s := range requested {
		want[s] = struct{}{}
	}
	var out []Scope
	for _, s := range parent {
		if _, ok := want[s]; ok {
			out = append(out, s)
		}
	}
	return canonicalize(out)
}

// Verify checks a token end to end. It walks the embedded link chain and, at
// every link, verifies BOTH:
//
//   - HMAC integrity: the recomputed tag matches the recorded tag in constant
//     time, so no link's claims were mutated after sealing and the right key
//     signed it; and
//   - the never-widen invariant: each non-root link's budget fits within its
//     parent's and its scope set is a subset of its parent's, and it names the
//     same sandbox.
//
// A forged wider child therefore fails even if its own HMAC re-seals cleanly,
// because Verify rejects the widening structurally between the (embedded) parent
// and child claims. This is the load-bearing property: attenuation can never
// widen, and no holder of the key can make it.
func Verify(s *Signer, t Token) error {
	if s == nil {
		return errors.New("captoken: nil signer")
	}
	if len(t.links) == 0 || len(t.tags) != len(t.links) {
		return errors.New("captoken: malformed or unsealed token")
	}
	rootTag, err := s.rootTag(t.links[0])
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(t.tags[0], rootTag) != 1 {
		return errors.New("captoken: root tag mismatch")
	}
	for i := 1; i < len(t.links); i++ {
		parent, child := t.links[i-1], t.links[i]
		wantTag, err := s.linkTag(t.tags[i-1], child)
		if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare(t.tags[i], wantTag) != 1 {
			return fmt.Errorf("captoken: link %d tag mismatch", i)
		}
		if !child.Budget.fitsWithin(parent.Budget) {
			return fmt.Errorf("captoken: link %d widens budget beyond parent", i)
		}
		for _, sc := range child.Scopes {
			if !parent.hasScope(sc) {
				return fmt.Errorf("captoken: link %d widens scope %q beyond parent", i, sc)
			}
		}
		if child.SandboxID != parent.SandboxID {
			return fmt.Errorf("captoken: link %d re-parents to a different sandbox", i)
		}
	}
	return nil
}

// reseal recomputes the chain tags for a token after its claims were mutated. It
// exists for tests that forge a wider child and re-sign it: a resealed forged
// token still fails Verify because the never-widen check between the embedded
// parent and the widened child claims rejects it structurally, proving the
// property holds even against an attacker who knows the HMAC key. It is
// unexported: production code never reseals a token in place.
func (s *Signer) reseal(t Token) (Token, error) {
	if len(t.links) == 0 {
		return Token{}, errors.New("captoken: unsealed token")
	}
	for i := range t.links {
		t.links[i].Scopes = canonicalize(t.links[i].Scopes)
	}
	tags := make([][]byte, len(t.links))
	tag, err := s.rootTag(t.links[0])
	if err != nil {
		return Token{}, err
	}
	tags[0] = tag
	for i := 1; i < len(t.links); i++ {
		tag, err := s.linkTag(tags[i-1], t.links[i])
		if err != nil {
			return Token{}, err
		}
		tags[i] = tag
	}
	t.tags = tags
	return t, nil
}

// wireToken is the serialized form. Each link's claims and tag are carried so a
// holder can present the full chain for Verify.
type wireToken struct {
	Links []claims `json:"l"`
	Tags  []string `json:"h"`
}

// Serialize encodes a token to its compact wire form. The output is a BEARER
// CREDENTIAL: never log it, never place it in an error, condition, event, or
// host path.
func Serialize(t Token) (string, error) {
	if len(t.links) == 0 || len(t.tags) != len(t.links) {
		return "", errors.New("captoken: cannot serialize an unsealed token")
	}
	w := wireToken{Links: t.links}
	for _, tag := range t.tags {
		w.Tags = append(w.Tags, base64.RawURLEncoding.EncodeToString(tag))
	}
	b, err := json.Marshal(w)
	if err != nil {
		return "", fmt.Errorf("captoken: serialize: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Parse decodes a wire token. It does NOT verify; the caller must call Verify
// before trusting any field. Parse errors never echo the token value.
func Parse(encoded string) (Token, error) {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return Token{}, errors.New("captoken: token is not valid base64")
	}
	var w wireToken
	if err := json.Unmarshal(raw, &w); err != nil {
		return Token{}, errors.New("captoken: token is not valid JSON")
	}
	if len(w.Links) == 0 || len(w.Tags) != len(w.Links) {
		return Token{}, errors.New("captoken: token chain is malformed")
	}
	t := Token{}
	for _, l := range w.Links {
		l.Scopes = canonicalize(l.Scopes)
		t.links = append(t.links, l)
	}
	for _, tag := range w.Tags {
		b, err := base64.RawURLEncoding.DecodeString(tag)
		if err != nil {
			return Token{}, errors.New("captoken: caveat tag is not valid base64")
		}
		t.tags = append(t.tags, b)
	}
	return t, nil
}

func subFloor(a, b int64) int64 {
	if b >= a {
		return 0
	}
	return a - b
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
