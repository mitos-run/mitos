package saas

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fixedClock returns a clock function pinned to t.
func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// newTestOrg seeds an org so CreateKey (which validates the org exists) succeeds.
func newTestOrg(t *testing.T, store Store, id string) {
	t.Helper()
	if err := store.PutOrg(context.Background(), Organization{ID: id, Name: id}); err != nil {
		t.Fatalf("seed org: %v", err)
	}
}

// TestCreateKeyReturnsRawOnceAndStoresOnlyHash asserts the raw key is prefix
// tagged, the stored record never carries the raw value, and the masked prefix
// reveals no usable secret.
func TestCreateKeyReturnsRawOnceAndStoresOnlyHash(t *testing.T) {
	store := NewMemStore()
	newTestOrg(t, store, "org-a")
	svc := NewKeyService(store)

	got, err := svc.CreateKey(context.Background(), CreateKeyRequest{OrgID: "org-a", Name: "ci", Scopes: []string{ScopeSandboxes}})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if !strings.HasPrefix(got.RawKey, keyPrefix) {
		t.Errorf("raw key %q missing prefix %q", got.RawKey, keyPrefix)
	}
	if got.Record.Hash == "" {
		t.Error("stored record has empty hash")
	}
	if strings.Contains(got.Record.Hash, got.RawKey) {
		t.Error("stored hash contains the raw key value")
	}
	if got.Record.Prefix == got.RawKey {
		t.Error("masked prefix equals the raw key; secret would be exposed")
	}
	if !strings.HasSuffix(got.Record.Prefix, "...") {
		t.Errorf("masked prefix %q is not redacted", got.Record.Prefix)
	}
	// The stored record's hash must not be the raw key, and the raw key must not
	// be retrievable from the store in any form.
	rec, err := store.GetApiKeyByHash(context.Background(), got.Record.Hash)
	if err != nil {
		t.Fatalf("GetApiKeyByHash: %v", err)
	}
	if rec.Hash == got.RawKey {
		t.Error("store returned the raw key as the hash")
	}
}

// TestVerifyAcceptsAGenuineKey is the happy path: a freshly minted key with the
// required scope verifies and resolves to its org.
func TestVerifyAcceptsAGenuineKey(t *testing.T) {
	store := NewMemStore()
	newTestOrg(t, store, "org-a")
	svc := NewKeyService(store)
	created, err := svc.CreateKey(context.Background(), CreateKeyRequest{OrgID: "org-a", Scopes: []string{ScopeSandboxes}})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	res, err := svc.Verify(context.Background(), created.RawKey, ScopeSandboxes)
	if err != nil {
		t.Fatalf("Verify rejected a genuine key: %v", err)
	}
	if res.Key.OrgID != "org-a" {
		t.Errorf("resolved org = %q, want org-a", res.Key.OrgID)
	}
}

// TestVerifyRejectsForgedKey asserts a key that was never issued does not verify.
func TestVerifyRejectsForgedKey(t *testing.T) {
	store := NewMemStore()
	newTestOrg(t, store, "org-a")
	svc := NewKeyService(store)
	if _, err := svc.CreateKey(context.Background(), CreateKeyRequest{OrgID: "org-a", Scopes: []string{ScopeSandboxes}}); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	forged := keyPrefix + "this-was-never-issued-aaaaaaaaaaaaaaaaaaaaaa"
	_, err := svc.Verify(context.Background(), forged, ScopeSandboxes)
	if !errors.Is(err, ErrKeyUnknown) {
		t.Errorf("Verify(forged) err = %v, want ErrKeyUnknown", err)
	}
}

// TestVerifyRejectsMalformedKey asserts a credential without the mitos prefix is
// rejected before any store lookup.
func TestVerifyRejectsMalformedKey(t *testing.T) {
	store := NewMemStore()
	svc := NewKeyService(store)
	for _, bad := range []string{"", "not-a-mitos-key", keyPrefix} {
		if _, err := svc.Verify(context.Background(), bad, ScopeSandboxes); !errors.Is(err, ErrKeyMalformed) {
			t.Errorf("Verify(%q) err = %v, want ErrKeyMalformed", bad, err)
		}
	}
}

// TestVerifyRejectsExpiredKey asserts a key past its expiry does not verify.
func TestVerifyRejectsExpiredKey(t *testing.T) {
	store := NewMemStore()
	newTestOrg(t, store, "org-a")
	base := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	svc := NewKeyService(store, WithClock(fixedClock(base)))
	created, err := svc.CreateKey(context.Background(), CreateKeyRequest{OrgID: "org-a", Scopes: []string{ScopeSandboxes}, TTL: time.Hour})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	// Advance the clock past expiry.
	expired := NewKeyService(store, WithClock(fixedClock(base.Add(2*time.Hour))))
	if _, err := expired.Verify(context.Background(), created.RawKey, ScopeSandboxes); !errors.Is(err, ErrKeyExpired) {
		t.Errorf("Verify(expired) err = %v, want ErrKeyExpired", err)
	}
	// Before expiry it still verifies.
	if _, err := svc.Verify(context.Background(), created.RawKey, ScopeSandboxes); err != nil {
		t.Errorf("Verify before expiry: %v", err)
	}
}

