package billing

import (
	"context"
	"sync"
)

// MemSpendCapStore is the in-memory SpendCapStore. Safe for concurrent use.
type MemSpendCapStore struct {
	mu  sync.RWMutex
	out map[string]SpendCap
}

// NewMemSpendCapStore returns an empty spend-cap store.
func NewMemSpendCapStore() *MemSpendCapStore {
	return &MemSpendCapStore{out: map[string]SpendCap{}}
}

func (s *MemSpendCapStore) Get(_ context.Context, orgID string) (SpendCap, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.out[orgID]
	return c, ok, nil
}

func (s *MemSpendCapStore) Set(_ context.Context, cap SpendCap) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out[cap.OrgID] = cap
	return nil
}

// MemStatusStore is the in-memory StatusStore. An org with no recorded status
// defaults to StatusActive (a new org is in good standing). Safe for concurrent
// use.
type MemStatusStore struct {
	mu  sync.RWMutex
	out map[string]BillingStatus
}

// NewMemStatusStore returns an empty status store.
func NewMemStatusStore() *MemStatusStore {
	return &MemStatusStore{out: map[string]BillingStatus{}}
}

func (s *MemStatusStore) Status(_ context.Context, orgID string) (BillingStatus, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if st, ok := s.out[orgID]; ok {
		return st, nil
	}
	return StatusActive, nil
}

func (s *MemStatusStore) SetStatus(_ context.Context, orgID string, st BillingStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out[orgID] = st
	return nil
}
