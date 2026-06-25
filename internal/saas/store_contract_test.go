package saas_test

import (
	"testing"

	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/storetest"
)

// TestMemStoreContract runs the shared saas.Store contract against the in-memory
// MemStore, the reference implementation. The same contract runs against PgStore
// in internal/saas/pgstore so the two are proven behaviorally equivalent.
func TestMemStoreContract(t *testing.T) {
	storetest.RunContract(t, func(t *testing.T) saas.Store {
		t.Helper()
		return saas.NewMemStore()
	})
}
