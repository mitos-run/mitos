// Package dnsproxy implements a controlled DNS resolver for sandbox egress.
//
// A sandbox is allowed to reach a set of DNS names on specific ports. Names
// cannot be enforced by nftables directly (it matches on IP, not on the name a
// guest looked up), so the proxy is the single resolver the guest is allowed to
// query: it resolves only allowlisted names, and for each resolved address it
// pins (address . port) into that sandbox's dynamic nftables set for the
// record's TTL. The guest can then connect to exactly the address the proxy
// resolved, for exactly as long as the answer is valid.
package dnsproxy

import (
	"net"
	"strings"
	"sync"
)

// Allowlist maps a lowercased fully qualified domain name to the set of TCP
// ports the sandbox may reach for that name. The inner map is used as a set:
// the bool value is always true for an allowed port.
type Allowlist map[string]map[int]bool

// Registry maps a sandbox guest IP (the source address of its DNS queries) to
// the names and ports that sandbox may resolve. It is safe for concurrent use:
// the DNS proxy reads it on every query while the daemon registers and
// deregisters sandboxes.
type Registry struct {
	mu      sync.RWMutex
	byGuest map[string]Allowlist
}

// NewRegistry returns an empty Registry ready for use.
func NewRegistry() *Registry {
	return &Registry{byGuest: make(map[string]Allowlist)}
}

// Register records the allowlist for a guest IP. names maps a DNS name to the
// ports allowed for it; names are lowercased and any trailing dot is dropped so
// lookups match regardless of how the guest spells the query. Registering the
// same guest again replaces its allowlist.
func (r *Registry) Register(guestIP net.IP, names map[string][]int) {
	al := make(Allowlist, len(names))
	for name, ports := range names {
		key := canonicalName(name)
		set := make(map[int]bool, len(ports))
		for _, p := range ports {
			set[p] = true
		}
		al[key] = set
	}
	r.mu.Lock()
	r.byGuest[guestIP.String()] = al
	r.mu.Unlock()
}

// Deregister removes a guest's allowlist. Subsequent lookups for that guest
// return ok=false so its queries are refused.
func (r *Registry) Deregister(guestIP net.IP) {
	r.mu.Lock()
	delete(r.byGuest, guestIP.String())
	r.mu.Unlock()
}

// Lookup returns the allowed ports for a name queried by a guest. A query name
// matches an entry when EITHER:
//
//   - the entry equals the query exactly (case-insensitive, trailing-dot
//     tolerant), OR
//   - the entry is a wildcard "*.D" and the query is a subdomain of D: it ends
//     with ".D" and has a non-empty label before that ".D" (the anchor rule).
//
// The wildcard match is a LITERAL anchored suffix check (no regex): a wildcard
// "*.D" matches "<label>.D" for a non-empty <label> and nothing else. It never
// matches the apex D, never a look-alike whose tail merely resembles D
// (notexample.com vs example.com), and never a name that contains D only as a
// non-suffix label (example.com.evil.com). When a name matches both an exact
// and a wildcard entry, the returned port set is the UNION of both. ok is false
// when the guest is not registered or no entry matches.
func (r *Registry) Lookup(guestIP net.IP, name string) (ports []int, ok bool) {
	key := canonicalName(name)
	if key == "" {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	al, found := r.byGuest[guestIP.String()]
	if !found {
		return nil, false
	}

	union := make(map[int]bool)
	matched := false
	for entry, set := range al {
		if !nameMatchesEntry(key, entry) {
			continue
		}
		matched = true
		for p := range set {
			union[p] = true
		}
	}
	if !matched {
		return nil, false
	}
	ports = make([]int, 0, len(union))
	for p := range union {
		ports = append(ports, p)
	}
	return ports, true
}

// nameMatchesEntry reports whether the canonical query name matches a registry
// entry under the exact-or-anchored-wildcard rule. Both query and entry are
// already canonical (lowercased, trailing dot stripped); entry may be a
// wildcard of the form "*.D".
func nameMatchesEntry(query, entry string) bool {
	suffix, isWildcard := strings.CutPrefix(entry, "*.")
	if !isWildcard {
		return query == entry
	}
	// Anchored suffix match: the query must end with "." + suffix and have a
	// non-empty label before it. strings.TrimSuffix on ".suffix" leaves the
	// label(s); a non-empty remainder is the required non-empty leading label.
	dotSuffix := "." + suffix
	label, ok := strings.CutSuffix(query, dotSuffix)
	if !ok {
		return false
	}
	// label must be non-empty (rejects ".D" with an empty label and the apex D,
	// which would leave label == "" after cutting, or not cut at all).
	return label != ""
}

// canonicalName lowercases a DNS name and strips a single trailing dot so the
// registry keys and query names compare equal regardless of FQDN spelling.
func canonicalName(name string) string {
	return strings.TrimSuffix(strings.ToLower(name), ".")
}
