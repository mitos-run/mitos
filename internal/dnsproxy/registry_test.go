package dnsproxy

import (
	"net"
	"sort"
	"testing"
)

func TestRegistryLookupExactMatch(t *testing.T) {
	r := NewRegistry()
	guest := net.ParseIP("10.200.0.2")
	r.Register(guest, map[string][]int{"egress.test": {8080, 443}})

	ports, ok := r.Lookup(guest, "egress.test")
	if !ok {
		t.Fatal("expected egress.test to be allowed")
	}
	sort.Ints(ports)
	if len(ports) != 2 || ports[0] != 443 || ports[1] != 8080 {
		t.Errorf("ports = %v, want [443 8080]", ports)
	}
}

func TestRegistryLookupCaseAndTrailingDot(t *testing.T) {
	r := NewRegistry()
	guest := net.ParseIP("10.200.0.2")
	r.Register(guest, map[string][]int{"Egress.Test": {8080}})

	if _, ok := r.Lookup(guest, "EGRESS.TEST."); !ok {
		t.Error("expected case-insensitive match with trailing dot")
	}
	if _, ok := r.Lookup(guest, "egress.test"); !ok {
		t.Error("expected lowercase match")
	}
}

func TestRegistryLookupUnknownNameAndGuest(t *testing.T) {
	r := NewRegistry()
	guest := net.ParseIP("10.200.0.2")
	r.Register(guest, map[string][]int{"egress.test": {8080}})

	if _, ok := r.Lookup(guest, "other.test"); ok {
		t.Error("unknown name must not be allowed")
	}
	if _, ok := r.Lookup(net.ParseIP("10.200.0.6"), "egress.test"); ok {
		t.Error("unregistered guest must not be allowed")
	}
}

func TestRegistryDeregister(t *testing.T) {
	r := NewRegistry()
	guest := net.ParseIP("10.200.0.2")
	r.Register(guest, map[string][]int{"egress.test": {8080}})
	r.Deregister(guest)
	if _, ok := r.Lookup(guest, "egress.test"); ok {
		t.Error("deregistered guest must not be allowed")
	}
}

// TestRegistryWildcardMatch is the security-critical bypass suite for the
// anchored suffix matcher. A wildcard entry *.D must match a subdomain of D
// (a non-empty label before .D) and ONLY a subdomain: never the apex D itself,
// never a look-alike (notexample.com, evilexample.com, xexample.com), never a
// name that merely contains D as a non-suffix label (example.com.evil.com),
// and never the empty or empty-label name. Each row is an explicit assertion;
// the MUST NOT rows are the deliverable.
func TestRegistryWildcardMatch(t *testing.T) {
	r := NewRegistry()
	guest := net.ParseIP("10.200.0.2")
	r.Register(guest, map[string][]int{"*.example.com": {443}})

	cases := []struct {
		query string
		want  bool
		why   string
	}{
		// MUST match: a non-empty label before .example.com.
		{"a.example.com", true, "single subdomain label"},
		{"a.b.example.com", true, "multi-label subdomain"},
		{"A.EXAMPLE.COM", true, "case-insensitive subdomain"},
		{"a.example.com.", true, "trailing dot tolerated"},
		{"DEEP.sub.Example.Com.", true, "case + trailing dot + multi-label"},

		// MUST NOT match: the bypass cases.
		{"example.com", false, "apex must not match the wildcard"},
		{"example.com.", false, "apex with trailing dot must not match"},
		{"notexample.com", false, "look-alike prefix must not match"},
		{"evilexample.com", false, "look-alike prefix must not match"},
		{"xexample.com", false, "single-char look-alike must not match"},
		{"example.com.evil.com", false, "D as a non-suffix label must not match"},
		{"a.example.com.evil.com", false, "subdomain of D under another suffix must not match"},
		{"", false, "empty name must not match"},
		{".example.com", false, "empty label before .D must not match"},
		{"other.com", false, "unrelated name must not match"},
	}
	for _, c := range cases {
		_, ok := r.Lookup(guest, c.query)
		if ok != c.want {
			t.Errorf("Lookup(%q) = %v, want %v (%s)", c.query, ok, c.want, c.why)
		}
	}
}

// TestRegistryWildcardPortsPreserved asserts a wildcard match returns that
// entry's allowed ports.
func TestRegistryWildcardPortsPreserved(t *testing.T) {
	r := NewRegistry()
	guest := net.ParseIP("10.200.0.2")
	r.Register(guest, map[string][]int{"*.example.com": {443, 8443}})

	ports, ok := r.Lookup(guest, "api.example.com")
	if !ok {
		t.Fatal("expected api.example.com to match *.example.com")
	}
	sort.Ints(ports)
	if len(ports) != 2 || ports[0] != 443 || ports[1] != 8443 {
		t.Errorf("ports = %v, want [443 8443]", ports)
	}
}

// TestRegistryExactNotSubdomain asserts an EXACT entry matches exactly and not
// its subdomains: an exact entry is not implicitly a wildcard.
func TestRegistryExactNotSubdomain(t *testing.T) {
	r := NewRegistry()
	guest := net.ParseIP("10.200.0.2")
	r.Register(guest, map[string][]int{"example.com": {443}})

	if _, ok := r.Lookup(guest, "example.com"); !ok {
		t.Error("exact entry must match itself")
	}
	if _, ok := r.Lookup(guest, "a.example.com"); ok {
		t.Error("exact entry must NOT match a subdomain")
	}
}

// TestRegistryExactAndWildcardCoexistUnionPorts asserts an exact and a wildcard
// entry coexist, and a name matching BOTH gets the union of their ports.
func TestRegistryExactAndWildcardCoexistUnionPorts(t *testing.T) {
	r := NewRegistry()
	guest := net.ParseIP("10.200.0.2")
	// api.example.com matches both the exact entry (443) and the wildcard (8443).
	r.Register(guest, map[string][]int{
		"api.example.com": {443},
		"*.example.com":   {8443},
	})

	ports, ok := r.Lookup(guest, "api.example.com")
	if !ok {
		t.Fatal("expected api.example.com to match")
	}
	sort.Ints(ports)
	if len(ports) != 2 || ports[0] != 443 || ports[1] != 8443 {
		t.Errorf("union ports = %v, want [443 8443]", ports)
	}

	// A different subdomain matches only the wildcard.
	wports, ok := r.Lookup(guest, "other.example.com")
	if !ok {
		t.Fatal("expected other.example.com to match the wildcard")
	}
	if len(wports) != 1 || wports[0] != 8443 {
		t.Errorf("wildcard-only ports = %v, want [8443]", wports)
	}

	// The apex matches neither.
	if _, ok := r.Lookup(guest, "example.com"); ok {
		t.Error("apex must match neither the exact subdomain entry nor the wildcard")
	}
}

func TestRegistryTwoGuestsDistinct(t *testing.T) {
	r := NewRegistry()
	a := net.ParseIP("10.200.0.2")
	b := net.ParseIP("10.200.0.6")
	r.Register(a, map[string][]int{"egress.test": {8080}})
	r.Register(b, map[string][]int{"other.test": {443}})

	if _, ok := r.Lookup(a, "egress.test"); !ok {
		t.Error("guest a should allow egress.test")
	}
	if _, ok := r.Lookup(a, "other.test"); ok {
		t.Error("guest a should not allow other.test")
	}
	if _, ok := r.Lookup(b, "egress.test"); ok {
		t.Error("guest b should not allow egress.test")
	}
}
