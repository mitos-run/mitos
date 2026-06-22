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

// TestSecretNamespaceForOrg asserts the org→namespace mapping uses the
// configured prefix.
func TestSecretNamespaceForOrg(t *testing.T) {
	t.Setenv("MITOS_CONSOLE_SECRET_NAMESPACE_PREFIX", "")
	if got := secretNamespaceFor("abc"); got != "mitos-org-abc" {
		t.Errorf("default namespace = %q, want mitos-org-abc", got)
	}
	t.Setenv("MITOS_CONSOLE_SECRET_NAMESPACE_PREFIX", "tenant-")
	if got := secretNamespaceFor("abc"); got != "tenant-abc" {
		t.Errorf("prefixed namespace = %q, want tenant-abc", got)
	}
}
