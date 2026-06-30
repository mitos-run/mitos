package usage_test

import (
	"testing"

	"mitos.run/mitos/internal/usage"
	"mitos.run/mitos/internal/usage/usagestoretest"
)

// TestMemUsageStoreContract runs the shared UsageStore contract against the
// in-memory reference implementation. The durable Postgres store runs the SAME
// contract (internal/saas/pgstore) so the two are proven equivalent.
func TestMemUsageStoreContract(t *testing.T) {
	usagestoretest.RunContract(t, func(t *testing.T) usage.UsageStore {
		t.Helper()
		return usage.NewMemUsageStore()
	})
}
