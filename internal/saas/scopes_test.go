package saas

import (
	"context"
	"errors"
	"testing"
)

// TestScopelessKeyFailsClosed is the MANDATORY security property: a key stored
// with NO scopes recorded satisfies NOTHING. There are no genuine pre-scopes
// records to preserve (api_keys.scopes is NOT NULL DEFAULT '{}' from the first
// migration and enforcement shipped with the api-key feature), so an empty scope
// set is an inert key, not a legacy full-access one. Defaulting it to full access
// would silently promote every empty-scope key (which the console can mint) to an
// admin credential. Failing closed matches how the gateway already treats such a
// key today.
func TestScopelessKeyFailsClosed(t *testing.T) {
	scopeless := ApiKey{ID: "k1", OrgID: "org-a"} // Scopes is nil.
	for _, required := range []string{ScopeReadOnly, ScopeExecute, ScopeLifecycle, ScopeAdmin, ScopeSandboxes} {
		if scopeless.HasScope(required) {
			t.Errorf("a scopeless key must satisfy NOTHING; it satisfied %q (fail-open)", required)
		}
	}
	// An empty (non-nil) slice is the same case.
	empty := ApiKey{ID: "k2", OrgID: "org-a", Scopes: []string{}}
	if empty.HasScope(ScopeLifecycle) || empty.HasScope(ScopeAdmin) {
		t.Error("a key with an empty scope slice must also fail closed")
	}
}

// TestLegacySandboxesScopeGrantsResourceOps: existing keys carry the legacy
// "sandboxes" full-lifecycle scope (the onboarding default). It must keep
// satisfying read, execute, and lifecycle so no existing key is broken by the
// finer scope split. It does NOT grant admin (keys/billing): a resource key was
// never a management credential.
func TestLegacySandboxesScopeGrantsResourceOps(t *testing.T) {
	k := ApiKey{Scopes: []string{ScopeSandboxes}}
	for _, s := range []string{ScopeReadOnly, ScopeExecute, ScopeLifecycle} {
		if !k.HasScope(s) {
			t.Errorf("legacy sandboxes scope must satisfy %q", s)
		}
	}
	if k.HasScope(ScopeAdmin) {
		t.Error("legacy sandboxes scope must NOT grant admin")
	}
}

// TestScopeImplications pins the implication graph. read is the floor: any
// resource scope satisfies it so a key that can act on a sandbox can always list
// and status it (no dead end). The reverse never holds, and execute and
// lifecycle are orthogonal to each other so a browser-safe or CI-safe key can
// grant one without the other.
func TestScopeImplications(t *testing.T) {
	cases := []struct {
		held     string
		required string
		want     bool
	}{
		{ScopeReadOnly, ScopeReadOnly, true},
		{ScopeReadOnly, ScopeExecute, false},
		{ScopeReadOnly, ScopeLifecycle, false},
		{ScopeReadOnly, ScopeAdmin, false},

		{ScopeExecute, ScopeReadOnly, true}, // execute implies read
		{ScopeExecute, ScopeExecute, true},
		{ScopeExecute, ScopeLifecycle, false},
		{ScopeExecute, ScopeAdmin, false},

		{ScopeLifecycle, ScopeReadOnly, true}, // lifecycle implies read
		{ScopeLifecycle, ScopeExecute, false}, // but NOT execute
		{ScopeLifecycle, ScopeLifecycle, true},
		{ScopeLifecycle, ScopeAdmin, false},

		{ScopeAdmin, ScopeReadOnly, false}, // admin is orthogonal to resource access
		{ScopeAdmin, ScopeExecute, false},
		{ScopeAdmin, ScopeLifecycle, false},
		{ScopeAdmin, ScopeAdmin, true},
	}
	for _, c := range cases {
		k := ApiKey{Scopes: []string{c.held}}
		if got := k.HasScope(c.required); got != c.want {
			t.Errorf("key{%s}.HasScope(%s) = %v, want %v", c.held, c.required, got, c.want)
		}
	}
}

