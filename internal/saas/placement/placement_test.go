package placement

import "testing"

func TestValueParseValues(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []Value
	}{
		{
			name: "single name only, becomes the default",
			raw:  "default",
			want: []Value{{Name: "default", Display: "default", Default: true, Available: true}},
		},
		{
			name: "name with display",
			raw:  "fra:Frankfurt (EU)",
			want: []Value{{Name: "fra", Display: "Frankfurt (EU)", Default: true, Available: true}},
		},
		{
			name: "multiple values, only the first is default",
			raw:  "fra:Frankfurt (EU),iad:Ashburn (US)",
			want: []Value{
				{Name: "fra", Display: "Frankfurt (EU)", Default: true, Available: true},
				{Name: "iad", Display: "Ashburn (US)", Default: false, Available: true},
			},
		},
		{
			name: "whitespace around tokens is trimmed",
			raw:  " fra : Frankfurt (EU) , iad ",
			want: []Value{
				{Name: "fra", Display: "Frankfurt (EU)", Default: true, Available: true},
				{Name: "iad", Display: "iad", Default: false, Available: true},
			},
		},
		{
			name: "empty tokens are skipped",
			raw:  "fra,,iad",
			want: []Value{
				{Name: "fra", Display: "fra", Default: true, Available: true},
				{Name: "iad", Display: "iad", Default: false, Available: true},
			},
		},
		{
			name: "empty input yields no values",
			raw:  "",
			want: nil,
		},
		{
			// A value name is stamped verbatim as the mitos.run/region label on
			// a Sandbox, so a name that is not a valid Kubernetes label value
			// (spaces, punctuation, over 63 chars) is dropped at parse time
			// rather than accepted and then failing opaquely at create. Only
			// the NAME segment is constrained; Display text is unrestricted.
			name: "invalid label-value names are skipped, display is unconstrained",
			raw:  "fra:Frankfurt (EU),bad name,iad",
			want: []Value{
				{Name: "fra", Display: "Frankfurt (EU)", Default: true, Available: true},
				{Name: "iad", Display: "iad", Default: false, Available: true},
			},
		},
		{
			name: "first token invalid, second valid token becomes default",
			raw:  "us east,iad:Ashburn (US)",
			want: []Value{
				{Name: "iad", Display: "Ashburn (US)", Default: true, Available: true},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseValues(tt.raw)
			if len(got) != len(tt.want) {
				t.Fatalf("ParseValues(%q) = %#v, want %#v", tt.raw, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("ParseValues(%q)[%d] = %#v, want %#v", tt.raw, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestRegistryValid(t *testing.T) {
	r := Registry{Key: "region", Values: []Value{
		{Name: "fra", Display: "Frankfurt (EU)", Default: true, Available: true},
		{Name: "iad", Display: "Ashburn (US)", Default: false, Available: true},
		{Name: "syd", Display: "Sydney (planned)", Default: false, Available: false},
	}}

	if !r.Valid("fra") {
		t.Error("Valid(fra) = false, want true")
	}
	if !r.Valid("iad") {
		t.Error("Valid(iad) = false, want true")
	}
	if r.Valid("syd") {
		t.Error("Valid(syd) = true, want false: not available")
	}
	if r.Valid("xyz") {
		t.Error("Valid(xyz) = true, want false: unknown value")
	}
	if r.Valid("") {
		t.Error("Valid(\"\") = true, want false")
	}
}

func TestRegistryDefaultName(t *testing.T) {
	r := Registry{Key: "region", Values: []Value{
		{Name: "iad", Display: "Ashburn (US)", Default: false, Available: true},
		{Name: "fra", Display: "Frankfurt (EU)", Default: true, Available: true},
	}}
	if got := r.DefaultName(); got != "fra" {
		t.Errorf("DefaultName() = %q, want %q", got, "fra")
	}

	empty := Registry{Key: "region"}
	if got := empty.DefaultName(); got != "" {
		t.Errorf("DefaultName() on empty registry = %q, want empty", got)
	}
}

func TestRegistryMulti(t *testing.T) {
	single := Registry{Key: "cluster", Values: []Value{{Name: "default", Default: true, Available: true}}}
	if single.Multi() {
		t.Error("Multi() = true for a single-value registry, want false")
	}
	multi := Registry{Key: "region", Values: []Value{
		{Name: "fra", Default: true, Available: true},
		{Name: "iad", Available: true},
	}}
	if !multi.Multi() {
		t.Error("Multi() = false for a two-value registry, want true")
	}
}

func TestNew(t *testing.T) {
	r := New("region", "fra:Frankfurt (EU),iad:Ashburn (US)")
	if r.Key != "region" {
		t.Errorf("Key = %q, want region", r.Key)
	}
	if !r.Valid("fra") || !r.Valid("iad") {
		t.Errorf("New registry values not valid: %#v", r)
	}
	if r.DefaultName() != "fra" {
		t.Errorf("DefaultName() = %q, want fra", r.DefaultName())
	}
}