// TestVerifyRejectsRevokedKey asserts a revoked key does not verify.
func TestVerifyRejectsRevokedKey(t *testing.T) {
	store := NewMemStore()
	newTestOrg(t, store, "org-a")
	svc := NewKeyService(store)
	created, err := svc.CreateKey(context.Background(), CreateKeyRequest{OrgID: "org-a", Scopes: []string{ScopeSandboxes}})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if err := store.RevokeApiKey(context.Background(), created.Record.ID, time.Now()); err != nil {
		t.Fatalf("RevokeApiKey: %v", err)
	}
	if _, err := svc.Verify(context.Background(), created.RawKey, ScopeSandboxes); !errors.Is(err, ErrKeyRevoked) {
		t.Errorf("Verify(revoked) err = %v, want ErrKeyRevoked", err)
	}
}

// TestVerifyRejectsWrongScope asserts a key without the required scope is
// refused, and that a read-only key cannot satisfy a sandbox-scope requirement.
func TestVerifyRejectsWrongScope(t *testing.T) {
	store := NewMemStore()
	newTestOrg(t, store, "org-a")
	svc := NewKeyService(store)
	created, err := svc.CreateKey(context.Background(), CreateKeyRequest{OrgID: "org-a", Scopes: []string{ScopeReadOnly}})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if _, err := svc.Verify(context.Background(), created.RawKey, ScopeSandboxes); !errors.Is(err, ErrKeyScope) {
		t.Errorf("Verify(wrong scope) err = %v, want ErrKeyScope", err)
	}
	// The read scope it does carry still verifies.
	if _, err := svc.Verify(context.Background(), created.RawKey, ScopeReadOnly); err != nil {
		t.Errorf("Verify(matching scope): %v", err)
	}
}

// TestOrgAKeyCannotResolveOrgB is the cross-org resolution property at the key
// layer: a key minted for org A resolves to org A and ONLY org A. There is no
// input by which a verifier can make it resolve to org B.
func TestOrgAKeyCannotResolveOrgB(t *testing.T) {
	store := NewMemStore()
	newTestOrg(t, store, "org-a")
	newTestOrg(t, store, "org-b")
	svc := NewKeyService(store)

	keyA, err := svc.CreateKey(context.Background(), CreateKeyRequest{OrgID: "org-a", Scopes: []string{ScopeSandboxes}})
	if err != nil {
		t.Fatalf("CreateKey A: %v", err)
	}
	keyB, err := svc.CreateKey(context.Background(), CreateKeyRequest{OrgID: "org-b", Scopes: []string{ScopeSandboxes}})
	if err != nil {
		t.Fatalf("CreateKey B: %v", err)
	}

	resA, err := svc.Verify(context.Background(), keyA.RawKey, ScopeSandboxes)
	if err != nil {
		t.Fatalf("Verify A: %v", err)
	}
	if resA.Key.OrgID != "org-a" {
		t.Errorf("key A resolved to org %q, want org-a", resA.Key.OrgID)
	}
	resB, err := svc.Verify(context.Background(), keyB.RawKey, ScopeSandboxes)
	if err != nil {
		t.Fatalf("Verify B: %v", err)
	}
	if resB.Key.OrgID != "org-b" {
		t.Errorf("key B resolved to org %q, want org-b", resB.Key.OrgID)
	}
	// The two keys must never collide on hash, so one can never resolve the other.
	if keyA.Record.Hash == keyB.Record.Hash {
		t.Fatal("two distinct keys produced the same hash")
	}
}

// TestCreateKeyRequiresExistingOrg asserts a key cannot be minted for an org
// that does not exist.
func TestCreateKeyRequiresExistingOrg(t *testing.T) {
	store := NewMemStore()
	svc := NewKeyService(store)
	if _, err := svc.CreateKey(context.Background(), CreateKeyRequest{OrgID: "ghost"}); err == nil {
		t.Fatal("CreateKey for a non-existent org succeeded, want error")
	}
}

// TestSaltChangesHash asserts the pepper participates in the hash, so two
// services with different salts produce different stored hashes for the same raw
// key and a dump from one cannot be replayed against the other.
func TestSaltChangesHash(t *testing.T) {
	s1 := NewKeyService(NewMemStore(), WithSalt([]byte("pepper-one")))
	s2 := NewKeyService(NewMemStore(), WithSalt([]byte("pepper-two")))
	raw := keyPrefix + "same-secret-body"
	if s1.hash(raw) == s2.hash(raw) {
		t.Error("different salts produced the same hash")
	}
}
