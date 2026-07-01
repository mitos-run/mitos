package main

import (
	"reflect"
	"testing"
)

// TestParseAutoAllowDomains covers the pure parseAutoAllowDomains helper.
// Empty input must yield the default ["mitos.run"]; comma-separated values
// are trimmed and lowercased; blank entries are dropped.
func TestParseAutoAllowDomains(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "empty returns default",
			input: "",
			want:  []string{"mitos.run"},
		},
		{
			name:  "comma separated with mixed case",
			input: "a.com, B.COM",
			want:  []string{"a.com", "b.com"},
		},
		{
			name:  "whitespace and empty entries dropped",
			input: "  foo.com ,  , bar.com  ",
			want:  []string{"foo.com", "bar.com"},
		},
		{
			name:  "single domain no extra whitespace",
			input: "example.com",
			want:  []string{"example.com"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseAutoAllowDomains(tc.input)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseAutoAllowDomains(%q) = %v; want %v", tc.input, got, tc.want)
			}
		})
	}
}
