package quota

import (
	"errors"
	"strings"
	"testing"

	"mitos.run/mitos/internal/apierr"
)

// A quota denial is the most likely failure a hosted user hits (for example a
// fork(n) that would exceed the plan's concurrent-sandbox cap). Per the
// LLM-legible-error rule (issue #28) and the no-dead-ends journey rule, each
// distinct quota denial must carry its OWN cause and an actionable remediation,
// not collapse into one opaque "exceeded a hosted-plan quota" with no way
// forward.
func TestQuotaDenialsCarrySpecificRemediation(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode string
		// substrings the remediation must contain so the caller knows what to do.
		wantRemediation []string
	}{
		{
			name:            "concurrency",
			err:             ErrConcurrencyExceeded,
			wantCode:        string(apierr.CodeQuotaExceeded),
			wantRemediation: []string{"terminate", "upgrade"},
		},
		{
			name:            "aggregate",
			err:             ErrAggregateExceeded,
			wantCode:        string(apierr.CodeQuotaExceeded),
			wantRemediation: []string{"upgrade"},
		},
		{
			name:            "size",
			err:             ErrSandboxTooLarge,
			wantCode:        string(apierr.CodeQuotaExceeded),
			wantRemediation: []string{"smaller"},
		},
	}

	seenCauses := map[string]string{}
	for _, tc := range cases {
		env := envelopeFor(tc.err)
		if env.Code != tc.wantCode {
			t.Errorf("%s: code = %q, want %q", tc.name, env.Code, tc.wantCode)
		}
		if strings.TrimSpace(env.Remediation) == "" {
			t.Errorf("%s: remediation is empty; a quota denial must tell the caller how to proceed", tc.name)
		}
		low := strings.ToLower(env.Remediation)
		for _, want := range tc.wantRemediation {
			if !strings.Contains(low, want) {
				t.Errorf("%s: remediation %q does not mention %q", tc.name, env.Remediation, want)
			}
		}
		if strings.TrimSpace(env.Cause) == "" {
			t.Errorf("%s: cause is empty", tc.name)
		}
		if prev, ok := seenCauses[env.Cause]; ok {
			t.Errorf("%s and %s share the same cause %q; each quota type must be distinguishable", tc.name, prev, env.Cause)
		}
		seenCauses[env.Cause] = tc.name
	}
}

// Rate-limit and suspension denials keep their own envelopes and are not
// mislabeled as quota_exceeded.
func TestNonQuotaDenialsKeepTheirEnvelopes(t *testing.T) {
	if env := envelopeFor(ErrRateLimited); env.Code != string(apierr.CodeRateLimited) {
		t.Errorf("rate-limit code = %q, want %q", env.Code, apierr.CodeRateLimited)
	}
	if env := envelopeFor(ErrSuspended); env.Code != string(apierr.CodeForbidden) {
		t.Errorf("suspended code = %q, want %q", env.Code, apierr.CodeForbidden)
	}
	if env := envelopeFor(errors.New("some other error")); env.Code != string(apierr.CodeQuotaExceeded) {
		t.Errorf("default code = %q, want %q", env.Code, apierr.CodeQuotaExceeded)
	}
}
