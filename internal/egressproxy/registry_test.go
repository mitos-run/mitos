package egressproxy

import (
	"net"
	"testing"
)

// The Registry must satisfy the SandboxResolver seam the Proxy consumes.
var _ SandboxResolver = (*Registry)(nil)

func TestRegistryRegisterAndLookup(t *testing.T) {
	r := NewRegistry()
	guest := net.ParseIP("10.200.0.2")
	r.Register(guest, "sbx-1")

	id, ok := r.Lookup(guest)
	if !ok || id != "sbx-1" {
		t.Fatalf("Lookup = %q %v, want sbx-1 true", id, ok)
	}
}

func TestRegistryLookupUnknownGuest(t *testing.T) {
	r := NewRegistry()
	if id, ok := r.Lookup(net.ParseIP("10.200.0.6")); ok {
		t.Fatalf("unregistered guest must not resolve, got %q", id)
	}
}

func TestRegistryDeregister(t *testing.T) {
	r := NewRegistry()
	guest := net.ParseIP("10.200.0.2")
	r.Register(guest, "sbx-1")
	r.Deregister(guest)
	if _, ok := r.Lookup(guest); ok {
		t.Fatal("deregistered guest must not resolve")
	}
}

func TestRegistryReRegisterReplaces(t *testing.T) {
	r := NewRegistry()
	guest := net.ParseIP("10.200.0.2")
	r.Register(guest, "sbx-1")
	r.Register(guest, "sbx-2")
	if id, ok := r.Lookup(guest); !ok || id != "sbx-2" {
		t.Fatalf("re-register must replace, got %q %v", id, ok)
	}
}

func TestRegistryTwoGuestsDistinct(t *testing.T) {
	r := NewRegistry()
	a := net.ParseIP("10.200.0.2")
	b := net.ParseIP("10.200.0.6")
	r.Register(a, "sbx-a")
	r.Register(b, "sbx-b")
	if id, ok := r.Lookup(a); !ok || id != "sbx-a" {
		t.Errorf("guest a = %q %v, want sbx-a", id, ok)
	}
	if id, ok := r.Lookup(b); !ok || id != "sbx-b" {
		t.Errorf("guest b = %q %v, want sbx-b", id, ok)
	}
}
