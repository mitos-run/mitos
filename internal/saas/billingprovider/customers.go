package billingprovider

import (
	"context"
	"sync"
)

// OrgCustomers resolves an org to its provider customer id: the direction the
// portal and top-up links need (the webhook needs the inverse,
// CustomerResolver). An unknown org is ("", false, nil); a store failure is an
// error the caller must surface, never treat as not-linked.
type OrgCustomers interface {
	CustomerForOrg(ctx context.Context, orgID string) (customerRef string, ok bool, err error)
}

// Customers is the full bidirectional org <-> customer store seam: both lookup
// directions plus Link, the write recorded when the org's checkout or
// subscription first names its provider customer. MemCustomers is the dev
// fallback; pgstore.PgCustomers is the durable implementation (issue #614).
type Customers interface {
	CustomerResolver
	OrgCustomers
	Link(ctx context.Context, orgID, customerRef string) error
}

// MemCustomers is the in-memory bidirectional org <-> customer map. It
// satisfies Customers (and therefore CustomerResolver and OrgCustomers). Safe
// for concurrent use. DEV ONLY: it survives no restart; production wires the
// durable pgstore.PgCustomers.
type MemCustomers struct {
	mu         sync.RWMutex
	byOrg      map[string]string
	byCustomer map[string]string
}

// NewMemCustomers returns an empty bidirectional map.
func NewMemCustomers() *MemCustomers {
	return &MemCustomers{byOrg: map[string]string{}, byCustomer: map[string]string{}}
}

// compile-time assertion that MemCustomers satisfies the full customers seam.
var _ Customers = (*MemCustomers)(nil)

// Link records the org <-> customer association (made at checkout or
// subscription). Relinking either side replaces the stale entry in both
// directions, matching the durable store's last-write-wins semantics.
func (m *MemCustomers) Link(_ context.Context, orgID, customerRef string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.byOrg[orgID]; ok {
		delete(m.byCustomer, old)
	}
	if old, ok := m.byCustomer[customerRef]; ok {
		delete(m.byOrg, old)
	}
	m.byOrg[orgID] = customerRef
	m.byCustomer[customerRef] = orgID
	return nil
}

// OrgForCustomer implements CustomerResolver.
func (m *MemCustomers) OrgForCustomer(_ context.Context, customerRef string) (string, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	o, ok := m.byCustomer[customerRef]
	return o, ok, nil
}

// CustomerForOrg implements OrgCustomers.
func (m *MemCustomers) CustomerForOrg(_ context.Context, orgID string) (string, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.byOrg[orgID]
	return c, ok, nil
}
