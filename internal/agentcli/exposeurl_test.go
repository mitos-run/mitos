package agentcli

import (
	"strings"
	"testing"
)

func TestBuildExposeURL(t *testing.T) {
	tests := []struct {
		name        string
		label       string
		domain      string
		wantURL     string
		wantErrFrag string
	}{
		{
			name:    "valid label and domain",
			label:   "openclaw",
			domain:  "mitos.app",
			wantURL: "https://openclaw.mitos.app/",
		},
		{
			name:        "empty label",
			label:       "",
			domain:      "mitos.app",
			wantErrFrag: "label",
		},
		{
			name:        "empty domain",
			label:       "openclaw",
			domain:      "",
			wantErrFrag: "domain",
		},
		{
			name:        "reserved label api",
			label:       "api",
			domain:      "mitos.app",
			wantErrFrag: "reserved",
		},
		{
			name:    "uppercase label normalized to lowercase",
			label:   "OpenClaw",
			domain:  "mitos.app",
			wantURL: "https://openclaw.mitos.app/",
		},
		{
			name:        "dotted label rejected",
			label:       "foo.bar",
			domain:      "mitos.app",
			wantErrFrag: "label",
		},
		{
			name:        "label with invalid chars",
			label:       "foo_bar",
			domain:      "mitos.app",
			wantErrFrag: "label",
		},
		{
			name:        "label too long",
			label:       strings.Repeat("a", 64),
			domain:      "mitos.app",
			wantErrFrag: "label",
		},
		{
			name:    "label at max length 63",
			label:   strings.Repeat("a", 63),
			domain:  "mitos.app",
			wantURL: "https://" + strings.Repeat("a", 63) + ".mitos.app/",
		},
		{
			name:        "label starting with hyphen",
			label:       "-bad",
			domain:      "mitos.app",
			wantErrFrag: "label",
		},
		{
			name:        "label ending with hyphen",
			label:       "bad-",
			domain:      "mitos.app",
			wantErrFrag: "label",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := BuildExposeURL(tc.label, tc.domain)
			if tc.wantErrFrag != "" {
				if err == nil {
					t.Fatalf("BuildExposeURL(%q, %q) = %q, want error containing %q", tc.label, tc.domain, got, tc.wantErrFrag)
				}
				if !strings.Contains(err.Error(), tc.wantErrFrag) {
					t.Fatalf("BuildExposeURL(%q, %q) error = %q, want containing %q", tc.label, tc.domain, err.Error(), tc.wantErrFrag)
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildExposeURL(%q, %q) error = %v, want nil", tc.label, tc.domain, err)
			}
			if got != tc.wantURL {
				t.Fatalf("BuildExposeURL(%q, %q) = %q, want %q", tc.label, tc.domain, got, tc.wantURL)
			}
		})
	}
}

func TestDefaultExposeDomain(t *testing.T) {
	t.Run("env set", func(t *testing.T) {
		t.Setenv("MITOS_EXPOSE_DOMAIN", "example.com")
		if got := DefaultExposeDomain(); got != "example.com" {
			t.Fatalf("DefaultExposeDomain() = %q, want %q", got, "example.com")
		}
	})

	t.Run("env unset", func(t *testing.T) {
		t.Setenv("MITOS_EXPOSE_DOMAIN", "")
		if got := DefaultExposeDomain(); got != "" {
			t.Fatalf("DefaultExposeDomain() = %q, want empty", got)
		}
	})
}
