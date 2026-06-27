package onboarding

import (
	"context"
	"testing"
	"time"

	"mitos.run/mitos/internal/saas"
	"mitos.run/mitos/internal/saas/billing"
)

// newE2EHarness builds a Service with a MemE2ETokenSink wired in.
func newE2EHarness(t *testing.T) (*Service, *MemE2ETokenSink) {
	t.Helper()
	store := saas.NewMemStore()
	clock := staticClock()
	keys := saas.NewKeyService(store, saas.WithClock(clock))
	accounts := saas.NewAccountService(store, keys, saas.WithClock(clock))
	ledger := billing.NewMemCreditLedger()
	email := NewFakeEmailSender()
	sink := NewMemE2ETokenSink()
	n := 0
	tok := 0
	svc := NewService(accounts, store, NewMemPendingStore(), ledger, email,
		WithMode(ModeOpen),
		WithClock(clock),
		WithIDGen(func() string { n++; return "e2e-id-" + string(rune('a'+n)) }),
		WithTokenGen(func() (string, error) { tok++; return "e2e-tok-" + string(rune('0'+tok)), nil }),
		WithE2ETokenSink(sink),
	)
	return svc, sink
}

// staticClock returns a deterministic clock shared across e2e test helpers.
func staticClock() func() time.Time {
	t := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

func TestE2ESinkRecordsTokenOnSignup(t *testing.T) {
	svc, sink := newE2EHarness(t)
	if _, err := svc.SignUp(context.Background(), "qa@e2e.mitos.run"); err != nil {
		t.Fatalf("sign up: %v", err)
	}
	tok, ok := sink.Last("qa@e2e.mitos.run")
	if !ok || tok == "" {
		t.Fatal("sink must record the raw token after signup")
	}
}
