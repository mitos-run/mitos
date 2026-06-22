package billingprovider

import (
	"context"
	"sync"
)

// OrgCustomers resolves an org to its provider customer id — the direction the
// portal link needs (the webhook needs the inverse, CustomerResolver). A
// durable mapping (recorded at checkout) is a follow-up; MemCustomers is the
// tested default.
type OrgCustomers interface {
	CustomerForOrg(ctx context.Context, orgID string) (customerRef string, ok bool)
}

// MemCustomers is the in-memory bidirectional org↔customer map. It satisfies
// both CustomerResolver (customer→org, for the webhook) and OrgCustomers
// (org→customer, for the portal). Safe for concurrent use.
type MemCustomers struct {
	mu         sync.RWMutex
	byOrg      map[string]string
	byCustomer map[string]string
}

// NewMemCustomers returns an empty bidirectional map.
func NewMemCustomers() *MemCustomers {
	return &MemCustomers{byOrg: map[string]string{}, byCustomer: map[string]string{}}
}

// Link records the org↔customer association (made at checkout/subscription).
func (m *MemCustomers) Link(orgID, customerRef string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byOrg[orgID] = customerRef
	m.byCustomer[customerRef] = orgID
}

// OrgForCustomer implements CustomerResolver.
func (m *MemCustomers) OrgForCustomer(_ context.Context, customerRef string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	o, ok := m.byCustomer[customerRef]
	return o, ok
}

// CustomerForOrg implements OrgCustomers.
func (m *MemCustomers) CustomerForOrg(_ context.Context, orgID string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.byOrg[orgID]
	return c, ok
}
