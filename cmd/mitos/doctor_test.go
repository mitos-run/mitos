package main

import "testing"

func TestDoctorNamespace(t *testing.T) {
	cases := []struct {
		name   string
		global string
		args   []string
		want   string
	}{
		{"default", "", nil, "mitos"},
		{"global only", "ns-a", nil, "ns-a"},
		{"local -n wins over global", "ns-a", []string{"-n", "ns-b"}, "ns-b"},
		{"local --namespace wins", "ns-a", []string{"--namespace", "ns-c"}, "ns-c"},
		{"trailing flag without value ignored", "ns-a", []string{"-n"}, "ns-a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := doctorNamespace(tc.global, tc.args); got != tc.want {
				t.Fatalf("doctorNamespace(%q, %v) = %q, want %q", tc.global, tc.args, got, tc.want)
			}
		})
	}
}
