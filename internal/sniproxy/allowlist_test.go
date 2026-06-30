package sniproxy

import (
	"net"
	"testing"

	"mitos.run/mitos/internal/dnsproxy"
)

// TestRegistryAllowlistReusesMatcher proves the SNI allowlist decision is the
// SAME per-sandbox domain allowlist the DNS proxy enforces: exact match, the
// anchored-wildcard rule, the per-name port set, source attribution by guest IP,
// and the empty-SNI deny, all delegated to dnsproxy.Registry (issue #47), not a
// new matcher.
func TestRegistryAllowlistReusesMatcher(t *testing.T) {
	guest := net.ParseIP("10.0.0.7")
	other := net.ParseIP("10.0.0.8")

	reg := dnsproxy.NewRegistry()
	reg.Register(guest, map[string][]int{
		"api.example.com":   {443},
		"*.svc.example.net": {443},
		"plain.example.com": {80},
	})

	al := RegistryAllowlist{Registry: reg}

	cases := []struct {
		name    string
		srcIP   net.IP
		sni     string
		port    int
		allowed bool
	}{
		{"exact match on allowed port", guest, "api.example.com", 443, true},
		{"exact match case-insensitive", guest, "API.Example.Com", 443, true},
		{"exact match wrong port", guest, "api.example.com", 80, false},
		{"wildcard one label", guest, "a.svc.example.net", 443, true},
		{"wildcard multi label", guest, "a.b.svc.example.net", 443, true},
		{"wildcard apex not matched", guest, "svc.example.net", 443, false},
		{"wildcard lookalike not matched", guest, "evilsvc.example.net", 443, false},
		{"unlisted name", guest, "evil.com", 443, false},
		{"http name not on 443", guest, "plain.example.com", 443, false},
		{"http name on 80", guest, "plain.example.com", 80, true},
		{"unregistered guest", other, "api.example.com", 443, false},
		{"empty SNI denied", guest, "", 443, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := al.Allowed(tc.srcIP, tc.sni, tc.port); got != tc.allowed {
				t.Fatalf("Allowed(%s, %q, %d) = %v, want %v", tc.srcIP, tc.sni, tc.port, got, tc.allowed)
			}
		})
	}
}

func TestRegistryAllowlistNilRegistryDenies(t *testing.T) {
	al := RegistryAllowlist{Registry: nil}
	if al.Allowed(net.ParseIP("10.0.0.7"), "api.example.com", 443) {
		t.Fatal("nil registry must deny (fail closed)")
	}
}
