package onboarding

import "testing"

func TestCanonicalEmail(t *testing.T) {
	t.Parallel()

	type tc struct {
		input   string
		want    string
		wantOK  bool
	}

	cases := []tc{
		// Gmail: dots removed, plus-tag dropped, domain folded.
		{input: "U.s.e.r+x@Gmail.com", want: "user@gmail.com", wantOK: true},
		// googlemail.com -> gmail.com with dots stripped.
		{input: "user@googlemail.com", want: "user@gmail.com", wantOK: true},
		// Gmail dot-equivalence: a.b and ab map to the same identity.
		{input: "a.b@gmail.com", want: "ab@gmail.com", wantOK: true},
		{input: "ab@gmail.com", want: "ab@gmail.com", wantOK: true},
		// Non-Gmail: dots are significant, only plus-tag is dropped.
		{input: "First.Last+promo@Outlook.com", want: "first.last@outlook.com", wantOK: true},
		// Non-Gmail plain address.
		{input: "plain@example.com", want: "plain@example.com", wantOK: true},
		// Non-Gmail plus-tag dropped.
		{input: "user+news@fastmail.com", want: "user@fastmail.com", wantOK: true},
		// Malformed inputs must return ("", false).
		{input: "not-an-email", want: "", wantOK: false},
		{input: "", want: "", wantOK: false},
		{input: "a@", want: "", wantOK: false},
		{input: "@b.com", want: "", wantOK: false},
		// Plus-only local on Gmail: stripping the plus-tag leaves an empty local.
		{input: "+x@gmail.com", want: "", wantOK: false},
	}

	for _, c := range cases {
		c := c
		t.Run(c.input, func(t *testing.T) {
			t.Parallel()
			got, ok := canonicalEmail(c.input)
			if ok != c.wantOK {
				t.Errorf("canonicalEmail(%q) ok=%v, want %v", c.input, ok, c.wantOK)
			}
			if got != c.want {
				t.Errorf("canonicalEmail(%q) = %q, want %q", c.input, got, c.want)
			}
		})
	}
}
