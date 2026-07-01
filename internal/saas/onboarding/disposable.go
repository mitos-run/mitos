package onboarding

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

//go:embed disposable_domains.json
var disposableJSON []byte

// Disposable reports whether an email domain is a disposable or explicitly blocked
// domain that may not sign up. Construct with NewDisposable or LoadDisposable.
// A nil *Disposable means the check is disabled; every domain is allowed
// (self-host no-op).
type Disposable struct {
	blocked    map[string]struct{}
	staffAllow map[string]struct{}
}

// NewDisposable returns a Disposable seeded with the blocked domain list and
// the staff-allow list. Both slices are lowercased on intake.
func NewDisposable(blocked, staffAllow []string) *Disposable {
	b := make(map[string]struct{}, len(blocked))
	for _, d := range blocked {
		b[strings.ToLower(d)] = struct{}{}
	}
	s := make(map[string]struct{}, len(staffAllow))
	for _, d := range staffAllow {
		s[strings.ToLower(d)] = struct{}{}
	}
	return &Disposable{blocked: b, staffAllow: s}
}

// Blocked reports whether domain is in the blocklist and NOT in the staff-allow
// list. The check is case-insensitive. A nil *Disposable always returns false
// so that an unconfigured checker is a safe no-op.
func (d *Disposable) Blocked(domain string) bool {
	if d == nil {
		return false
	}
	dom := strings.ToLower(domain)
	if _, ok := d.staffAllow[dom]; ok {
		return false
	}
	_, ok := d.blocked[dom]
	return ok
}

// LoadDisposable reads the embedded JSON blocklist and builds a Disposable from
// it plus the optional comma-separated staff-allow list (e.g. the value of
// MITOS_CONSOLE_DISPOSABLE_ALLOW). An empty staffAllowCSV adds no exemptions.
// A parse error returns an error; the caller should warn and leave the check
// disabled (fail open) rather than blocking all signup on a bad file.
func LoadDisposable(staffAllowCSV string) (*Disposable, error) {
	var raw struct {
		Disposable []string `json:"disposable"`
	}
	if err := json.Unmarshal(disposableJSON, &raw); err != nil {
		return nil, fmt.Errorf("disposable: unmarshal embedded JSON: %w", err)
	}
	var staffAllow []string
	for _, p := range strings.Split(staffAllowCSV, ",") {
		if t := strings.TrimSpace(p); t != "" {
			staffAllow = append(staffAllow, t)
		}
	}
	return NewDisposable(raw.Disposable, staffAllow), nil
}
