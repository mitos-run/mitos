package console

import (
	"context"
	"sort"
	"sync"
)

// MemSecretStore is the in-memory SecretStore tested default and the seam the
// real providers (kube-namespaced Secrets, OpenBao) plug into. It retains ONLY
// metadata: the value is consumed to compute the fingerprint and is then
// discarded, so there is no code path that can return a stored value. Every
// method scopes its effect to the supplied org. Safe for concurrent use.
type MemSecretStore struct {
	mu sync.RWMutex
	// byOrg maps orgID -> secret name -> metadata. No plaintext value is held.
	byOrg map[string]map[string]SecretView
}

// NewMemSecretStore returns an empty in-memory secret store.
func NewMemSecretStore() *MemSecretStore {
	return &MemSecretStore{byOrg: map[string]map[string]SecretView{}}
}

// List returns the org's secrets (metadata only), sorted by name.
func (m *MemSecretStore) List(_ context.Context, orgID string) ([]SecretView, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []SecretView{}
	for _, v := range m.byOrg[orgID] {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Put creates or rotates the named secret. The value is used only to compute the
// fingerprint and is never stored. A re-Put bumps the version; the default
// provider is "kube" and mode "copy_in".
func (m *MemSecretStore) Put(_ context.Context, orgID, name, value string) (SecretView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byOrg[orgID] == nil {
		m.byOrg[orgID] = map[string]SecretView{}
	}
	prev := m.byOrg[orgID][name]
	view := SecretView{
		Name:        name,
		OrgID:       orgID,
		Provider:    "kube",
		Mode:        "copy_in",
		Version:     prev.Version + 1,
		Fingerprint: Fingerprint(value),
	}
	m.byOrg[orgID][name] = view
	return view, nil
}

// Delete removes the org's named secret. A name not present for the org (which
// also covers another org's secret) is reported as ErrNotFound.
func (m *MemSecretStore) Delete(_ context.Context, orgID, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byOrg[orgID][name]; !ok {
		return ErrNotFound
	}
	delete(m.byOrg[orgID], name)
	return nil
}
