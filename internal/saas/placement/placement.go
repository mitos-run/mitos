// Package placement is the Phase 0 placement registry (issue #712): the
// operator-defined key and value set a deployment advertises for where a
// resource lives. Hosted Mitos calls the key "region" and lists hosted
// regions; a self-host operator can rename the key to whatever fits their
// fleet (cluster, zone, dc) and list whatever values they run.
//
// Phase 0 is deliberately single-cluster and region-SHAPED, not
// multi-cluster: there is exactly one registry per deployment, loaded once
// at boot from env (see cmd/console), and every value in it resolves to the
// same cluster today. The registry exists so the API, the SDKs, and the
// console UI already speak in placement values before Phase 1 wires a real
// second cluster behind one of them. Nothing here talks to Kubernetes,
// dials a cluster, or picks where a Sandbox actually runs; Registry.Valid is
// pure validation, used by the console handler to reject an unknown value
// with a 400 before it ever reaches the cluster adapter.
package placement

import "strings"

// Value is one placement option a deployment advertises. Name is the wire
// value a client sends (e.g. "fra"); Display is the human-readable label the
// console UI shows next to it (e.g. "Frankfurt (EU)"). Default marks the
// value used when a caller does not specify one; at most one value in a
// Registry should carry Default = true. Available lets a deployment list a
// value as known but not yet selectable (a "coming soon" entry, or a value
// being drained); Registry.Valid rejects any name whose Value is not
// Available, even if the name is present.
type Value struct {
	Name      string `json:"name"`
	Display   string `json:"display"`
	Default   bool   `json:"default"`
	Available bool   `json:"available"`
}

// Registry is the placement key and its allowed values for one deployment.
// Key names the dimension being chosen (hosted: "region"; self-host:
// operator-defined, e.g. "cluster" or "zone"). Values is the ordered list of
// options; order is display order, not priority.
type Registry struct {
	Key    string  `json:"key"`
	Values []Value `json:"values"`
}

// Valid reports whether name names an available value in the registry. An
// empty name, an unknown name, and a known-but-unavailable name all return
// false; callers that want to allow "unspecified means default" must check
// for the empty string themselves before calling Valid.
func (r Registry) Valid(name string) bool {
	if name == "" {
		return false
	}
	for _, v := range r.Values {
		if v.Name == name {
			return v.Available
		}
	}
	return false
}

// Default returns the registry's default value and true, or the zero Value
// and false if no value is marked default.
func (r Registry) Default() (Value, bool) {
	for _, v := range r.Values {
		if v.Default {
			return v, true
		}
	}
	return Value{}, false
}

// DefaultName returns the name of the registry's default value, or "" if
// none is marked default. This is the value stamped on a new org's home
// region and on a sandbox tree root that does not request one explicitly.
func (r Registry) DefaultName() string {
	v, ok := r.Default()
	if !ok {
		return ""
	}
	return v.Name
}

// Multi reports whether the registry has more than one value. The console
// SPA uses this to decide whether a region picker is shown at all: a
// single-value deployment (the Phase 0 default, both hosted and
// self-hosted) sees no picker, only its one value applied silently.
func (r Registry) Multi() bool {
	return len(r.Values) > 1
}

// ParseValues parses a comma-separated list of "name" or "name:display"
// tokens into a Value slice. Surrounding whitespace on each token and around
// the colon is trimmed. A token with no display segment uses the name as its
// display text. The FIRST non-empty token becomes the default value; every
// parsed value is marked Available. An empty or all-empty input returns nil.
func ParseValues(raw string) []Value {
	var out []Value
	for _, tok := range strings.Split(raw, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		name, display, hasDisplay := strings.Cut(tok, ":")
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if hasDisplay {
			display = strings.TrimSpace(display)
		}
		if display == "" {
			display = name
		}
		out = append(out, Value{
			Name:      name,
			Display:   display,
			Default:   len(out) == 0,
			Available: true,
		})
	}
	return out
}

// New builds a Registry with key and values parsed from raw via ParseValues.
// It is the constructor cmd/console uses to build the deployment's registry
// from its two env vars (MITOS_CONSOLE_PLACEMENT_KEY, MITOS_CONSOLE_PLACEMENT_VALUES).
func New(key, raw string) Registry {
	return Registry{Key: key, Values: ParseValues(raw)}
}
