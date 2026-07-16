package main

import (
	"testing"
	"time"
)

// TestSplitNonEmpty pins the checkout-pools flag parsing: comma separation,
// whitespace trimming, and empty items dropped (so a trailing comma or a bare
// value never enables a pool named "").
func TestSplitNonEmpty(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{" , ,", nil},
		{"python", []string{"python"}},
		{"python, node ,", []string{"python", "node"}},
	}
	for _, tc := range cases {
		got := splitNonEmpty(tc.in)
		if len(got) != len(tc.want) {
			t.Fatalf("splitNonEmpty(%q) = %v, want %v", tc.in, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("splitNonEmpty(%q) = %v, want %v", tc.in, got, tc.want)
			}
		}
	}
}

// TestEnvHelpersKeepDefaultsOnGarbage pins that a malformed env value keeps
// the compiled-in default instead of zeroing a limit.
func TestEnvHelpersKeepDefaultsOnGarbage(t *testing.T) {
	t.Setenv("TEST_CHECKOUT_INT", "not-a-number")
	if got := envInt("TEST_CHECKOUT_INT", 7); got != 7 {
		t.Fatalf("envInt(garbage) = %d, want the default 7", got)
	}
	t.Setenv("TEST_CHECKOUT_INT", "3")
	if got := envInt("TEST_CHECKOUT_INT", 7); got != 3 {
		t.Fatalf("envInt(3) = %d, want 3", got)
	}
	t.Setenv("TEST_CHECKOUT_DUR", "soon")
	if got := envDuration("TEST_CHECKOUT_DUR", time.Minute); got != time.Minute {
		t.Fatalf("envDuration(garbage) = %s, want the default 1m", got)
	}
	t.Setenv("TEST_CHECKOUT_DUR", "90s")
	if got := envDuration("TEST_CHECKOUT_DUR", time.Minute); got != 90*time.Second {
		t.Fatalf("envDuration(90s) = %s, want 90s", got)
	}
	if got := envInt("TEST_CHECKOUT_UNSET_KEY", 5); got != 5 {
		t.Fatalf("envInt(unset) = %d, want 5", got)
	}
}
