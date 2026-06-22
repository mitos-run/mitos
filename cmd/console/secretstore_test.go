package main

import "testing"

// TestPrimarySecretProviderPicksFirstKnown asserts the primary provider is the
// first recognized entry in the advertised list, defaulting to kube.
func TestPrimarySecretProviderPicksFirstKnown(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{nil, "kube"},
		{[]string{}, "kube"},
		{[]string{"kube"}, "kube"},
		{[]string{"openbao", "kube"}, "openbao"},
		{[]string{"bogus", "openbao"}, "openbao"},
		{[]string{"bogus"}, "kube"},
	}
	for _, c := range cases {
		if got := primarySecretProvider(c.in); got != c.want {
			t.Errorf("primarySecretProvider(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