// TestCreateKeyDefaultsToNonAdminScopes: a key minted with NO scopes at mint
// time defaults to the resource set (read, execute, lifecycle) and MUST NOT carry
// admin. Minting is routine, so the default must never hand out a management
// credential; admin is granted only when explicitly requested. The persisted
// record carries the explicit default scopes so listings show them.
func TestCreateKeyDefaultsToNonAdminScopes(t *testing.T) {
	store := NewMemStore()
	newTestOrg(t, store, "org-a")
	svc := NewKeyService(store)
	created, err := svc.CreateKey(context.Background(), CreateKeyRequest{OrgID: "org-a", Name: "default"})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	for _, s := range []string{ScopeReadOnly, ScopeExecute, ScopeLifecycle} {
		if _, err := svc.Verify(context.Background(), created.RawKey, s); err != nil {
			t.Errorf("a default key must satisfy the resource scope %q, got %v", s, err)
		}
	}
	// The load-bearing assertion: the default is NOT an admin credential.
	if _, err := svc.Verify(context.Background(), created.RawKey, ScopeAdmin); err == nil {
		t.Error("a key minted with no scopes must NOT satisfy admin; the default must exclude the management scope")
	}
}

// TestCreateKeyAdminRequiresExplicitRequest: admin is reachable, but only when
// asked for by name, so an operator can still mint a management key deliberately.
func TestCreateKeyAdminRequiresExplicitRequest(t *testing.T) {
	store := NewMemStore()
	newTestOrg(t, store, "org-a")
	svc := NewKeyService(store)
	created, err := svc.CreateKey(context.Background(), CreateKeyRequest{OrgID: "org-a", Name: "mgmt", Scopes: []string{ScopeAdmin}})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if _, err := svc.Verify(context.Background(), created.RawKey, ScopeAdmin); err != nil {
		t.Errorf("an explicitly admin-scoped key must satisfy admin, got %v", err)
	}
}

// TestCreateKeyRejectsUnknownScope: a typo'd scope at mint is rejected with an
// actionable error rather than silently minting a key that grants nothing (a
// dead end). The known scope vocabulary is closed.
func TestCreateKeyRejectsUnknownScope(t *testing.T) {
	store := NewMemStore()
	newTestOrg(t, store, "org-a")
	svc := NewKeyService(store)
	_, err := svc.CreateKey(context.Background(), CreateKeyRequest{OrgID: "org-a", Scopes: []string{"reed"}})
	if !errors.Is(err, ErrUnknownScope) {
		t.Fatalf("CreateKey(unknown scope) err = %v, want ErrUnknownScope", err)
	}
}

// TestVerifyExecuteScopeCannotCreate: an execute-scoped key (the CI-safe or
// browser-safe key that may exec in existing sandboxes) cannot create, fork, or
// terminate; those require the lifecycle scope.
func TestVerifyExecuteScopeCannotCreate(t *testing.T) {
	store := NewMemStore()
	newTestOrg(t, store, "org-a")
	svc := NewKeyService(store)
	created, err := svc.CreateKey(context.Background(), CreateKeyRequest{OrgID: "org-a", Scopes: []string{ScopeExecute}})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if _, err := svc.Verify(context.Background(), created.RawKey, ScopeExecute); err != nil {
		t.Errorf("execute key must satisfy the execute scope, got %v", err)
	}
	if _, err := svc.Verify(context.Background(), created.RawKey, ScopeReadOnly); err != nil {
		t.Errorf("execute key must satisfy read (no dead end), got %v", err)
	}
	if _, err := svc.Verify(context.Background(), created.RawKey, ScopeLifecycle); !errors.Is(err, ErrKeyScope) {
		t.Errorf("execute key must be refused the lifecycle scope, got %v", err)
	}
}
