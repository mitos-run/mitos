package onboarding_test

import (
	"context"
	"testing"
	"time"

	"mitos.run/mitos/internal/saas/onboarding"
)

func TestMemAllowlist_IsAllowed(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	t.Run("auto-allow domain hit is true without any row", func(t *testing.T) {
		al := onboarding.NewMemAllowlist([]string{"mitos.run"})
		ok, err := al.IsAllowed(ctx, "x@mitos.run")
		if err != nil {
			t.Fatalf("IsAllowed: %v", err)
		}
		if !ok {
			t.Fatal("want true for auto-allow domain, got false")
		}
	})

	t.Run("auto-allow domain compare is case-insensitive", func(t *testing.T) {
		al := onboarding.NewMemAllowlist([]string{"mitos.run"})
		ok, err := al.IsAllowed(ctx, "X@MITOS.RUN")
		if err != nil {
			t.Fatalf("IsAllowed: %v", err)
		}
		if !ok {
			t.Fatal("want true for uppercase email on auto-allow domain, got false")
		}
	})

	t.Run("exact allowlist row is true", func(t *testing.T) {
		al := onboarding.NewMemAllowlist(nil)
		if err := al.Add(ctx, "alice@example.com", "test", now); err != nil {
			t.Fatalf("Add: %v", err)
		}
		ok, err := al.IsAllowed(ctx, "alice@example.com")
		if err != nil {
			t.Fatalf("IsAllowed: %v", err)
		}
		if !ok {
			t.Fatal("want true for allowlisted email, got false")
		}
	})

	t.Run("neither domain nor row is false", func(t *testing.T) {
		al := onboarding.NewMemAllowlist([]string{"mitos.run"})
		ok, err := al.IsAllowed(ctx, "bob@other.com")
		if err != nil {
			t.Fatalf("IsAllowed: %v", err)
		}
		if ok {
			t.Fatal("want false for unknown email, got true")
		}
	})

	t.Run("Add then IsAllowed returns true", func(t *testing.T) {
		al := onboarding.NewMemAllowlist(nil)
		if err := al.Add(ctx, "carol@example.com", "note", now); err != nil {
			t.Fatalf("Add: %v", err)
		}
		ok, err := al.IsAllowed(ctx, "carol@example.com")
		if err != nil {
			t.Fatalf("IsAllowed: %v", err)
		}
		if !ok {
			t.Fatal("want true after Add, got false")
		}
	})

	t.Run("Add twice for same email is idempotent", func(t *testing.T) {
		al := onboarding.NewMemAllowlist(nil)
		if err := al.Add(ctx, "dave@example.com", "note", now); err != nil {
			t.Fatalf("first Add: %v", err)
		}
		if err := al.Add(ctx, "dave@example.com", "note", now); err != nil {
			t.Fatalf("second Add: %v", err)
		}
	})
}
